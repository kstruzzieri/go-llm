package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// destinationAdmissionConfig assembles a destinationAdmission. Edges come
// from the mode's frozen network plan; AllowFlags are the raw repeatable
// -allow-destination values; PromptYN is the interactive seam (backed by the
// REPL's lineSource in production, absent in noninteractive modes).
type destinationAdmissionConfig struct {
	Gate        *provider.DestinationGate
	Edges       []provider.DestinationEdge
	AllowFlags  []string
	Interactive bool
	PromptYN    func(ctx context.Context, prompt string) (bool, error)
	Out         io.Writer
}

// destinationAdmission is golem's consent surface for #477: it renders the
// decision-complete manifest, collects ONE batch decision for every remote
// destination (D11), installs the resulting generation on the shared gate,
// and owns the destination-grant lifetime (D12) — which is deliberately NOT
// the tool-grant lifetime: /new, /clear, and /resume reset conversations and
// tool grants but leave destination authority standing, while /grants clear
// revokes it atomically and the next goal re-runs this same batch gate.
type destinationAdmission struct {
	gate        *provider.DestinationGate
	manifest    *provider.DestinationManifest
	edges       []provider.DestinationEdge
	allowed     []provider.Destination
	interactive bool
	promptYN    func(ctx context.Context, prompt string) (bool, error)
	out         io.Writer

	mu       sync.Mutex
	admitted bool
	// granted is the policy the current generation was installed with —
	// always an EXACT set (D4), never allow-all: the gate's Narrow carries
	// the policy into later generations, and an allow-all here would
	// silently admit whatever a future manifest adds. Held for /grants
	// display and pinned by test.
	granted provider.DestinationPolicy
	// effectiveCfg is the materialized config the edges derive from, held so
	// discovery consumers read the same URLs the manifest admitted. Set by
	// admitForSubcommand; main.go passes eff.Config() to discovery directly.
	effectiveCfg *config.Config
}

// newDestinationAdmission validates the allow flags and freezes the manifest.
// Both are pure: no I/O and no prompting until ensure.
func newDestinationAdmission(cfg destinationAdmissionConfig) (*destinationAdmission, error) {
	if cfg.Gate == nil {
		return nil, fmt.Errorf("golem: destination admission requires a gate")
	}
	m, err := provider.NewDestinationManifest(cfg.Edges...)
	if err != nil {
		return nil, err
	}
	allowed := make([]provider.Destination, 0, len(cfg.AllowFlags))
	for _, raw := range cfg.AllowFlags {
		d, err := provider.ParseDestination(raw)
		if err != nil {
			// Never echo the raw flag: it may carry the very credential the
			// destination identity refuses to hold.
			return nil, fmt.Errorf("golem: -allow-destination: %w", err)
		}
		allowed = append(allowed, d)
	}
	out := cfg.Out
	if out == nil {
		out = io.Discard
	}
	return &destinationAdmission{
		gate:        cfg.Gate,
		manifest:    m,
		edges:       append([]provider.DestinationEdge(nil), cfg.Edges...),
		allowed:     allowed,
		interactive: cfg.Interactive,
		promptYN:    cfg.PromptYN,
		out:         out,
	}, nil
}

// ensure admits the manifest if it is not already admitted this generation:
// render the manifest, auto-admit when every destination is local or covered
// by the exact allowlist, otherwise collect the one interactive batch
// decision — or fail closed noninteractively, naming an uncovered
// destination, a use case that reaches it, and the exact flag that would
// cover it (I6). Idempotent after success; revoke re-arms it.
func (a *destinationAdmission) ensure(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitted {
		return nil
	}

	a.render(a.out)

	// No explicit local/remote split: DestinationPolicy.Permits auto-admits
	// literal loopback, so firstUncovered can only ever surface a remote —
	// a separate filter would be a second copy of that rule.
	dests := a.manifest.Destinations()
	policy := provider.NewDestinationPolicy(a.allowed...)
	if uncovered := firstUncovered(dests, policy); uncovered != nil {
		if !a.interactive || a.promptYN == nil {
			denial := &provider.DestinationDeniedError{
				Destination: *uncovered,
				Purpose:     a.purposeReaching(*uncovered),
			}
			return fmt.Errorf("%w; pass -allow-destination %q to permit it", denial, uncovered.String())
		}
		ok, err := a.promptYN(ctx, "Allow this session to send data to the remote destinations listed above? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			return &provider.DestinationDeniedError{
				Destination: *uncovered,
				Purpose:     a.purposeReaching(*uncovered),
			}
		}
		// The approval grants exactly the manifest's destination set — not
		// allow-all — plus whatever the flags already named.
		policy = provider.NewDestinationPolicy(append(dests, a.allowed...)...)
	}

	if err := a.gate.Install(policy, a.manifest); err != nil {
		return err
	}
	a.admitted = true
	a.granted = policy
	return nil
}

