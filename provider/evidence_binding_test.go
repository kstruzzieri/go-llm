// provider/evidence_binding_test.go
//
// Tests for the registry's read path over the capability evidence log and its
// freshness across processes (#734).
package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

const (
	logDay    = 24 * time.Hour
	logModel  = "log-model" // not in the catalog, so only the log can give it a bit
	logDigest = "sha256:current"
)

// logT0 is a fixed instant on a whole millisecond, the stored resolution.
var logT0 = time.UnixMilli(1_800_000_000_000)

func openEvidenceLog(t *testing.T, path string) *fingerprint.SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := fingerprint.NewStore(t.Context(), db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func newEvidenceLog(t *testing.T) *fingerprint.SQLiteStore {
	t.Helper()
	return openEvidenceLog(t, filepath.Join(t.TempDir(), "fingerprints.db"))
}

func mustBinding(t *testing.T, baseURL, apiFormat string) EvidenceBinding {
	t.Helper()
	b, err := EvidenceBindingFor(baseURL, apiFormat)
	if err != nil {
		t.Fatalf("EvidenceBindingFor(%q, %q): %v", baseURL, apiFormat, err)
	}
	return b
}

// logRow is a chat-stream-tools probe answer through b, with the expiry a writer
// would give it.
func logRow(b EvidenceBinding, capability string, state fingerprint.CapProbeState, digest string, at time.Time) fingerprint.EvidenceRow {
	r := fingerprint.EvidenceRow{
		KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: b.Endpoint, Adapter: b.Adapter,
		Model: logModel, Digest: digest, Capability: capability,
		Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer, State: state,
		Method: fingerprint.MethodChatStreamTools, MethodVersion: 1, ObservedAt: at,
	}
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

// showRow is what Ollama's listing reported for a capability.
func showRow(b EvidenceBinding, capability string, state fingerprint.CapProbeState, digest string, at time.Time) fingerprint.EvidenceRow {
	r := logRow(b, capability, state, digest, at)
	r.Adapter, r.Method, r.Source = fingerprint.AdapterOllama, fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider
	return r
}

func appendRows(t *testing.T, store fingerprint.EvidenceStore, rows ...fingerprint.EvidenceRow) {
	t.Helper()
	if err := store.AppendEvidence(t.Context(), rows...); err != nil {
		t.Fatalf("AppendEvidence: %v", err)
	}
}

// logStubProvider counts Models calls and runs onModels during each.
type logStubProvider struct {
	*mrMockProvider
	calls    atomic.Int64
	onModels func()
}

func (p *logStubProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	p.calls.Add(1)
	if p.onModels != nil {
		p.onModels()
	}
	return p.mrMockProvider.Models(ctx)
}

func newStubProvider(name string, models ...ModelInfo) *logStubProvider {
	return &logStubProvider{mrMockProvider: &mrMockProvider{name: name, models: models}}
}

// logClock is a settable clock for a registry's unexported now.
type logClock struct{ at atomic.Int64 }

func newLogClock(mr *ModelRegistry, at time.Time) *logClock {
	c := &logClock{}
	c.set(at)
	mr.now = func() time.Time { return time.Unix(0, c.at.Load()) }
	return c
}

func (c *logClock) set(at time.Time) { c.at.Store(at.UnixNano()) }

// logRegistry builds a registry on one provider bound to b, whose listing holds
// logModel at digest, with its clock stopped at logT0.
func logRegistry(t *testing.T, store fingerprint.EvidenceStore, name string, b EvidenceBinding, digest string, opts ...ModelRegistryOption) (*ModelRegistry, *logStubProvider) {
	t.Helper()
	prov := newStubProvider(name, ModelInfo{Name: logModel, Digest: digest})
	opts = append(opts, WithEvidenceLog(store, func(provider string) (EvidenceBinding, bool) {
		return b, provider == name
	}))
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{name: prov}}, nil, opts...)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	newLogClock(mr, logT0)
	return mr, prov
}

func lookupCaps(t *testing.T, mr *ModelRegistry, name string) Capability {
	t.Helper()
	p, err := mr.Lookup(t.Context(), ModelKey{Provider: name, Model: logModel})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return p.Caps
}

func wantCaps(t *testing.T, label string, got, want Capability) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: caps = %v; want %v", label, got, want)
	}
}

func wantModelsCalls(t *testing.T, label string, prov *logStubProvider, want int64) {
	t.Helper()
	if got := prov.calls.Load(); got != want {
		t.Fatalf("%s: Models called %d times; want %d", label, got, want)
	}
}

// --- The endpoint key ---

func TestEvidenceBindingFor(t *testing.T) {
	tests := []struct {
		name, baseURL, apiFormat string
		want                     EvidenceBinding
	}{
		{"empty format is ollama", "http://127.0.0.1:11434", "", EvidenceBinding{"http://127.0.0.1:11434", "ollama"}},
		{"ollama", "http://127.0.0.1:11434/", "ollama", EvidenceBinding{"http://127.0.0.1:11434", "ollama"}},
		{"openai-compat", "HTTP://Example.COM:80/v1/", "openai-compat", EvidenceBinding{"http://example.com/v1", "openai-compat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvidenceBindingFor(tt.baseURL, tt.apiFormat)
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v; want %+v", got, tt.want)
			}
		})
	}
	if _, err := EvidenceBindingFor("http://127.0.0.1:8080", "grpc"); err == nil {
		t.Fatal("an unknown api format must be an error")
	}
	const secretURL = "http://user:hunter2@host"
	_, err := EvidenceBindingFor(secretURL, "ollama")
	if err == nil {
		t.Fatal("a base URL with userinfo must be an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error repeats the base URL: %v", err)
	}
}

