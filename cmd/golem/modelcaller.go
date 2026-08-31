package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// chatStreamer is the minimal slice of *provider.RoutePlan the caller needs;
// mirrors agent.planExecutor (which is unexported, so we redeclare it here).
type chatStreamer interface {
	ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error
}

// chainModelCaller is an agent.ModelCaller that routes strictly down a
// configured fallback chain. When chain is non-empty it pins
// RoutingRequest.PreferredChain + StrictChain so the Router never appends a
// global-recommend tail — a coding agent must know which model answered. When
// chain is empty it falls back to the empty-Model recommend behavior of
// agent.NewRouterModelCaller.
type chainModelCaller struct {
	chain   []string
	useCase string
	route   func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error)
}

// newActiveChainCaller builds the process's ONE orchestrator caller from the
// resolved active chainPlan (#476 D3): the chain and the use case arrive as
// the single value the network plan admitted, so the caller cannot route a
// different use case than the one whose destinations were consented.
func newActiveChainCaller(r *provider.Router, plan chainPlan) agent.ModelCaller {
	return newRouterChainCallerFor(r, plan.chain, plan.useCase)
}

// newRouterChainCallerFor builds a chainModelCaller that routes UseCase useCase
// strictly down chain. An empty useCase is treated as "agent". Used to pin a
// delegate caller to the coding role chain with UseCase "coding".
func newRouterChainCallerFor(r *provider.Router, chain []string, useCase string) agent.ModelCaller {
	return &chainModelCaller{
		chain:   chain,
		useCase: useCase,
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			plan, err := r.Route(ctx, rr)
			if err != nil {
				return nil, err // avoid wrapping a nil *RoutePlan in a non-nil interface
			}
			return plan, nil
		},
	}
}

func (m *chainModelCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	useCase := m.useCase
	if useCase == "" {
		useCase = "agent"
	}
	rr := provider.RoutingRequest{
		UseCase:        useCase,
		Messages:       req.Messages,
		Tools:          req.Tools,
		Options:        req.Options,
		ExpectedOutput: req.Options.NumPredict,
		RequiredCaps:   provider.CapChat | provider.CapStream,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps |= provider.CapToolCall
	}
	if len(m.chain) > 0 {
		rr.PreferredChain = m.chain
		rr.StrictChain = true
	}

	plan, err := m.route(ctx, rr)
	if err != nil {
		return agent.ModelResult{}, err
	}

	var outcome *provider.RouteOutcome
	wrapped, getFinal := provider.Collect(func(chunk provider.ChatResponse) error {
		if chunk.RouteOutcome != nil {
			outcome = chunk.RouteOutcome
		}
		if onToken != nil {
			return onToken(chunk)
		}
		return nil
	})
	execErr := plan.ExecuteChatStream(ctx, wrapped)
	final := getFinal()
	final.RouteOutcome = outcome
	return agent.ModelResult{Response: final, RouteOutcome: outcome}, execErr
}

// capChecker is the narrow subset of *provider.ModelRegistry the preflight needs.
type capChecker interface {
	Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error)
	LookupAny(ctx context.Context, model string) ([]*provider.ModelProfile, error)
	Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error)
}

const toolRouteCaps = provider.CapChat | provider.CapStream | provider.CapToolCall

// toolCallResolver is the narrow slice of *provider.ModelRegistry the preflight
// needs for active capability resolution. A nil resolver disables probing
// (-no-cap-probe or no store), in which case the preflight relies solely on the
// merged (catalog+floor+declared) profile capabilities.
type toolCallResolver interface {
	ResolveToolCall(ctx context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error)
}

// remediationCaps is the exact models.json fix fragment surfaced in remediation
// hints: the capability list a tool-capable model entry must declare.
const remediationCaps = `"capabilities": ["chat","generate","stream","tool_call"]`

// remediationHint renders the models.json fix line for a chain entry that
// resolved without tool_call, naming the selector and the exact capabilities
// fragment the user should add.
func remediationHint(sel string) string {
	return fmt.Sprintf("if it supports function calling, add %s to the model entry for %q in models.json", remediationCaps, sel)
}

// notToolCapableWarn is the bare capability-gap warning for a selector that
// resolved (or matched no provider) without tool_call, with its remediation
// hint. Used wherever a probe cannot or need not run: nil resolver, unknown
// verdict (probing disabled for the provider), and bare-selector no-match.
func notToolCapableWarn(sel string) string {
	return fmt.Sprintf("agent fallback %q is not tool-capable (chat|stream|tool_call); %s", sel, remediationHint(sel))
}