// revoke atomically drops destination authority (D12: the /grants clear
// path) and re-arms ensure, so the next goal re-runs the same batch gate.
// Conversation resets never call this.
func (a *destinationAdmission) revoke() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gate.Clear()
	a.admitted = false
}

// setPrompt rebinds the interactive seam — the REPL installs its lineSource
// here once it exists, so post-startup re-admissions (after /grants clear)
// run through the same input discipline as every other prompt.
func (a *destinationAdmission) setPrompt(promptYN func(ctx context.Context, prompt string) (bool, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.promptYN = promptYN
	a.interactive = promptYN != nil
}

// pinLoopback re-installs the current generation with one provider's
// destination replaced by the LOOPBACK destination discovery selected (#477
// D11). The granted policy carries over unchanged and the replacement must
// classify local, so no authority is added: the remote set is untouched and
// the new destination auto-admits under any policy. Rebuilding the manifest
// (rather than narrowing) is required because the edges must retarget the
// provider's admitted URL to the one the clients will actually dial —
// leaving the configured URL in place would deny every request to the
// discovered backend.
func (a *destinationAdmission) pinLoopback(providerKey string, d provider.Destination) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.admitted {
		return fmt.Errorf("golem: pin before admission")
	}
	if !d.IsLocal() {
		return fmt.Errorf("golem: refusing to pin non-loopback destination for %q", providerKey)
	}
	if d.Provider() != providerKey {
		return fmt.Errorf("golem: pin destination provider %q does not match %q", d.Provider(), providerKey)
	}
	edges := make([]provider.DestinationEdge, len(a.edges))
	for i, e := range a.edges {
		if e.Destination.Provider() == providerKey {
			e.Destination = d
		}
		edges[i] = e
	}
	m, err := provider.NewDestinationManifest(edges...)
	if err != nil {
		return err
	}
	if err := a.gate.Install(a.granted, m); err != nil {
		return err
	}
	a.manifest = m
	a.edges = edges
	_, _ = fmt.Fprintf(a.out, "destination pinned: %s -> %s (discovered)\n", providerKey, d.BaseURL())
	return nil
}

// purposeReaching returns one purpose whose edge reaches d — deterministic,
// for diagnostics that must name the use case that made the destination
// reachable.
func (a *destinationAdmission) purposeReaching(d provider.Destination) string {
	for _, e := range a.manifest.Edges() {
		if e.Destination == d {
			return e.Purpose
		}
	}
	return ""
}

// firstUncovered returns the first destination the policy does not permit,
// in the manifest's deterministic order. Loopback destinations always pass
// Permits, so only remotes can surface here.
func firstUncovered(dests []provider.Destination, policy provider.DestinationPolicy) *provider.Destination {
	for _, d := range dests {
		if !policy.Permits(d) {
			return &d
		}
	}
	return nil
}

// render writes the consent surface: one line per deduplicated destination,
// grouped local-first, each followed by the purposes that reach it with
// primary/fallback marking. D14 fields only — provider key, canonical URL,
// purpose, marker, locality. Deterministic order.
func (a *destinationAdmission) render(w io.Writer) {
	byDest := make(map[provider.Destination][]provider.DestinationEdge)
	for _, e := range a.manifest.Edges() {
		byDest[e.Destination] = append(byDest[e.Destination], e)
	}
	dests := a.manifest.Destinations()
	sort.SliceStable(dests, func(i, j int) bool {
		if dests[i].IsLocal() != dests[j].IsLocal() {
			return dests[i].IsLocal()
		}
		return dests[i].String() < dests[j].String()
	})

	_, _ = fmt.Fprintln(w, "destinations:")
	for _, d := range dests {
		kind := "remote"
		if d.IsLocal() {
			kind = "local, auto-admitted"
		}
		_, _ = fmt.Fprintf(w, "  %s  %s  (%s)\n", d.Provider(), d.BaseURL(), kind)
		var parts []string
		for _, e := range byDest[d] {
			label := e.Purpose
			if e.IsFallback {
				label += " (fallback)"
			}
			parts = append(parts, label)
		}
		_, _ = fmt.Fprintf(w, "      %s\n", strings.Join(parts, ", "))
	}
}

