package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeToolResolver is a fake toolCallResolver: it returns canned probe states
// (or errors) per key and records the order of ResolveToolCall calls so tests
// can assert bounded-eager probing (probe only until the first capable entry).
type fakeToolResolver struct {
	states map[provider.ModelKey]fingerprint.CapProbeState
	errs   map[provider.ModelKey]error
	calls  []provider.ModelKey
}

func (f *fakeToolResolver) ResolveToolCall(ctx context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error) {
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return "", err
	}
	return f.states[key], nil
}

type fakeReg struct {
	byKey      map[provider.ModelKey]provider.Capability
	errByKey   map[provider.ModelKey]error
	errByModel map[string]error
	recommend  []provider.Capability
}

func (f fakeReg) Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	if e, ok := f.errByKey[key]; ok {
		return nil, e
	}
	c, ok := f.byKey[key]
	if !ok {
		return nil, context.Canceled // not-found: any non-nil error
	}
	return &provider.ModelProfile{Caps: c}, nil
}

func (f fakeReg) LookupAny(ctx context.Context, model string) ([]*provider.ModelProfile, error) {
	if e, ok := f.errByModel[model]; ok {
		return nil, e
	}
	var out []*provider.ModelProfile
	for k, c := range f.byKey {
		if k.Model == model {
			out = append(out, &provider.ModelProfile{Caps: c})
		}
	}
	return out, nil
}

func (f fakeReg) Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	var out []*provider.ModelProfile
	for _, c := range f.recommend {
		out = append(out, &provider.ModelProfile{Caps: c})
	}
	return out, nil
}

const fullCaps = provider.CapChat | provider.CapStream | provider.CapToolCall

// noEndpoints is a resolver that never resolves an endpoint (base_url-free
// diagnostics); used where endpoints are not asserted.
func noEndpoints(string) (preflightEndpoint, bool) { return preflightEndpoint{}, false }

// fixedEndpoints returns a resolver backed by a static map.
func fixedEndpoints(m map[string]preflightEndpoint) endpointResolver {
	return func(name string) (preflightEndpoint, bool) {
		ep, ok := m[name]
		return ep, ok
	}
}

// connStatusErr is a fake lookup error carrying an HTTP status.
type connStatusErr struct{ code int }

func (e connStatusErr) Error() string       { return fmt.Sprintf("list models: %d", e.code) }
func (e connStatusErr) HTTPStatusCode() int { return e.code }

func TestPreflight_HeadCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"}, noEndpoints, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestPreflight_PrimaryIncapableFallbackCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: provider.CapChat | provider.CapStream, // no tool_call
		{Provider: "ollama", Model: "m2"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1", "ollama/m2"}, noEndpoints, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ollama/m1") || !strings.Contains(warns[0], "not tool-capable") {
		t.Errorf("warnings = %v, want one capability warning naming ollama/m1", warns)
	}
}

func TestPreflight_NoneCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: provider.CapChat | provider.CapStream,
	}}
	_, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"}, noEndpoints, nil)
	if err == nil {
		t.Fatal("err = nil, want failure (no tool-capable entry)")
	}
}

func TestPreflight_EmptyChain_UsesRecommend(t *testing.T) {
	ok := fakeReg{recommend: []provider.Capability{fullCaps}}
	if _, err := preflightToolCapable(context.Background(), ok, nil, noEndpoints, nil); err != nil {
		t.Fatalf("recommend-capable err = %v, want nil", err)
	}
	empty := fakeReg{recommend: nil}
	if _, err := preflightToolCapable(context.Background(), empty, nil, noEndpoints, nil); err == nil {
		t.Fatal("empty recommend err = nil, want failure")
	}
}