func TestCanonicalEndpoint(t *testing.T) {
	canonical, local, err := CanonicalEndpoint("http://LOCALHOST:80/")
	if err != nil || canonical != "http://localhost" || !local {
		t.Fatalf("CanonicalEndpoint = %q, %v, %v; want http://localhost, true, nil", canonical, local, err)
	}
	canonical, local, err = CanonicalEndpoint("https://example.com:8443/a/")
	if err != nil || canonical != "https://example.com:8443/a" || local {
		t.Fatalf("CanonicalEndpoint = %q, %v, %v; want https://example.com:8443/a, false, nil", canonical, local, err)
	}
	if _, _, err := CanonicalEndpoint("not a url"); err == nil {
		t.Fatal("a bad base URL must be an error")
	}
}

func TestEvidenceBinding_SameNameDifferentEndpointsNeverShare(t *testing.T) {
	store := newEvidenceLog(t)
	at8080 := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	at9090 := mustBinding(t, "http://127.0.0.1:9090", "openai-compat")
	appendRows(t, store, logRow(at8080, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	answered, _ := logRegistry(t, store, "local", at8080, logDigest)
	other, _ := logRegistry(t, store, "local", at9090, logDigest)
	wantCaps(t, "the endpoint with the answer", lookupCaps(t, answered, "local"), CapToolCall)
	wantCaps(t, "the same name at another endpoint", lookupCaps(t, other, "local"), 0)
}

func TestEvidenceBinding_RenameKeepsAnswer(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	before, _ := logRegistry(t, store, "before", b, logDigest)
	after, _ := logRegistry(t, store, "after", b, logDigest)
	wantCaps(t, "before the rename", lookupCaps(t, before, "before"), CapToolCall)
	wantCaps(t, "after the rename", lookupCaps(t, after, "after"), CapToolCall)
}

func TestEvidenceBinding_LocalhostAndLoopbackDistinct(t *testing.T) {
	store := newEvidenceLog(t)
	host := mustBinding(t, "http://localhost:8080", "openai-compat")
	loopback := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	if host.Endpoint != "http://localhost:8080" || loopback.Endpoint != "http://127.0.0.1:8080" {
		t.Fatalf("endpoints = %q, %q", host.Endpoint, loopback.Endpoint)
	}
	appendRows(t, store, logRow(host, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	byName, _ := logRegistry(t, store, "local", host, logDigest)
	byIP, _ := logRegistry(t, store, "local", loopback, logDigest)
	wantCaps(t, "localhost", lookupCaps(t, byName, "local"), CapToolCall)
	wantCaps(t, "127.0.0.1", lookupCaps(t, byIP, "local"), 0)
}

func TestEvidenceBinding_DefaultPortAndTrailingSlashSameKey(t *testing.T) {
	store := newEvidenceLog(t)
	spelled := mustBinding(t, "https://h:443/", "openai-compat")
	plain := mustBinding(t, "https://h", "openai-compat")
	if spelled != plain || plain.Endpoint != "https://h" {
		t.Fatalf("bindings = %+v, %+v; want both at https://h", spelled, plain)
	}
	appendRows(t, store, logRow(spelled, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	mr, _ := logRegistry(t, store, "cloud", plain, logDigest)
	wantCaps(t, "the other spelling", lookupCaps(t, mr, "cloud"), CapToolCall)
}

func TestEvidenceBinding_OllamaShowNotCountedForOpenAICompat(t *testing.T) {
	store := newEvidenceLog(t)
	native := mustBinding(t, "http://127.0.0.1:11434", "")
	compat := mustBinding(t, "http://127.0.0.1:11434", "openai-compat")
	if native.Endpoint != compat.Endpoint {
		t.Fatalf("endpoints differ: %q, %q", native.Endpoint, compat.Endpoint)
	}
	appendRows(t, store, showRow(native, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	throughShow, _ := logRegistry(t, store, "ollama", native, logDigest)
	throughCompat, _ := logRegistry(t, store, "compat", compat, logDigest)
	wantCaps(t, "through Ollama's own API", lookupCaps(t, throughShow, "ollama"), CapToolCall)
	wantCaps(t, "through the OpenAI-compatible API", lookupCaps(t, throughCompat, "compat"), 0)
}

// --- What a reading contributes ---

func TestEvidenceLog_EveryYesReadingSetsItsBit(t *testing.T) {
	bits := map[string]Capability{
		"chat": CapChat, "generate": CapGenerate, "insert": CapInsert, "embed": CapEmbed,
		"stream": CapStream, "tool_call": CapToolCall, "thinking": CapThinking,
	}
	if len(bits) != len(CanonicalCapabilityNames) {
		t.Fatalf("the table covers %d capabilities; there are %d", len(bits), len(CanonicalCapabilityNames))
	}
	b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	for name, bit := range bits {
		t.Run(name, func(t *testing.T) {
			store := newEvidenceLog(t)
			appendRows(t, store, showRow(b, name, fingerprint.CapProbeYes, logDigest, logT0))
			mr, _ := logRegistry(t, store, "ollama", b, logDigest)
			wantCaps(t, name, lookupCaps(t, mr, "ollama"), bit)
		})
	}
}

// A reading can hold several answers. A direct one outranks what the provider
// reported, and between direct ones the newer wins.
func TestEvidenceLog_DisagreeingAnswers(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	source := func(s fingerprint.EvidenceSource, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
		r := logRow(b, "tool_call", state, logDigest, at)
		r.Source = s
		return r
	}
	const (
		probe    = fingerprint.EvidenceSourceProbe
		observed = fingerprint.EvidenceSourceObserved
		reported = fingerprint.EvidenceSourceProvider
		yes      = fingerprint.CapProbeYes
		no       = fingerprint.CapProbeNo
	)
	older, newer := logT0.Add(-logDay), logT0
	tests := []struct {
		name string
		rows []fingerprint.EvidenceRow
		want Capability
	}{
		{"provider yes alone", []fingerprint.EvidenceRow{source(reported, yes, newer)}, CapToolCall},
		{"probe no over a newer provider yes", []fingerprint.EvidenceRow{source(probe, no, older), source(reported, yes, newer)}, 0},
		{"observed no over a newer provider yes", []fingerprint.EvidenceRow{source(observed, no, older), source(reported, yes, newer)}, 0},
		{"probe yes over a newer provider no", []fingerprint.EvidenceRow{source(probe, yes, older), source(reported, no, newer)}, CapToolCall},
		{"newer probe no over older observed yes", []fingerprint.EvidenceRow{source(observed, yes, older), source(probe, no, newer)}, 0},
		{"newer observed no over older probe yes", []fingerprint.EvidenceRow{source(probe, yes, older), source(observed, no, newer)}, 0},
		{"newer observed yes over older probe no", []fingerprint.EvidenceRow{source(probe, no, older), source(observed, yes, newer)}, CapToolCall},
		{"newer probe yes over older observed no", []fingerprint.EvidenceRow{source(observed, no, older), source(probe, yes, newer)}, CapToolCall},
		{"a tie goes to the probe", []fingerprint.EvidenceRow{source(probe, no, newer), source(observed, yes, newer)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newEvidenceLog(t)
			appendRows(t, store, tt.rows...)
			mr, _ := logRegistry(t, store, "local", b, logDigest)
			wantCaps(t, tt.name, lookupCaps(t, mr, "local"), tt.want)
		})
	}
}

// The read digest is the runtime's own, empty when name-only. The legacy table's
// key.String() fallback must not stand in for it, or a row written with that value
// would count and a name-only row would not.
func TestEvidenceLog_DigestComesFromRuntimeMetadata(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	key := ModelKey{Provider: "local", Model: logModel}
	tests := []struct {
		name         string
		row, runtime string
		want         Capability
	}{
		{"name-only row, name-only runtime", "", "", CapToolCall},
		{"row under the key's string, name-only runtime", key.String(), "", 0},
		{"row at the runtime's digest", logDigest, logDigest, CapToolCall},
		{"row at another digest", "sha256:other", logDigest, 0},
		{"name-only row, runtime with a digest", "", logDigest, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newEvidenceLog(t)
			appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, tt.row, logT0))
			mr, _ := logRegistry(t, store, "local", b, tt.runtime)
			wantCaps(t, tt.name, lookupCaps(t, mr, "local"), tt.want)
		})
	}
}

func TestExplainToolCall_LogProvenance(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	at := logT0.Add(-logDay)
	expired := func(r fingerprint.EvidenceRow) fingerprint.EvidenceRow {
		r.ExpiresAt = logT0.Add(-time.Hour)
		return r
	}
	as := func(s fingerprint.EvidenceSource, r fingerprint.EvidenceRow) fingerprint.EvidenceRow {
		r.Source = s
		return r
	}
	attempt := func(at time.Time) fingerprint.EvidenceRow {
		r := logRow(b, "tool_call", fingerprint.CapProbeInconclusive, logDigest, at)
		r.Kind = fingerprint.EvidenceKindAttempt
		return r
	}
	tests := []struct {
		name   string
		rows   []fingerprint.EvidenceRow
		listed []string // what the runtime itself lists
		want   ToolCallExplanation
	}{
		{name: "nothing", want: ToolCallExplanation{Source: "unknown"}},
		{
			name: "the runtime's bit beside a probe no is the runtime's",
			rows: []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeNo, logDigest, at)}, listed: []string{"tools"},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "runtime", State: fingerprint.CapProbeNo, TestedAt: at, Valid: true},
		},
		{
			name: "the runtime's bit with nothing in the log", listed: []string{"tools"},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "runtime"},
		},
		{
			name: "probe yes", rows: []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, at)},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "probe", State: fingerprint.CapProbeYes, TestedAt: at, Valid: true},
		},
		{
			name: "observed yes", rows: []fingerprint.EvidenceRow{as(fingerprint.EvidenceSourceObserved, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, at))},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "probe", State: fingerprint.CapProbeYes, TestedAt: at, Valid: true},
		},
		{
			name: "provider yes reads as runtime", rows: []fingerprint.EvidenceRow{as(fingerprint.EvidenceSourceProvider, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, at))},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "runtime", State: fingerprint.CapProbeYes, TestedAt: at, Valid: true},
		},
		{
			name: "probe no", rows: []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeNo, logDigest, at)},
			want: ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeNo, TestedAt: at, Valid: true},
		},
		{
			name: "provider no reads as runtime", rows: []fingerprint.EvidenceRow{as(fingerprint.EvidenceSourceProvider, logRow(b, "tool_call", fingerprint.CapProbeNo, logDigest, at))},
			want: ToolCallExplanation{Source: "runtime", State: fingerprint.CapProbeNo, TestedAt: at, Valid: true},
		},
		{
			name: "live attempt", rows: []fingerprint.EvidenceRow{attempt(logT0.Add(-time.Hour))},
			want: ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeInconclusive, TestedAt: logT0.Add(-time.Hour), Valid: true},
		},
		{
			name: "expired attempt", rows: []fingerprint.EvidenceRow{attempt(logT0.Add(-2 * logDay))},
			want: ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeInconclusive, TestedAt: logT0.Add(-2 * logDay), Valid: false},
		},
		{
			name: "an answer outranks a later attempt", rows: []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, at), attempt(logT0.Add(-time.Hour))},
			want: ToolCallExplanation{Caps: CapToolCall, Has: true, Source: "probe", State: fingerprint.CapProbeYes, TestedAt: at, Valid: true},
		},
		{
			name: "an expired no says nothing", rows: []fingerprint.EvidenceRow{expired(logRow(b, "tool_call", fingerprint.CapProbeNo, logDigest, at))},
			want: ToolCallExplanation{Source: "unknown"},
		},
		{
			name: "another digest says nothing", rows: []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeYes, "sha256:other", at)},
			want: ToolCallExplanation{Source: "unknown"},
		},
		{
			name: "another capability says nothing about tool_call", rows: []fingerprint.EvidenceRow{logRow(b, "chat", fingerprint.CapProbeYes, logDigest, at)},
			want: ToolCallExplanation{Caps: CapChat, Source: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newEvidenceLog(t)
			if len(tt.rows) > 0 {
				appendRows(t, store, tt.rows...)
			}
			mr, prov := logRegistry(t, store, "local", b, logDigest)
			prov.models[0].Capabilities = tt.listed
			got, err := mr.ExplainToolCall(t.Context(), ModelKey{Provider: "local", Model: logModel})
			if err != nil {
				t.Fatalf("ExplainToolCall: %v", err)
			}
			if !got.TestedAt.Equal(tt.want.TestedAt) {
				t.Fatalf("TestedAt = %v; want %v", got.TestedAt, tt.want.TestedAt)
			}
			got.TestedAt, tt.want.TestedAt = time.Time{}, time.Time{}
			if got != tt.want {
				t.Fatalf("got %+v; want %+v", got, tt.want)
			}
		})
	}
}