// startupPromptYN is the pre-REPL consent reader: one line from stdin, read
// byte-at-a-time so no buffered read-ahead can steal bytes from the line
// editor that takes over the same descriptor moments later.
func startupPromptYN(in io.Reader, out io.Writer) func(context.Context, string) (bool, error) {
	return func(_ context.Context, prompt string) (bool, error) {
		_, _ = fmt.Fprint(out, prompt)
		var line []byte
		buf := make([]byte, 1)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				if buf[0] == '\n' {
					break
				}
				line = append(line, buf[0])
			}
			if err != nil {
				return false, err
			}
		}
		answer := strings.ToLower(strings.TrimSpace(string(line)))
		return answer == "y" || answer == "yes", nil
	}
}

// lineSourcePromptYN adapts the REPL lineSource to the admission prompt seam.
// EOF declines rather than admits.
func lineSourcePromptYN(src lineSource) func(context.Context, string) (bool, error) {
	return func(ctx context.Context, prompt string) (bool, error) {
		line, ok, err := src.ReadGoal(ctx, prompt)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

// discoveryCandidateGuard builds the per-candidate guard for the loopback
// scan (#477): each candidate gets its own single-edge gate — the candidate
// destination under the discovery purpose — a guarded no-redirect client
// bound to it, and a context carrying the discovery capability. A candidate
// that is not literal loopback cannot even construct a gate here, so the
// scan structurally cannot leave the host.
func discoveryCandidateGuard(ctx context.Context, providerKey string) func(candidate string) (*http.Client, context.Context, error) {
	return func(candidate string) (*http.Client, context.Context, error) {
		d, err := provider.NewDestination(providerKey, candidate)
		if err != nil {
			return nil, nil, err
		}
		// No explicit IsLocal check: the zero policy below denies every
		// remote destination at Install, so a non-loopback candidate cannot
		// produce a client — proven equivalent by mutation, so the second
		// copy of the rule is deleted rather than tested.
		g := provider.NewDestinationGate()
		m, err := provider.NewDestinationManifest(provider.DestinationEdge{
			Purpose:     provider.DestinationPurposeDiscovery,
			Destination: d,
		})
		if err != nil {
			return nil, nil, err
		}
		var loopbackOnly provider.DestinationPolicy
		if err := g.Install(loopbackOnly, m); err != nil {
			return nil, nil, err
		}
		hc, err := provider.GuardHTTPClient(g, d, &http.Client{Timeout: backendProbeTimeout})
		if err != nil {
			return nil, nil, err
		}
		bctx, err := g.Bind(ctx, provider.DestinationPurposeDiscovery, providerKey)
		if err != nil {
			return nil, nil, err
		}
		return hc, bctx, nil
	}
}

// effectiveConfigForDiscovery returns the config discovery should read.
// The admission was built from the EFFECTIVE config's edges; discovery must
// see the same base URLs, so a caller that only holds the raw config asks
// here rather than re-materializing. Falls back to the raw config when the
// admission carries no effective copy (never the case for subcommands).
func (a *destinationAdmission) effectiveConfigForDiscovery(raw *config.Config) *config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.effectiveCfg != nil {
		return a.effectiveCfg
	}
	return raw
}

// admitForSubcommand is the shared noninteractive admission sequence for the
// semantic one-shot subcommands (models/index/source): materialize, plan,
// render, admit via the exact -allow-destination set — NEVER a prompt (D10;
// these commands do not read stdin for consent). A remote destination outside
// the allowlist fails closed with the diagnostic naming it and the flag that
// would cover it.
func admitForSubcommand(
	ctx context.Context,
	cfg *config.Config,
	routes []providerbootstrap.PlannedRoute,
	opts providerbootstrap.PlanOptions,
	allow []string,
	ollamaOverride, ocProv, ocURL string,
	errOut io.Writer,
) (*provider.DestinationGate, *providerbootstrap.NetworkPlan, *destinationAdmission, error) {
	eff, err := providerbootstrap.Materialize(cfg, ollamaOverride, ocProv, ocURL)
	if err != nil {
		return nil, nil, nil, err
	}
	netPlan, err := providerbootstrap.BuildNetworkPlan(eff, routes, opts)
	if err != nil {
		return nil, nil, nil, err
	}
	gate := provider.NewDestinationGate()
	adm, err := newDestinationAdmission(destinationAdmissionConfig{
		Gate:       gate,
		Edges:      netPlan.Edges,
		AllowFlags: allow,
		Out:        errOut,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	adm.effectiveCfg = eff.Config()
	if err := adm.ensure(ctx); err != nil {
		return nil, nil, nil, err
	}
	return gate, netPlan, adm, nil
}
