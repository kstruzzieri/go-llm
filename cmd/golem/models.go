package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// modelsRegistry is the narrow slice of *provider.ModelRegistry that the
// listing needs: profile lookup and tool_call provenance. Checks never go
// through it.
type modelsRegistry interface {
	Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error)
	ExplainToolCall(ctx context.Context, key provider.ModelKey) (provider.ToolCallExplanation, error)
}

// runModels is the `golem models` entry point: resolve the agent chain and
// print each entry's capabilities + tool_call provenance, after checking the
// chain's entries with -probe-all (unforced) or -reprobe (forced). It runs
// under its own interrupt handling: Ctrl-C or SIGTERM cancels a check in
// flight, so nothing more is sent, and prints one "cancelled" line.
func runModels(ctx context.Context, args []string, out, errOut io.Writer) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runModelsCommand(ctx, args, out, errOut)
	if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		_, _ = fmt.Fprintln(errOut, "cancelled")
	}
	return err
}

// runModelsCommand builds the real providerbootstrap deps then delegates
// rendering to runModelsWith.
func runModelsCommand(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		configPath string
		rootFlag   string
		baseURL    string
		baseURLSet bool
		ollamaURL  string
		noProbe    bool
		probeAll   bool
		reprobe    bool
		jsonOut    bool
	)
	fs := flag.NewFlagSet("golem models", flag.ContinueOnError)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&rootFlag, "root", ".", "workspace root (scopes the cap-probe cache path guard)")
	fs.StringVar(&baseURL, "base-url", "", "override the openai-compat backend base URL (server root, without /v1); used as given, disables discovery")
	fs.BoolVar(&noProbe, "no-probe", false, "disable openai-compat backend port discovery; explicit and configured URLs are still used as resolved")
	fs.StringVar(&ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.BoolVar(&probeAll, "probe-all", false, "check every agent-chain entry without fresh answers; prints the plan and results on stderr")
	fs.BoolVar(&reprobe, "reprobe", false, "check every agent-chain entry again, fresh answers included; deletes nothing")
	fs.BoolVar(&jsonOut, "json", false, "emit the configview snapshot as JSON (no probing; excludes -probe-all/-reprobe)")
	var allowDest stringSliceFlag
	fs.Var(&allowDest, "allow-destination", "admit a remote model destination: \"<provider>/<canonical base URL>\" (repeatable; this command never prompts)")
	if err := parseQuietly(fs, args, errOut); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return fmt.Errorf("golem models: %w", err)
	}
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name == "base-url" {
			baseURLSet = true
		}
	})
	if jsonOut && (probeAll || reprobe) {
		return fmt.Errorf("golem models: -json cannot be combined with -probe-all or -reprobe")
	}

	var doc *config.Document
	var cfg *config.Config
	var err error
	if jsonOut {
		doc, err = loadDocumentFor(configPath)
		if err != nil {
			return err
		}
		if doc == nil {
			return renderModelsJSON(out, modelsJSONInput(nil, configview.Inventory{}))
		}
		cfg = doc.Config()
	} else {
		cfg, err = loadConfig(configPath)
		if err != nil {
			return err
		}
	}

	root, err := filepath.Abs(rootFlag)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	// #477: plan and admit BEFORE discovery and bootstrap. -json lists the
	// full inventory, so it activates every configured provider (a recommend
	// route); the normal listing activates the agent route's providers.
	var routes []providerbootstrap.PlannedRoute
	agentRoute, err := providerbootstrap.PlanAgentRoute(cfg)
	if err != nil {
		return err
	}
	if jsonOut {
		routes = []providerbootstrap.PlannedRoute{{UseCase: "agent", Recommend: true}}
	} else {
		routes = []providerbootstrap.PlannedRoute{agentRoute}
	}
	explicitURL, _, err := explicitBaseURL(baseURL, baseURLSet, os.LookupEnv)
	if err != nil {
		return err
	}
	targetKey, _, targetOK := openAICompatTargetFromRoute(cfg, agentRoute)
	ocProv, ocURL := "", ""
	if explicitURL != "" && targetOK {
		ocProv, ocURL = targetKey, explicitURL
	}
	gate, netPlan, adm, err := admitForSubcommand(ctx, cfg, routes,
		providerbootstrap.PlanOptions{CapabilityProbes: !jsonOut},
		allowDest, ollamaURL, ocProv, ocURL, errOut)
	if err != nil {
		return err
	}

	backendRes, err := resolveBackend(ctx, adm.effectiveConfigForDiscovery(cfg), backendResolveOpts{
		flagBaseURL:    baseURL,
		flagSet:        baseURLSet,
		noProbe:        noProbe || jsonOut,
		lookupEnv:      os.LookupEnv,
		prober:         openaicompat.DiscoverBaseURL,
		activeRoute:    &agentRoute,
		guardCandidate: discoveryCandidateGuard(ctx, targetKey),
	})
	if err != nil {
		return err
	}
	if backendRes.source == "discovered" {
		pinned, perr := provider.NewDestination(backendRes.providerKey, backendRes.baseURL)
		if perr != nil {
			return fmt.Errorf("golem models: pin discovered backend: %w", perr)
		}
		if perr := adm.pinLoopback(backendRes.providerKey, pinned); perr != nil {
			return perr
		}
	}

	// The shared store's evidence log holds the answers the listing reads and
	// the checks write, mirroring run()'s handle lifecycle.
	var evidenceLog fingerprint.EvidenceStore
	capHandle, capStoreWarn := openModelsStore(ctx, os.Getenv, root)
	if capHandle != nil {
		evidenceLog = capHandle.Log()
		defer func() { _ = capHandle.Close() }()
	}
	if capStoreWarn != "" {
		_, _ = fmt.Fprintln(errOut, "warning: "+capStoreWarn)
	}
	// Store fully unavailable (not even the in-memory fallback): a check could
	// save nothing. Tell the operator why nothing gets checked, then drop the
	// switches so runModelsWith renders what the runtime and config say.
	if evidenceLog == nil {
		if probeAll || reprobe {
			reason := capStoreWarn
			if reason == "" {
				reason = "no capability-probe store"
			}
			_, _ = fmt.Fprintf(errOut, "cannot probe: capability-probe store unavailable (%s); showing cached provenance only\n", reason)
			probeAll = false
			reprobe = false
		}
	}

	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:                          cfg,
		OllamaURLOverride:               ollamaURL,
		OpenAICompatURLOverrideProvider: backendRes.providerKey,
		OpenAICompatURLOverride:         backendRes.baseURL,
		EvidenceLog:                     evidenceLog,
		DestinationGate:                 gate,
		ActiveProviders:                 netPlan.ActiveProviders,
	})
	if err != nil {
		return fmt.Errorf("bootstrap providers: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	// #477 D8: the agent chain comes from the frozen route, never
	// re-resolved after admission.
	plan := chainPlanFor(agentRoute)
	for _, w := range backendRes.warns {
		_, _ = fmt.Fprintln(errOut, "warning: "+w)
	}

	if jsonOut {
		inv, ierr := buildInventoryFromRegistry(ctx, bundle.Providers, bundle.Models,
			func(bctx context.Context, name string) (context.Context, error) {
				return gate.Bind(bctx, provider.DestinationPurposeModelRefresh, name)
			})
		if ierr != nil {
			return ierr
		}
		return renderModelsJSON(out, modelsJSONInput(doc, inv))
	}

	var checks *modelsCheck
	if probeAll || reprobe {
		checks = &modelsCheck{cfg: bundle.Config, store: evidenceLog, listings: bundle.Listings, policy: adm.grantedPolicy(), force: reprobe,
			memoryOnly: capHandle.Status() == fingerprint.StoreMemoryOnly}
	}
	resolveEndpoint := newPreflightEndpointResolver(bundle.Config, ollamaURL, backendRes.providerKey, backendRes.diagSource())
	return runModelsWith(ctx, bundle.Models, plan.chain, bundle.Config, resolveEndpoint, checks, out, errOut)
}

// runModelsWith renders the models listing against injectable deps so the
// output logic is unit-testable without standing up providerbootstrap. reg
// supplies lookup + provenance; resolveEndpoint feeds the same connectivity
// diagnostics the run() preflight uses on a lookup error. A non-nil checks runs
// first, so the listing shows what it wrote. The caller's cancellation is the
// only error: it stops the checks and the listing alike.
func runModelsWith(
	ctx context.Context,
	reg modelsRegistry,
	chain []string,
	cfg *config.Config,
	resolveEndpoint endpointResolver,
	checks *modelsCheck,
	out, errOut io.Writer,
) error {
	if len(chain) == 0 {
		_, _ = fmt.Fprintln(out, "no defaults.agent configured; the run will route to the model recommendation.")
		_, _ = fmt.Fprintln(out, "configure a defaults.agent chain in models.json to list per-entry capabilities.")
		return nil
	}
	if checks != nil {
		if err := checks.run(ctx, chain, errOut); err != nil {
			return err
		}
	}

	now := time.Now()
	for _, sel := range chain {
		key, parsed := parseSelector(sel)
		if !parsed {
			// Bare selector: no single provider/model to explain per-entry.
			_, _ = fmt.Fprintf(out, "%s\t(bare selector; resolved across providers at route time)\n", sel)
			continue
		}

		// Lookup first so a connectivity/config failure renders the same
		// diagnostic the startup preflight uses, rather than a bare zero row.
		if _, lerr := reg.Lookup(ctx, key); lerr != nil {
			if err := ctx.Err(); err != nil {
				return err // cancelled: not a connectivity failure, and no entry after it is read
			}
			ep, epOK := resolvePreflightEndpoint(resolveEndpoint, key.Provider)
			_, _ = fmt.Fprintf(out, "%s\t%s\n", sel, preflightConnectivityWarn("agent", sel, key.Provider, ep, epOK, lerr))
			continue
		}

		exp, err := reg.ExplainToolCall(ctx, key)
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			ep, epOK := resolvePreflightEndpoint(resolveEndpoint, key.Provider)
			_, _ = fmt.Fprintf(out, "%s\t%s\n", sel, preflightConnectivityWarn("agent", sel, key.Provider, ep, epOK, err))
			continue
		}
		_, _ = fmt.Fprintln(out, renderModelLine(sel, key, exp, cfg, now))
	}
	return nil
}