// A catalog model's bit is the catalog's, whatever the log adds.
func TestExplainToolCall_LogKeepsCatalogSource(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	row := logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0)
	row.Model = "qwen3:8b" // the catalog gives it tools
	appendRows(t, store, row)
	prov := newStubProvider("local", ModelInfo{Name: "qwen3:8b", Digest: logDigest})
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": prov}}, nil,
		WithEvidenceLog(store, func(string) (EvidenceBinding, bool) { return b, true }))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	newLogClock(mr, logT0)
	got, err := mr.ExplainToolCall(t.Context(), ModelKey{Provider: "local", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("ExplainToolCall: %v", err)
	}
	if got.Source != "catalog" || !got.Has || got.State != fingerprint.CapProbeYes || !got.Valid {
		t.Fatalf("got %+v; want a catalog source beside the valid probe row", got)
	}
}

func TestExplainToolCall_LogExplicitOverrideStillWins(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	mr.SetCapabilityOverride(func(ModelKey) []string { return []string{"chat"} })
	got, err := mr.ExplainToolCall(t.Context(), ModelKey{Provider: "local", Model: logModel})
	if err != nil {
		t.Fatalf("ExplainToolCall: %v", err)
	}
	if want := (ToolCallExplanation{Caps: CapChat, Source: "explicit"}); got != want {
		t.Fatalf("got %+v; want %+v", got, want)
	}
	wantModelsCalls(t, "the explicit short cut", prov, 0)
}

