package configio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	cpLocal  = "http://127.0.0.1:8080"
	cpOllama = "http://127.0.0.1:11434"
	cpRemote = "https://api.example.test"
	cpKey    = "sk-plan-test-key-7f3a"
	// cpURLPassword is the password of a base URL that carries userinfo.
	cpURLPassword = "pw-url-secret-41c"
)

const cpTools = provider.CapChat | provider.CapStream | provider.CapToolCall

// cpConfig has openai-compat "lc" on loopback, ollama "ol" and openai-compat
// "rc" on a remote host, all keyed with cpKey, and the given roles.
func cpConfig(roles map[string]config.ModelConfig) *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: cpLocal, APIFormat: "openai-compat", APIKey: cpKey},
			"ol": {BaseURL: cpOllama, APIFormat: "ollama", APIKey: cpKey},
			"rc": {BaseURL: cpRemote, APIFormat: "openai-compat", APIKey: cpKey},
		},
		Models: roles,
	}
}

// cpAuthored is one dense role per model on providerName. A dense role's type
// floor says yes to chat and stream, which never makes an answer fresh.
func cpAuthored(providerName string, models ...string) map[string]config.ModelConfig {
	roles := map[string]config.ModelConfig{}
	for i, m := range models {
		roles[fmt.Sprintf("%s-%02d", providerName, i)] = config.ModelConfig{Provider: providerName, Name: m, Type: "dense"}
	}
	return roles
}

func cpBinding(t *testing.T, cfg *config.Config, name string) provider.EvidenceBinding {
	t.Helper()
	pc := cfg.Providers[name]
	b, err := provider.EvidenceBindingFor(pc.BaseURL, pc.APIFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cpRow is an understood row for model at b, observed at, with the expiry
// ExpiryFor writes: through openai-compat a chat-stream-tools probe, through
// ollama what ollama-show reported. An inconclusive state is an attempt.
func cpRow(b provider.EvidenceBinding, model, digest, capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	method, source := fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceProbe
	if b.Adapter == fingerprint.AdapterOllama {
		method, source = fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider
	}
	kind := fingerprint.EvidenceKindAnswer
	if state == fingerprint.CapProbeInconclusive {
		kind = fingerprint.EvidenceKindAttempt
	}
	r := fingerprint.EvidenceRow{
		KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: b.Endpoint, Adapter: b.Adapter,
		Model: model, Digest: digest, Capability: capability, Source: source, Kind: kind,
		State: state, Method: method, MethodVersion: 1, ObservedAt: at,
	}
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

// cpYes is a yes row per capability for model at b, observed an hour ago.
func cpYes(b provider.EvidenceBinding, model string, capabilities ...string) []fingerprint.EvidenceRow {
	at := time.Now().Add(-time.Hour)
	var rows []fingerprint.EvidenceRow
	for _, c := range capabilities {
		rows = append(rows, cpRow(b, model, "", c, fingerprint.CapProbeYes, at))
	}
	return rows
}

func cpAppend(t *testing.T, s fingerprint.EvidenceWriter, rows ...fingerprint.EvidenceRow) {
	t.Helper()
	if err := s.AppendEvidence(t.Context(), rows...); err != nil {
		t.Fatal(err)
	}
}

// cpList publishes b's listing for key.
func cpList(t *testing.T, s fingerprint.EvidenceWriter, b provider.EvidenceBinding, key string, members ...string) {
	t.Helper()
	pub := fingerprint.ListingPublish{Endpoint: b.Endpoint, Adapter: b.Adapter, APIKey: key, StartedAt: time.Now().Add(-time.Minute), Members: []fingerprint.ListingMember{}}
	for _, m := range members {
		pub.Members = append(pub.Members, fingerprint.ListingMember{Model: m})
	}
	if err := s.PublishListings(t.Context(), []fingerprint.ListingPublish{pub}); err != nil {
		t.Fatal(err)
	}
}

func cpPlan(t *testing.T, cfg *config.Config, store fingerprint.EvidenceReader, providerName string, models []string, caps provider.Capability, force bool) CheckPlan {
	t.Helper()
	p, err := PlanCheck(t.Context(), providerName, models, caps, CheckOptions{Config: cfg, Store: store, Force: force})
	if err != nil {
		t.Fatalf("PlanCheck(%s, %q, %s, force=%v) error = %v", providerName, models, caps, force, err)
	}
	return p
}

func cpPlanned(p CheckPlan) []string {
	out := []string{}
	for _, it := range p.Items {
		out = append(out, it.Model)
	}
	return out
}

// cpWantCode fails unless err carries want and the plan beside it is zero.
func cpWantCode(t *testing.T, p CheckPlan, err error, want ErrorCode) {
	t.Helper()
	if code, ok := CodeOf(err); !ok || code != want {
		t.Fatalf("error = %v (code %q, %v); want code %q", err, code, ok, want)
	}
	if !reflect.DeepEqual(p, CheckPlan{}) {
		t.Fatalf("plan beside the error = %+v; want zero", p)
	}
}

// cpRead is one ReadSnapshot call's arguments.
type cpRead struct {
	endpoint, adapter, apiKey, legacy string
	models                            []string
}

// cpReader is an EvidenceReader that records ReadSnapshot calls and answers
// them with snap and err; every other method fails the read it is asked for.
type cpReader struct {
	snap   fingerprint.EvidenceSnapshot
	err    error
	onRead func() // runs inside ReadSnapshot, before it answers

	mu    sync.Mutex
	reads []cpRead
	other []string
}

func (r *cpReader) ReadSnapshot(_ context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, _ time.Time) (fingerprint.EvidenceSnapshot, error) {
	key := "<nil>"
	if apiKey != nil {
		key = *apiKey
	}
	r.mu.Lock()
	r.reads = append(r.reads, cpRead{endpoint: endpoint, adapter: adapter, apiKey: key, legacy: legacyProvider, models: slices.Clone(models)})
	r.mu.Unlock()
	if r.onRead != nil {
		r.onRead()
	}
	return r.snap, r.err
}

func (r *cpReader) note(m string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.other = append(r.other, m)
	return errors.New("cpReader: unexpected " + m)
}

func (r *cpReader) EvidenceRows(context.Context, string, string) ([]fingerprint.EvidenceRow, error) {
	return nil, r.note("EvidenceRows")
}

func (r *cpReader) GetCapProbe(context.Context, string, string, string) (*fingerprint.CapProbe, error) {
	return nil, r.note("GetCapProbe")
}

func (r *cpReader) CurrentListing(context.Context, string, string, string) (*fingerprint.ListingSnapshot, error) {
	return nil, r.note("CurrentListing")
}

func (r *cpReader) NamedModels(context.Context, string) ([]fingerprint.NamedModel, error) {
	return nil, r.note("NamedModels")
}

func (r *cpReader) LatestModelAccess(context.Context, string, string, string) (*fingerprint.ModelAccess, error) {
	return nil, r.note("LatestModelAccess")
}

func (r *cpReader) Generation(context.Context) (int64, error) { return 0, r.note("Generation") }

func (r *cpReader) calls() ([]cpRead, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads), slices.Clone(r.other)
}

// cpCloneInputs deep-copies in, so a test can change one field.
func cpCloneInputs(in planInputs) planInputs {
	out := in
	out.models = slices.Clone(in.models)
	out.policies = make([]config.ModelCapabilityPolicy, len(in.policies))
	for i, p := range in.policies {
		p.Explicit, p.Derived, p.TypeNames = slices.Clone(p.Explicit), slices.Clone(p.Derived), slices.Clone(p.TypeNames)
		out.policies[i] = p
	}
	out.items = make([]planItem, len(in.items))
	for i, it := range in.items {
		it.item.Capabilities, it.item.Methods = slices.Clone(it.item.Capabilities), slices.Clone(it.item.Methods)
		it.recipes = slices.Clone(it.recipes)
		for j := range it.recipes {
			it.recipes[j].Capabilities = slices.Clone(it.recipes[j].Capabilities)
		}
		out.items[i] = it
	}
	return out
}

func TestPlanCheck_SendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	roles := cpAuthored("lc", "m")
	for k, v := range cpAuthored("ol", "m") {
		roles["x"+k] = v
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: srv.URL, APIFormat: "openai-compat", APIKey: cpKey},
			"ol": {BaseURL: srv.URL, APIFormat: "ollama", APIKey: cpKey},
		},
		Models: roles,
	}
	store := newStore(t)
	for _, name := range []string{"lc", "ol"} {
		if p := cpPlan(t, cfg, store, name, []string{"m"}, cpTools, true); !slices.Equal(cpPlanned(p), []string{"m"}) {
			t.Fatalf("%s: planned %q; want [m]", name, cpPlanned(p))
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("server received %d requests; want none", n)
	}
}