// renderModelLine formats one chain entry: selector, capabilities, and
// tool_call provenance. A MISSING entry appends the shared-key annotation (when
// another role declares caps for the same key) and a remediation hint line.
func renderModelLine(sel string, key provider.ModelKey, exp provider.ToolCallExplanation, cfg *config.Config, now time.Time) string {
	line := fmt.Sprintf("%s\tcaps=%s\t%s", sel, exp.Caps.String(), toolCallField(exp, now))
	if opts := samplingDefaultsForKey(cfg, key); opts != nil {
		if field := samplingDefaultsField(opts); field != "" {
			line += "\t" + field
		}
	}

	if role, ok := declaringRole(cfg, key); ok {
		line += fmt.Sprintf(" (declared by model entry %q)", role)
	}
	if !exp.Has {
		line += "\n  " + remediationHint(sel)
	}
	return line
}

func samplingDefaultsForKey(cfg *config.Config, key provider.ModelKey) *config.SamplingOptions {
	if cfg == nil {
		return nil
	}
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		model := cfg.Models[role]
		if model.Provider == key.Provider && model.Name == key.Model && model.Options != nil {
			return model.Options
		}
	}
	return nil
}

func samplingDefaultsField(opts *config.SamplingOptions) string {
	parts := make([]string, 0, 3)
	if opts.Temperature != nil {
		parts = append(parts, fmt.Sprintf("temperature=%g", *opts.Temperature))
	}
	if opts.TopP != nil {
		parts = append(parts, fmt.Sprintf("top_p=%g", *opts.TopP))
	}
	if opts.TopK != nil {
		parts = append(parts, fmt.Sprintf("top_k=%d", *opts.TopK))
	}
	if len(parts) == 0 {
		return ""
	}
	return "sampling=" + strings.Join(parts, ",")
}