// preflightEndpoint is the discovery endpoint a provider exposes. It is used
// only to build a human-facing connectivity diagnostic, never to make a call.
type preflightEndpoint struct {
	BaseURL    string // provider base_url from config ("" if unknown)
	ModelsPath string // "/v1/models" (openai-compat) or "/api/tags" (ollama)
	Source     string // explicit override source ("-base-url"/"GO_LLM_BASE_URL"); "" for config-sourced URLs
}

// endpointResolver maps a provider name to its discovery endpoint. It returns
// ok=false when the provider is absent or the config is nil, in which case the
// diagnostic falls back to a base_url-free message.
type endpointResolver func(providerName string) (preflightEndpoint, bool)

// httpStatuser is satisfied by *openaicompat.statusError and *ollama.APIError,
// both preserved through ModelRegistry.Lookup's %w wrapping. The interface
// exposes only the numeric code; the status text is reconstructed locally.
type httpStatuser interface{ HTTPStatusCode() int }

// statusLabel renders an HTTP status code as "<code> <text>" (e.g.
// "404 Not Found"), falling back to "HTTP <code>" for codes without a known
// textual status.
func statusLabel(code int) string {
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("HTTP %d", code)
}

// redactBaseURL strips any userinfo (user:pass@) from a base_url before it is
// printed in a diagnostic, so credentials never leak into logs. A string that
// fails to parse and looks userinfo-shaped (contains "@") is replaced wholesale
// rather than printed raw.
//
// This logic is mirrored in provider/openaicompat/discover.go's
// redactURLUserinfo (that package cannot import cmd/golem). Keep the two in
// sync when either changes.
func redactBaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "@") {
			return "<invalid base_url>"
		}
		return raw
	}
	if strings.Contains(raw, "@") && u.User == nil && u.Host == "" {
		return "<invalid base_url>"
	}
	u.User = nil
	return u.String()
}

// newPreflightEndpointResolver builds an endpointResolver from a config. It
// mirrors providerbootstrap's ollama base-URL override (providers.go), which is
// applied to the live client but not written back into the returned Config, so
// the resolver re-applies it to avoid printing a stale ollama base_url under
// `golem -ollama-url ...`. The openai-compat override IS written into the
// bundle config by providerbootstrap, so no URL overlay is needed for it; the
// resolver only stamps the explicit-source label (ocSourceProvider/ocSource)
// so connectivity diagnostics can name a failing -base-url/GO_LLM_BASE_URL.
func newPreflightEndpointResolver(cfg *config.Config, ollamaURLOverride, ocSourceProvider, ocSource string) endpointResolver {
	return func(name string) (preflightEndpoint, bool) {
		if cfg == nil {
			return preflightEndpoint{}, false
		}
		pc, ok := cfg.Providers[name]
		if !ok {
			return preflightEndpoint{}, false
		}
		apiFormat := pc.APIFormat
		if apiFormat == "" {
			apiFormat = "ollama"
		}
		if name == "ollama" && apiFormat == "ollama" && ollamaURLOverride != "" {
			pc.BaseURL = ollamaURLOverride
		}
		path := "/api/tags"
		if apiFormat == "openai-compat" {
			path = "/v1/models"
		}
		ep := preflightEndpoint{BaseURL: pc.BaseURL, ModelsPath: path}
		if name == ocSourceProvider && ocSource != "" {
			ep.Source = ocSource
		}
		return ep, true
	}
}

// resolvePreflightEndpoint guards a nil resolver before invoking it, so callers
// (and tests) can pass nil to mean "no endpoint metadata available."
func resolvePreflightEndpoint(resolve endpointResolver, providerName string) (preflightEndpoint, bool) {
	if resolve == nil {
		return preflightEndpoint{}, false
	}
	return resolve(providerName)
}

func providerDiscoveryFailure(err error) bool {
	var hs httpStatuser
	if errors.As(err, &hs) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &typeErr)
}