func TestPlanCheck_ListingRequired(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	lc := cpBinding(t, cfg, "lc")
	cpList(t, store, lc, cpKey, "a")
	// b is listed only under another key, and named only for another endpoint.
	cpList(t, store, lc, "sk-other-key", "b")
	if err := store.AddNamedModel(t.Context(), cpBinding(t, cfg, "ol").Endpoint, "b"); err != nil {
		t.Fatal(err)
	}
	if p := cpPlan(t, cfg, store, "lc", []string{"a"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"a"}) {
		t.Fatalf("control: planned %q; want [a]", cpPlanned(p))
	}
	p, err := PlanCheck(t.Context(), "lc", []string{"a", "b"}, cpTools, CheckOptions{Config: cfg, Store: store})
	cpWantCode(t, p, err, CodeListingRequired)
}

func TestPlanCheck_BatchTooLarge(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	var models []string
	for i := range 26 {
		models = append(models, fmt.Sprintf("m%02d", i))
	}
	cpList(t, store, cpBinding(t, cfg, "lc"), cpKey, models...)

	p, err := PlanCheck(t.Context(), "lc", models, cpTools, CheckOptions{Config: cfg, Store: store})
	cpWantCode(t, p, err, CodeBatchTooLarge)
	if p := cpPlan(t, cfg, store, "lc", models[:25], cpTools, false); len(p.Items) != 25 {
		t.Fatalf("25 models: %d items; want 25", len(p.Items))
	}
}

func TestPlanCheck_AuthoredWithoutListingPlans(t *testing.T) {
	// The provider listed nothing; an authored model still plans.
	cfg := cpConfig(cpAuthored("lc", "m"))
	store := newStore(t)
	cpList(t, store, cpBinding(t, cfg, "lc"), cpKey)
	if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"m"}) {
		t.Fatalf("planned %q; want [m]", cpPlanned(p))
	}
	p, err := PlanCheck(t.Context(), "lc", []string{"x"}, cpTools, CheckOptions{Config: cfg, Store: store})
	cpWantCode(t, p, err, CodeListingRequired)
}

func TestPlanCheck_NamedPlans(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	if err := store.AddNamedModel(t.Context(), cpBinding(t, cfg, "lc").Endpoint, "n"); err != nil {
		t.Fatal(err)
	}
	if p := cpPlan(t, cfg, store, "lc", []string{"n"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"n"}) {
		t.Fatalf("lc: planned %q; want [n]", cpPlanned(p))
	}
	// A name stored under the other Ollama spelling names the model, as a
	// listing member does.
	if err := store.AddNamedModel(t.Context(), cpBinding(t, cfg, "ol").Endpoint, "q:latest"); err != nil {
		t.Fatal(err)
	}
	if p := cpPlan(t, cfg, store, "ol", []string{"q"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"q"}) {
		t.Fatalf("ol: planned %q; want [q]", cpPlanned(p))
	}
}

func TestPlanCheck_ListedUnderOtherOllamaSpelling(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	cpList(t, store, cpBinding(t, cfg, "ol"), cpKey, "qwen3:latest", "gemma")
	for _, m := range []string{"qwen3", "gemma:latest"} {
		if p := cpPlan(t, cfg, store, "ol", []string{m}, cpTools, false); !slices.Equal(cpPlanned(p), []string{m}) {
			t.Fatalf("%s: planned %q; want [%s]", m, cpPlanned(p), m)
		}
	}
	// Through openai-compat a spelling is only itself.
	cpList(t, store, cpBinding(t, cfg, "lc"), cpKey, "m:latest")
	p, err := PlanCheck(t.Context(), "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: store})
	cpWantCode(t, p, err, CodeListingRequired)
}

func TestPlanCheck_AllFreshIsEmpty(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	lc := cpBinding(t, cfg, "lc")
	cpList(t, store, lc, cpKey, "m")
	cpAppend(t, store, cpYes(lc, "m", "chat", "stream", "tool_call")...)
	p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false)
	if len(p.Items) != 0 {
		t.Fatalf("items = %+v; want none", p.Items)
	}
	if p.Provider != "lc" || p.EndpointHost != "127.0.0.1:8080" || p.Adapter != fingerprint.AdapterOpenAICompat || p.Force || p.Identity() == "" {
		t.Fatalf("plan = %+v, identity %q; want lc at 127.0.0.1:8080 through openai-compat, unforced, sealed", p, p.Identity())
	}
}

func TestPlanCheck_FreshNoSkippedLikeFreshYes(t *testing.T) {
	for _, state := range []fingerprint.CapProbeState{fingerprint.CapProbeYes, fingerprint.CapProbeNo} {
		t.Run(string(state), func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			cpAppend(t, store, cpYes(lc, "m", "chat", "stream")...)
			cpAppend(t, store, cpRow(lc, "m", "", "tool_call", state, time.Now().Add(-time.Hour)))
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); len(p.Items) != 0 {
				t.Fatalf("planned %q; want none", cpPlanned(p))
			}
		})
	}
}

func TestPlanCheck_StaleYesPlanned(t *testing.T) {
	// A yes past its FreshUntil is still valid, and still planned.
	cfg := cpConfig(nil)
	store := newStore(t)
	lc := cpBinding(t, cfg, "lc")
	cpList(t, store, lc, cpKey, "m")
	cpAppend(t, store, cpYes(lc, "m", "chat", "stream")...)
	stale := cpRow(lc, "m", "", "tool_call", fingerprint.CapProbeYes, time.Now().Add(-2*time.Hour))
	stale.FreshUntil = time.Now().Add(-time.Hour)
	cpAppend(t, store, stale)
	if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"m"}) {
		t.Fatalf("planned %q; want [m]", cpPlanned(p))
	}
}

