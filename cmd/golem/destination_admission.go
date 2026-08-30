package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

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
