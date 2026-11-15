package configview

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	exOpenAIBase = "http://127.0.0.1:8080"
	exOllamaBase = "http://127.0.0.1:11434"
)

// exConfig has an openai-compat provider "lc" and an ollama provider "ol",
// both keyed with key, and the given roles.
func exConfig(key string, models map[string]config.ModelConfig) config.Config {
	return config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: key},
			"ol": {BaseURL: exOllamaBase, APIFormat: "ollama", APIKey: key},
		},
		Models: models,
	}
}

func exBinding(t *testing.T, cfg config.Config, name string) provider.EvidenceBinding {
	t.Helper()
	pc := cfg.Providers[name]
	b, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// exStore is a real evidence store in a file of its own.
func exStore(t *testing.T) *fingerprint.SQLiteStore {
	t.Helper()
	u := url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "ev.db"), RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := fingerprint.NewStore(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// exRow is an understood answer row: ollama-show through ollama is what the
// provider reported, chat-stream-tools through openai-compat is a probe.
func exRow(b provider.EvidenceBinding, model, digest, capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	method, source := fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceProbe
	if b.Adapter == fingerprint.AdapterOllama {
		method, source = fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider
	}
	r := fingerprint.EvidenceRow{
		KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: b.Endpoint, Adapter: b.Adapter,
		Model: model, Digest: digest, Capability: capability, Source: source, Kind: fingerprint.EvidenceKindAnswer,
		State: state, Method: method, MethodVersion: 1, ObservedAt: at,
	}
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

func exPublish(t *testing.T, s fingerprint.EvidenceWriter, b provider.EvidenceBinding, key string, at time.Time, facts []fingerprint.EvidenceRow, members ...fingerprint.ListingMember) {
	t.Helper()
	err := s.PublishListings(t.Context(), []fingerprint.ListingPublish{{
		Endpoint: b.Endpoint, Adapter: b.Adapter, APIKey: key, StartedAt: at, Members: members, Facts: facts,
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func exFact(t *testing.T, e CapabilityExplanation, capability string) provider.CapabilityFact {
	t.Helper()
	for _, f := range e.Capabilities {
		if f.Capability == capability {
			return f
		}
	}
	t.Fatalf("no %s fact in %+v", capability, e.Capabilities)
	return provider.CapabilityFact{}
}

func mustExplain(t *testing.T, cfg config.Config, store fingerprint.EvidenceReader, catalog *provider.Catalog, key provider.ModelKey) CapabilityExplanation {
	t.Helper()
	e, err := ExplainCapabilities(t.Context(), cfg, store, catalog, key)
	if err != nil {
		t.Fatalf("ExplainCapabilities(%s) error = %v", key, err)
	}
	return e
}

func mustListing(t *testing.T, cfg config.Config, store fingerprint.EvidenceReader, providerName string) ListingExplanation {
	t.Helper()
	l, err := ExplainListing(t.Context(), cfg, store, providerName)
	if err != nil {
		t.Fatalf("ExplainListing(%s) error = %v", providerName, err)
	}
	return l
}

func listedNames(l ListingExplanation) []string {
	var out []string
	for _, m := range l.Models {
		out = append(out, m.Model)
	}
	return out
}

// snapshotCall is one ReadSnapshot's arguments.
type snapshotCall struct {
	endpoint, adapter string
	apiKey            *string
	legacy            string
	models            []string
}

// recordingReader is an EvidenceReader that records every method called. Its
// ReadSnapshot runs onRead, then delegates to next when set, else returns snap
// and err. Every other method answers nothing.
type recordingReader struct {
	next   fingerprint.EvidenceReader
	snap   fingerprint.EvidenceSnapshot
	err    error
	onRead func()

	mu    sync.Mutex
	calls []string
	reads []snapshotCall
}

func (r *recordingReader) note(method string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, method)
}

func (r *recordingReader) callNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

func (r *recordingReader) snapshotCalls() []snapshotCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads)
}

func (r *recordingReader) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (fingerprint.EvidenceSnapshot, error) {
	var key *string
	if apiKey != nil {
		k := *apiKey
		key = &k
	}
	r.mu.Lock()
	r.calls = append(r.calls, "ReadSnapshot")
	r.reads = append(r.reads, snapshotCall{endpoint: endpoint, adapter: adapter, apiKey: key, legacy: legacyProvider, models: slices.Clone(models)})
	r.mu.Unlock()
	if r.onRead != nil {
		r.onRead()
	}
	if r.next != nil {
		return r.next.ReadSnapshot(ctx, endpoint, adapter, apiKey, legacyProvider, models, now)
	}
	return r.snap, r.err
}

func (r *recordingReader) EvidenceRows(context.Context, string, string) ([]fingerprint.EvidenceRow, error) {
	r.note("EvidenceRows")
	return nil, nil
}

func (r *recordingReader) GetCapProbe(context.Context, string, string, string) (*fingerprint.CapProbe, error) {
	r.note("GetCapProbe")
	return nil, nil
}

func (r *recordingReader) CurrentListing(context.Context, string, string, string) (*fingerprint.ListingSnapshot, error) {
	r.note("CurrentListing")
	return nil, nil
}

func (r *recordingReader) NamedModels(context.Context, string) ([]fingerprint.NamedModel, error) {
	r.note("NamedModels")
	return nil, nil
}

func (r *recordingReader) LatestModelAccess(context.Context, string, string, string) (*fingerprint.ModelAccess, error) {
	r.note("LatestModelAccess")
	return nil, nil
}

func (r *recordingReader) Generation(context.Context) (int64, error) {
	r.note("Generation")
	return 0, nil
}

// goodSnapshot is a snapshot with a listing, rows, history and access for m.
func goodSnapshot(b provider.EvidenceBinding) fingerprint.EvidenceSnapshot {
	at := time.Now().Add(-time.Hour)
	return fingerprint.EvidenceSnapshot{
		Generation: 3,
		Listing:    &fingerprint.ListingSnapshot{Endpoint: b.Endpoint, StartedAt: at, CommittedAt: at, Members: []fingerprint.ListingMember{{Model: "m", Digest: "sha256:d"}}},
		Named:      []fingerprint.NamedModel{{Model: "n", AddedAt: at}},
		Rows:       []fingerprint.EvidenceRow{exRow(b, "m", "sha256:d", "chat", fingerprint.CapProbeYes, at)},
		Access:     map[string]*fingerprint.ModelAccess{"m": {Model: "m", State: fingerprint.AccessAnswered, ObservedAt: at}},
	}
}

func TestExplain_TwoKeysSeeOnlyOwnAccess(t *testing.T) {
	store := exStore(t)
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"a": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: "sk-alpha-key"},
		"b": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: "sk-bravo-key"},
		"c": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: "sk-charlie-key"},
	}}
	b := exBinding(t, cfg, "a")
	at := time.Now().Add(-time.Hour)
	if err := store.RecordModelAccess(t.Context(), b.Endpoint, "sk-alpha-key", "m", fingerprint.AccessRefused, 403, at); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordModelAccess(t.Context(), b.Endpoint, "sk-bravo-key", "m", fingerprint.AccessAnswered, 0, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ea := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "a", Model: "m"})
	if ea.Access == nil || ea.Access.State != "refused" || ea.Access.Status != 403 || !ea.Access.At.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("key A access = %+v; want refused 403 at %v", ea.Access, at)
	}
	eb := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "b", Model: "m"})
	if eb.Access == nil || eb.Access.State != "answered" || eb.Access.Status != 0 {
		t.Fatalf("key B access = %+v; want answered", eb.Access)
	}
	if ec := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "c", Model: "m"}); ec.Access != nil {
		t.Fatalf("key C access = %+v; want none", ec.Access)
	}
}