// TestPreflight_BareModelNotFound exercises the unparseable-selector branch: a
// bare model name (no "/") routes through LookupAny, which returns nothing here,
// so the entry is not tool-capable and — being the only entry — fails the gate.
func TestPreflight_BareModelNotFound(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "other"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ghost"}, noEndpoints, nil)
	if err == nil {
		t.Fatal("err = nil for bare model with no capable match, want failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ghost") {
		t.Errorf("warnings = %v, want one naming ghost", warns)
	}
}

// TestPreflight_LookupErrorIsConnectivity: a qualified lookup error with a known
// endpoint surfaces a connectivity diagnostic naming base_url + status, NOT a
// capability message.
func TestPreflight_LookupErrorIsConnectivity(t *testing.T) {
	key := provider.ModelKey{Provider: "llamacpp", Model: "gemma4:31b"}
	reg := fakeReg{errByKey: map[provider.ModelKey]error{
		key: fmt.Errorf("provider: lookup %v: %w", key, connStatusErr{404}),
	}}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, resolve, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1", warns)
	}
	w := warns[0]
	if !strings.Contains(w, "cannot reach provider") || !strings.Contains(w, "http://127.0.0.1:8080") ||
		!strings.Contains(w, "GET /v1/models -> 404 Not Found") {
		t.Errorf("warning = %q, want connectivity message with base_url + status", w)
	}
	if strings.Contains(w, "not tool-capable") {
		t.Errorf("warning = %q, must NOT be a capability message", w)
	}
	// Terminal error inlines the diagnostic (not dropped).
	if !strings.Contains(err.Error(), "cannot reach provider") {
		t.Errorf("err = %q, want inlined connectivity diagnostic", err)
	}
}

// TestPreflight_LookupErrorResolverMiss: a qualified lookup error with no
// endpoint metadata falls back to the verbatim error, no base_url invented.
func TestPreflight_LookupErrorResolverMiss(t *testing.T) {
	key := provider.ModelKey{Provider: "llamacpp", Model: "gemma4:31b"}
	reg := fakeReg{errByKey: map[provider.ModelKey]error{
		key: fmt.Errorf("boom: %w", connStatusErr{404}),
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, noEndpoints, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "provider lookup failed:") ||
		strings.Contains(warns[0], "http://") {
		t.Errorf("warning = %v, want verbatim fallback without base_url", warns)
	}
}

// TestPreflight_LookupErrorNonStatus: a qualified lookup error without a typed
// HTTP status still surfaces provider + base_url + discovery path and includes
// the underlying error.
func TestPreflight_LookupErrorNonStatus(t *testing.T) {
	key := provider.ModelKey{Provider: "llamacpp", Model: "gemma4:31b"}
	reg := fakeReg{errByKey: map[provider.ModelKey]error{
		key: &net.DNSError{Err: "no such host", Name: "llamacpp.local"},
	}}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, resolve, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1", warns)
	}
	w := warns[0]
	if !strings.Contains(w, `cannot reach provider "llamacpp" at http://127.0.0.1:8080 (GET /v1/models): lookup llamacpp.local`) {
		t.Errorf("warning = %q, want non-status connectivity message with endpoint + underlying error", w)
	}
	if strings.Contains(w, "->") {
		t.Errorf("warning = %q, must not include status arrow for non-status errors", w)
	}
}

// TestPreflight_QualifiedModelNotFoundIsLookupFailure: a configured provider
// with a missing model must not be diagnosed as an unreachable provider.
func TestPreflight_QualifiedModelNotFoundIsLookupFailure(t *testing.T) {
	key := provider.ModelKey{Provider: "ollama", Model: "missing"}
	reg := fakeReg{errByKey: map[provider.ModelKey]error{
		key: fmt.Errorf(`provider: lookup %v: model %q not found on %q`, key, key.Model, key.Provider),
	}}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"ollama": {BaseURL: "http://localhost:11434", ModelsPath: "/api/tags"},
	})
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/missing"}, resolve, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1", warns)
	}
	w := warns[0]
	if !strings.Contains(w, "provider lookup failed:") || !strings.Contains(w, `model "missing" not found on "ollama"`) {
		t.Errorf("warning = %q, want neutral lookup failure with model-not-found cause", w)
	}
	if strings.Contains(w, "cannot reach provider") || strings.Contains(w, "GET /api/tags") {
		t.Errorf("warning = %q, must not diagnose missing model as provider reachability", w)
	}
}

// TestPreflight_MixedCapableAndErrored: one capable entry + one unreachable
// entry => nil err (gate passes), one connectivity warning.
func TestPreflight_MixedCapableAndErrored(t *testing.T) {
	good := provider.ModelKey{Provider: "ollama", Model: "m1"}
	bad := provider.ModelKey{Provider: "llamacpp", Model: "down"}
	reg := fakeReg{
		byKey:    map[provider.ModelKey]provider.Capability{good: fullCaps},
		errByKey: map[provider.ModelKey]error{bad: connStatusErr{503}},
	}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	warns, err := preflightToolCapable(context.Background(), reg,
		[]string{"ollama/m1", "llamacpp/down"}, resolve, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil (one entry capable)", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "cannot reach provider") {
		t.Errorf("warnings = %v, want one connectivity warning", warns)
	}
}

