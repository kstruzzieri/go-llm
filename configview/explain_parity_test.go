// configview/explain_parity_test.go
//
// Parity (#736): a host's registry, built by bootstrap from a
// config over an evidence log, and the offline readers over the same log and
// config answer alike for one key spelling: routing's profile (Lookup), the
// listing projection of the runtime's own info (ProjectListedModels),
// ExplainToolCall, and ExplainCapabilities. The offline side reads a current
// listing and stored provider rows matching the runtime's live facts, as a
// refresh with WithPublish leaves them. The TestParity_*Gap* and
// _UnreadableLogDivergence tests pin the documented places where the two
// sides differ.
package configview

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// parityModel is what a stub serves for one model: Ollama's /api/tags digest,
// and its /api/show family, template and capabilities (nil omits the array).
type parityModel struct {
	digest, family, template string
	caps                     []string
}

// parityServer serves Ollama's /api/tags and /api/show and openai-compat's
// /v1/models for models; /api/show answers either Ollama spelling of a name.
func parityServer(t *testing.T, models map[string]parityModel) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 0, len(models))
		for name := range models {
			names = append(names, name)
		}
		sort.Strings(names)
		var out any
		switch r.URL.Path {
		case "/api/tags":
			var tags []map[string]string
			for _, name := range names {
				tags = append(tags, map[string]string{"name": name, "digest": models[name].digest})
			}
			out = map[string]any{"models": tags}
		case "/api/show":
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			m, ok := models[body.Name]
			if !ok {
				m, ok = models[body.Name+":latest"]
			}
			if !ok {
				m, ok = models[strings.TrimSuffix(body.Name, ":latest")]
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			show := map[string]any{"template": m.template, "details": map[string]string{"family": m.family}}
			if m.caps != nil {
				show["capabilities"] = m.caps
			}
			out = show
		case "/v1/models":
			var data []map[string]string
			for _, name := range names {
				data = append(data, map[string]string{"id": name, "object": "model"})
			}
			out = map[string]any{"object": "list", "data": data}
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// parityConfig has an ollama provider "ol" serving ol and an openai-compat
// provider "lc" serving lc, each on a stub of its own, and roles.
func parityConfig(t *testing.T, ol, lc map[string]parityModel, roles map[string]config.ModelConfig) config.Config {
	t.Helper()
	return config.Config{
		Providers: map[string]config.ProviderConfig{
			"ol": {BaseURL: parityServer(t, ol), APIFormat: "ollama"},
			"lc": {BaseURL: parityServer(t, lc), APIFormat: "openai-compat"},
		},
		Models: roles,
	}
}

// parityRig is one host: the registry bootstrap builds from a config over an
// evidence log (none when log is nil), and the offline readers over that log.
type parityRig struct {
	cfg       config.Config // bootstrap's effective config
	log       fingerprint.EvidenceStore
	providers *provider.Registry
	models    *provider.ModelRegistry
	catalog   *provider.Catalog
}

// newParityRig bootstraps cfg over log, with fp as the stored fingerprint
// profiles when non-nil.
func newParityRig(t *testing.T, cfg config.Config, log fingerprint.EvidenceStore, fp fingerprint.Store) *parityRig {
	t.Helper()
	b, err := providerbootstrap.New(t.Context(), providerbootstrap.Options{Config: &cfg, EvidenceLog: log, FingerprintProfileStore: fp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	catalog, err := provider.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return &parityRig{cfg: *b.Config, log: log, providers: b.Providers, models: b.Models, catalog: catalog}
}

func (r *parityRig) binding(t *testing.T, name string) provider.EvidenceBinding {
	t.Helper()
	return exBinding(t, r.cfg, name)
}

// publish stores what a refresh with WithPublish stores for provider name: its
// live listing as the current listing and, through Ollama, each member's live
// facts as provider rows. rename, when set, spells members and rows as another
// writer did.
func (r *parityRig) publish(t *testing.T, name string, rename func(string) string) {
	t.Helper()
	p, ok := r.providers.Get(name)
	if !ok {
		t.Fatalf("no provider %q", name)
	}
	infos, err := p.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b := r.binding(t, name)
	at := time.Now().Add(-2 * time.Hour)
	var members []fingerprint.ListingMember
	var facts []fingerprint.EvidenceRow
	for _, info := range infos {
		model := info.Name
		if rename != nil {
			model = rename(model)
		}
		members = append(members, fingerprint.ListingMember{Model: model, Digest: info.Digest})
		if b.Adapter != fingerprint.AdapterOllama {
			continue
		}
		for capability, state := range provider.OllamaListingFacts(info) {
			facts = append(facts, exRow(b, model, info.Digest, capability, state, at))
		}
	}
	exPublish(t, r.log, b, "", at, facts, members...)
}

// appendRows stores rows in the given order.
func (r *parityRig) appendRows(t *testing.T, rows ...fingerprint.EvidenceRow) {
	t.Helper()
	if err := r.log.AppendEvidence(t.Context(), rows...); err != nil {
		t.Fatal(err)
	}
}

// runtimeInfo is what the registry's runtime query finds for key: the listed
// member of that exact name, else the provider's single-model lookup.
func (r *parityRig) runtimeInfo(t *testing.T, key provider.ModelKey) provider.ModelInfo {
	t.Helper()
	p, ok := r.providers.Get(key.Provider)
	if !ok {
		t.Fatalf("no provider %q", key.Provider)
	}
	infos, err := p.Models(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.Name == key.Model {
			return info
		}
	}
	direct, ok := p.(interface {
		ModelInfo(ctx context.Context, name string) (*provider.ModelInfo, error)
	})
	if !ok {
		t.Fatalf("%s is not listed", key)
	}
	info, err := direct.ModelInfo(t.Context(), key.Model)
	if err != nil {
		t.Fatal(err)
	}
	return *info
}

// parityAnswers are the four answers for one key spelling.
type parityAnswers struct {
	lookup    provider.Capability          // routing: Lookup's profile
	projected provider.ListedModelFacts    // the projection of the runtime's info
	explained provider.ToolCallExplanation // ExplainToolCall
	read      CapabilityExplanation        // the offline reader
	readErr   error
}

func (r *parityRig) answers(t *testing.T, key provider.ModelKey) parityAnswers {
	t.Helper()
	var a parityAnswers
	p, err := r.models.Lookup(t.Context(), key)
	if err != nil {
		t.Fatalf("Lookup(%s): %v", key, err)
	}
	a.lookup = p.Caps
	facts, err := r.models.ProjectListedModels(t.Context(), key.Provider, []provider.ModelInfo{r.runtimeInfo(t, key)})
	if err != nil || len(facts) != 1 {
		t.Fatalf("ProjectListedModels(%s) = %+v, %v", key, facts, err)
	}
	a.projected = facts[0]
	if a.explained, err = r.models.ExplainToolCall(t.Context(), key); err != nil {
		t.Fatalf("ExplainToolCall(%s): %v", key, err)
	}
	a.read, a.readErr = ExplainCapabilities(t.Context(), r.cfg, r.log, r.catalog, key)
	return a
}

// agree asserts that all four answers for key say yes to exactly want, and that
// the projection and the reader know the same capabilities.
func (r *parityRig) agree(t *testing.T, key provider.ModelKey, want provider.Capability) {
	t.Helper()
	a := r.answers(t, key)
	if a.readErr != nil {
		t.Fatalf("%s: ExplainCapabilities: %v", key, a.readErr)
	}
	yes, no, _ := a.read.Masks()
	if a.lookup != want || a.projected.Caps != want || a.explained.Caps != want || yes != want {
		t.Errorf("%s: Lookup %s, projected %s, ExplainToolCall %s, reader %s; want all %s",
			key, a.lookup, a.projected.Caps, a.explained.Caps, yes, want)
	}
	if a.projected.KnownMask != yes|no {
		t.Errorf("%s: projected KnownMask %s; reader knows %s", key, a.projected.KnownMask, yes|no)
	}
	if a.explained.Has != want.Has(provider.CapToolCall) || a.explained.EvidenceUnreadable {
		t.Errorf("%s: ExplainToolCall = %+v; want Has %v, a readable log", key, a.explained, want.Has(provider.CapToolCall))
	}
}

// probeRow is a streamed check's answer through lc, name-only.
func (r *parityRig) probeRow(t *testing.T, model, capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	t.Helper()
	return exRow(r.binding(t, "lc"), model, "", capability, state, at)
}

// observedRow is an in-use answer through ol at digest: a direct answer.
func (r *parityRig) observedRow(t *testing.T, model, digest, capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	t.Helper()
	row := exRow(r.binding(t, "ol"), model, digest, capability, state, at)
	row.Source = fingerprint.EvidenceSourceObserved
	return row
}

const (
	capChat, capGenerate, capInsert, capEmbed = provider.CapChat, provider.CapGenerate, provider.CapInsert, provider.CapEmbed
	capStream, capToolCall, capThinking       = provider.CapStream, provider.CapToolCall, provider.CapThinking
	parityCatalogModel                        = "qwen3:8b" // the catalog gives it chat, generate, stream and tool_call
	suffixTemplate                            = "{{ .Prompt }}{{ .Suffix }}"
)

func olKey(model string) provider.ModelKey { return provider.ModelKey{Provider: "ol", Model: model} }
func lcKey(model string) provider.ModelKey { return provider.ModelKey{Provider: "lc", Model: model} }

func TestRoutingInventoryAndExplainAgree(t *testing.T) {
	hourAgo := time.Now().Add(-time.Hour)
	dense := func(name string, caps ...string) config.ModelConfig {
		return config.ModelConfig{Provider: "lc", Name: name, Type: "dense", Capabilities: caps}
	}
	tests := []struct {
		name   string
		ol, lc map[string]parityModel
		roles  map[string]config.ModelConfig
		rows   func(*parityRig) []fingerprint.EvidenceRow
		key    provider.ModelKey
		want   provider.Capability
	}{
		// Every canonical capability, as the provider reports it.
		{
			name: "ollama reports every capability but embed",
			ol:   map[string]parityModel{"native-all": {digest: "sha256:all", template: suffixTemplate, caps: []string{"completion", "tools", "insert", "thinking"}}},
			key:  olKey("native-all"), want: capChat | capGenerate | capStream | capToolCall | capInsert | capThinking,
		},
		{
			name: "ollama reports embed",
			ol:   map[string]parityModel{"native-embed": {digest: "sha256:emb", caps: []string{"embedding"}}},
			key:  olKey("native-embed"), want: capEmbed,
		},
		{
			name: "a streamed check's answers",
			lc:   map[string]parityModel{"probed": {}},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{
					r.probeRow(t, "probed", "chat", fingerprint.CapProbeYes, hourAgo),
					r.probeRow(t, "probed", "stream", fingerprint.CapProbeYes, hourAgo),
					r.probeRow(t, "probed", "tool_call", fingerprint.CapProbeYes, hourAgo),
				}
			},
			key: lcKey("probed"), want: capChat | capStream | capToolCall,
		},
		{
			name:  "the type tier",
			lc:    map[string]parityModel{"typed": {}},
			roles: map[string]config.ModelConfig{"chat": dense("typed")},
			key:   lcKey("typed"), want: capChat | capGenerate | capStream,
		},
		{
			name: "the catalog alone",
			lc:   map[string]parityModel{parityCatalogModel: {}},
			key:  lcKey(parityCatalogModel), want: capChat | capGenerate | capStream | capToolCall,
		},
		// The conflicts, each with exactly its two sources.
		{
			name: "a checked no beats a catalog yes",
			lc:   map[string]parityModel{parityCatalogModel: {}},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{r.probeRow(t, parityCatalogModel, "tool_call", fingerprint.CapProbeNo, hourAgo)}
			},
			key: lcKey(parityCatalogModel), want: capChat | capGenerate | capStream,
		},
		{
			name:  "a checked no beats a type yes",
			lc:    map[string]parityModel{"typed": {}},
			roles: map[string]config.ModelConfig{"chat": dense("typed")},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{r.probeRow(t, "typed", "stream", fingerprint.CapProbeNo, hourAgo)}
			},
			key: lcKey("typed"), want: capChat | capGenerate,
		},
		{
			name: "a direct no beats the provider's yes",
			ol:   map[string]parityModel{"live-tools": {digest: "sha256:lt", caps: []string{"completion", "tools"}}},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{r.observedRow(t, "live-tools", "sha256:lt", "tool_call", fingerprint.CapProbeNo, hourAgo)}
			},
			key: olKey("live-tools"), want: capChat | capGenerate | capStream,
		},
		{
			// ["completion"] is an array without tools: the provider's no. Its
			// chat, generate and stream agree with the catalog.
			name: "the provider's no beats a catalog yes",
			ol:   map[string]parityModel{parityCatalogModel: {digest: "sha256:q8", caps: []string{"completion"}}},
			key:  olKey(parityCatalogModel), want: capChat | capGenerate | capStream,
		},
		{
			name:  "an explicit list beats a checked yes",
			lc:    map[string]parityModel{"listed": {}},
			roles: map[string]config.ModelConfig{"agent": dense("listed", "chat")},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{r.probeRow(t, "listed", "tool_call", fingerprint.CapProbeYes, hourAgo)}
			},
			key: lcKey("listed"), want: capChat,
		},
		{
			name:  "an explicit list beats a checked no",
			lc:    map[string]parityModel{"listed": {}},
			roles: map[string]config.ModelConfig{"agent": dense("listed", "chat", "tool_call")},
			rows: func(r *parityRig) []fingerprint.EvidenceRow {
				return []fingerprint.EvidenceRow{r.probeRow(t, "listed", "tool_call", fingerprint.CapProbeNo, hourAgo)}
			},
			key: lcKey("listed"), want: capChat | capToolCall,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newParityRig(t, parityConfig(t, tt.ol, tt.lc, tt.roles), exStore(t), nil)
			r.publish(t, tt.key.Provider, nil)
			if tt.rows != nil {
				r.appendRows(t, tt.rows(r)...)
			}
			r.agree(t, tt.key, tt.want)
		})
	}

	// A stored fingerprint profile's capabilities count without the log (the
	// control) and not with it, where such profiles keep only their memory and
	// context figures; the readers never see them.
	t.Run("stored fingerprint capabilities do not count", func(t *testing.T) {
		cfg := parityConfig(t, map[string]parityModel{"fp-model": {digest: "sha256:fp"}}, nil, nil)
		fp := exStore(t)
		err := fp.Save(t.Context(), fingerprint.Profile{
			ModelName: "fp-model", ModelDigest: "sha256:fp", BackendID: "ol", ModelKind: fingerprint.ModelKindChat,
			Capabilities: []string{"completion", "tools", "embedding"}, ProfileVersion: fingerprint.CurrentProfileVersion, TestedAt: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		control, err := newParityRig(t, cfg, nil, fp).models.Lookup(t.Context(), olKey("fp-model"))
		if err != nil || !control.Caps.Has(capToolCall|capEmbed) {
			t.Fatalf("control without the log: Lookup = %+v, %v; want the profile's tool_call and embed", control, err)
		}
		r := newParityRig(t, cfg, exStore(t), fp)
		r.publish(t, "ol", nil)
		r.agree(t, olKey("fp-model"), 0)
	})
}

// A model shared by several roles: bootstrap's override and floor builders and
// config.CapabilityPolicy take the first explicit list in sorted role order
// even when an earlier role has none, treat two orderings of one list as the
// same list, and union the types of the roles without a list.
func TestParity_SharedRoleExplicitAndTypeUnion(t *testing.T) {
	role := func(name, typ string, caps ...string) config.ModelConfig {
		return config.ModelConfig{Provider: "lc", Name: name, Type: typ, Capabilities: caps}
	}
	cfg := parityConfig(t, nil,
		map[string]parityModel{"shared": {}, "same-list": {}, "union": {}},
		map[string]config.ModelConfig{
			// Only the later role (in sorted order) declares a list.
			"a-chat":  role("shared", "dense"),
			"b-agent": role("shared", "dense", "chat", "stream", "tool_call"),
			// The same list in another order is no conflict.
			"c-one": role("same-list", "dense", "chat", "tool_call"),
			"d-two": role("same-list", "dense", "tool_call", "chat"),
			// Roles without a list contribute the union of their types.
			"e-chat":  role("union", "dense"),
			"f-embed": role("union", "embedding"),
		})
	r := newParityRig(t, cfg, exStore(t), nil)
	r.publish(t, "lc", nil)
	r.agree(t, lcKey("shared"), capChat|capStream|capToolCall)
	r.agree(t, lcKey("same-list"), capChat|capToolCall)
	r.agree(t, lcKey("union"), capChat|capGenerate|capStream|capEmbed)
}

// Uncatalogued Ollama models whose embed and thinking only the provider reports
// (ollama-show rows, the one method that answers them).
func TestParity_UnfamiliarNativeEmbeddingAndThinking(t *testing.T) {
	cfg := parityConfig(t, map[string]parityModel{
		"unfamiliar-embed": {digest: "sha256:ue", caps: []string{"embedding"}},
		"unfamiliar-think": {digest: "sha256:ut", caps: []string{"completion", "thinking"}},
	}, nil, nil)
	r := newParityRig(t, cfg, exStore(t), nil)
	r.publish(t, "ol", nil)
	for model, c := range map[string]struct {
		capability string
		want       provider.Capability
	}{
		"unfamiliar-embed": {"embed", capEmbed},
		"unfamiliar-think": {"thinking", capChat | capGenerate | capStream | capThinking},
	} {
		if p := r.catalog.ProfileFor(model, ""); p != nil {
			t.Fatalf("fixture: the catalog knows %s", model)
		}
		r.agree(t, olKey(model), c.want)
		e := mustExplain(t, r.cfg, r.log, r.catalog, olKey(model))
		if f := exFact(t, e, c.capability); f.State != "yes" || f.Source != "provider" || f.Method != fingerprint.MethodOllamaShow {
			t.Errorf("%s %s = %+v; want the provider's ollama-show yes", model, c.capability, f)
		}
	}
}

// Row order does not matter, and rows the readers do not understand or that do
// not count for this route are ignored on every path.
func TestParity_RowPermutationAndInvalidRows(t *testing.T) {
	now := time.Now()
	rows := func(r *parityRig) []fingerprint.EvidenceRow {
		valid := func(capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
			return r.probeRow(t, "perm", capability, state, at)
		}
		invalid := func(edit func(*fingerprint.EvidenceRow)) fingerprint.EvidenceRow {
			row := valid("stream", fingerprint.CapProbeYes, now.Add(-time.Hour))
			edit(&row)
			return row
		}
		other := r.binding(t, "ol")
		return []fingerprint.EvidenceRow{
			valid("tool_call", fingerprint.CapProbeYes, now.Add(-3*time.Hour)),
			valid("chat", fingerprint.CapProbeYes, now.Add(-2*time.Hour)),
			valid("tool_call", fingerprint.CapProbeNo, now.Add(-time.Hour)), // newest tool_call answer
			invalid(func(row *fingerprint.EvidenceRow) { row.Method = "future-check" }),
			invalid(func(row *fingerprint.EvidenceRow) { row.MethodVersion = 99 }),
			invalid(func(row *fingerprint.EvidenceRow) { row.Source = "rumor" }),
			invalid(func(row *fingerprint.EvidenceRow) { row.State = "maybe" }),
			invalid(func(row *fingerprint.EvidenceRow) { row.Capability = "embed" }), // chat-stream-tools never answers embed
			invalid(func(row *fingerprint.EvidenceRow) { row.ExpiresAt = now.Add(-time.Minute) }),
			invalid(func(row *fingerprint.EvidenceRow) { row.ObservedAt = now.Add(24 * time.Hour) }), // beyond the clock-skew allowance
			invalid(func(row *fingerprint.EvidenceRow) { row.Digest = "sha256:other" }),              // the runtime is name-only
			invalid(func(row *fingerprint.EvidenceRow) { row.Endpoint = "http://127.0.0.2:9/v1" }),
			invalid(func(row *fingerprint.EvidenceRow) { // another adapter's answer is shown, never counted
				row.Adapter, row.Method, row.Source = other.Adapter, fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider
			}),
		}
	}
	for _, order := range []string{"forward", "reversed", "one at a time, reversed"} {
		t.Run(order, func(t *testing.T) {
			r := newParityRig(t, parityConfig(t, nil, map[string]parityModel{"perm": {}}, nil), exStore(t), nil)
			r.publish(t, "lc", nil)
			rs := rows(r)
			switch order {
			case "forward":
				r.appendRows(t, rs...)
			case "reversed":
				slices.Reverse(rs)
				r.appendRows(t, rs...)
			default:
				slices.Reverse(rs)
				for _, row := range rs {
					r.appendRows(t, row)
				}
			}
			r.agree(t, lcKey("perm"), capChat)
		})
	}
}

// An Ollama model configured under one spelling whose evidence was stored under
// the other (m and m:latest): rows of either spelling count for both. The direct no is the only source live facts do
// not repeat, so a path that reads one spelling sees the provider's yes.
func TestParity_AliasBothDirections(t *testing.T) {
	const digest = "sha256:alias"
	live := parityModel{digest: digest, caps: []string{"completion", "tools"}}
	want := capChat | capGenerate | capStream
	tests := []struct {
		name       string
		served     string // the name Ollama lists
		configured string // the role's name
		stored     string // the spelling the listing and every row were stored under
	}{
		{"config m, rows m:latest", "alias-a:latest", "alias-a", "alias-a:latest"},
		{"config m:latest, rows m", "alias-b:latest", "alias-b:latest", "alias-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := parityConfig(t, map[string]parityModel{tt.served: live}, nil,
				map[string]config.ModelConfig{"chat": {Provider: "ol", Name: tt.configured, Type: "dense"}})
			r := newParityRig(t, cfg, exStore(t), nil)
			r.publish(t, "ol", func(string) string { return tt.stored })
			r.appendRows(t, r.observedRow(t, tt.stored, digest, "tool_call", fingerprint.CapProbeNo, time.Now().Add(-time.Hour)))
			r.agree(t, olKey(tt.configured), want)
			r.agree(t, olKey(tt.stored), want)
		})
	}
}