func TestPlanCheck_StaleNameOnlyYesPlanned(t *testing.T) {
	for _, tc := range []struct {
		age     time.Duration
		planned bool
	}{
		{29 * 24 * time.Hour, false},
		{31 * 24 * time.Hour, true},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			cpAppend(t, store, cpYes(lc, "m", "chat", "stream")...)
			cpAppend(t, store, cpRow(lc, "m", "", "tool_call", fingerprint.CapProbeYes, time.Now().Add(-tc.age)))
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); (len(p.Items) == 1) != tc.planned {
				t.Fatalf("planned %q; want planned=%v", cpPlanned(p), tc.planned)
			}
		})
	}
}

func TestPlanCheck_ExpiredNoPlanned(t *testing.T) {
	for _, tc := range []struct {
		age     time.Duration
		planned bool
	}{
		{6 * 24 * time.Hour, false},
		{8 * 24 * time.Hour, true},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			cpAppend(t, store, cpYes(lc, "m", "chat", "stream")...)
			cpAppend(t, store, cpRow(lc, "m", "", "tool_call", fingerprint.CapProbeNo, time.Now().Add(-tc.age)))
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); (len(p.Items) == 1) != tc.planned {
				t.Fatalf("planned %q; want planned=%v", cpPlanned(p), tc.planned)
			}
		})
	}
}

func TestPlanCheck_ForcePlansFresh(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	lc := cpBinding(t, cfg, "lc")
	cpList(t, store, lc, cpKey, "m")
	cpAppend(t, store, cpYes(lc, "m", "chat", "stream", "tool_call")...)
	if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); len(p.Items) != 0 {
		t.Fatalf("unforced: planned %q; want none", cpPlanned(p))
	}
	p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, true)
	if !slices.Equal(cpPlanned(p), []string{"m"}) || !p.Force {
		t.Fatalf("forced: planned %q, Force %v; want [m], true", cpPlanned(p), p.Force)
	}
}

func TestPlanCheck_ForceNeedsAllThreeFresh(t *testing.T) {
	all := []string{"chat", "stream", "tool_call"}
	for _, missing := range append([]string{""}, all...) {
		t.Run("missing="+missing, func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			fresh := slices.DeleteFunc(slices.Clone(all), func(c string) bool { return c == missing })
			cpAppend(t, store, cpYes(lc, "m", fresh...)...)
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); (len(p.Items) == 1) != (missing != "") {
				t.Fatalf("unforced: planned %q; want planned=%v", cpPlanned(p), missing != "")
			}
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, true); !slices.Equal(cpPlanned(p), []string{"m"}) {
				t.Fatalf("forced: planned %q; want [m]", cpPlanned(p))
			}
		})
	}
}

func TestPlanCheck_ExplicitListNeverPlannedEvenForced(t *testing.T) {
	for _, name := range []string{"lc", "ol"} {
		t.Run(name, func(t *testing.T) {
			cfg := cpConfig(map[string]config.ModelConfig{
				"fixed": {Provider: name, Name: "fixed", Capabilities: []string{"chat", "stream"}},
				"free":  {Provider: name, Name: "free"},
			})
			p := cpPlan(t, cfg, nil, name, []string{"fixed", "free"}, cpTools, true)
			if !slices.Equal(cpPlanned(p), []string{"free"}) {
				t.Fatalf("planned %q; want [free]", cpPlanned(p))
			}
		})
	}
}

func TestPlanCheck_LaterRoleExplicitListSkips(t *testing.T) {
	// The explicit list sits on the role that sorts last; the effective policy
	// still has it, as it does for bootstrap and the explanation.
	cfg := cpConfig(map[string]config.ModelConfig{
		"a-chat":  {Provider: "lc", Name: "m", Type: "dense"},
		"z-tools": {Provider: "lc", Name: "m", Capabilities: []string{"chat", "stream"}},
	})
	if p := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, true); len(p.Items) != 0 {
		t.Fatalf("planned %q; want none", cpPlanned(p))
	}
	e, err := configview.ExplainCapabilities(t.Context(), *cfg, nil, nil, provider.ModelKey{Provider: "lc", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range e.Capabilities {
		if f.Source != "explicit" {
			t.Fatalf("explanation: %s source %q; want explicit", f.Capability, f.Source)
		}
	}
}

func TestPlanCheck_ToolCallNotRequestedUsesChatStream(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	for _, tc := range []struct {
		caps   provider.Capability
		method string
	}{
		{provider.CapChat, fingerprint.MethodChatStream},
		{provider.CapStream, fingerprint.MethodChatStream},
		{provider.CapChat | provider.CapStream, fingerprint.MethodChatStream},
		{provider.CapToolCall, fingerprint.MethodChatStreamTools},
		{provider.CapChat | provider.CapToolCall, fingerprint.MethodChatStreamTools},
		{cpTools, fingerprint.MethodChatStreamTools},
	} {
		p := cpPlan(t, cfg, nil, "lc", []string{"m"}, tc.caps, false)
		if len(p.Items) != 1 || !slices.Equal(p.Items[0].Methods, []string{tc.method}) || !slices.Equal(p.Items[0].Capabilities, tc.caps.Names()) {
			t.Fatalf("%s: items = %+v; want one %s item for %q", tc.caps, p.Items, tc.method, tc.caps.Names())
		}
	}
}

func TestPlanCheck_CeilingsPerAdapter(t *testing.T) {
	const mib = 1 << 20
	all7 := provider.CapChat | provider.CapGenerate | provider.CapInsert | provider.CapEmbed | provider.CapStream | provider.CapToolCall | provider.CapThinking
	cases := []struct {
		name     string
		provider string
		timeout  time.Duration // provider timeout; 0 keeps the default
		caps     provider.Capability
		want     CheckItem
	}{
		{"ollama", "ol", 0, cpTools, CheckItem{Methods: []string{"ollama-show"}, UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 0, ModelLoads: 0, RequestTimeout: 30 * time.Second, MaxResponseBytes: mib}},
		{"ollama all seven", "ol", 0, all7, CheckItem{Methods: []string{"ollama-show"}, UsualRequests: 1, MaxRequests: 1, RequestTimeout: 30 * time.Second, MaxResponseBytes: mib}},
		{"local tools", "lc", 0, cpTools, CheckItem{Methods: []string{"chat-stream-tools"}, UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, ModelLoads: 1, RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib}},
		{"local chat", "lc", 0, provider.CapChat | provider.CapStream, CheckItem{Methods: []string{"chat-stream"}, UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 1, ModelLoads: 1, RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib}},
		{"remote tools", "rc", 0, cpTools, CheckItem{Methods: []string{"chat-stream-tools"}, UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, ModelLoads: 0, RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib}},
		{"remote chat", "rc", 0, provider.CapChat, CheckItem{Methods: []string{"chat-stream"}, UsualRequests: 1, MaxRequests: 1, MaxOutputTokensPerRequest: 1, ModelLoads: 0, RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib}},
		{"shorter provider timeout wins", "lc", time.Minute, cpTools, CheckItem{Methods: []string{"chat-stream-tools"}, UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, ModelLoads: 1, RequestTimeout: time.Minute, MaxResponseBytes: mib}},
		{"shorter ollama timeout wins", "ol", 10 * time.Second, cpTools, CheckItem{Methods: []string{"ollama-show"}, UsualRequests: 1, MaxRequests: 1, RequestTimeout: 10 * time.Second, MaxResponseBytes: mib}},
		{"longer provider timeout loses", "lc", 10 * time.Minute, cpTools, CheckItem{Methods: []string{"chat-stream-tools"}, UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, ModelLoads: 1, RequestTimeout: 3 * time.Minute, MaxResponseBytes: mib}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cpConfig(cpAuthored(tc.provider, "m"))
			pc := cfg.Providers[tc.provider]
			pc.Timeout.Duration = tc.timeout
			cfg.Providers[tc.provider] = pc
			p := cpPlan(t, cfg, nil, tc.provider, []string{"m"}, tc.caps, false)
			want := tc.want
			want.Model, want.Capabilities = "m", tc.caps.Names()
			if len(p.Items) != 1 || !reflect.DeepEqual(p.Items[0], want) {
				t.Fatalf("items = %+v; want [%+v]", p.Items, want)
			}
		})
	}
}