// TestPreflight_MixedErroredAndNonCapable: zero capable; terminal error must
// inline BOTH the connectivity and the capability diagnostics, distinctly.
func TestPreflight_MixedErroredAndNonCapable(t *testing.T) {
	down := provider.ModelKey{Provider: "llamacpp", Model: "down"}
	weak := provider.ModelKey{Provider: "ollama", Model: "m1"}
	reg := fakeReg{
		byKey:    map[provider.ModelKey]provider.Capability{weak: provider.CapChat | provider.CapStream},
		errByKey: map[provider.ModelKey]error{down: connStatusErr{404}},
	}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	_, err := preflightToolCapable(context.Background(), reg,
		[]string{"llamacpp/down", "ollama/m1"}, resolve, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot reach provider") {
		t.Errorf("err = %q, missing connectivity diagnostic", msg)
	}
	if !strings.Contains(msg, `"ollama/m1" is not tool-capable`) {
		t.Errorf("err = %q, missing capability diagnostic", msg)
	}
}

// TestPreflight_BareLookupAnyError: a bare selector whose LookupAny errors
// surfaces the verbatim error under the neutral "provider lookup failed" framing
// (a bare selector names no single provider/endpoint).
func TestPreflight_BareLookupAnyError(t *testing.T) {
	reg := fakeReg{errByModel: map[string]error{
		"gemma4:31b": fmt.Errorf("provider: lookup any %q: all providers failed: %w", "gemma4:31b", connStatusErr{404}),
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"gemma4:31b"}, noEndpoints, nil)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "provider lookup failed:") ||
		!strings.Contains(warns[0], "all providers failed") {
		t.Errorf("warning = %v, want verbatim LookupAny error", warns)
	}
}

// TestPreflight_BoundedEager_StopsAfterFirstCapable: chain [A unknown->probe yes,
// B unknown, C unknown]. Probing is allowed only until the first capable entry,
// so the resolver is called ONLY for A. Both B and C are reached after capability
// is proven, so neither is probed at startup — each reports the non-fatal
// "probed on first use" line (deferred to route time), NOT a probe verdict.
func TestPreflight_BoundedEager_StopsAfterFirstCapable(t *testing.T) {
	a := provider.ModelKey{Provider: "ollama", Model: "a"}
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		a:                                provider.CapChat | provider.CapStream, // resolved but no tool_call: eligible to probe
		{Provider: "ollama", Model: "b"}: provider.CapChat | provider.CapStream, // unknown, reached after capable
		{Provider: "ollama", Model: "c"}: provider.CapChat | provider.CapStream, // unknown, reached after capable
	}}
	res := &fakeToolResolver{states: map[provider.ModelKey]fingerprint.CapProbeState{
		a: fingerprint.CapProbeYes,
	}}
	warns, err := preflightToolCapable(context.Background(), reg,
		[]string{"ollama/a", "ollama/b", "ollama/c"}, noEndpoints, res)
	if err != nil {
		t.Fatalf("err = %v, want nil (A probes capable)", err)
	}
	if len(res.calls) != 1 || res.calls[0] != a {
		t.Fatalf("resolver calls = %v, want only A (bounded-eager)", res.calls)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v, want 2 (B and C probed-on-first-use)", warns)
	}
	if !strings.Contains(warns[0], "ollama/b") || !strings.Contains(warns[0], "probed on first use") {
		t.Errorf("warn[0] = %q, want B probed-on-first-use", warns[0])
	}
	if !strings.Contains(warns[1], "ollama/c") || !strings.Contains(warns[1], "probed on first use") {
		t.Errorf("warn[1] = %q, want C probed-on-first-use", warns[1])
	}
}