func TestExplain_SerializedHasNoKeyOrAccount(t *testing.T) {
	store := exStore(t)
	keys := map[string]string{"a": "sk-alpha-secret-0001", "b": "sk-bravo-secret-0002"}
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"a": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: keys["a"]},
		"b": {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: keys["b"]},
	}}
	b := exBinding(t, cfg, "a")
	at := time.Now().Add(-time.Hour)
	for name, key := range keys {
		model, digest := "m-"+name, "sha256:"+name
		// Fact records with a detail and a flag, so they serialize too.
		probe := exRow(b, model, digest, "tool_call", fingerprint.CapProbeYes, at)
		probe.Detail = fingerprint.DetailToolCalled
		flag := exRow(b, model, digest, "tool_call", fingerprint.CapProbeFlagged, at.Add(time.Minute))
		flag.Kind, flag.Source, flag.Detail, flag.HTTPStatus = fingerprint.EvidenceKindFlag, fingerprint.EvidenceSourceObserved, fingerprint.DetailRunRefused, 400
		flag.FreshUntil, flag.ExpiresAt = fingerprint.ExpiryFor(flag, 0)
		if err := store.AppendEvidence(t.Context(), probe, flag); err != nil {
			t.Fatal(err)
		}
		exPublish(t, store, b, key, at, nil, fingerprint.ListingMember{Model: model, OwnedBy: "org", Digest: digest})
		if err := store.RecordModelAccess(t.Context(), b.Endpoint, key, model, fingerprint.AccessRefused, 401, at); err != nil {
			t.Fatal(err)
		}
	}
	account := regexp.MustCompile(`[0-9a-fA-F]{64}`)
	for name := range keys {
		e := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: name, Model: "m-" + name})
		l := mustListing(t, cfg, store, name)
		tool := exFact(t, e, "tool_call")
		if e.Access == nil || !l.HasListing || tool.Detail != fingerprint.DetailToolCalled || tool.Flag == nil || tool.Flag.Status != 400 {
			t.Fatalf("%s: fixture not read: access %+v, listing %+v, tool_call %+v", name, e.Access, l, tool)
		}
		for _, v := range []any{e, l} {
			out, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				if strings.Contains(string(out), key) {
					t.Errorf("%s: output carries an API key: %s", name, out)
				}
			}
			if account.Match(out) {
				t.Errorf("%s: output carries a 64-hex value: %s", name, out)
			}
		}
	}
}

func TestExplain_UnexpandedKeyTemplateReturnsNoAccountData(t *testing.T) {
	store := exStore(t)
	cfg := exConfig("${GOLEM_TEST_UNSET_KEY}", nil)
	b := exBinding(t, cfg, "lc")
	at := time.Now().Add(-time.Hour)
	// Account data exists for the keyless account and for a real key; a
	// template that was never expanded is neither.
	for _, key := range []string{"", "sk-real"} {
		exPublish(t, store, b, key, at, nil, fingerprint.ListingMember{Model: "m"})
		if err := store.RecordModelAccess(t.Context(), b.Endpoint, key, "m", fingerprint.AccessAnswered, 0, at); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddNamedModel(t.Context(), b.Endpoint, "named"); err != nil {
		t.Fatal(err)
	}
	e := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if e.Access != nil {
		t.Fatalf("Access = %+v; want none", e.Access)
	}
	l := mustListing(t, cfg, store, "lc")
	want := ListingExplanation{Named: []string{"named"}, Unavailable: "credential_unresolved"}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("listing = %+v; want %+v", l, want)
	}
}

func TestExplainListing_KeyRotation(t *testing.T) {
	store := exStore(t)
	cfgA := exConfig("sk-key-a", nil)
	cfgB := exConfig("sk-key-b", nil)
	b := exBinding(t, cfgA, "lc")
	at := time.Now().Add(-time.Hour)
	exPublish(t, store, b, "sk-key-a", at, nil, fingerprint.ListingMember{Model: "from-a"})

	if got := listedNames(mustListing(t, cfgA, store, "lc")); !reflect.DeepEqual(got, []string{"from-a"}) {
		t.Fatalf("under A: listed %v; want [from-a]", got)
	}
	if l := mustListing(t, cfgB, store, "lc"); l.HasListing || l.Models != nil {
		t.Fatalf("under B before B's listing: %+v; want none", l)
	}
	// A's listing is still in the table, read directly: B's view is empty
	// because the account differs, not because A's rows are gone.
	if cur, err := store.CurrentListing(t.Context(), b.Endpoint, b.Adapter, "sk-key-a"); err != nil || cur == nil || len(cur.Members) != 1 {
		t.Fatalf("A's listing read directly = %+v, %v; want one member", cur, err)
	}
	exPublish(t, store, b, "sk-key-b", at.Add(time.Minute), nil, fingerprint.ListingMember{Model: "from-b"})
	if got := listedNames(mustListing(t, cfgB, store, "lc")); !reflect.DeepEqual(got, []string{"from-b"}) {
		t.Fatalf("under B after B's listing: listed %v; want [from-b]", got)
	}
}