func TestPlanCheck_UnexpandedKeyRefused(t *testing.T) {
	for _, key := range []string{"${OPENAI_API_KEY}", "sk-${SUFFIX}"} {
		cfg := cpConfig(cpAuthored("lc", "m"))
		pc := cfg.Providers["lc"]
		pc.APIKey = key
		cfg.Providers["lc"] = pc
		rec := &cpReader{}
		p, err := PlanCheck(t.Context(), "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: rec})
		cpWantCode(t, p, err, CodeInvalidArgument)
		if reads, other := rec.calls(); len(reads) != 0 || len(other) != 0 {
			t.Fatalf("%s: store calls %+v %v; want none", key, reads, other)
		}
	}
}

func TestPlanCheck_IdentityOpaqueAndKeyDependent(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	p := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, true)
	id := p.Identity()
	if raw, err := hex.DecodeString(id); err != nil || len(raw) != 32 {
		t.Fatalf("identity %q; want 32 bytes of hex", id)
	}
	if again := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, true); again.Identity() == id {
		t.Fatal("two plans of the same inputs share an identity; want a per-plan secret")
	}

	// With the secret fixed, the identity depends on the key and nothing else
	// of it is kept.
	secret := [32]byte{1, 2, 3}
	in := p.state.inputs
	a := sealPlan(secret, "sk-key-a", &in)
	if a != sealPlan(secret, "sk-key-a", &in) {
		t.Fatal("sealPlan is not deterministic")
	}
	for _, other := range []string{"sk-key-b", "", "sk-key-a "} {
		if sealPlan(secret, other, &in) == a {
			t.Fatalf("key %q seals like sk-key-a", other)
		}
	}
	if strings.Contains(a, "sk-key-a") {
		t.Fatal("identity holds the key")
	}

	js, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	secretHex := hex.EncodeToString(p.state.secret[:])
	for _, out := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), string(js), fmt.Sprintf("%#v", *p.state)} {
		if strings.Contains(out, cpKey) {
			t.Fatalf("plan output holds the key: %s", out)
		}
	}
	for _, out := range []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), string(js)} {
		if strings.Contains(out, secretHex) || strings.Contains(out, fmt.Sprint(p.state.secret)) {
			t.Fatalf("plan output holds the plan secret: %s", out)
		}
	}
}

func TestPlanCheck_ZeroUnknownAndUnsupportedMasksRejected(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	for k, v := range cpAuthored("ol", "m") {
		cfg.Models["x"+k] = v
	}
	for _, tc := range []struct {
		provider string
		caps     provider.Capability
	}{
		{"lc", 0},
		{"lc", 1 << 7},
		{"lc", provider.CapChat | 1<<31},
		{"lc", provider.CapEmbed},
		{"lc", provider.CapGenerate},
		{"lc", provider.CapInsert},
		{"lc", provider.CapThinking},
		{"lc", provider.CapChat | provider.CapEmbed},
		{"lc", cpTools | provider.CapThinking},
		{"ol", 0},
		{"ol", 1 << 7},
		{"ol", provider.CapEmbed | 1<<12},
	} {
		rec := &cpReader{}
		p, err := PlanCheck(t.Context(), tc.provider, []string{"m"}, tc.caps, CheckOptions{Config: cfg, Store: rec})
		cpWantCode(t, p, err, CodeInvalidArgument)
		if reads, _ := rec.calls(); len(reads) != 0 {
			t.Fatalf("%s %d: read the store; want no read", tc.provider, tc.caps)
		}
	}
	// Ollama answers every canonical capability from metadata.
	if p := cpPlan(t, cfg, nil, "ol", []string{"m"}, provider.CapEmbed|provider.CapInsert|provider.CapThinking, false); len(p.Items) != 1 {
		t.Fatalf("ollama embed: items %+v; want one", p.Items)
	}
}

func TestPlanCheck_EmptyAndInvalidModelIDsRejected(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "ok"))
	for _, models := range [][]string{nil, {}, {""}, {" "}, {"\t\n"}, {"a\x00b"}, {"\xff"}, {"ok", ""}, {"ok", "x\x00"}} {
		rec := &cpReader{}
		p, err := PlanCheck(t.Context(), "lc", models, cpTools, CheckOptions{Config: cfg, Store: rec})
		cpWantCode(t, p, err, CodeInvalidArgument)
		if reads, _ := rec.calls(); len(reads) != 0 {
			t.Fatalf("%q: read the store; want no read", models)
		}
	}
}