// TestPreflight_ProbeNoContinuesToNextEntry: chain [A probe->no, B registry-capable].
// A's probe returns no, so preflight continues to B (capable) => nil error. The
// resolver is called for A only (B is capable from the registry). A's warning
// names "did not produce a tool call" and carries the remediation hint with the
// exact capabilities fragment.
func TestPreflight_ProbeNoContinuesToNextEntry(t *testing.T) {
	a := provider.ModelKey{Provider: "ollama", Model: "a"}
	b := provider.ModelKey{Provider: "ollama", Model: "b"}
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		a: provider.CapChat | provider.CapStream,
		b: fullCaps,
	}}
	res := &fakeToolResolver{states: map[provider.ModelKey]fingerprint.CapProbeState{
		a: fingerprint.CapProbeNo,
	}}
	warns, err := preflightToolCapable(context.Background(), reg,
		[]string{"ollama/a", "ollama/b"}, noEndpoints, res)
	if err != nil {
		t.Fatalf("err = %v, want nil (B capable)", err)
	}
	if len(res.calls) != 1 || res.calls[0] != a {
		t.Fatalf("resolver calls = %v, want only A", res.calls)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1 (A probed-no)", warns)
	}
	if !strings.Contains(warns[0], "ollama/a") || !strings.Contains(warns[0], "did not produce a tool call") ||
		!strings.Contains(warns[0], remediationCaps) {
		t.Errorf("warn = %q, want A probed-no + remediation with caps fragment", warns[0])
	}
}

// TestPreflight_AllExhaustedFatalIncludesProbeOutcomes: chain [A probe->no,
// B probe->inconclusive, C lookup error]. No entry is capable, so preflight
// fails and the terminal error inlines all three per-entry diagnostics: A's
// probed-no line, B's inconclusive line, and C's #217 connectivity diagnostic
// (preserved verbatim).
func TestPreflight_AllExhaustedFatalIncludesProbeOutcomes(t *testing.T) {
	a := provider.ModelKey{Provider: "ollama", Model: "a"}
	b := provider.ModelKey{Provider: "ollama", Model: "b"}
	c := provider.ModelKey{Provider: "llamacpp", Model: "down"}
	reg := fakeReg{
		byKey: map[provider.ModelKey]provider.Capability{
			a: provider.CapChat | provider.CapStream,
			b: provider.CapChat | provider.CapStream,
		},
		errByKey: map[provider.ModelKey]error{c: connStatusErr{404}},
	}
	res := &fakeToolResolver{states: map[provider.ModelKey]fingerprint.CapProbeState{
		a: fingerprint.CapProbeNo,
		b: fingerprint.CapProbeInconclusive,
	}}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	_, err := preflightToolCapable(context.Background(), reg,
		[]string{"ollama/a", "ollama/b", "llamacpp/down"}, resolve, res)
	if err == nil {
		t.Fatal("err = nil, want failure (none capable)")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ollama/a") || !strings.Contains(msg, "did not produce a tool call") {
		t.Errorf("err = %q, missing A probed-no line", msg)
	}
	if !strings.Contains(msg, "ollama/b") || !strings.Contains(msg, "probe was inconclusive") ||
		!strings.Contains(msg, "declare capabilities to override") {
		t.Errorf("err = %q, missing B inconclusive line", msg)
	}
	if !strings.Contains(msg, "cannot reach provider") || !strings.Contains(msg, "GET /v1/models -> 404 Not Found") {
		t.Errorf("err = %q, missing C connectivity diagnostic", msg)
	}
}

// TestPreflight_NoCapProbe_NeverResolves: with a nil resolver, active probing
// never runs. The existing #217 lookup/connectivity table runs through the new
// signature to prove those diagnostics are unchanged; a catalog/floor-capable
// entry still passes (merge is upstream, not disabled by -no-cap-probe).
func TestPreflight_NoCapProbe_NeverResolves(t *testing.T) {
	// Capable catalog/floor entry passes with a nil resolver, no warnings.
	regCapable := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), regCapable, []string{"ollama/m1"}, noEndpoints, nil)
	if err != nil || len(warns) != 0 {
		t.Fatalf("capable-with-nil-resolver: warns=%v err=%v, want none/nil", warns, err)
	}

	// #217 connectivity diagnostic preserved byte-identically with a nil resolver.
	key := provider.ModelKey{Provider: "llamacpp", Model: "gemma4:31b"}
	regDown := fakeReg{errByKey: map[provider.ModelKey]error{
		key: fmt.Errorf("provider: lookup %v: %w", key, connStatusErr{404}),
	}}
	resolve := fixedEndpoints(map[string]preflightEndpoint{
		"llamacpp": {BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"},
	})
	warns, err = preflightToolCapable(context.Background(), regDown, []string{"llamacpp/gemma4:31b"}, resolve, nil)
	if err == nil {
		t.Fatal("err = nil, want connectivity failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "cannot reach provider") ||
		!strings.Contains(warns[0], "GET /v1/models -> 404 Not Found") {
		t.Errorf("warnings = %v, want unchanged #217 connectivity diagnostic", warns)
	}
}

// TestPreflight_NoCapProbe_CatalogKnownStillCapable: nil resolver; a chain with a
// floor+catalog-merged capable entry passes with zero warnings, while an
// undeclared catalog-miss entry in the same chain stays not-capable and carries
// the remediation hint.
func TestPreflight_NoCapProbe_CatalogKnownStillCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "known"}: fullCaps,                              // merged upstream
		{Provider: "ollama", Model: "miss"}:  provider.CapChat | provider.CapStream, // undeclared
	}}
	warns, err := preflightToolCapable(context.Background(), reg,
		[]string{"ollama/known", "ollama/miss"}, noEndpoints, nil)
	if err != nil {
		t.Fatalf("err = %v, want nil (known entry capable)", err)
	}
	if len(warns) != 1 {
		t.Fatalf("warnings = %v, want 1 (miss not-capable)", warns)
	}
	if !strings.Contains(warns[0], "ollama/miss") || !strings.Contains(warns[0], "not tool-capable") ||
		!strings.Contains(warns[0], remediationCaps) {
		t.Errorf("warn = %q, want catalog-miss capability warning with remediation hint", warns[0])
	}
}