func TestExplainListing_NamedAlwaysShown(t *testing.T) {
	store := exStore(t)
	cfg := exConfig("", nil)
	b := exBinding(t, cfg, "lc")
	for _, name := range []string{"n2", "n1"} {
		if err := store.AddNamedModel(t.Context(), b.Endpoint, name); err != nil {
			t.Fatal(err)
		}
	}
	named, err := store.NamedModels(t.Context(), b.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, n := range named {
		want = append(want, n.Model)
	}

	rec := &recordingReader{next: store}
	if l := mustListing(t, cfg, rec, "lc"); l.HasListing || !reflect.DeepEqual(l.Named, want) {
		t.Fatalf("no listing: %+v; want named %v", l, want)
	}
	calls := rec.snapshotCalls()
	if len(calls) != 1 || calls[0].legacy != "" || calls[0].models != nil || calls[0].apiKey == nil || *calls[0].apiKey != "" {
		t.Fatalf("ReadSnapshot calls = %+v; want one with the key as given, no legacy provider, no models", calls)
	}
	exPublish(t, store, b, "", time.Now().Add(-time.Hour), nil, fingerprint.ListingMember{Model: "listed"})
	if l := mustListing(t, cfg, store, "lc"); !l.HasListing || !reflect.DeepEqual(l.Named, want) {
		t.Fatalf("with a listing: %+v; want named %v", l, want)
	}
	unresolved := exConfig("${GOLEM_TEST_UNSET_KEY}", nil)
	if l := mustListing(t, unresolved, store, "lc"); l.HasListing || !reflect.DeepEqual(l.Named, want) {
		t.Fatalf("unresolved key: %+v; want named %v", l, want)
	}
}

func TestExplain_ReadErrorIsBounded(t *testing.T) {
	// A failed read names neither host nor path.
	const base = "http://host:8080/llm/v1"
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"remote": {BaseURL: base, APIFormat: "openai-compat", APIKey: "sk-bounded"},
	}}
	storeErr := errors.New(`fingerprint: read snapshot "http://host:8080/llm/v1": disk I/O error`)
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, provider.ErrEvidenceUnreadable) {
			t.Fatalf("%s: error = %v; want ErrEvidenceUnreadable", name, err)
		}
		for _, leak := range []string{"host", "8080", "/llm", "disk", "sk-bounded"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("%s: error %q carries %q", name, err, leak)
			}
		}
	}
	e, err := ExplainCapabilities(t.Context(), cfg, &recordingReader{err: storeErr}, nil, provider.ModelKey{Provider: "remote", Model: "m"})
	check("ExplainCapabilities", err)
	if !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("explanation beside the error = %+v; want zero", e)
	}
	l, err := ExplainListing(t.Context(), cfg, &recordingReader{err: storeErr}, "remote")
	check("ExplainListing", err)
	if !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("listing beside the error = %+v; want zero", l)
	}
}

func cancelledContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func TestExplain_CancelledAtEntry(t *testing.T) {
	cfg := exConfig("", nil)
	rec := &recordingReader{snap: goodSnapshot(exBinding(t, cfg, "lc"))}
	ctx := cancelledContext(t)
	e, err := ExplainCapabilities(ctx, cfg, rec, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero, context.Canceled", e, err)
	}
	l, err := ExplainListing(ctx, cfg, rec, "lc")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("ExplainListing = %+v, %v; want zero, context.Canceled", l, err)
	}
	if calls := rec.callNames(); len(calls) != 0 {
		t.Fatalf("store calls = %v; want none", calls)
	}
}

func TestExplain_CancelledWithNilStore(t *testing.T) {
	// A done context and no store is the context's error.
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "lc", Name: "m", Type: "dense"}})
	ctx := cancelledContext(t)
	e, err := ExplainCapabilities(ctx, cfg, nil, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero, context.Canceled", e, err)
	}
	l, err := ExplainListing(ctx, cfg, nil, "lc")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("ExplainListing = %+v, %v; want zero, context.Canceled", l, err)
	}
}

func TestExplain_CancelledAfterReadReturnsContextError(t *testing.T) {
	// The host cancels while the store is answering.
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "lc", Name: "m", Type: "dense"}})
	snap := goodSnapshot(exBinding(t, cfg, "lc"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	rec := &recordingReader{snap: snap, onRead: cancel}
	e, err := ExplainCapabilities(ctx, cfg, rec, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero, context.Canceled", e, err)
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	rec = &recordingReader{snap: snap, onRead: cancel}
	l, err := ExplainListing(ctx, cfg, rec, "lc")
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("ExplainListing = %+v, %v; want zero, context.Canceled", l, err)
	}
}

func TestExplain_CancelledAfterReadWithPolicyError(t *testing.T) {
	cfg := exConfig("", map[string]config.ModelConfig{
		"chat": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"completion"}},
	})
	key := provider.ModelKey{Provider: "lc", Model: "m"}
	snap := goodSnapshot(exBinding(t, cfg, "lc"))
	// Without cancellation the fixture's invalid list is the error.
	if _, err := ExplainCapabilities(t.Context(), cfg, &recordingReader{snap: snap}, nil, key); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("uncancelled: error = %v; want the policy error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e, err := ExplainCapabilities(ctx, cfg, &recordingReader{snap: snap, onRead: cancel}, nil, key)
	if !errors.Is(err, context.Canceled) || err != ctx.Err() || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero and the context's error, not the policy error", e, err)
	}
}

func TestExplain_CancelledAfterFailedRead(t *testing.T) {
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "lc", Name: "m", Type: "dense"}})
	readErr := errors.New("fingerprint: read snapshot: interrupted")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e, err := ExplainCapabilities(ctx, cfg, &recordingReader{err: readErr, onRead: cancel}, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, provider.ErrEvidenceUnreadable) || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero, context.Canceled and not ErrEvidenceUnreadable", e, err)
	}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	l, err := ExplainListing(ctx, cfg, &recordingReader{err: readErr, onRead: cancel}, "lc")
	if !errors.Is(err, context.Canceled) || errors.Is(err, provider.ErrEvidenceUnreadable) || !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("ExplainListing = %+v, %v; want zero, context.Canceled and not ErrEvidenceUnreadable", l, err)
	}
}