func TestExplainToolCall_LogRuntimeErrorAndCancel(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	key := ModelKey{Provider: "local", Model: logModel}

	boom := errors.New("listing failed")
	prov.modelsErr, prov.detailsErr = boom, boom
	if _, err := mr.ExplainToolCall(t.Context(), key); !errors.Is(err, boom) {
		t.Fatalf("error = %v; want the listing failure", err)
	}
	prov.modelsErr, prov.detailsErr = nil, nil

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := mr.ExplainToolCall(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
}

// --- Legacy rows ---

func TestLegacyRowsNotCountedWithEvidenceLog(t *testing.T) {
	ctx := t.Context()
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	key := ModelKey{Provider: "local", Model: logModel}
	legacy := newFakeCapProbeStore()
	if err := legacy.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID: "local", ModelName: logModel, Capability: "tool_call",
		State: fingerprint.CapProbeYes, ModelDigest: logDigest,
		ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveCapProbe: %v", err)
	}

	bound, _ := logRegistry(t, newEvidenceLog(t), "local", b, logDigest, WithCapabilityProbeStore(legacy))
	prov := newStubProvider("local", ModelInfo{Name: logModel, Digest: logDigest})
	// The binder's false is what counts, whatever binding comes with it.
	answered := newEvidenceLog(t)
	appendRows(t, answered, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	unbound, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": prov}}, nil,
		WithCapabilityProbeStore(legacy),
		WithEvidenceLog(answered, func(string) (EvidenceBinding, bool) { return b, false }))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	newLogClock(unbound, logT0)
	noBinder, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": prov}}, nil,
		WithCapabilityProbeStore(legacy), WithEvidenceLog(newEvidenceLog(t), nil))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	for name, mr := range map[string]*ModelRegistry{"bound": bound, "not resolved by the binder": unbound, "no binder": noBinder} {
		t.Run(name, func(t *testing.T) {
			wantCaps(t, "Lookup", lookupCaps(t, mr, "local"), 0)

			exp, err := mr.ExplainToolCall(ctx, key)
			if err != nil {
				t.Fatalf("ExplainToolCall: %v", err)
			}
			if want := (ToolCallExplanation{Source: "unknown"}); exp != want {
				t.Fatalf("ExplainToolCall = %+v; want %+v", exp, want)
			}

			facts, err := mr.ProjectListedModels(ctx, "local", []ModelInfo{{Name: logModel, Digest: logDigest}})
			if err != nil {
				t.Fatalf("ProjectListedModels: %v", err)
			}
			if f := facts[0]; f.Caps != 0 || f.KnownMask != 0 {
				t.Fatalf("ProjectListedModels: Caps=%v KnownMask=%v; want neither", f.Caps, f.KnownMask)
			}
		})
	}
}

