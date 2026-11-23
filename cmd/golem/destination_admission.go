package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	// Out receives the consent section until the REPL binds its own writer
	// with setReplOut: stderr at startup and for the subcommands.
	Out io.Writer
	// KeySources maps provider key -> the consent line saying where its key
	// comes from (see keySourceLines); nil renders none.
	KeySources map[string]string
	// NoColor is the command's -no-color; NO_COLOR, TERM=dumb and a
	// non-terminal writer turn color off without it.
	NoColor bool
	// Operation is "" for a session, or the subcommand ("models", "index",
	// "source") whose requests the section describes.
	Operation string
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
	keySources  map[string]string
	noColor     bool
	operation   string

	mu sync.Mutex
	// replOut is the REPL line source's writer once setReplOut binds it; the
	// consent section then renders there, beside the question.
	replOut  io.Writer
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
		d, err := provider.ParseDestinationFlag(raw)
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
		keySources:  cfg.KeySources,
		noColor:     cfg.NoColor,
		operation:   cfg.Operation,
	}, nil
}

// keySourceLines maps each provider that sends a key to the consent line
// naming its source: the environment variable names (never values) or a
// literal. Names come from cfg as loaded, so a later rewrite of a key on an
// effective copy cannot change what the user is told. Keyless providers are
// absent; a nil cfg yields nil.
func keySourceLines(cfg *config.Config) map[string]string {
	if cfg == nil {
		return nil
	}
	lines := make(map[string]string)
	for key, p := range cfg.Providers {
		if names := cfg.APIKeyRefNames(key); len(names) > 0 {
			lines[key] = "sends api_key from $" + strings.Join(names, ", $")
		} else if p.APIKey != "" {
			lines[key] = "sends a literal api_key"
		}
	}
	return lines
}

// ensure admits the manifest if it is not already admitted this generation:
// render the manifest, auto-admit when every destination is local or covered
// by the exact allowlist, otherwise ask the one interactive batch question
// naming every uncovered remote, or fail closed noninteractively, naming an
// uncovered destination, a use case that reaches it, and the exact flag that
// would cover it (I6). A reader that answers on a cancelled context installs
// nothing. Idempotent after success; revoke re-arms it.
func (a *destinationAdmission) ensure(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.admitted {
		return nil
	}

	// At startup the baseline is only the exact allow flags (plus the
	// loopback rule inside Permits): nothing has been granted yet.
	flags := provider.NewDestinationPolicy(a.allowed...)
	renderErr := a.renderForConsent(a.manifest, flags, flags)

	policy, _, err := a.consent(ctx, a.manifest, flags, renderErr)
	if err != nil {
		return err
	}

	if err := a.gate.Install(policy, a.manifest); err != nil {
		return err
	}
	a.admitted = true
	a.granted = policy
	return nil
}