// preflightConnectivityWarn builds the bare warning string for a chain entry
// whose registry lookup errored — a connectivity/config failure rather than a
// capability gap. When the provider's endpoint is known it names the provider +
// base_url + discovery path (and the HTTP status, when the underlying error
// carries one). With no endpoint, or when the lookup error is not a discovery
// failure, it surfaces the underlying error verbatim under a neutral
// "provider lookup failed" framing.
// The returned string is bare: the startup renderer prepends "warning: ".
func preflightConnectivityWarn(sel, providerName string, ep preflightEndpoint, epOK bool, lookupErr error) string {
	if !epOK || ep.BaseURL == "" || !providerDiscoveryFailure(lookupErr) {
		return fmt.Sprintf("agent fallback %q: provider lookup failed: %v", sel, lookupErr)
	}
	addr := redactBaseURL(ep.BaseURL)
	if ep.Source != "" {
		// A failing explicit override must be named as such: the fix is the
		// flag/env value the user passed, not models.json.
		addr += " (from " + ep.Source + ")"
	}
	var hs httpStatuser
	if errors.As(lookupErr, &hs) {
		return fmt.Sprintf("agent fallback %q: cannot reach provider %q at %s (GET %s -> %s); check server/base_url",
			sel, providerName, addr, ep.ModelsPath, statusLabel(hs.HTTPStatusCode()))
	}
	return fmt.Sprintf("agent fallback %q: cannot reach provider %q at %s (GET %s): %v",
		sel, providerName, addr, ep.ModelsPath, lookupErr)
}

func profileToolCapable(p *provider.ModelProfile) bool {
	// Capability.Has tests c&flag == flag across all bits, so reusing the
	// toolRouteCaps mask keeps the required-capability set defined in one place.
	return p != nil && p.Caps.Has(toolRouteCaps)
}

func parseSelector(sel string) (provider.ModelKey, bool) {
	pname, model, ok := strings.Cut(sel, "/")
	if !ok {
		return provider.ModelKey{}, false
	}
	return provider.ModelKey{Provider: pname, Model: model}, true
}

// preflightToolCapable verifies the agent chain can route a tool-capable model.
// For each configured entry it classifies the outcome as capable,
// not-tool-capable (resolved but lacks tool_call), or lookup-errored
// (provider unreachable / config failure). It returns a bare warning per
// non-capable entry, and an error only when NO entry — or, for an empty chain,
// the recommend set — can satisfy chat|stream|tool_call. On that failure the
// error INLINES every per-entry diagnostic, because the caller returns on the
// error and would otherwise drop the warnings. Pure registry lookup unless a
// resolver is supplied; never makes a live chat call. resolveEndpoint supplies
// provider base_url + discovery path for connectivity diagnostics; a nil
// resolver disables active capability probing (a nil endpointResolver yields
// base_url-free messages).
//
// Bounded-eager: active probing runs only until the FIRST capable entry. Startup
// proves ONE usable model then stops loading fallbacks (llama-swap load cost);
// unknown entries reached after that resolve lazily at route time, reported here
// as a non-fatal "probed on first use" line.
func preflightToolCapable(ctx context.Context, reg capChecker, chain []string, resolveEndpoint endpointResolver, resolver toolCallResolver) (warnings []string, err error) {
	if len(chain) == 0 {
		return preflightRecommendToolCapable(ctx, reg, resolver)
	}

	capable := 0
	for _, sel := range chain {
		// Probe only until the first capable entry is proven; later unknowns
		// are deferred to route time.
		allowProbe := capable == 0
		ok, warn := evalChainEntry(ctx, reg, sel, resolveEndpoint, resolver, allowProbe)
		if ok {
			capable++
			continue
		}
		warnings = append(warnings, warn)
	}
	if capable == 0 {
		return warnings, fmt.Errorf("golem: tool-capability preflight failed:\n%s", strings.Join(warnings, "\n"))
	}
	return warnings, nil
}