// A provider the binder does not resolve has nothing to fence on, so its profile
// still caches rather than rebuilding on every Lookup.
func TestEvidenceLog_UnboundProviderStillCaches(t *testing.T) {
	store := newEvidenceLog(t)
	// Some other provider's row, so the store's generation is not a view's zero.
	appendRows(t, store, logRow(mustBinding(t, "http://127.0.0.1:9090", "openai-compat"), "chat", fingerprint.CapProbeYes, "", logT0))
	prov := newStubProvider("local", ModelInfo{Name: logModel})
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": prov}}, nil,
		WithEvidenceLog(store, func(string) (EvidenceBinding, bool) { return EvidenceBinding{}, false }))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	for range 3 {
		lookupCaps(t, mr, "local")
	}
	wantModelsCalls(t, "three lookups", prov, 1)
}

func TestLegacyPathUnchangedWithoutEvidenceLog(t *testing.T) {
	ctx := t.Context()
	key := ModelKey{Provider: "local", Model: logModel}
	legacy := newFakeCapProbeStore()
	testedAt := time.Now()
	if err := legacy.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID: "local", ModelName: logModel, Capability: "tool_call",
		State: fingerprint.CapProbeYes, ModelDigest: logDigest,
		ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: testedAt,
	}); err != nil {
		t.Fatalf("SaveCapProbe: %v", err)
	}
	prov := newStubProvider("local", ModelInfo{Name: logModel, Digest: logDigest})
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": prov}}, nil, WithCapabilityProbeStore(legacy))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	if mr.evidenceStore != nil {
		t.Fatal("no option, yet a log is attached")
	}

	wantCaps(t, "the legacy yes", lookupCaps(t, mr, "local"), CapToolCall)
	lookupCaps(t, mr, "local")
	wantModelsCalls(t, "a cached profile", prov, 1)
	if len(mr.profileDeadlines) != 0 {
		t.Fatalf("deadlines = %v; the legacy path has none", mr.profileDeadlines)
	}

	// syncGeneration does nothing and needs no store, on a nil registry too.
	mr.syncGeneration(ctx)
	(*ModelRegistry)(nil).syncGeneration(ctx)
	lookupCaps(t, mr, "local")
	wantModelsCalls(t, "after a boundary with no log", prov, 1)

	exp, err := mr.ExplainToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ExplainToolCall: %v", err)
	}
	if exp.Source != "probe" || exp.State != fingerprint.CapProbeYes || !exp.Valid || !exp.TestedAt.Equal(testedAt) {
		t.Fatalf("ExplainToolCall = %+v; want the legacy row", exp)
	}
	facts, err := mr.ProjectListedModels(ctx, "local", []ModelInfo{{Name: logModel, Digest: logDigest}})
	if err != nil {
		t.Fatalf("ProjectListedModels: %v", err)
	}
	if f := facts[0]; f.Caps != CapToolCall || f.KnownMask != CapToolCall {
		t.Fatalf("ProjectListedModels: Caps=%v KnownMask=%v; want tool_call in both", f.Caps, f.KnownMask)
	}
}

// --- Freshness ---

func TestRegistryFreshness_SecondRegistrySeesAnswerAtNextRoute(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	otherProcess := openEvidenceLog(t, path) // its own handle on the shared file
	reader := openEvidenceLog(t, path)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")

	prov := newStubProvider("local", ModelInfo{Name: logModel, Digest: logDigest, ContextWindow: 32768})
	provReg := NewRegistry()
	if err := provReg.Register(prov); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := provReg.RefreshModels(ctx, "local"); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	mr, err := NewModelRegistry(provReg, nil, WithEvidenceLog(reader, func(string) (EvidenceBinding, bool) { return b, true }))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	newLogClock(mr, logT0)
	router := NewRouter(mr, provReg)
	t.Cleanup(func() { _ = router.Close() })
	req := RoutingRequest{Model: "local/" + logModel, UseCase: "chat", RequiredCaps: CapToolCall}

	if _, err := router.Route(ctx, req); !errors.Is(err, ErrNoViableCandidate) {
		t.Fatalf("Route before the answer: error = %v; want ErrNoViableCandidate", err)
	}
	appendRows(t, otherProcess, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))

	plan, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Route after the answer: %v", err)
	}
	if !plan.Profile.Caps.Has(CapToolCall) {
		t.Fatalf("plan caps = %v; want tool_call", plan.Profile.Caps)
	}
}