// grantedPolicy returns the destination policy the current generation was
// installed with.
func (a *destinationAdmission) grantedPolicy() provider.DestinationPolicy {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.granted
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

// consent is THE destination-consent decision, shared by ensure and extend so
// the discipline cannot drift between the startup admission and a model
// switch: at most one question for the complete manifest, judged against
// baseline, naming exactly the remotes baseline does not permit: the set a
// yes admits. It reports the policy to publish with and whether approval
// added a remote the baseline did not already permit.
//
// Callers hold a.mu and render m before calling, so the user sees what each
// remote the question names is used for, and pass renderErr, the error that
// rendering returned: a question whose section failed to print is never asked.
func (a *destinationAdmission) consent(ctx context.Context, m *provider.DestinationManifest, baseline provider.DestinationPolicy, renderErr error) (provider.DestinationPolicy, bool, error) {
	// No explicit local/remote split: DestinationPolicy.Permits auto-admits
	// literal loopback, so uncoveredAll can only ever surface remotes; a
	// separate filter would be a second copy of that rule.
	dests := m.Destinations()
	uncovered := uncoveredAll(dests, baseline)
	if len(uncovered) == 0 {
		return baseline, false, nil
	}
	// The diagnostic names the first uncovered remote in manifest order, so
	// its bytes do not depend on how the question sorts.
	denied := &provider.DestinationDeniedError{
		Destination: uncovered[0],
		Purpose:     purposeReaching(m, uncovered[0]),
	}
	// Fail closed without reading anything: noninteractive modes do not ask
	// for consent, they name the destination, a use case that reaches it,
	// and the exact flag that would cover it (I6). consentMode is the same
	// test the rendering used for "needs your OK", so the screen and the
	// decision cannot disagree.
	if a.consentMode() != renderAsking {
		return baseline, false, fmt.Errorf("%w; pass -allow-destination %q to permit it", denied, uncovered[0].String())
	}
	// Fail closed when the section did not print: the user has not seen what
	// a yes admits, and a typed-ahead "y" must not answer for them. In the
	// REPL (replOut is bound only there, beside the line-source prompt) the
	// line typed after the command is still its answer, never a goal, so one
	// line is read and dropped through the same seam; no question is shown and
	// whatever it returns is ignored. At startup and in the subcommands a
	// refusal ends the run and no goal is read, so a discard read there could
	// only wait on stdin with nothing on screen.
	if renderErr != nil {
		if a.replOut != nil {
			_, _ = a.promptYN(ctx, "")
		}
		return baseline, false, fmt.Errorf("%w; the model-server section could not be shown: %w", denied, renderErr)
	}
	ok, err := a.promptYN(ctx, consentQuestion(uncovered))
	if err != nil {
		return baseline, false, err
	}
	// A reader that answers yes on a cancelled context (Ctrl-C racing a
	// newline) has consented to nothing.
	if err := ctx.Err(); err != nil {
		return baseline, false, err
	}
	if !ok {
		return baseline, false, denied
	}
	// The approval grants exactly the manifest's destination set — not
	// allow-all — plus whatever the flags already named.
	return provider.NewDestinationPolicy(append(dests, a.allowed...)...), true, nil
}

// extend admits a candidate model's destinations additively (#376). The
// proposal is the union of the session's stored edges with the candidate
// ones, validated, consented to, and published ONCE — never the old manifest
// first and the candidate after. While a generation is installed the
// publication is an Extend, which keeps the revocation token so a request
// already in flight is not killed by a model switch; after /grants clear
// there is no generation to extend, so the same complete proposal takes the
// Install publication ensure uses.
//
// newGrantAdmitted reports whether publication needed a REMOTE destination
// the prior baseline did not already permit. Everything here runs under the
// same mutex ensure uses, so revoke cannot land between the publication and
// the bookkeeping that records it.
func (a *destinationAdmission) extend(ctx context.Context, edges []provider.DestinationEdge) (newGrantAdmitted bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// The stored edges stay known after a revoke, so the proposal is the
	// complete session reachability graph either way. Validate it BEFORE any
	// publication: a malformed proposal changes nothing.
	m, err := provider.NewDestinationManifest(append(append([]provider.DestinationEdge(nil), a.edges...), edges...)...)
	if err != nil {
		return false, err
	}

	if a.admitted && manifestCovers(a.manifest, m) {
		// The installed generation already carries every proposed edge:
		// re-rendering and re-publishing would be noise, not authority.
		return false, nil
	}

	// The baseline both the prompt and the outcome are judged against. Once
	// revoked, granted is authority the user WITHDREW: reusing it would
	// silently re-admit a remote they cleared, so only the exact flags (and
	// the normal loopback rule inside Permits) survive a revoke.
	flags := provider.NewDestinationPolicy(a.allowed...)
	baseline := flags
	if a.admitted {
		baseline = a.granted
	}

	// Rendered before the decision: the question names the remotes of the
	// PROPOSED manifest that need a yes, and the rendering shows their uses.
	renderErr := a.renderForConsent(m, flags, baseline)

	policy, newGrant, err := a.consent(ctx, m, baseline, renderErr)
	if err != nil {
		return false, err
	}

	// Last look before authority changes hands — and, on a proposal the
	// baseline already covers, the only cancellation guard, because that
	// path asks no question. Cancellation arriving AFTER this point does not
	// undo the grant: publication is what decides.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	publish := a.gate.Extend
	if !a.admitted {
		publish = a.gate.Install
	}
	if err := publish(policy, m); err != nil {
		return false, err
	}
	a.manifest = m
	a.edges = m.Edges()
	a.granted = policy
	a.admitted = true
	return newGrant, nil
}

// manifestCovers reports whether every edge in proposed already appears in
// cur with the same purpose and destination. The fallback marking is display
// metadata, not authority (a reachable fallback is exactly as reachable as a
// primary), so it alone never makes an edge new.
func manifestCovers(cur, proposed *provider.DestinationManifest) bool {
	have := make(map[provider.DestinationEdge]struct{})
	for _, e := range cur.Edges() {
		e.IsFallback = false
		have[e] = struct{}{}
	}
	for _, e := range proposed.Edges() {
		e.IsFallback = false
		if _, ok := have[e]; !ok {
			return false
		}
	}
	return true
}

// setPrompt rebinds the interactive seam — the REPL installs its lineSource
// here once it exists, so post-startup re-admissions (after /grants clear)
// run through the same input discipline as every other prompt. setReplOut
// moves the section those questions sit under onto the same stream.
func (a *destinationAdmission) setPrompt(promptYN func(ctx context.Context, prompt string) (bool, error)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.promptYN = promptYN
	a.interactive = promptYN != nil
}

// setReplOut binds the writer the REPL's line source prints to. Every consent
// section drawn after that (a /model set extension, the re-admission after
// /grants clear) renders there, on the stream its question is asked on, with
// color and width measured on it. Startup admission and the subcommands never
// call it and stay on Out.
func (a *destinationAdmission) setReplOut(out io.Writer) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.replOut = out
}