func TestExplain_NilStoreStillExplainsConfigAndCatalog(t *testing.T) {
	catalog, err := provider.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	const known = "qwen3:8b"
	profile := catalog.ProfileFor(known, "")
	if profile == nil || profile.Caps == 0 {
		t.Fatalf("catalog has no profile for %s; the fixture needs one", known)
	}
	cfg := exConfig("", map[string]config.ModelConfig{
		"agent": {Provider: "lc", Name: "typed", Type: "dense"},
		"fixed": {Provider: "lc", Name: "declared", Type: "dense", Capabilities: []string{"chat", "tool_call"}},
	})

	typed := mustExplain(t, cfg, nil, catalog, provider.ModelKey{Provider: "lc", Model: "typed"})
	for _, f := range typed.Capabilities {
		wantYes := f.Capability == "chat" || f.Capability == "generate" || f.Capability == "stream"
		if wantYes && (f.State != "yes" || f.Source != "type" || f.Method != "type:dense") {
			t.Errorf("typed %s = %+v; want a type yes", f.Capability, f)
		}
		if !wantYes && (f.State != "unknown" || f.Source != "none") {
			t.Errorf("typed %s = %+v; want unknown", f.Capability, f)
		}
	}

	declared := mustExplain(t, cfg, nil, catalog, provider.ModelKey{Provider: "lc", Model: "declared"})
	for _, f := range declared.Capabilities {
		want := "no"
		if f.Capability == "chat" || f.Capability == "tool_call" {
			want = "yes"
		}
		if f.State != want || f.Source != "explicit" {
			t.Errorf("declared %s = %+v; want explicit %s", f.Capability, f, want)
		}
	}

	cataloged := mustExplain(t, cfg, nil, catalog, provider.ModelKey{Provider: "ol", Model: known})
	if cataloged.Key != (provider.ModelKey{Provider: "ol", Model: known}) {
		t.Fatalf("Key = %+v", cataloged.Key)
	}
	for _, f := range cataloged.Capabilities {
		bit, err := provider.ParseCapsStrict([]string{f.Capability})
		if err != nil {
			t.Fatal(err)
		}
		if profile.Caps.Has(bit) != (f.State == "yes" && f.Source == "catalog") {
			t.Errorf("cataloged %s = %+v; catalog says %v", f.Capability, f, profile.Caps.Has(bit))
		}
	}

	if l := mustListing(t, cfg, nil, "lc"); !reflect.DeepEqual(l, ListingExplanation{}) {
		t.Fatalf("listing with no store = %+v; want none", l)
	}
}

func TestExplain_OneSnapshotAcrossConcurrentPublish(t *testing.T) {
	store := exStore(t)
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "ol", Name: "m", Type: "dense"}})
	b := exBinding(t, cfg, "ol")
	key := provider.ModelKey{Provider: "ol", Model: "m"}
	base := time.Now().Add(-time.Hour)
	// Each publish lists m at a new digest and, in the same transaction, the
	// provider's chat yes at that digest. A reader that took the listing and
	// the rows from different reads would see a digest with no rows.
	publish := func(i int) error {
		digest := "sha256:gen" + strconv.Itoa(i)
		at := base.Add(time.Duration(i) * time.Millisecond)
		return store.PublishListings(context.Background(), []fingerprint.ListingPublish{{
			Endpoint: b.Endpoint, Adapter: b.Adapter, StartedAt: at,
			Members: []fingerprint.ListingMember{{Model: "m", Digest: digest}},
			Facts:   []fingerprint.EvidenceRow{exRow(b, "m", digest, "chat", fingerprint.CapProbeYes, at)},
		}})
	}
	if err := publish(0); err != nil {
		t.Fatal(err)
	}

	const publishes = 20
	done := make(chan error, 1)
	go func() {
		for i := 1; i <= publishes; i++ {
			if err := publish(i); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	reads := 0
	for running := true; running || reads < 5; reads++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			running = false
		default:
		}
		rec := &recordingReader{next: store}
		e := mustExplain(t, cfg, rec, nil, key)
		if calls := rec.callNames(); !reflect.DeepEqual(calls, []string{"ReadSnapshot"}) {
			t.Fatalf("read %d: store calls = %v; want one ReadSnapshot", reads, calls)
		}
		chat := exFact(t, e, "chat")
		if chat.State != "yes" || chat.Source != "provider" || chat.Identity != "content-digest" {
			t.Fatalf("read %d: chat = %+v; want the provider's yes at the listed digest", reads, chat)
		}
	}
}

func TestExplain_UnknownProviderIsError(t *testing.T) {
	// A provider the config does not define, or whose binding fails, is an error.
	cfg := exConfig("", nil)
	cfg.Providers["badformat"] = config.ProviderConfig{BaseURL: exOpenAIBase, APIFormat: "grpc"}
	cfg.Providers["badurl"] = config.ProviderConfig{BaseURL: "ftp://secret-host.example/path", APIFormat: "openai-compat"}
	for _, name := range []string{"missing", "badformat", "badurl"} {
		rec := &recordingReader{}
		if e, err := ExplainCapabilities(t.Context(), cfg, rec, nil, provider.ModelKey{Provider: name, Model: "m"}); err == nil || !reflect.DeepEqual(e, CapabilityExplanation{}) {
			t.Errorf("ExplainCapabilities(%s) = %+v, %v; want zero and an error", name, e, err)
		} else if strings.Contains(err.Error(), "secret-host") {
			t.Errorf("ExplainCapabilities(%s) error %q carries the URL", name, err)
		}
		if l, err := ExplainListing(t.Context(), cfg, rec, name); err == nil || !reflect.DeepEqual(l, ListingExplanation{}) {
			t.Errorf("ExplainListing(%s) = %+v, %v; want zero and an error", name, l, err)
		} else if strings.Contains(err.Error(), "secret-host") {
			t.Errorf("ExplainListing(%s) error %q carries the URL", name, err)
		}
		if calls := rec.callNames(); len(calls) != 0 {
			t.Errorf("%s: store calls = %v; want none", name, calls)
		}
	}
}