// flakyLog is an evidence log whose snapshot reads fail while failing is set.
type flakyLog struct {
	fingerprint.EvidenceStore
	failing atomic.Bool
}

func (l *flakyLog) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (fingerprint.EvidenceSnapshot, error) {
	if l.failing.Load() {
		return fingerprint.EvidenceSnapshot{}, errors.New("database is locked")
	}
	return l.EvidenceStore.ReadSnapshot(ctx, endpoint, adapter, apiKey, legacyProvider, models, now)
}

// A documented divergence (#735): while the log cannot be read, routing keeps
// the answer it cached; ExplainToolCall and the projection, which read fresh,
// fall back to the catalog, ExplainToolCall saying so; the reader refuses.
func TestParity_UnreadableLogDivergence(t *testing.T) {
	log := &flakyLog{EvidenceStore: exStore(t)}
	r := newParityRig(t, parityConfig(t, nil, map[string]parityModel{parityCatalogModel: {}}, nil), log, nil)
	r.publish(t, "lc", nil)
	r.appendRows(t, r.probeRow(t, parityCatalogModel, "tool_call", fingerprint.CapProbeNo, time.Now().Add(-time.Hour)))
	key := lcKey(parityCatalogModel)
	checked, catalog := capChat|capGenerate|capStream, capChat|capGenerate|capStream|capToolCall
	r.agree(t, key, checked)
	// Routing caches its answer. (agree's ExplainToolCall made the registry's
	// first generation sync, which drops what was cached before it.)
	if p, err := r.models.Lookup(t.Context(), key); err != nil || p.Caps != checked {
		t.Fatalf("Lookup = %+v, %v; want %s", p, err, checked)
	}

	log.failing.Store(true)
	a := r.answers(t, key)
	if a.lookup != checked {
		t.Errorf("Lookup = %s; want the cached %s", a.lookup, checked)
	}
	if want := (provider.ToolCallExplanation{Caps: catalog, Has: true, Source: "catalog", EvidenceUnreadable: true}); a.explained != want {
		t.Errorf("ExplainToolCall = %+v; want %+v", a.explained, want)
	}
	if a.projected.Caps != catalog {
		t.Errorf("projected = %s; want the catalog's %s", a.projected.Caps, catalog)
	}
	if !errors.Is(a.readErr, provider.ErrEvidenceUnreadable) {
		t.Errorf("reader error = %v; want ErrEvidenceUnreadable", a.readErr)
	}

	log.failing.Store(false)
	r.agree(t, key, checked)
}