func TestRegistryFreshness_SyncsAtEveryBoundary(t *testing.T) {
	key := ModelKey{Provider: "local", Model: logModel}
	boundaries := map[string]func(t *testing.T, mr *ModelRegistry){
		"ResolveToolCall": func(t *testing.T, mr *ModelRegistry) {
			if _, err := mr.ResolveToolCall(t.Context(), key); err != nil {
				t.Fatalf("ResolveToolCall: %v", err)
			}
		},
		"EnsureToolCallResolved": func(t *testing.T, mr *ModelRegistry) {
			mr.EnsureToolCallResolved(t.Context(), nil, 0)
		},
		"ExplainToolCall": func(t *testing.T, mr *ModelRegistry) {
			if _, err := mr.ExplainToolCall(t.Context(), key); err != nil {
				t.Fatalf("ExplainToolCall: %v", err)
			}
		},
	}
	for name, cross := range boundaries {
		t.Run(name, func(t *testing.T) {
			store := newEvidenceLog(t)
			b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
			mr, _ := logRegistry(t, store, "local", b, logDigest)
			mr.syncGeneration(t.Context())
			wantCaps(t, "before the write", lookupCaps(t, mr, "local"), 0)

			appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
			wantCaps(t, "a Lookup alone keeps the cached profile", lookupCaps(t, mr, "local"), 0)

			cross(t, mr)
			wantCaps(t, "after the boundary", lookupCaps(t, mr, "local"), CapToolCall)
		})
	}
}

// A profile cached before any boundary was built under a generation nothing has
// checked, so the first sync drops it.
func TestRegistryFreshness_FirstSyncDropsProfilesCachedBeforeIt(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, _ := logRegistry(t, store, "local", b, logDigest)
	wantCaps(t, "before the write", lookupCaps(t, mr, "local"), 0)
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	mr.syncGeneration(t.Context())
	wantCaps(t, "after the first sync", lookupCaps(t, mr, "local"), CapToolCall)
}

func TestRegistryFreshness_UnmovedGenerationKeepsCache(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	mr.syncGeneration(t.Context())
	lookupCaps(t, mr, "local")
	for range 3 {
		mr.syncGeneration(t.Context())
		lookupCaps(t, mr, "local")
	}
	wantModelsCalls(t, "no write", prov, 1)
}

func TestRegistryFreshness_CachedNegativeDropsOnExpiryNoWrite(t *testing.T) {
	ctx := t.Context()
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	// A finite no over an older yes: when the no expires, the yes speaks again.
	yes := logRow(b, "tool_call", fingerprint.CapProbeYes, "", logT0.Add(-logDay))
	no := logRow(b, "tool_call", fingerprint.CapProbeNo, "", logT0)
	if want := logT0.Add(7 * logDay); !no.ExpiresAt.Equal(want) {
		t.Fatalf("no.ExpiresAt = %v; want %v", no.ExpiresAt, want)
	}
	appendRows(t, store, yes, no)
	generation, err := store.Generation(ctx)
	if err != nil {
		t.Fatalf("Generation: %v", err)
	}

	mr, prov := logRegistry(t, store, "local", b, "")
	clock := newLogClock(mr, logT0)
	wantCaps(t, "while the no holds", lookupCaps(t, mr, "local"), 0)
	wantModelsCalls(t, "first lookup", prov, 1)

	clock.set(no.ExpiresAt) // its last live instant
	wantCaps(t, "on the last live instant", lookupCaps(t, mr, "local"), 0)
	wantModelsCalls(t, "on the last live instant", prov, 1)

	clock.set(no.ExpiresAt.Add(time.Millisecond))
	wantCaps(t, "after it expires", lookupCaps(t, mr, "local"), CapToolCall)
	wantModelsCalls(t, "after it expires", prov, 2)

	if got, err := store.Generation(ctx); err != nil || got != generation {
		t.Fatalf("Generation = %d, %v; want %d, because nothing wrote", got, err, generation)
	}
}

func TestRegistryFreshness_StaleYesRebuildsOnlyOnce(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	yes := logRow(b, "tool_call", fingerprint.CapProbeYes, "", logT0)
	if want := logT0.Add(30 * logDay); !yes.FreshUntil.Equal(want) || !yes.ExpiresAt.IsZero() {
		t.Fatalf("yes: FreshUntil = %v, ExpiresAt = %v; want %v and none", yes.FreshUntil, yes.ExpiresAt, want)
	}
	appendRows(t, store, yes)

	mr, prov := logRegistry(t, store, "local", b, "")
	clock := newLogClock(mr, logT0.Add(logDay))
	wantCaps(t, "while fresh", lookupCaps(t, mr, "local"), CapToolCall)
	clock.set(yes.FreshUntil)
	wantCaps(t, "on the last fresh instant", lookupCaps(t, mr, "local"), CapToolCall)
	wantModelsCalls(t, "still fresh", prov, 1)

	clock.set(yes.FreshUntil.Add(time.Millisecond))
	wantCaps(t, "just stale", lookupCaps(t, mr, "local"), CapToolCall)
	wantModelsCalls(t, "crossing day 30", prov, 2)

	for _, later := range []time.Duration{time.Millisecond, time.Hour, logDay, 400 * logDay} {
		clock.set(yes.FreshUntil.Add(time.Millisecond + later))
		wantCaps(t, "stale", lookupCaps(t, mr, "local"), CapToolCall)
	}
	wantModelsCalls(t, "stale and cached", prov, 2)
}