func TestExplain_DigestFromCurrentListing(t *testing.T) {
	// A digest-keyed row counts only once a listing names the model at
	// that digest; with no listing, or no member for the model, the reader
	// reads name-only and the row does not count.
	store := exStore(t)
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "ol", Name: "m", Type: "dense"}})
	b := exBinding(t, cfg, "ol")
	key := provider.ModelKey{Provider: "ol", Model: "m"}
	at := time.Now().Add(-time.Hour)
	if err := store.AppendEvidence(t.Context(), exRow(b, "m", "sha256:cur", "chat", fingerprint.CapProbeYes, at)); err != nil {
		t.Fatal(err)
	}
	if chat := exFact(t, mustExplain(t, cfg, store, nil, key), "chat"); chat.Source != "none" {
		t.Fatalf("no listing: chat = %+v; want unknown", chat)
	}
	exPublish(t, store, b, "", at, nil, fingerprint.ListingMember{Model: "other", Digest: "sha256:cur"})
	if chat := exFact(t, mustExplain(t, cfg, store, nil, key), "chat"); chat.Source != "none" {
		t.Fatalf("listing without m: chat = %+v; want unknown", chat)
	}
	exPublish(t, store, b, "", at.Add(time.Minute), nil, fingerprint.ListingMember{Model: "m", Digest: "sha256:cur"})
	chat := exFact(t, mustExplain(t, cfg, store, nil, key), "chat")
	if chat.State != "yes" || chat.Source != "provider" || chat.Identity != "content-digest" {
		t.Fatalf("listed at the digest: chat = %+v; want the provider's yes", chat)
	}
}

func TestExplain_AliasReadsBothSpellings(t *testing.T) {
	// An Ollama model configured untagged whose evidence and
	// listing are under ":latest", and the reverse.
	for _, tc := range []struct{ configured, stored string }{
		{configured: "qwen3", stored: "qwen3:latest"},
		{configured: "qwen3:latest", stored: "qwen3"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			store := exStore(t)
			cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "ol", Name: tc.configured, Type: "dense"}})
			b := exBinding(t, cfg, "ol")
			at := time.Now().Add(-time.Hour)
			exPublish(t, store, b, "", at, []fingerprint.EvidenceRow{
				exRow(b, tc.stored, "sha256:q", "chat", fingerprint.CapProbeYes, at),
				exRow(b, tc.configured, "sha256:q", "tool_call", fingerprint.CapProbeNo, at),
			}, fingerprint.ListingMember{Model: tc.stored, Digest: "sha256:q"})

			rec := &recordingReader{next: store}
			e := mustExplain(t, cfg, rec, nil, provider.ModelKey{Provider: "ol", Model: tc.configured})
			if chat := exFact(t, e, "chat"); chat.State != "yes" || chat.Source != "provider" || chat.Identity != "content-digest" {
				t.Errorf("chat = %+v; want the other spelling's yes at the listed digest", chat)
			}
			if tool := exFact(t, e, "tool_call"); tool.State != "no" || tool.Source != "provider" {
				t.Errorf("tool_call = %+v; want the configured spelling's no", tool)
			}
			calls := rec.snapshotCalls()
			want := snapshotCall{endpoint: b.Endpoint, adapter: b.Adapter, legacy: "ol", models: []string{"qwen3", "qwen3:latest"}}
			if len(calls) != 1 || calls[0].apiKey == nil || *calls[0].apiKey != "" {
				t.Fatalf("ReadSnapshot calls = %+v; want one with the configured key", calls)
			}
			calls[0].apiKey = nil
			if !reflect.DeepEqual(calls[0], want) {
				t.Fatalf("ReadSnapshot = %+v; want %+v", calls[0], want)
			}
		})
	}
}

func TestExplain_AccessExactSpellingOnly(t *testing.T) {
	// Access is read under the configured spelling only. The newer refusal
	// under m:latest has expired; the older answer under m must not stand in.
	store := exStore(t)
	cfg := exConfig("sk-access", map[string]config.ModelConfig{"chat": {Provider: "ol", Name: "m:latest", Type: "dense"}})
	b := exBinding(t, cfg, "ol")
	now := time.Now()
	if err := store.RecordModelAccess(t.Context(), b.Endpoint, "sk-access", "m", fingerprint.AccessAnswered, 0, now.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordModelAccess(t.Context(), b.Endpoint, "sk-access", "m:latest", fingerprint.AccessRefused, 403, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if e := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "ol", Model: "m:latest"}); e.Access != nil {
		t.Fatalf("m:latest access = %+v; want none", e.Access)
	}
	if e := mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "ol", Model: "m"}); e.Access == nil || e.Access.State != "answered" {
		t.Fatalf("m access = %+v; want the answer under m", e.Access)
	}
}

// endpointLog is an evidence store that records the endpoint of every
// ReadSnapshot.
type endpointLog struct {
	fingerprint.EvidenceStore
	mu        sync.Mutex
	endpoints []string
}

func (l *endpointLog) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (fingerprint.EvidenceSnapshot, error) {
	l.mu.Lock()
	l.endpoints = append(l.endpoints, endpoint)
	l.mu.Unlock()
	return l.EvidenceStore.ReadSnapshot(ctx, endpoint, adapter, apiKey, legacyProvider, models, now)
}

func (l *endpointLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.endpoints)
}