// toolCallField renders the tool_call verdict + its provenance source. A
// present bit reads "tool_call=yes (<source>)"; an absent bit reads
// "tool_call=MISSING" plus the last probe verdict + relative age when known.
func toolCallField(exp provider.ToolCallExplanation, now time.Time) string {
	if exp.Has {
		return fmt.Sprintf("tool_call=yes (%s)", exp.Source)
	}
	if exp.State != "" {
		return fmt.Sprintf("tool_call=MISSING (probe: %s, %s)", exp.State, relAge(exp.TestedAt, now))
	}
	return "tool_call=MISSING (" + exp.Source + ")"
}

// relAge renders a coarse "Nh ago" / "Nm ago" / "Ns ago" age for a probe
// timestamp. Zero time yields "unknown time".
func relAge(t, now time.Time) string {
	if t.IsZero() {
		return "unknown time"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	}
}

// declaringRole walks cfg.Models for a config entry whose explicit
// Capabilities declare this key (same provider+name). It names the entry that
// declared the caps, which may be the chain entry itself (self-declaration) --
// still useful, since it tells the operator the caps are config-declared rather
// than catalog/probe-derived. Returns the first such role name found, or
// ok=false when no entry declares explicit caps for the key.
func declaringRole(cfg *config.Config, key provider.ModelKey) (string, bool) {
	if cfg == nil {
		return "", false
	}
	for role, m := range cfg.Models {
		if len(m.Capabilities) == 0 {
			continue
		}
		if m.Provider == key.Provider && m.Name == key.Model {
			return role, true
		}
	}
	return "", false
}