// preflightRecommendToolCapable handles the empty-chain case: no explicit
// fallback chain, so the gate asks the registry to recommend a tool-capable
// model. With no resolver it requires the declared tool_call capability up
// front. With a resolver it relaxes the recommend query (drops CapToolCall so
// undeclared-but-probeable models are considered) and probes candidates,
// passing on the first that already declares tool_call or probes yes.
func preflightRecommendToolCapable(ctx context.Context, reg capChecker, resolver toolCallResolver) ([]string, error) {
	if resolver == nil {
		profs, rerr := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps})
		if rerr != nil {
			return nil, fmt.Errorf("golem: tool-capability preflight (recommend): %w", rerr)
		}
		if len(profs) == 0 {
			return nil, fmt.Errorf("golem: no tool-capable model available (require chat|stream|tool_call)")
		}
		return nil, nil
	}

	profs, rerr := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps &^ provider.CapToolCall})
	if rerr != nil {
		return nil, fmt.Errorf("golem: tool-capability preflight (recommend): %w", rerr)
	}
	// Two passes, deliberately not fused: exhaust the cheap declared-capability
	// check across ALL candidates before doing any probe I/O, so a later
	// already-capable model is preferred over probing an earlier one.
	for _, p := range profs {
		if p.Caps.Has(provider.CapToolCall) {
			return nil, nil
		}
	}
	for _, p := range profs {
		if state, err := resolver.ResolveToolCall(ctx, p.Key); err == nil && state == fingerprint.CapProbeYes {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("golem: no tool-capable model available (require chat|stream|tool_call)")
}

// evalChainEntry classifies one agent-chain selector. It returns capable=true
// when the selector resolves to a tool-capable model; otherwise it returns a
// bare warning string: a connectivity diagnostic when the registry lookup
// errored, or a capability message (possibly a probe outcome) when the model
// resolved without a declared tool_call. allowProbe gates active probing per the
// bounded-eager policy.
func evalChainEntry(ctx context.Context, reg capChecker, sel string, resolveEndpoint endpointResolver, resolver toolCallResolver, allowProbe bool) (capable bool, warning string) {
	if key, parsed := parseSelector(sel); parsed {
		p, lerr := reg.Lookup(ctx, key)
		if lerr != nil {
			ep, epOK := resolvePreflightEndpoint(resolveEndpoint, key.Provider)
			return false, preflightConnectivityWarn(sel, key.Provider, ep, epOK, lerr)
		}
		if profileToolCapable(p) {
			return true, ""
		}
		return classifyToolCapability(ctx, sel, key, resolver, allowProbe)
	}

	// Bare model name: resolve across providers. A bare selector names no single
	// provider, so there is no endpoint to resolve — reuse the connectivity
	// builder's no-endpoint branch (epOK=false) so the message has one source.
	profs, lerr := reg.LookupAny(ctx, sel)
	if lerr != nil {
		return false, preflightConnectivityWarn(sel, "", preflightEndpoint{}, false, lerr)
	}
	for _, p := range profs {
		if profileToolCapable(p) {
			return true, ""
		}
	}
	if len(profs) == 0 {
		// No matching provider: nothing to probe, and no single provider to name.
		return false, notToolCapableWarn(sel)
	}
	warning = notToolCapableWarn(sel)
	for _, p := range profs {
		if p == nil {
			continue
		}
		capable, w := classifyToolCapability(ctx, sel, p.Key, resolver, allowProbe)
		if capable {
			return true, ""
		}
		if w != "" {
			warning = w
		}
	}
	return false, warning
}

// classifyToolCapability resolves a chain entry that looked up cleanly but does
// not declare tool_call. With no resolver, or when probing is not allowed for
// this entry (bounded-eager: a capable entry already found), it reports a
// non-fatal / capability-gap message rather than making a call. Otherwise it
// probes and maps the tri-state verdict to a capable flag + diagnostic.
func classifyToolCapability(ctx context.Context, sel string, key provider.ModelKey, resolver toolCallResolver, allowProbe bool) (bool, string) {
	if resolver == nil {
		return false, notToolCapableWarn(sel)
	}
	if !allowProbe {
		return false, fmt.Sprintf("agent fallback %q: tool capability unknown; probed on first use", sel)
	}
	state, err := resolver.ResolveToolCall(ctx, key)
	switch {
	case err != nil:
		return false, fmt.Sprintf("agent fallback %q: tool-capability probe failed: %v; %s", sel, err, remediationHint(sel))
	case state == fingerprint.CapProbeYes:
		return true, ""
	case state == fingerprint.CapProbeNo:
		return false, fmt.Sprintf("agent fallback %q: model did not produce a tool call when probed; %s", sel, remediationHint(sel))
	case state == fingerprint.CapProbeInconclusive:
		return false, fmt.Sprintf("agent fallback %q: tool-capability probe was inconclusive; declare capabilities to override: %s", sel, remediationHint(sel))
	default: // "" unknown: resolution disabled for this provider (no prober/store)
		return false, notToolCapableWarn(sel)
	}
}