func TestRegistryFreshness_NoPastDeadlineRepublished(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	// Every instant these rows define is behind the clock.
	appendRows(t, store,
		logRow(b, "tool_call", fingerprint.CapProbeYes, "", logT0.Add(-100*logDay)),
		logRow(b, "chat", fingerprint.CapProbeNo, "", logT0.Add(-100*logDay)),
	)
	mr, prov := logRegistry(t, store, "local", b, "")
	for range 4 {
		wantCaps(t, "stale yes, expired no", lookupCaps(t, mr, "local"), CapToolCall)
	}
	wantModelsCalls(t, "nothing ahead of the clock", prov, 1)
	if len(mr.profileDeadlines) != 0 {
		t.Fatalf("deadlines = %v; want none", mr.profileDeadlines)
	}
}

// A rebuild that crossed a deadline replaces it with the next one, not the old one.
func TestRegistryFreshness_NextDeadlineAfterRebuild(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	key := ModelKey{Provider: "local", Model: logModel}
	no := logRow(b, "chat", fingerprint.CapProbeNo, "", logT0)
	yes := logRow(b, "tool_call", fingerprint.CapProbeYes, "", logT0)
	appendRows(t, store, no, yes)
	mr, prov := logRegistry(t, store, "local", b, "")
	clock := newLogClock(mr, logT0)

	lookupCaps(t, mr, "local")
	if want := no.ExpiresAt.Add(time.Nanosecond); !mr.profileDeadlines[key].Equal(want) {
		t.Fatalf("deadline = %v; want %v", mr.profileDeadlines[key], want)
	}
	clock.set(no.ExpiresAt.Add(time.Millisecond))
	lookupCaps(t, mr, "local")
	wantModelsCalls(t, "crossing the first", prov, 2)
	if want := yes.FreshUntil.Add(time.Nanosecond); !mr.profileDeadlines[key].Equal(want) {
		t.Fatalf("deadline = %v; want %v", mr.profileDeadlines[key], want)
	}
}

func TestRegistryFreshness_FutureRowBecomesEligibleWithoutWrite(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0.Add(2*time.Hour)))
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	clock := newLogClock(mr, logT0)

	wantCaps(t, "beyond the skew allowance", lookupCaps(t, mr, "local"), 0)
	clock.set(logT0.Add(time.Hour - time.Millisecond))
	wantCaps(t, "just before admission", lookupCaps(t, mr, "local"), 0)
	wantModelsCalls(t, "before admission", prov, 1)

	clock.set(logT0.Add(time.Hour)) // observed_at less the one-hour allowance
	wantCaps(t, "on admission", lookupCaps(t, mr, "local"), CapToolCall)
	wantModelsCalls(t, "on admission", prov, 2)

	clock.set(logT0.Add(3 * time.Hour))
	lookupCaps(t, mr, "local")
	wantModelsCalls(t, "after admission", prov, 2)
}

func TestRegistryFreshness_BuildNotPublishedWhenGenerationMoved(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	key := ModelKey{Provider: "local", Model: logModel}

	prov.onModels = func() {
		prov.onModels = nil // one write, during the first build
		appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	}
	wantCaps(t, "the build that started before the write", lookupCaps(t, mr, "local"), 0)
	mr.mu.RLock()
	_, cached := mr.profiles[key]
	mr.mu.RUnlock()
	if cached {
		t.Fatal("a build the write overtook was published")
	}

	wantCaps(t, "the next build", lookupCaps(t, mr, "local"), CapToolCall)
	wantModelsCalls(t, "two builds", prov, 2)
	lookupCaps(t, mr, "local")
	wantModelsCalls(t, "the second build was published", prov, 2)
}

// hookedEvidenceStore fails or observes the reads the registry makes.
type hookedEvidenceStore struct {
	fingerprint.EvidenceStore
	generationErr, snapshotErr error
	snapshots                  atomic.Int64
	onRead                     func()
	afterGeneration            func() // runs once a Generation read has its answer
}

func (s *hookedEvidenceStore) Generation(ctx context.Context) (int64, error) {
	if s.onRead != nil {
		s.onRead()
	}
	if s.generationErr != nil {
		return 0, s.generationErr
	}
	generation, err := s.EvidenceStore.Generation(ctx)
	if after := s.afterGeneration; after != nil {
		s.afterGeneration = nil
		after()
	}
	return generation, err
}

func (s *hookedEvidenceStore) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (fingerprint.EvidenceSnapshot, error) {
	s.snapshots.Add(1)
	if s.onRead != nil {
		s.onRead()
	}
	if s.snapshotErr != nil {
		return fingerprint.EvidenceSnapshot{}, s.snapshotErr
	}
	if apiKey != nil {
		return fingerprint.EvidenceSnapshot{}, errors.New("the registry asked for account data")
	}
	if legacyProvider != "" {
		return fingerprint.EvidenceSnapshot{}, errors.New("the registry asked for legacy rows")
	}
	return s.EvidenceStore.ReadSnapshot(ctx, endpoint, adapter, apiKey, legacyProvider, models, now)
}

func TestRegistryFreshness_GenerationReadErrorKeepsCache(t *testing.T) {
	store := &hookedEvidenceStore{EvidenceStore: newEvidenceLog(t)}
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	mr.syncGeneration(t.Context())
	wantCaps(t, "cached", lookupCaps(t, mr, "local"), 0)

	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
	store.generationErr = errors.New("database is locked")
	mr.syncGeneration(t.Context())
	wantCaps(t, "a failed read keeps the cache", lookupCaps(t, mr, "local"), 0)
	wantModelsCalls(t, "a failed read keeps the cache", prov, 1)

	store.generationErr = nil
	mr.syncGeneration(t.Context())
	wantCaps(t, "the read recovers", lookupCaps(t, mr, "local"), CapToolCall)
}