func TestPlanCheck_InvalidInputsRejected(t *testing.T) {
	type mutate func(cfg *config.Config, opts *CheckOptions)
	setProvider := func(name string, pc config.ProviderConfig) mutate {
		return func(cfg *config.Config, _ *CheckOptions) { cfg.Providers[name] = pc }
	}
	valid := config.ProviderConfig{BaseURL: cpLocal, APIFormat: "openai-compat", APIKey: cpKey}
	withURL := func(u string) config.ProviderConfig { pc := valid; pc.BaseURL = u; return pc }
	cases := []struct {
		name     string
		ctx      context.Context
		provider string
		models   []string
		change   mutate
	}{
		{name: "nil context", ctx: nil, provider: "lc", models: []string{"m"}},
		{name: "nil config", provider: "lc", models: []string{"m"}, change: func(_ *config.Config, o *CheckOptions) { o.Config = nil }},
		{name: "unknown provider", provider: "nope", models: []string{"m"}},
		{name: "empty provider name", provider: "", models: []string{"m"}, change: setProvider("", valid)},
		{name: "control character in provider name", provider: "a\nb", models: []string{"m"}, change: setProvider("a\nb", valid)},
		{name: "slash in provider name", provider: "a/b", models: []string{"m"}, change: setProvider("a/b", valid)},
		{name: "not a URL", provider: "lc", models: []string{"m"}, change: setProvider("lc", withURL("not a url"))},
		{name: "ftp URL", provider: "lc", models: []string{"m"}, change: setProvider("lc", withURL("ftp://127.0.0.1:8080"))},
		{name: "userinfo", provider: "lc", models: []string{"m"}, change: setProvider("lc", withURL("http://u:"+cpURLPassword+"@127.0.0.1:8080"))},
		{name: "query", provider: "lc", models: []string{"m"}, change: setProvider("lc", withURL("http://127.0.0.1:8080/?q=1"))},
		{name: "unknown api format", provider: "lc", models: []string{"m"}, change: setProvider("lc", config.ProviderConfig{BaseURL: cpLocal, APIFormat: "grpc", APIKey: cpKey})},
		{name: "unresolved key", provider: "lc", models: []string{"m"}, change: setProvider("lc", config.ProviderConfig{BaseURL: cpLocal, APIFormat: "openai-compat", APIKey: "${K}"})},
		{name: "invalid UTF-8 model", provider: "lc", models: []string{"m", "m\xc3"}},
		{name: "empty runtime digest", provider: "lc", models: []string{"m"}, change: func(_ *config.Config, o *CheckOptions) { o.RuntimeDigests = map[string]string{"m": ""} }},
		{name: "NUL in runtime digest", provider: "lc", models: []string{"m"}, change: func(_ *config.Config, o *CheckOptions) { o.RuntimeDigests = map[string]string{"m": "sha256:a\x00b"} }},
		{name: "invalid explicit list", provider: "lc", models: []string{"m"}, change: func(cfg *config.Config, _ *CheckOptions) {
			cfg.Models["bad"] = config.ModelConfig{Provider: "lc", Name: "m", Capabilities: []string{"completion"}}
		}},
		{name: "conflicting explicit lists", provider: "lc", models: []string{"m"}, change: func(cfg *config.Config, _ *CheckOptions) {
			cfg.Models["e1"] = config.ModelConfig{Provider: "lc", Name: "m", Capabilities: []string{"chat"}}
			cfg.Models["e2"] = config.ModelConfig{Provider: "lc", Name: "m", Capabilities: []string{"stream"}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cpConfig(cpAuthored("lc", "m"))
			rec := &cpReader{}
			opts := CheckOptions{Config: cfg, Store: rec}
			if tc.change != nil {
				tc.change(cfg, &opts)
			}
			ctx := tc.ctx
			if tc.name != "nil context" {
				ctx = t.Context()
			}
			p, err := PlanCheck(ctx, tc.provider, tc.models, cpTools, opts)
			cpWantCode(t, p, err, CodeInvalidArgument)
			if reads, _ := rec.calls(); len(reads) != 0 {
				t.Fatalf("read the store; want no read")
			}
			if strings.Contains(err.Error(), cpKey) || strings.Contains(err.Error(), cpURLPassword) {
				t.Fatalf("error %q holds the key or the URL's password", err)
			}
		})
	}
	// A digest for a model not requested is ignored, invalid or not.
	cfg := cpConfig(cpAuthored("lc", "m"))
	p, err := PlanCheck(t.Context(), "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, RuntimeDigests: map[string]string{"other": "", "x": "a\x00"}})
	if err != nil || len(p.Items) != 1 {
		t.Fatalf("PlanCheck = %+v, %v; want one item", p, err)
	}
	if len(p.state.runtimeDigests) != 0 {
		t.Fatalf("plan keeps digests %v of models not requested", p.state.runtimeDigests)
	}
}

func TestPlanCheck_DeduplicatesBeforeBatchLimit(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	var models []string
	for i := range 25 {
		models = append(models, fmt.Sprintf("m%02d", i))
	}
	cpList(t, store, cpBinding(t, cfg, "lc"), cpKey, models...)
	inputs := append(slices.Clone(models), "m07") // 26 inputs, 25 unique
	p := cpPlan(t, cfg, store, "lc", inputs, cpTools, false)
	if !slices.Equal(cpPlanned(p), models) {
		t.Fatalf("planned %q; want each of %q once", cpPlanned(p), models)
	}
}

func TestPlanCheck_NormalizationPreservesExactIDs(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	cpList(t, store, cpBinding(t, cfg, "lc"), cpKey, " m", "m ", "M", "m")
	p := cpPlan(t, cfg, store, "lc", []string{"m ", "M", " m", "m", "M"}, cpTools, false)
	if want := []string{" m", "M", "m", "m "}; !slices.Equal(cpPlanned(p), want) {
		t.Fatalf("planned %q; want %q", cpPlanned(p), want)
	}
	if want := []string{" m", "M", "m", "m "}; !slices.Equal(p.state.inputs.models, want) {
		t.Fatalf("frozen models %q; want %q", p.state.inputs.models, want)
	}
}

func TestPlanCheck_InconclusiveCanBeExplicitlyRechecked(t *testing.T) {
	// An inconclusive attempt inside its cooldown, 24 hours or a check_failed
	// attempt's 5 minutes, does not stop an explicit, unforced check.
	for _, tt := range []struct {
		detail string
		age    time.Duration
	}{
		{fingerprint.DetailToolIgnored, time.Hour},
		{fingerprint.DetailCheckFailed, time.Minute},
	} {
		t.Run(tt.detail, func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			cpAppend(t, store, cpYes(lc, "m", "chat", "stream")...)
			attempt := cpRow(lc, "m", "", "tool_call", fingerprint.CapProbeInconclusive, time.Now().Add(-tt.age))
			attempt.Detail = tt.detail
			attempt.FreshUntil, attempt.ExpiresAt = fingerprint.ExpiryFor(attempt, 0)
			if !attempt.ExpiresAt.After(time.Now()) {
				t.Fatalf("precondition: attempt expires at %s, already past", attempt.ExpiresAt)
			}
			cpAppend(t, store, attempt)
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false); !slices.Equal(cpPlanned(p), []string{"m"}) {
				t.Fatalf("planned %q; want [m]", cpPlanned(p))
			}
		})
	}
}

func TestPlanCheck_NilStoreAuthoredModelHasSealedPlan(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	p := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	if !slices.Equal(cpPlanned(p), []string{"m"}) || p.state == nil || len(p.Identity()) != 64 {
		t.Fatalf("plan = %+v, identity %q; want [m] sealed", p, p.Identity())
	}
	if p.Identity() != sealPlan(p.state.secret, cpKey, &p.state.inputs) {
		t.Fatal("identity is not the seal of the frozen inputs and the key")
	}
}

func TestPlanCheck_NilStoreUnauthoredModelNeedsListing(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	p, err := PlanCheck(t.Context(), "lc", []string{"m", "x"}, cpTools, CheckOptions{Config: cfg})
	cpWantCode(t, p, err, CodeListingRequired)
}

