package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"}, noEndpoints)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1", "ollama/m2"}, noEndpoints)
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
	_, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"}, noEndpoints)
	if err == nil {
		t.Fatal("err = nil, want failure (no tool-capable entry)")
	}
}

func TestPreflight_EmptyChain_UsesRecommend(t *testing.T) {
	ok := fakeReg{recommend: []provider.Capability{fullCaps}}
	if _, err := preflightToolCapable(context.Background(), ok, nil, noEndpoints); err != nil {
		t.Fatalf("recommend-capable err = %v, want nil", err)
	}
	empty := fakeReg{recommend: nil}
	if _, err := preflightToolCapable(context.Background(), empty, nil, noEndpoints); err == nil {
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ghost"}, noEndpoints)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, resolve)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, noEndpoints)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"llamacpp/gemma4:31b"}, resolve)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/missing"}, resolve)
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
		[]string{"ollama/m1", "llamacpp/down"}, resolve)
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
		[]string{"llamacpp/down", "ollama/m1"}, resolve)
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
	warns, err := preflightToolCapable(context.Background(), reg, []string{"gemma4:31b"}, noEndpoints)
	if err == nil {
		t.Fatal("err = nil, want failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "provider lookup failed:") ||
		!strings.Contains(warns[0], "all providers failed") {
		t.Errorf("warning = %v, want verbatim LookupAny error", warns)
	}
}