// A documented gap: the runtime's family reaches routing's catalog ladder
// only; the reader passes no hint, so a model the catalog knows only by that family is unknown
// offline.
func TestParity_FamilyHintGapDocumented(t *testing.T) {
	const model = "custom-build"
	r := newParityRig(t, parityConfig(t, map[string]parityModel{model: {digest: "sha256:cb", family: "qwen3"}}, nil, nil), exStore(t), nil)
	r.publish(t, "ol", nil)
	family := r.catalog.ProfileFor(model, "qwen3")
	if r.catalog.ProfileFor(model, "") != nil || family == nil || family.Caps == 0 {
		t.Fatalf("fixture: the catalog must know %s only by the qwen3 family", model)
	}
	a := r.answers(t, olKey(model))
	if a.readErr != nil {
		t.Fatal(a.readErr)
	}
	if a.lookup != family.Caps || a.projected.Caps != family.Caps || a.explained.Caps != family.Caps {
		t.Errorf("Lookup %s, projected %s, ExplainToolCall %s; want the family's %s", a.lookup, a.projected.Caps, a.explained.Caps, family.Caps)
	}
	if yes, _, _ := a.read.Masks(); yes != 0 {
		t.Errorf("reader yes = %s; want none (no family hint offline)", yes)
	}
}