// recordingReg wraps Recommend to capture the RequiredCaps it was called with,
// so the empty-chain test can assert the recommend query dropped CapToolCall
// when a resolver is present (probe fills the gap).
type recordingReg struct {
	fakeReg
	recProfiles  []*provider.ModelProfile
	gotRequired  provider.Capability
	recommendHit bool
}

func (r *recordingReg) Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	r.recommendHit = true
	r.gotRequired = opts.RequiredCaps
	return r.recProfiles, nil
}

// TestPreflight_EmptyChainRecommendResolves: empty chain with a resolver. The
// recommend query must NOT include CapToolCall (the probe fills that gap);
// Recommend returns one model lacking tool_call, the resolver probes it yes, and
// preflight passes.
func TestPreflight_EmptyChainRecommendResolves(t *testing.T) {
	key := provider.ModelKey{Provider: "ollama", Model: "m1"}
	reg := &recordingReg{
		recProfiles: []*provider.ModelProfile{
			{Key: key, Caps: provider.CapChat | provider.CapStream},
		},
	}
	res := &fakeToolResolver{states: map[provider.ModelKey]fingerprint.CapProbeState{
		key: fingerprint.CapProbeYes,
	}}
	if _, err := preflightToolCapable(context.Background(), reg, nil, noEndpoints, res); err != nil {
		t.Fatalf("err = %v, want nil (recommend model probes capable)", err)
	}
	if !reg.recommendHit {
		t.Fatal("Recommend was not called")
	}
	if reg.gotRequired.Has(provider.CapToolCall) {
		t.Errorf("RequiredCaps = %v, must NOT include CapToolCall when resolving", reg.gotRequired)
	}
	if len(res.calls) != 1 || res.calls[0] != key {
		t.Errorf("resolver calls = %v, want probe of recommended model", res.calls)
	}
}

// TestPreflight_ProbeErrorNonFatalButNotCapable: the first (allowProbe) entry's
// probe returns a transient error. The entry is non-fatal-but-not-capable
// (falls through, not treated as capable); being the only entry, preflight fails
// and the terminal error includes the "tool-capability probe failed:" warning
// plus the remediation hint.
func TestPreflight_ProbeErrorNonFatalButNotCapable(t *testing.T) {
	a := provider.ModelKey{Provider: "ollama", Model: "a"}
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		a: provider.CapChat | provider.CapStream, // resolved, no tool_call: eligible to probe
	}}
	res := &fakeToolResolver{errs: map[provider.ModelKey]error{
		a: fmt.Errorf("dial tcp: connection refused"),
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/a"}, noEndpoints, res)
	if err == nil {
		t.Fatal("err = nil, want failure (only entry probed with error, not capable)")
	}
	if len(res.calls) != 1 || res.calls[0] != a {
		t.Fatalf("resolver calls = %v, want probe of A", res.calls)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "tool-capability probe failed:") ||
		!strings.Contains(warns[0], "connection refused") || !strings.Contains(warns[0], remediationCaps) {
		t.Errorf("warnings = %v, want probe-failed warning with underlying error + remediation hint", warns)
	}
	if !strings.Contains(err.Error(), "tool-capability probe failed:") {
		t.Errorf("err = %q, want inlined probe-failed diagnostic", err)
	}
}