// renderForConsent draws m before the consent decision on the writer the
// question is asked on (replOut once bound, else Out), asking when a question
// can be asked and refusing otherwise, and returns the write error for consent.
// Callers hold a.mu.
func (a *destinationAdmission) renderForConsent(m *provider.DestinationManifest, flags, baseline provider.DestinationPolicy) error {
	w := a.out
	if a.replOut != nil {
		w = a.replOut
	}
	return renderManifest(w, m, a.keySources, remoteStates(m, flags, baseline, a.consentMode()),
		manifestOpts{style: styleFor(w, a.noColor), operation: a.operation})
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
	_, _ = fmt.Fprintf(a.out, "destination pinned: %s -> %s (discovered)\n", destinationName(providerKey), displayURL(d.BaseURL()))
	return nil
}

// purposeReaching returns one purpose in m whose edge reaches d —
// deterministic, for diagnostics that must name the use case that made the
// destination reachable. It takes the manifest rather than reading the
// admission's, because extend diagnoses a PROPOSED manifest the session has
// not stored yet.
func purposeReaching(m *provider.DestinationManifest, d provider.Destination) string {
	for _, e := range m.Edges() {
		if e.Destination == d {
			return e.Purpose
		}
	}
	return ""
}

// render writes the /grants listing of the session's manifest in style: every
// remote with its current state, plus the cleared note while /grants clear has
// left it unadmitted. Uses and fallback marks are as admitted, not as the
// routes now stand: a route switched away from stays listed, and a fallback a
// switch made primary can still read "(fallback)". It takes a.mu, so the
// states, the cleared note and the manifest come from one moment; ensure and
// extend hold a.mu and render through renderForConsent, never through this.
func (a *destinationAdmission) render(w io.Writer, style consoleStyle) {
	a.mu.Lock()
	defer a.mu.Unlock()
	flags := provider.NewDestinationPolicy(a.allowed...)
	baseline := flags
	if a.admitted {
		baseline = a.granted
	}
	// A listing asks nothing, so a failed write admits nothing; like the rest
	// of the slash command's output, it is not reported.
	_ = renderManifest(w, a.manifest, a.keySources, remoteStates(a.manifest, flags, baseline, renderListing),
		manifestOpts{style: style, cleared: !a.admitted})
}

// consentMode is the render mode for a manifest shown before the consent
// decision: asking when a question can be asked, refusing otherwise.
func (a *destinationAdmission) consentMode() renderMode {
	if a.interactive && a.promptYN != nil {
		return renderAsking
	}
	return renderRefusing
}

// startupPromptYN is the pre-REPL consent reader: one line from stdin, read
// byte-at-a-time so no buffered read-ahead can steal bytes from the line
// editor that takes over the same descriptor moments later. A prompt that
// fails to print is an error, and nothing is read: a typed-ahead answer must
// not reply to a question the user never saw.
func startupPromptYN(in io.Reader, out io.Writer) func(context.Context, string) (bool, error) {
	return func(_ context.Context, prompt string) (bool, error) {
		if _, err := fmt.Fprint(out, prompt); err != nil {
			return false, err
		}
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
		line, ok, err := src.ReadAnswer(ctx, prompt)
		if errors.Is(err, errInterrupted) {
			return false, context.Canceled
		}
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
// would cover it. The section on errOut describes operation (the subcommand's
// name) and honors noColor.
func admitForSubcommand(
	ctx context.Context,
	cfg *config.Config,
	routes []providerbootstrap.PlannedRoute,
	opts providerbootstrap.PlanOptions,
	allow []string,
	ollamaOverride, ocProv, ocURL string,
	operation string,
	noColor bool,
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
		KeySources: keySourceLines(cfg),
		NoColor:    noColor,
		Operation:  operation,
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