// A documented gap: routing reads Ollama's live facts; the reader sees provider
// facts only once a listing published them.
func TestParity_LiveFactsWithoutStoredRowsGap(t *testing.T) {
	const model = "live-only"
	r := newParityRig(t, parityConfig(t, map[string]parityModel{model: {digest: "sha256:lo", caps: []string{"completion", "tools"}}}, nil, nil), exStore(t), nil)
	want := capChat | capGenerate | capStream | capToolCall
	a := r.answers(t, olKey(model))
	if a.readErr != nil {
		t.Fatal(a.readErr)
	}
	if a.lookup != want || a.projected.Caps != want || a.explained.Caps != want {
		t.Errorf("Lookup %s, projected %s, ExplainToolCall %s; want the live %s", a.lookup, a.projected.Caps, a.explained.Caps, want)
	}
	if yes, no, _ := a.read.Masks(); yes|no != 0 {
		t.Errorf("reader yes %s, no %s; want nothing before a listing is published", yes, no)
	}
	r.publish(t, "ol", nil)
	r.agree(t, olKey(model), want)
}

// A documented gap: config decisions (the explicit list, the type floor) match
// the selector's exact spelling, so a configured qwen3 and a listed
// qwen3:latest are two selectors, with or without the log, as before #736.
// Only evidence rows and listing members are read under both spellings.
func TestParity_TaggedListingIsASeparateSelectorGap(t *testing.T) {
	explicit := capChat | capStream
	listed := capChat | capGenerate | capStream | capToolCall // the catalog's qwen3 and the live array agree
	cfg := parityConfig(t, map[string]parityModel{"qwen3:latest": {digest: "sha256:q3", caps: []string{"completion", "tools"}}}, nil,
		map[string]config.ModelConfig{"chat": {Provider: "ol", Name: "qwen3", Type: "dense", Capabilities: []string{"chat", "stream"}}})
	for _, withLog := range []bool{false, true} {
		var log fingerprint.EvidenceStore
		if withLog {
			log = exStore(t)
		}
		r := newParityRig(t, cfg, log, nil)
		if withLog {
			r.publish(t, "ol", nil)
		}
		if p, err := r.models.Lookup(t.Context(), olKey("qwen3")); err != nil || p.Caps != explicit {
			t.Fatalf("log %v: Lookup(qwen3) = %+v, %v; want the list %s", withLog, p, err, explicit)
		}
		info := r.runtimeInfo(t, olKey("qwen3:latest"))
		facts, err := r.models.ProjectListedModels(t.Context(), "ol", []provider.ModelInfo{info})
		if err != nil || len(facts) != 1 || facts[0].Caps != listed {
			t.Fatalf("log %v: projection of qwen3:latest = %+v, %v; want %s, not qwen3's list", withLog, facts, err, listed)
		}
		if withLog {
			r.agree(t, olKey("qwen3:latest"), listed)
			r.agree(t, olKey("qwen3"), explicit)
		}
	}
}