func TestExplain_BindingMatchesBootstrap(t *testing.T) {
	// With a URL override, a host passing bootstrap's effective config
	// reads under the endpoint bootstrap's registry reads under.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	const configured = "http://127.0.0.1:1"
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {BaseURL: configured, APIFormat: "openai-compat"}},
		Models:    map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "m", Type: "dense"}},
	}
	log := &endpointLog{EvidenceStore: exStore(t)}
	bundle, err := providerbootstrap.New(t.Context(), providerbootstrap.Options{
		Config: cfg, EvidenceLog: log,
		OpenAICompatURLOverrideProvider: "lc", OpenAICompatURLOverride: srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	key := provider.ModelKey{Provider: "lc", Model: "m"}
	if _, err := bundle.Models.ExplainToolCall(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	registry := log.seen()
	if len(registry) == 0 {
		t.Fatal("the registry read no snapshot")
	}

	rec := &recordingReader{next: log.EvidenceStore}
	mustExplain(t, *bundle.Config, rec, nil, key)
	mustListing(t, *bundle.Config, rec, "lc")
	override, err := provider.EvidenceBindingFor(srv.URL, "openai-compat")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.snapshotCalls() {
		if c.endpoint != override.Endpoint || c.adapter != override.Adapter {
			t.Fatalf("reader read %s through %s; want the override's %s", c.endpoint, c.adapter, override.Endpoint)
		}
	}
	for _, endpoint := range registry {
		if endpoint != override.Endpoint {
			t.Fatalf("registry read %s; want the override's %s", endpoint, override.Endpoint)
		}
	}
	// The configured URL is not where either reads.
	if b := exBinding(t, *cfg, "lc"); b.Endpoint == override.Endpoint {
		t.Fatalf("fixture: configured and override endpoints are both %s", b.Endpoint)
	}
}

func TestExplain_NeverSeesKeptRows(t *testing.T) {
	// Kept rows live only in a registry; the shared store's log returns
	// stored rows, so nothing the readers explain is ever unsaved. This
	// documents that rule (the readers have no path to a registry's kept
	// rows) more than it guards a plausible regression: only a reader that
	// took a registry could break it, and the readers take none.
	dir := t.TempDir()
	shared, err := fingerprint.OpenSharedStore(t.Context(), func(k string) string {
		if k == "XDG_DATA_HOME" {
			return dir
		}
		return ""
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	log := shared.Log()
	if log == nil {
		t.Fatalf("shared store disabled: %s", shared.Warning())
	}
	cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "lc", Name: "m", Type: "dense"}})
	b := exBinding(t, cfg, "lc")
	at := time.Now().Add(-time.Hour)
	attempt := exRow(b, "m", "", "tool_call", fingerprint.CapProbeInconclusive, at)
	attempt.Kind = fingerprint.EvidenceKindAttempt
	attempt.FreshUntil, attempt.ExpiresAt = fingerprint.ExpiryFor(attempt, 0)
	flag := exRow(b, "m", "", "chat", fingerprint.CapProbeFlagged, at.Add(time.Minute))
	flag.Kind, flag.Source, flag.Detail = fingerprint.EvidenceKindFlag, fingerprint.EvidenceSourceObserved, fingerprint.DetailRunRefused
	if err := log.AppendEvidence(t.Context(),
		exRow(b, "m", "", "chat", fingerprint.CapProbeYes, at),
		exRow(b, "m", "", "stream", fingerprint.CapProbeNo, at),
		attempt, flag,
	); err != nil {
		t.Fatal(err)
	}
	rows, err := log.EvidenceRows(t.Context(), b.Endpoint, "m")
	if err != nil || len(rows) != 4 {
		t.Fatalf("stored rows = %d, %v; want 4", len(rows), err)
	}
	for _, r := range rows {
		if r.ID <= 0 {
			t.Fatalf("stored row has ID %d", r.ID)
		}
	}
	e := mustExplain(t, cfg, log, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if chat := exFact(t, e, "chat"); chat.Source != "probe" || chat.Flag == nil {
		t.Fatalf("chat = %+v; want the stored probe with its flag", chat)
	}
	for _, f := range e.Capabilities {
		unsaved := f.Unsaved || f.LastCheck != nil && f.LastCheck.Unsaved || f.LastAttempt != nil && f.LastAttempt.Unsaved ||
			f.Flag != nil && f.Flag.Unsaved || f.OtherAdapter != nil && f.OtherAdapter.Unsaved
		if unsaved {
			t.Errorf("%s is marked unsaved: %+v", f.Capability, f)
		}
	}
}

// TestExplain_GoldenJSON pins both readers' wire contract. The snapshots come
// from a fake with fixed UTC times, so the output does not depend on the clock
// or the zone. Between them the outputs set every field once, nested fact
// records included, and leave every omitempty or omitzero field absent once,
// except the few the core never leaves empty (a checked answer's method and
// time, a flag's time, an other-adapter answer's method and time). Some inputs
// are ones the store never returns: negative row IDs, so the golden shows
// "unsaved", and access and legacy records with no time, so it shows "at"
// omitted. ModelKey is untagged, as InventoryModel.Key is.
//
// To rewrite the golden after a reviewed change: temporarily add
// os.WriteFile(path, got, 0o644) before the os.ReadFile below, run this test
// once, read `git diff testdata/explain_golden.json` end to end, then remove
// the line.
func TestExplain_GoldenJSON(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	far := time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc":    {BaseURL: exOpenAIBase, APIFormat: "openai-compat", APIKey: "sk-golden"},
			"vault": {BaseURL: "http://127.0.0.1:8081", APIFormat: "openai-compat", APIKey: "${GOLDEN_KEY}"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "m", Type: "dense"},
			"fixed": {Provider: "lc", Name: "x", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}},
		},
	}
	b := exBinding(t, cfg, "lc")
	const digest = "sha256:golden"
	row := func(id int64, capability string, source fingerprint.EvidenceSource, kind fingerprint.EvidenceKind, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
		return fingerprint.EvidenceRow{
			ID: id, KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: b.Endpoint, Adapter: b.Adapter, Model: "m", Digest: digest,
			Capability: capability, Source: source, Kind: kind, State: state,
			Method: fingerprint.MethodChatStreamTools, MethodVersion: 1, ObservedAt: at,
		}
	}
	chatProbe := row(-1, "chat", fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeYes, t0.Add(time.Hour))
	chatProbe.Detail = fingerprint.DetailToolCalled
	chatObserved := row(2, "chat", fingerprint.EvidenceSourceObserved, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeYes, t0.Add(2*time.Hour))
	toolNo := row(3, "tool_call", fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeNo, t0.Add(time.Hour))
	toolNo.Detail, toolNo.ExpiresAt = fingerprint.DetailToolIgnored, far
	toolAttempt := row(4, "tool_call", fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAttempt, fingerprint.CapProbeInconclusive, t0)
	toolAttempt.ExpiresAt = t0.Add(24 * time.Hour)
	chatFlag := row(8, "chat", fingerprint.EvidenceSourceObserved, fingerprint.EvidenceKindFlag, fingerprint.CapProbeFlagged, t0.Add(3*time.Hour))
	chatFlag.Detail, chatFlag.ExpiresAt = fingerprint.DetailRequestRejected, far
	chatOther := row(9, "chat", fingerprint.EvidenceSourceProvider, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeYes, t0.Add(time.Hour))
	chatOther.Adapter, chatOther.Method = fingerprint.AdapterOllama, fingerprint.MethodOllamaShow
	toolFlag := row(-5, "tool_call", fingerprint.EvidenceSourceObserved, fingerprint.EvidenceKindFlag, fingerprint.CapProbeFlagged, t0.Add(3*time.Hour))
	toolFlag.Detail, toolFlag.HTTPStatus, toolFlag.ExpiresAt = fingerprint.DetailRunRefused, 400, far
	toolOther := row(-6, "tool_call", fingerprint.EvidenceSourceProvider, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeYes, t0.Add(time.Hour))
	toolOther.Adapter, toolOther.Method = fingerprint.AdapterOllama, fingerprint.MethodOllamaShow
	streamStale := row(-7, "stream", fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAnswer, fingerprint.CapProbeYes, t0.Add(time.Hour))
	streamStale.FreshUntil = t0.Add(90 * time.Minute)

	full := &recordingReader{snap: fingerprint.EvidenceSnapshot{
		Generation: 7,
		Listing:    &fingerprint.ListingSnapshot{Endpoint: b.Endpoint, StartedAt: t0, CommittedAt: t0.Add(5 * time.Hour), Members: []fingerprint.ListingMember{{Model: "m", OwnedBy: "acme", Digest: digest}}},
		Rows:       []fingerprint.EvidenceRow{chatProbe, chatObserved, chatFlag, chatOther, toolNo, toolAttempt, toolFlag, toolOther, streamStale},
		Legacy:     map[string]*fingerprint.CapProbe{"m": {BackendID: "lc", ModelName: "m", Capability: "tool_call", State: fingerprint.CapProbeYes, TestedAt: t0}},
		Access:     map[string]*fingerprint.ModelAccess{"m": {Model: "m", State: fingerprint.AccessRefused, HTTPStatus: 403, ObservedAt: t0.Add(4 * time.Hour), ExpiresAt: far}},
	}}
	timeless := &recordingReader{snap: fingerprint.EvidenceSnapshot{
		Legacy: map[string]*fingerprint.CapProbe{"x": {BackendID: "lc", ModelName: "x", Capability: "tool_call", State: fingerprint.CapProbeNo}},
		Access: map[string]*fingerprint.ModelAccess{"x": {Model: "x", State: fingerprint.AccessAnswered}},
	}}
	listing := &recordingReader{snap: fingerprint.EvidenceSnapshot{
		Listing: &fingerprint.ListingSnapshot{Endpoint: b.Endpoint, StartedAt: t0, CommittedAt: t0.Add(5 * time.Hour), Members: []fingerprint.ListingMember{
			{Model: "m", OwnedBy: "acme", Digest: digest}, {Model: "n"},
		}},
		Named: []fingerprint.NamedModel{{Model: "q", AddedAt: t0}},
	}}

	golden := struct {
		Capabilities []CapabilityExplanation `json:"capabilities"`
		Listings     []ListingExplanation    `json:"listings"`
	}{
		Capabilities: []CapabilityExplanation{
			mustExplain(t, cfg, full, nil, provider.ModelKey{Provider: "lc", Model: "m"}),
			mustExplain(t, cfg, timeless, nil, provider.ModelKey{Provider: "lc", Model: "x"}),
			mustExplain(t, cfg, nil, nil, provider.ModelKey{Provider: "lc", Model: "x"}),
		},
		Listings: []ListingExplanation{
			mustListing(t, cfg, listing, "lc"),
			mustListing(t, cfg, nil, "vault"),
		},
	}
	got, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "explain_golden.json")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("explanation wire contract drifted from %s:\n--- got ---\n%s", path, got)
	}
}