// A profile built without the log's answer must not stay cached once the store
// recovers: no write would ever move the generation to drop it.
func TestRegistryFreshness_FailedReadsAreNotPublished(t *testing.T) {
	failing := errors.New("database is locked")
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	answer := []fingerprint.EvidenceRow{logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0)}
	tests := []struct {
		name        string
		rows        []fingerprint.EvidenceRow
		first, then Capability // what the failed build returns, and the next
		fail        func(*hookedEvidenceStore, *logStubProvider)
	}{
		{"snapshot", answer, 0, CapToolCall, func(s *hookedEvidenceStore, _ *logStubProvider) { s.snapshotErr = failing }},
		// A view's zero generation must not pass for a new store's real one.
		{"snapshot of a store never written", nil, 0, 0, func(s *hookedEvidenceStore, _ *logStubProvider) { s.snapshotErr = failing }},
		{"generation after the build", answer, CapToolCall, CapToolCall, func(s *hookedEvidenceStore, p *logStubProvider) {
			p.onModels = func() { s.generationErr = failing }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &hookedEvidenceStore{EvidenceStore: newEvidenceLog(t)}
			if len(tt.rows) > 0 {
				appendRows(t, store, tt.rows...)
			}
			mr, prov := logRegistry(t, store, "local", b, logDigest)
			tt.fail(store, prov)

			wantCaps(t, "the failed build", lookupCaps(t, mr, "local"), tt.first)
			store.snapshotErr, store.generationErr, prov.onModels = nil, nil, nil
			wantCaps(t, "after the store recovers", lookupCaps(t, mr, "local"), tt.then)
			wantModelsCalls(t, "two builds", prov, 2)
		})
	}
}

// The narrowest window: a write and a sync land after the build re-read the
// generation and before it takes the lock. Only overrideVersion can fence that.
func TestRegistryFreshness_SyncFencesBuildPastItsReread(t *testing.T) {
	store := &hookedEvidenceStore{EvidenceStore: newEvidenceLog(t)}
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, prov := logRegistry(t, store, "local", b, logDigest)
	key := ModelKey{Provider: "local", Model: logModel}
	prov.onModels = func() {
		prov.onModels = nil
		store.afterGeneration = func() { // the build's re-read, which saw no move
			appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, logT0))
			mr.syncGeneration(t.Context())
		}
	}
	wantCaps(t, "the overtaken build", lookupCaps(t, mr, "local"), 0)
	if _, cached := mr.profiles[key]; cached {
		t.Fatal("a build the sync overtook was published")
	}
	wantCaps(t, "the next build", lookupCaps(t, mr, "local"), CapToolCall)
}

func TestRegistryFreshness_NoStoreIOUnderLock(t *testing.T) {
	store := &hookedEvidenceStore{EvidenceStore: newEvidenceLog(t)}
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, _ := logRegistry(t, store, "local", b, logDigest)
	key := ModelKey{Provider: "local", Model: logModel}
	reads := 0
	store.onRead = func() {
		reads++
		if !mr.mu.TryLock() {
			t.Error("store I/O while holding r.mu")
			return
		}
		mr.mu.Unlock()
	}
	mr.syncGeneration(t.Context())
	lookupCaps(t, mr, "local")
	if _, err := mr.ExplainToolCall(t.Context(), key); err != nil {
		t.Fatalf("ExplainToolCall: %v", err)
	}
	if _, err := mr.ProjectListedModels(t.Context(), "local", []ModelInfo{{Name: logModel}}); err != nil {
		t.Fatalf("ProjectListedModels: %v", err)
	}
	// sync, build's snapshot and re-read, Explain's sync and snapshot, the projection's snapshot.
	if reads != 6 {
		t.Fatalf("%d store reads; want 6", reads)
	}
}

func TestRegistryFreshness_ConcurrentReadersSyncsAndWrites(t *testing.T) {
	store := newEvidenceLog(t)
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	mr, _ := logRegistry(t, store, "local", b, logDigest)
	key := ModelKey{Provider: "local", Model: logModel}
	clock := newLogClock(mr, logT0)
	base := logT0.Add(-time.Hour)
	// Expiries inside the run keep deadlines passing while builds publish.
	for i := range 20 {
		r := logRow(b, "chat", fingerprint.CapProbeNo, logDigest, base)
		r.ExpiresAt = logT0.Add(time.Duration(i) * time.Millisecond)
		appendRows(t, store, r)
	}

	var g sync.WaitGroup
	ctx := t.Context()
	errs := make(chan error, 64)
	for i := range 4 {
		g.Go(func() {
			for j := range 40 {
				clock.set(logT0.Add(time.Duration(i*j) * time.Millisecond))
				if _, err := mr.Lookup(ctx, key); err != nil {
					errs <- fmt.Errorf("Lookup: %w", err)
					return
				}
				mr.syncGeneration(ctx)
			}
		})
	}
	g.Go(func() {
		for range 10 {
			if err := store.AppendEvidence(ctx, logRow(b, "stream", fingerprint.CapProbeYes, logDigest, base)); err != nil {
				errs <- fmt.Errorf("AppendEvidence: %w", err)
				return
			}
		}
	})
	g.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	appendRows(t, store, logRow(b, "tool_call", fingerprint.CapProbeYes, logDigest, base))
	mr.syncGeneration(ctx)
	if caps := lookupCaps(t, mr, "local"); !caps.Has(CapToolCall) || !caps.Has(CapStream) {
		t.Fatalf("caps = %v; want the last writes", caps)
	}
}