func TestPlanCheck_StoreReadFailureIsNotEmptySuccess(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	rec := &cpReader{err: errors.New("fingerprint: read snapshot: disk I/O error MARKER-5f1")}
	p, err := PlanCheck(t.Context(), "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: rec})
	if err == nil || !errors.Is(err, provider.ErrEvidenceUnreadable) || strings.Contains(err.Error(), "MARKER") {
		t.Fatalf("error = %v; want ErrEvidenceUnreadable without the store's text", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("error carries code %q; want no bounded code", code)
	}
	if !reflect.DeepEqual(p, CheckPlan{}) {
		t.Fatalf("plan = %+v; want zero", p)
	}
}

func TestPlanCheck_OneSnapshotForAllInputs(t *testing.T) {
	cfg := cpConfig(nil)
	ol := cpBinding(t, cfg, "ol")
	rec := &cpReader{snap: fingerprint.EvidenceSnapshot{
		Listing: &fingerprint.ListingSnapshot{Endpoint: ol.Endpoint, Members: []fingerprint.ListingMember{{Model: "a"}, {Model: "b:7b"}, {Model: "c:latest"}}},
	}}
	p, err := PlanCheck(t.Context(), "ol", []string{"c:latest", "b:7b", "a", "a:latest"}, cpTools, CheckOptions{Config: cfg, Store: rec})
	if err != nil {
		t.Fatal(err)
	}
	reads, other := rec.calls()
	want := []cpRead{{endpoint: ol.Endpoint, adapter: fingerprint.AdapterOllama, apiKey: cpKey, legacy: "", models: []string{"a", "a:latest", "b:7b", "c", "c:latest"}}}
	if !reflect.DeepEqual(reads, want) || len(other) != 0 {
		t.Fatalf("store calls = %+v %v; want %+v", reads, other, want)
	}
	if !slices.Equal(cpPlanned(p), []string{"a", "a:latest", "b:7b", "c:latest"}) {
		t.Fatalf("planned %q", cpPlanned(p))
	}
}

func TestPlanCheck_RecipeMetadataAndIdentityStayAligned(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m", "n"))
	for k, v := range cpAuthored("ol", "m") {
		cfg.Models["x"+k] = v
	}
	for _, tc := range []struct {
		provider string
		caps     provider.Capability
	}{{"lc", cpTools}, {"lc", provider.CapChat}, {"ol", cpTools}} {
		p := cpPlan(t, cfg, nil, tc.provider, []string{"m"}, tc.caps, false)
		st := p.state
		if len(p.Items) != 1 || len(st.inputs.items) != 1 {
			t.Fatalf("%s: items %+v; want one", tc.provider, p.Items)
		}
		it, frozen := p.Items[0], st.inputs.items[0]
		recipe, ok := fingerprint.RecipeFor(it.Methods[0])
		if !ok || !reflect.DeepEqual(frozen.recipes, []fingerprint.ProbeRecipe{recipe}) {
			t.Fatalf("%s: frozen recipes %+v; want [%+v]", tc.provider, frozen.recipes, recipe)
		}
		if it.UsualRequests != recipe.UsualRequests || it.MaxRequests != recipe.MaxRequests ||
			it.MaxOutputTokensPerRequest != recipe.MaxOutputTokensPerRequest || it.RequestTimeout != recipe.RequestTimeout ||
			it.MaxResponseBytes != recipe.MaxResponseBytes {
			t.Fatalf("%s: item %+v disagrees with recipe %+v", tc.provider, it, recipe)
		}
		if !reflect.DeepEqual(it, frozen.item) || !slices.Equal(it.Capabilities, tc.caps.Names()) {
			t.Fatalf("%s: shown item %+v, frozen %+v; want equal, for %q", tc.provider, it, frozen.item, tc.caps.Names())
		}
		// The shown plan is a copy: changing it changes nothing frozen.
		p.Items[0].MaxRequests, p.Items[0].Capabilities[0] = 99, "embed"
		if st.inputs.items[0].item.MaxRequests == 99 || st.inputs.items[0].item.Capabilities[0] == "embed" {
			t.Fatalf("%s: the shown items alias the frozen ones", tc.provider)
		}
		if p.Identity() != sealPlan(st.secret, cpKey, &st.inputs) {
			t.Fatalf("%s: identity is not the seal of the frozen inputs", tc.provider)
		}
	}

	p := cpPlan(t, cfg, nil, "lc", []string{"m", "n"}, cpTools, false)
	base := p.state.inputs
	// One case per field of every sealed type, named type.Field.
	mutations := map[string]func(in *planInputs){
		"inputs.target":   func(in *planInputs) { in.target = checkTarget{} },
		"inputs.models":   func(in *planInputs) { in.models = in.models[:1] },
		"inputs.caps":     func(in *planInputs) { in.caps &^= provider.CapStream },
		"inputs.policies": func(in *planInputs) { in.policies = in.policies[:1] },
		"inputs.items":    func(in *planInputs) { in.items = in.items[:1] },
		"inputs.force":    func(in *planInputs) { in.force = !in.force },

		"target.provider": func(in *planInputs) { in.target.provider = "lc2" },
		"target.binding":  func(in *planInputs) { in.target.binding = provider.EvidenceBinding{} },
		"target.local":    func(in *planInputs) { in.target.local = !in.target.local },
		"target.timeout":  func(in *planInputs) { in.target.timeout++ },

		"binding.Endpoint": func(in *planInputs) { in.target.binding.Endpoint = "http://127.0.0.1:8081" },
		"binding.Adapter":  func(in *planInputs) { in.target.binding.Adapter = fingerprint.AdapterOllama },

		"policy.Authored":  func(in *planInputs) { in.policies[0].Authored = false },
		"policy.Explicit":  func(in *planInputs) { in.policies[0].Explicit = []string{"chat"} },
		"policy.Derived":   func(in *planInputs) { in.policies[0].Derived = nil },
		"policy.TypeNames": func(in *planInputs) { in.policies[0].TypeNames = []string{"moe"} },
		"policy.TypeCaps":  func(in *planInputs) { in.policies[0].TypeCaps = 0 },

		"planItem.item":    func(in *planInputs) { in.items[0].item = CheckItem{} },
		"planItem.recipes": func(in *planInputs) { in.items[0].recipes = nil },

		"item.Model":                     func(in *planInputs) { in.items[0].item.Model = "n" },
		"item.Capabilities":              func(in *planInputs) { in.items[0].item.Capabilities = in.items[0].item.Capabilities[:2] },
		"item.Methods":                   func(in *planInputs) { in.items[0].item.Methods[0] = fingerprint.MethodChatStream },
		"item.UsualRequests":             func(in *planInputs) { in.items[0].item.UsualRequests++ },
		"item.MaxRequests":               func(in *planInputs) { in.items[0].item.MaxRequests++ },
		"item.MaxOutputTokensPerRequest": func(in *planInputs) { in.items[0].item.MaxOutputTokensPerRequest++ },
		"item.ModelLoads":                func(in *planInputs) { in.items[0].item.ModelLoads++ },
		"item.RequestTimeout":            func(in *planInputs) { in.items[0].item.RequestTimeout++ },
		"item.MaxResponseBytes":          func(in *planInputs) { in.items[0].item.MaxResponseBytes++ },

		"recipe.Method":                    func(in *planInputs) { in.items[0].recipes[0].Method = fingerprint.MethodChatStream },
		"recipe.Adapter":                   func(in *planInputs) { in.items[0].recipes[0].Adapter = fingerprint.AdapterOllama },
		"recipe.Version":                   func(in *planInputs) { in.items[0].recipes[0].Version++ },
		"recipe.Capabilities":              func(in *planInputs) { in.items[0].recipes[0].Capabilities = in.items[0].recipes[0].Capabilities[:1] },
		"recipe.UsualRequests":             func(in *planInputs) { in.items[0].recipes[0].UsualRequests++ },
		"recipe.MaxRequests":               func(in *planInputs) { in.items[0].recipes[0].MaxRequests++ },
		"recipe.MaxOutputTokensPerRequest": func(in *planInputs) { in.items[0].recipes[0].MaxOutputTokensPerRequest++ },
		"recipe.RequestTimeout":            func(in *planInputs) { in.items[0].recipes[0].RequestTimeout++ },
		"recipe.MaxResponseBytes":          func(in *planInputs) { in.items[0].recipes[0].MaxResponseBytes++ },
		"recipe.Message":                   func(in *planInputs) { in.items[0].recipes[0].Message += "!" },
		"recipe.ToolName":                  func(in *planInputs) { in.items[0].recipes[0].ToolName += "!" },
		"recipe.ToolDescription":           func(in *planInputs) { in.items[0].recipes[0].ToolDescription += "!" },
		"recipe.ToolParameters":            func(in *planInputs) { in.items[0].recipes[0].ToolParameters = `{}` },
		"recipe.RequiredToolChoiceFirst":   func(in *planInputs) { in.items[0].recipes[0].RequiredToolChoiceFirst = false },

		// A string moved across a field boundary must not seal the same.
		"boundary shift": func(in *planInputs) {
			in.target.provider += in.target.binding.Endpoint[:1]
			in.target.binding.Endpoint = in.target.binding.Endpoint[1:]
		},
	}

	// The table covers every field of every sealed type, and names no field
	// that is gone. A field added to one of these types fails here until
	// sealPlan (configio/check_plan.go) writes it and this table changes it.
	fields := map[string]bool{"boundary shift": true}
	for _, sealed := range []struct {
		prefix string
		typ    reflect.Type
	}{
		{"inputs", reflect.TypeFor[planInputs]()},
		{"target", reflect.TypeFor[checkTarget]()},
		{"binding", reflect.TypeFor[provider.EvidenceBinding]()},
		{"policy", reflect.TypeFor[config.ModelCapabilityPolicy]()},
		{"planItem", reflect.TypeFor[planItem]()},
		{"item", reflect.TypeFor[CheckItem]()},
		{"recipe", reflect.TypeFor[fingerprint.ProbeRecipe]()},
	} {
		for i := range sealed.typ.NumField() {
			name := sealed.prefix + "." + sealed.typ.Field(i).Name
			fields[name] = true
			if mutations[name] == nil {
				t.Errorf("%s (%d fields in %s) has no case: write it in sealPlan, then add a case to this table", name, sealed.typ.NumField(), sealed.typ)
			}
		}
	}
	for name := range mutations {
		if !fields[name] {
			t.Errorf("case %q names no field of a sealed type; rename or remove it", name)
		}
	}

	secret := [32]byte{9}
	want := sealPlan(secret, cpKey, &base)
	for name, change := range mutations {
		in := cpCloneInputs(base)
		change(&in)
		if reflect.DeepEqual(in, base) {
			t.Fatalf("%s: the mutation changed nothing", name)
		}
		if sealPlan(secret, cpKey, &in) == want {
			t.Errorf("%s is not sealed", name)
		}
		if sealPlan(secret, cpKey, &base) != want {
			t.Fatalf("%s: the mutation reached the base inputs", name)
		}
	}
}

func TestPlanCheck_SealEncodingPinned(t *testing.T) {
	// A fixed input seals to a fixed value, so dropping or reordering any write
	// in sealPlan fails here, the version and the list counts included, which
	// no field change can show. A deliberate change to the encoding changes
	// planSealVersion and this value.
	in := planInputs{
		target: checkTarget{
			provider: "lc", binding: provider.EvidenceBinding{Endpoint: "http://127.0.0.1:8080", Adapter: "openai-compat"},
			local: true, timeout: 5 * time.Minute,
		},
		models: []string{"m", "n"},
		caps:   cpTools,
		policies: []config.ModelCapabilityPolicy{
			{Authored: true, Derived: []string{"chat", "stream"}, TypeNames: []string{"dense"}, TypeCaps: provider.CapChat | provider.CapStream},
			{Authored: true, Explicit: []string{"chat"}},
		},
		items: []planItem{{
			item: CheckItem{
				Model: "m", Capabilities: []string{"chat", "stream", "tool_call"}, Methods: []string{"chat-stream-tools"},
				UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, ModelLoads: 1,
				RequestTimeout: 3 * time.Minute, MaxResponseBytes: 1 << 20,
			},
			recipes: []fingerprint.ProbeRecipe{{
				Method: "chat-stream-tools", Adapter: "openai-compat", Version: 1, Capabilities: []string{"chat", "stream", "tool_call"},
				UsualRequests: 1, MaxRequests: 2, MaxOutputTokensPerRequest: 2048, RequestTimeout: 3 * time.Minute, MaxResponseBytes: 1 << 20,
				Message: "Call get_time.", ToolName: "get_time", ToolDescription: "Get the current time.",
				ToolParameters: `{"type":"object","properties":{}}`, RequiredToolChoiceFirst: true,
			}},
		}},
		force: true,
	}
	const want = "1b05358a9e85bcdadc0ee0f04a6b836102c2c70e7e512f9b4a448725114bba46"
	if got := sealPlan([32]byte{7}, "sk-golden-key", &in); got != want {
		t.Fatalf("sealPlan = %s; want %s", got, want)
	}
}

func TestPlanCheck_FreshMatchesRowFresh(t *testing.T) {
	// One stored row for chat, the only capability asked for: the model is
	// skipped exactly when that row wins and is fresh.
	now := time.Now()
	cases := []struct {
		name    string
		source  fingerprint.EvidenceSource
		state   fingerprint.CapProbeState
		age     time.Duration
		edit    func(r *fingerprint.EvidenceRow)
		planned bool
	}{
		{name: "probe yes", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeYes, age: time.Hour},
		{name: "observed yes", source: fingerprint.EvidenceSourceObserved, state: fingerprint.CapProbeYes, age: time.Hour},
		{name: "provider yes", source: fingerprint.EvidenceSourceProvider, state: fingerprint.CapProbeYes, age: time.Hour},
		{name: "probe no", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeNo, age: time.Hour},
		{name: "observed no", source: fingerprint.EvidenceSourceObserved, state: fingerprint.CapProbeNo, age: time.Hour},
		{name: "provider no", source: fingerprint.EvidenceSourceProvider, state: fingerprint.CapProbeNo, age: time.Hour},
		{name: "stale probe yes", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeYes, age: 31 * 24 * time.Hour, planned: true},
		{name: "stale observed yes", source: fingerprint.EvidenceSourceObserved, state: fingerprint.CapProbeYes, age: 31 * 24 * time.Hour, planned: true},
		{name: "stale provider yes", source: fingerprint.EvidenceSourceProvider, state: fingerprint.CapProbeYes, age: 31 * 24 * time.Hour, planned: true},
		{name: "expired probe no", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeNo, age: 8 * 24 * time.Hour, planned: true},
		{name: "expired provider no", source: fingerprint.EvidenceSourceProvider, state: fingerprint.CapProbeNo, age: 8 * 24 * time.Hour, planned: true},
		{name: "probe no past its fresh_until", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeNo, age: time.Hour,
			edit: func(r *fingerprint.EvidenceRow) { r.FreshUntil = now.Add(-30 * time.Minute) }, planned: true},
		{name: "observed no past its fresh_until", source: fingerprint.EvidenceSourceObserved, state: fingerprint.CapProbeNo, age: time.Hour,
			edit: func(r *fingerprint.EvidenceRow) { r.FreshUntil = now.Add(-30 * time.Minute) }, planned: true},
		{name: "probe yes fresh until later", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeYes, age: time.Hour,
			edit: func(r *fingerprint.EvidenceRow) { r.FreshUntil = now.Add(time.Hour) }},
		{name: "probe inconclusive", source: fingerprint.EvidenceSourceProbe, state: fingerprint.CapProbeInconclusive, age: time.Hour, planned: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cpConfig(nil)
			store := newStore(t)
			lc := cpBinding(t, cfg, "lc")
			cpList(t, store, lc, cpKey, "m")
			row := cpRow(lc, "m", "", "chat", tc.state, now.Add(-tc.age))
			row.Source = tc.source
			if tc.edit != nil {
				tc.edit(&row)
			}
			if row.Fresh(now) == tc.planned {
				t.Fatalf("fixture: row.Fresh = %v; the table says planned=%v", row.Fresh(now), tc.planned)
			}
			cpAppend(t, store, row)
			if p := cpPlan(t, cfg, store, "lc", []string{"m"}, provider.CapChat, false); (len(p.Items) == 1) != tc.planned {
				t.Fatalf("planned %q; want planned=%v", cpPlanned(p), tc.planned)
			}
		})
	}
}

func TestPlanCheck_ReadsAtRuntimeDigest(t *testing.T) {
	cfg := cpConfig(nil)
	store := newStore(t)
	ol := cpBinding(t, cfg, "ol")
	if err := store.PublishListings(t.Context(), []fingerprint.ListingPublish{{
		Endpoint: ol.Endpoint, Adapter: ol.Adapter, APIKey: cpKey, StartedAt: time.Now().Add(-time.Minute),
		Members: []fingerprint.ListingMember{{Model: "q", Digest: "sha256:d1"}},
	}}); err != nil {
		t.Fatal(err)
	}
	cpAppend(t, store, cpRow(ol, "q", "sha256:d2", "chat", fingerprint.CapProbeYes, time.Now().Add(-time.Hour)))
	plan := func(digests map[string]string) CheckPlan {
		t.Helper()
		p, err := PlanCheck(t.Context(), "ol", []string{"q"}, provider.CapChat, CheckOptions{Config: cfg, Store: store, RuntimeDigests: digests})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	runtime := map[string]string{"q": "sha256:d2", "unrequested": "sha256:x"}
	p := plan(runtime)
	if len(p.Items) != 0 {
		t.Fatalf("at the runtime digest: planned %q; want none", cpPlanned(p))
	}
	if !reflect.DeepEqual(p.state.runtimeDigests, map[string]string{"q": "sha256:d2"}) {
		t.Fatalf("frozen digests %v; want only the requested model's", p.state.runtimeDigests)
	}
	runtime["q"] = "sha256:changed"
	if p.state.runtimeDigests["q"] != "sha256:d2" {
		t.Fatal("the plan shares the caller's digest map")
	}
	if p := plan(nil); !slices.Equal(cpPlanned(p), []string{"q"}) {
		t.Fatalf("at the listing digest: planned %q; want [q]", cpPlanned(p))
	}
	// The digest is the requested model's own entry, not its other spelling's.
	if p := plan(map[string]string{"q:latest": "sha256:d2"}); !slices.Equal(cpPlanned(p), []string{"q"}) {
		t.Fatalf("digest under the other spelling: planned %q; want [q]", cpPlanned(p))
	}
}

func TestPlanCheck_OtherAdapterAnswerNotFresh(t *testing.T) {
	// One server answers both APIs; an answer through one never counts for a
	// check through the other.
	cfg := cpConfig(nil)
	cfg.Providers["oc"] = config.ProviderConfig{BaseURL: cpOllama, APIFormat: "openai-compat", APIKey: cpKey}
	store := newStore(t)
	ol, oc := cpBinding(t, cfg, "ol"), cpBinding(t, cfg, "oc")
	if ol.Endpoint != oc.Endpoint {
		t.Fatalf("fixture: endpoints %q and %q differ", ol.Endpoint, oc.Endpoint)
	}
	cpList(t, store, oc, cpKey, "q")
	cpAppend(t, store, cpRow(ol, "q", "", "chat", fingerprint.CapProbeYes, time.Now().Add(-time.Hour)))
	if p := cpPlan(t, cfg, store, "oc", []string{"q"}, provider.CapChat, false); !slices.Equal(cpPlanned(p), []string{"q"}) {
		t.Fatalf("planned %q; want [q]", cpPlanned(p))
	}
	cpAppend(t, store, cpRow(oc, "q", "", "chat", fingerprint.CapProbeYes, time.Now().Add(-time.Hour)))
	if p := cpPlan(t, cfg, store, "oc", []string{"q"}, provider.CapChat, false); len(p.Items) != 0 {
		t.Fatalf("control: planned %q; want none", cpPlanned(p))
	}
}

func TestPlanCheck_CancelledReturnsContextError(t *testing.T) {
	cfg := cpConfig(cpAuthored("lc", "m"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Validation comes first.
	p, err := PlanCheck(ctx, "lc", nil, cpTools, CheckOptions{Config: cfg})
	cpWantCode(t, p, err, CodeInvalidArgument)

	rec := &cpReader{}
	p, err = PlanCheck(ctx, "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: rec})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(p, CheckPlan{}) {
		t.Fatalf("PlanCheck = %+v, %v; want zero, context.Canceled", p, err)
	}
	if _, ok := CodeOf(err); ok {
		t.Fatalf("cancellation carries a code: %v", err)
	}
	if reads, _ := rec.calls(); len(reads) != 0 {
		t.Fatal("a cancelled plan read the store")
	}

	// Cancelled while the store answers: the read succeeds, the plan does not.
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	rec = &cpReader{onRead: cancel}
	p, err = PlanCheck(ctx, "lc", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: rec})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(p, CheckPlan{}) {
		t.Fatalf("cancelled during the read: PlanCheck = %+v, %v; want zero, context.Canceled", p, err)
	}
	if reads, _ := rec.calls(); len(reads) != 1 {
		t.Fatalf("reads = %d; want 1", len(reads))
	}
}

// TestPlanCheck_KnownCapsAreTheCanonicalNames pins that the capability set
// PlanCheck accepts, provider.CanonicalCaps, is exactly the bits of
// provider.CanonicalCapabilityNames, so a new canonical name needs no second
// list here.
func TestPlanCheck_KnownCapsAreTheCanonicalNames(t *testing.T) {
	named, err := provider.ParseCapsStrict(provider.CanonicalCapabilityNames)
	if err != nil {
		t.Fatal(err)
	}
	if named != provider.CanonicalCaps() {
		t.Fatalf("canonical names parse to %v; CanonicalCaps is %v", named, provider.CanonicalCaps())
	}
}