func TestMasks(t *testing.T) {
	e := CapabilityExplanation{Capabilities: []provider.CapabilityFact{
		{Capability: "chat", State: "yes"},
		{Capability: "stream", State: "yes"},
		{Capability: "tool_call", State: "no"},
		{Capability: "insert", State: "inconclusive"},
		{Capability: "embed", State: "unknown"},
	}}
	yes, no, unknown := e.Masks()
	if yes != provider.CapChat|provider.CapStream || no != provider.CapToolCall || unknown != provider.CapInsert|provider.CapEmbed {
		t.Fatalf("Masks = %v, %v, %v", yes, no, unknown)
	}
	if y, n, u := (CapabilityExplanation{}).Masks(); y != 0 || n != 0 || u != 0 {
		t.Fatalf("empty Masks = %v, %v, %v; want zero", y, n, u)
	}
}

func TestExplain_AccessStatusBounded(t *testing.T) {
	// Only a 4xx status is kept beside an access claim.
	cfg := exConfig("", nil)
	at := time.Now().Add(-time.Hour)
	for _, tc := range []struct{ stored, want int }{{0, 0}, {399, 0}, {400, 400}, {499, 499}, {500, 0}} {
		rec := &recordingReader{snap: fingerprint.EvidenceSnapshot{Access: map[string]*fingerprint.ModelAccess{
			"m": {Model: "m", State: fingerprint.AccessRefused, HTTPStatus: tc.stored, ObservedAt: at},
		}}}
		e := mustExplain(t, cfg, rec, nil, provider.ModelKey{Provider: "lc", Model: "m"})
		if e.Access == nil || e.Access.Status != tc.want {
			t.Errorf("stored status %d: Access = %+v; want status %d", tc.stored, e.Access, tc.want)
		}
	}
}

func TestExplain_AccessStateBounded(t *testing.T) {
	// A stored state other than answered or refused never reaches output.
	cfg := exConfig("", nil)
	at := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		state fingerprint.AccessState
		want  bool
	}{
		{fingerprint.AccessAnswered, true},
		{fingerprint.AccessRefused, true},
		{"upstream said: quota exceeded for org-12345", false},
		{"", false},
	} {
		rec := &recordingReader{snap: fingerprint.EvidenceSnapshot{Access: map[string]*fingerprint.ModelAccess{
			"m": {Model: "m", State: tc.state, ObservedAt: at},
		}}}
		e := mustExplain(t, cfg, rec, nil, provider.ModelKey{Provider: "lc", Model: "m"})
		if (e.Access != nil) != tc.want || e.Access != nil && e.Access.State != string(tc.state) {
			t.Errorf("stored state %q: Access = %+v; want shown %v", tc.state, e.Access, tc.want)
		}
	}
}

func TestExplain_DigestPrefersExactSpelling(t *testing.T) {
	// The listing holds both spellings at different digests: each config
	// reads at its own spelling's digest, not the other's.
	store := exStore(t)
	cfg := exConfig("", map[string]config.ModelConfig{
		"plain":  {Provider: "ol", Name: "m", Type: "dense"},
		"tagged": {Provider: "ol", Name: "m:latest", Type: "dense"},
	})
	b := exBinding(t, cfg, "ol")
	at := time.Now().Add(-time.Hour)
	exPublish(t, store, b, "", at, []fingerprint.EvidenceRow{
		exRow(b, "m", "sha256:A", "chat", fingerprint.CapProbeYes, at),
		exRow(b, "m:latest", "sha256:B", "chat", fingerprint.CapProbeNo, at),
	}, fingerprint.ListingMember{Model: "m", Digest: "sha256:A"}, fingerprint.ListingMember{Model: "m:latest", Digest: "sha256:B"})
	for _, tc := range []struct{ model, want string }{{"m", "yes"}, {"m:latest", "no"}} {
		chat := exFact(t, mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "ol", Model: tc.model}), "chat")
		if chat.State != tc.want || chat.Source != "provider" {
			t.Errorf("config %s: chat = %+v; want the provider's %s at its own digest", tc.model, chat, tc.want)
		}
	}
}

func TestExplain_RowsOfOtherModelsAndEndpointsIgnored(t *testing.T) {
	// A store that answers with rows it was not asked for: only rows for the
	// requested spellings at the binding's endpoint count.
	cfg := exConfig("", nil)
	b := exBinding(t, cfg, "lc")
	at := time.Now().Add(-time.Hour)
	otherModel := exRow(b, "other", "", "tool_call", fingerprint.CapProbeYes, at)
	otherEndpoint := exRow(b, "m", "", "tool_call", fingerprint.CapProbeYes, at)
	otherEndpoint.Endpoint = "http://127.0.0.1:9999"
	rec := &recordingReader{snap: fingerprint.EvidenceSnapshot{Rows: []fingerprint.EvidenceRow{
		exRow(b, "m", "", "chat", fingerprint.CapProbeYes, at), otherModel, otherEndpoint,
	}}}
	e := mustExplain(t, cfg, rec, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if chat := exFact(t, e, "chat"); chat.State != "yes" || chat.Source != "probe" {
		t.Fatalf("chat = %+v; want the requested row's yes", chat)
	}
	if tool := exFact(t, e, "tool_call"); tool.Source != "none" {
		t.Fatalf("tool_call = %+v; want unknown: neither stray row counts", tool)
	}
}

func TestExplain_UnresolvedKeyIgnoresStoreAccountData(t *testing.T) {
	// A store that answers account reads for an unexpanded key anyway: the
	// readers still show no listing, no digest and no access, so has_listing
	// and unavailable never contradict.
	cfg := exConfig("${GOLEM_TEST_UNSET_KEY}", nil)
	b := exBinding(t, cfg, "lc")
	at := time.Now().Add(-time.Hour)
	snap := fingerprint.EvidenceSnapshot{
		Listing: &fingerprint.ListingSnapshot{Endpoint: b.Endpoint, StartedAt: at, CommittedAt: at, Members: []fingerprint.ListingMember{{Model: "m", Digest: "sha256:d"}}},
		Named:   []fingerprint.NamedModel{{Model: "n", AddedAt: at}},
		Rows:    []fingerprint.EvidenceRow{exRow(b, "m", "sha256:d", "chat", fingerprint.CapProbeYes, at)},
		Access:  map[string]*fingerprint.ModelAccess{"m": {Model: "m", State: fingerprint.AccessAnswered, ObservedAt: at}},
	}
	e := mustExplain(t, cfg, &recordingReader{snap: snap}, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if e.Access != nil {
		t.Fatalf("Access = %+v; want none", e.Access)
	}
	if chat := exFact(t, e, "chat"); chat.Source != "none" {
		t.Fatalf("chat = %+v; want unknown: no digest without a listing", chat)
	}
	l := mustListing(t, cfg, &recordingReader{snap: snap}, "lc")
	want := ListingExplanation{Named: []string{"n"}, Unavailable: "credential_unresolved"}
	if !reflect.DeepEqual(l, want) {
		t.Fatalf("listing = %+v; want %+v", l, want)
	}
}

func TestExplain_EmptyModelIsError(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.ProviderConfig{
		"lc": {BaseURL: "http://secret-host.example:8080/llm", APIFormat: "openai-compat", APIKey: "sk-empty-model"},
	}}
	rec := &recordingReader{snap: goodSnapshot(exBinding(t, cfg, "lc"))}
	e, err := ExplainCapabilities(t.Context(), cfg, rec, nil, provider.ModelKey{Provider: "lc"})
	if err == nil || !reflect.DeepEqual(e, CapabilityExplanation{}) {
		t.Fatalf("ExplainCapabilities = %+v, %v; want zero and an error", e, err)
	}
	for _, leak := range []string{"secret-host", "/llm", "sk-empty-model"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q carries %q", err, leak)
		}
	}
	if calls := rec.callNames(); len(calls) != 0 {
		t.Fatalf("store calls = %v; want none", calls)
	}
}

func TestExplain_LegacyExactSpellingOnly(t *testing.T) {
	// Legacy history is the configured spelling's only, as routing's is.
	for _, tc := range []struct{ configured, legacy string }{
		{configured: "m:latest", legacy: "m"},
		{configured: "m", legacy: "m:latest"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			store := exStore(t)
			cfg := exConfig("", map[string]config.ModelConfig{"chat": {Provider: "ol", Name: tc.configured, Type: "dense"}})
			if err := store.SaveCapProbe(t.Context(), fingerprint.CapProbe{
				BackendID: "ol", ModelName: tc.legacy, Capability: "tool_call", State: fingerprint.CapProbeYes,
				ProbeVersion: 1, TestedAt: time.Now().Add(-time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			for _, f := range mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "ol", Model: tc.configured}).Capabilities {
				if f.History != nil {
					t.Errorf("%s: History = %+v from the other spelling; want none", f.Capability, f.History)
				}
			}
			// The same row under the configured spelling is shown.
			tool := exFact(t, mustExplain(t, cfg, store, nil, provider.ModelKey{Provider: "ol", Model: tc.legacy}), "tool_call")
			if tool.History == nil {
				t.Fatalf("explaining %s itself: History = nil; want the stored row", tc.legacy)
			}
		})
	}
}
