package configio

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func openStoreAt(t *testing.T, path string) *fingerprint.SQLiteStore {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
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

func newStore(t *testing.T) *fingerprint.SQLiteStore {
	return openStoreAt(t, filepath.Join(t.TempDir(), "ev.db"))
}

// cfgFor builds a config of providers: name -> base URL, api format, key.
func cfgFor(entries map[string][3]string) *config.Config {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	for name, e := range entries {
		cfg.Providers[name] = config.ProviderConfig{BaseURL: e[0], APIFormat: e[1], APIKey: e[2]}
	}
	return cfg
}

func canonical(t *testing.T, base, format string) string {
	t.Helper()
	b, err := provider.EvidenceBindingFor(base, format)
	if err != nil {
		t.Fatal(err)
	}
	return b.Endpoint
}

func listedNames(t *testing.T, s *fingerprint.SQLiteStore, endpoint, adapter, key string) []string {
	t.Helper()
	l, err := s.CurrentListing(t.Context(), endpoint, adapter, key)
	if err != nil {
		t.Fatal(err)
	}
	if l == nil {
		return nil
	}
	var out []string
	for _, m := range l.Members {
		out = append(out, m.Model)
	}
	return out
}

// stubModel is what a real Ollama serves for a model: its digest comes from
// /api/tags (/api/show does not carry one), the rest from /api/show.
type stubModel struct{ digest, show string }

// ollamaStub serves /api/tags and /api/show for the given models.
func ollamaStub(t *testing.T, models map[string]stubModel) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			var parts []string
			for name, m := range models {
				parts = append(parts, fmt.Sprintf(`{"name":%q,"digest":%q}`, name, m.digest))
			}
			_, _ = fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(parts, ","))
		case "/api/show":
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = fmt.Fprint(w, models[body.Name].show)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ollamaLister(srv *httptest.Server) *fakeLister {
	p := provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
	return &fakeLister{names: []string{"local"}, providers: map[string]provider.Provider{"local": p}}
}

func providerReading(t *testing.T, s *fingerprint.SQLiteStore, endpoint, model, digest, capability string) *fingerprint.EvidenceRow {
	t.Helper()
	rows, err := s.EvidenceRows(t.Context(), endpoint, model)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint.ReadCapabilities(rows, fingerprint.AdapterOllama, digest, time.Now())[capability].Provider
}

func TestRefreshPublish_OllamaInsertSurvivesOffline(t *testing.T) {
	srv := ollamaStub(t, map[string]stubModel{
		"fim": {"sha256:abc", `{"template":"{{ .Prompt }}{{ .Suffix }}","capabilities":["completion","insert"]}`},
	})
	path := filepath.Join(t.TempDir(), "ev.db")
	s := openStoreAt(t, path)
	endpoint := canonical(t, srv.URL, "")
	if _, err := RefreshInventory(t.Context(), ollamaLister(srv), nil, WithPublish(s, cfgFor(map[string][3]string{"local": {srv.URL, "", ""}}))); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	s2 := openStoreAt(t, path) // reopened, the stub is closed
	l, err := s2.CurrentListing(t.Context(), endpoint, fingerprint.AdapterOllama, "")
	if err != nil || l == nil || len(l.Members) != 1 || l.Members[0].Digest != "sha256:abc" {
		t.Fatalf("listing = %+v, %v, want the member carrying the /api/tags digest", l, err)
	}
	row := providerReading(t, s2, endpoint, "fim", "sha256:abc", "insert")
	if row == nil || row.State != fingerprint.CapProbeYes || row.Method != "ollama-show" || row.MethodVersion != 1 || row.Digest != "sha256:abc" {
		t.Fatalf("insert reading = %+v, want a provider yes carrying the digest", row)
	}
	if tc := providerReading(t, s2, endpoint, "fim", "sha256:abc", "tool_call"); tc == nil || tc.State != fingerprint.CapProbeNo {
		t.Fatalf("tool_call = %+v, want no (array without tools)", tc)
	}
}

func TestRefreshPublish_UnfamiliarEmbeddingSurvivesOffline(t *testing.T) {
	srv := ollamaStub(t, map[string]stubModel{"zz-unknown:1b": {"d1", `{"capabilities":["embedding"]}`}})
	path := filepath.Join(t.TempDir(), "ev.db")
	s := openStoreAt(t, path)
	endpoint := canonical(t, srv.URL, "")
	if _, err := RefreshInventory(t.Context(), ollamaLister(srv), nil, WithPublish(s, cfgFor(map[string][3]string{"local": {srv.URL, "", ""}}))); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	row := providerReading(t, openStoreAt(t, path), endpoint, "zz-unknown:1b", "d1", "embed")
	if row == nil || row.State != fingerprint.CapProbeYes {
		t.Fatalf("embed = %+v, want yes", row)
	}
}

func TestRefreshPublish_NilProjectorAllowedWithPublish(t *testing.T) {
	srv := ollamaStub(t, map[string]stubModel{"m": {"d", `{"capabilities":["completion","tools"]}`}})
	s := newStore(t)
	inv, err := RefreshInventory(t.Context(), ollamaLister(srv), nil, WithPublish(s, cfgFor(map[string][3]string{"local": {srv.URL, "", ""}})))
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Models) != 1 || inv.Models[0].Key != (provider.ModelKey{Provider: "local", Model: "m"}) {
		t.Fatalf("Models = %+v, want the one listed identity", inv.Models)
	}
	if want := provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall; inv.Models[0].Caps != want || inv.Models[0].KnownMask != want {
		t.Fatalf("caps %v known %v, want %v", inv.Models[0].Caps, inv.Models[0].KnownMask, want)
	}
	if got := listedNames(t, s, canonical(t, srv.URL, ""), adapterOf(""), ""); len(got) != 1 || got[0] != "m" {
		t.Fatalf("published members = %v", got)
	}
}

func TestRefreshPublish_CancelBeforeCommitPublishesNothing(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m"}}}
	b := &fakeProvider{name: "b", models: []provider.ModelInfo{{Name: "n"}}, onModels: cancel}
	lister := &fakeLister{names: []string{"a", "b"}, providers: map[string]provider.Provider{"a": a, "b": b}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", ""}, "b": {"http://b.test:1", "openai-compat", ""}})
	if _, err := RefreshInventory(ctx, lister, nil, WithPublish(s, cfg)); err == nil {
		t.Fatal("want the context error")
	}
	if g, err := s.Generation(t.Context()); err != nil || g != 0 {
		t.Fatalf("generation = %d, %v, want 0", g, err)
	}
	if got := listedNames(t, s, canonical(t, "http://a.test:1", "openai-compat"), adapterOf("openai-compat"), ""); got != nil {
		t.Fatalf("published %v after cancellation", got)
	}
}

func TestRefreshPublish_FailedListingKeepsPreviousSnapshot(t *testing.T) {
	s := newStore(t)
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m"}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", ""}})
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
		t.Fatal(err)
	}
	p.modelsErr = &statusErr{429}
	inv, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := inv.Providers[0]; got.Reachable || got.Reason != configview.ListingRateLimited {
		t.Fatalf("provider = %+v, want unreachable with rate_limited", got)
	}
	if got := listedNames(t, s, canonical(t, "http://a.test:1", "openai-compat"), adapterOf("openai-compat"), ""); len(got) != 1 || got[0] != "m" {
		t.Fatalf("snapshot = %v, want the previous one kept", got)
	}
}

func TestRefreshPublish_NamedModelSurvives(t *testing.T) {
	s := newStore(t)
	endpoint := canonical(t, "http://a.test:1", "openai-compat")
	if err := s.AddNamedModel(t.Context(), endpoint, "mine"); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m"}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", ""}})
	for i := 0; i < 2; i++ { // a good refresh, then a failed one
		if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
			t.Fatal(err)
		}
		p.modelsErr = &statusErr{404}
	}
	named, err := s.NamedModels(t.Context(), endpoint)
	if err != nil || len(named) != 1 || named[0].Model != "mine" {
		t.Fatalf("named = %+v, %v", named, err)
	}
}

func TestRefreshPublish_DirtyProviderDoesNotStopCleanOne(t *testing.T) {
	s := newStore(t)
	clean := &fakeProvider{name: "clean", models: []provider.ModelInfo{{Name: "ok"}}}
	dirty := &fakeProvider{name: "dirty", models: []provider.ModelInfo{{Name: "x"}}}
	nocfg := &fakeProvider{name: "nocfg", models: []provider.ModelInfo{{Name: "y"}}}
	badurl := &fakeProvider{name: "badurl", models: []provider.ModelInfo{{Name: "z"}}}
	lister := &fakeLister{
		names: []string{"badurl", "clean", "dirty", "nocfg"},
		providers: map[string]provider.Provider{
			"clean": clean, "dirty": dirty, "nocfg": nocfg, "badurl": badurl,
		},
	}
	cfg := cfgFor(map[string][3]string{
		"clean":  {"http://clean.test:1", "openai-compat", "sk-real"},
		"dirty":  {"http://dirty.test:1", "openai-compat", "${DIRTY_KEY}"},
		"badurl": {"not a url", "openai-compat", ""},
	})
	inv, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range inv.Providers {
		if !p.Reachable || p.Reason != "" {
			t.Errorf("provider %+v, want reachable with no reason", p)
		}
	}
	if len(inv.Models) != 4 {
		t.Errorf("listed %d models, want 4", len(inv.Models))
	}
	if got := listedNames(t, s, canonical(t, "http://clean.test:1", "openai-compat"), adapterOf("openai-compat"), "sk-real"); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("clean listing = %v", got)
	}
	if got := listedNames(t, s, canonical(t, "http://dirty.test:1", "openai-compat"), adapterOf("openai-compat"), ""); got != nil {
		t.Fatalf("dirty provider published %v", got)
	}
}

func TestRefreshPublish_5000DirtyMembersWithinBusyTimeout(t *testing.T) {
	s := newStore(t)
	var infos []provider.ModelInfo
	for i := 0; i < 3000; i++ {
		infos = append(infos, provider.ModelInfo{Name: fmt.Sprintf("m-%04d", i%2000)}) // 1000 duplicates
	}
	for i := 0; i < 1000; i++ {
		infos = append(infos, provider.ModelInfo{Name: ""}, provider.ModelInfo{Name: fmt.Sprintf("nul\x00-%d", i)})
	}
	p := &fakeProvider{name: "big", models: infos}
	lister := &fakeLister{names: []string{"big"}, providers: map[string]provider.Provider{"big": p}}
	cfg := cfgFor(map[string][3]string{"big": {"http://big.test:1", "openai-compat", ""}})
	start := time.Now()
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
	got := listedNames(t, s, canonical(t, "http://big.test:1", "openai-compat"), adapterOf("openai-compat"), "")
	if len(got) != 2000 {
		t.Fatalf("published %d members, want the 2000 distinct clean names", len(got))
	}
	for _, m := range got {
		if m == "" || strings.ContainsRune(m, 0) {
			t.Fatalf("unaddressable member %q", m)
		}
	}
}

// statusErr is a listing failure carrying only an HTTP status.
type statusErr struct{ code int }

func (e *statusErr) Error() string       { return "stub" }
func (e *statusErr) HTTPStatusCode() int { return e.code }

func httpProviders(t *testing.T, handler http.HandlerFunc, timeout time.Duration) map[string]*fakeLister {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return map[string]*fakeLister{
		"openai-compat": {names: []string{"p"}, providers: map[string]provider.Provider{"p": openaicompat.NewProvider(
			openaicompat.NewClient(srv.URL, openaicompat.WithHTTPClient(&http.Client{Timeout: timeout})))}},
		"ollama": {names: []string{"p"}, providers: map[string]provider.Provider{"p": provider.NewOllamaProvider(
			ollama.NewClient(ollama.WithBaseURL(srv.URL), ollama.WithTimeout(timeout)))}},
	}
}

func status(code int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); _, _ = fmt.Fprint(w, body) }
}

func TestListingFailureReason(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}
	hangup := func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
		timeout time.Duration // 0 = generous; only the slow case needs it short
	}{
		{"404", status(404, ""), "listing_unsupported", 0},
		{"405", status(405, ""), "listing_unsupported", 0},
		{"401", status(401, ""), "auth_failed", 0},
		{"403", status(403, ""), "auth_failed", 0},
		{"429", status(429, ""), "rate_limited", 0},
		{"400", status(400, ""), "rejected", 0},
		{"500", status(500, ""), "server_error", 0},
		{"slow", slow, "timeout", 150 * time.Millisecond},
		{"hangs up", hangup, "unreachable", 0},
		{"bad json", status(200, "{not json"), "malformed_response", 0},
		{"302", status(302, ""), "malformed_response", 0},
	}
	for _, c := range cases {
		for format, lister := range httpProviders(t, c.handler, cmp.Or(c.timeout, 10*time.Second)) {
			t.Run(c.name+"/"+format, func(t *testing.T) {
				inv, err := RefreshInventory(t.Context(), lister, newFakeProjector())
				if err != nil {
					t.Fatal(err)
				}
				if got := inv.Providers[0]; got.Reachable || got.Reason != c.want {
					t.Fatalf("provider = %+v, want unreachable with %q", got, c.want)
				}
			})
		}
	}

	closed := httptest.NewServer(status(200, ""))
	closedURL := closed.URL
	closed.Close()
	for format, lister := range map[string]*fakeLister{
		"openai-compat": {names: []string{"p"}, providers: map[string]provider.Provider{"p": openaicompat.NewProvider(openaicompat.NewClient(closedURL))}},
		"ollama":        {names: []string{"p"}, providers: map[string]provider.Provider{"p": provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(closedURL)))}},
	} {
		t.Run("closed port/"+format, func(t *testing.T) {
			inv, err := RefreshInventory(t.Context(), lister, newFakeProjector())
			if err != nil {
				t.Fatal(err)
			}
			if got := inv.Providers[0]; got.Reachable || got.Reason != "unreachable" {
				t.Fatalf("provider = %+v, want unreachable", got)
			}
		})
	}
}

func TestListingFailureReason_NoRawProviderText(t *testing.T) {
	for format, lister := range httpProviders(t, status(500, `{"error":{"message":"SECRET-MARKER"}}`), time.Second) {
		inv, err := RefreshInventory(t.Context(), lister, newFakeProjector())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "SECRET-MARKER") {
			t.Fatalf("%s: raw provider text reached the inventory: %s", format, raw)
		}
		if want := `"reason":"server_error"`; !strings.Contains(string(raw), want) {
			t.Fatalf("%s: %s lacks %s", format, raw, want)
		}
	}
}

// failingStore is an EvidenceWriter whose PublishListings fails, optionally
// cancelling the context first.
type failingStore struct {
	fingerprint.EvidenceWriter
	onPublish func()
}

func (f failingStore) PublishListings(context.Context, []fingerprint.ListingPublish) error {
	if f.onPublish != nil {
		f.onPublish()
	}
	return fmt.Errorf("disk full")
}

func TestRefreshPublish_StoreFailureKeepsInventoryAndReportsError(t *testing.T) {
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m", Family: "f"}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", "sk-secret-key"}})

	inv, err := RefreshInventory(t.Context(), lister, nil, WithPublish(failingStore{}, cfg))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the wrapped store failure", err)
	}
	if strings.Contains(err.Error(), "sk-secret-key") {
		t.Fatalf("error carries the API key: %v", err)
	}
	wantProviders := []configview.InventoryProvider{{Name: "a", Reachable: true}}
	if fmt.Sprint(inv.Providers) != fmt.Sprint(wantProviders) || len(inv.Models) != 1 ||
		inv.Models[0].Key != (provider.ModelKey{Provider: "a", Model: "m"}) || inv.Models[0].Family != "f" {
		t.Fatalf("inventory = %+v, want the fetched listing", inv)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	inv, err = RefreshInventory(ctx, lister, nil, WithPublish(failingStore{onPublish: cancel}, cfg))
	if err != context.Canceled || len(inv.Models) != 0 || len(inv.Providers) != 0 {
		t.Fatalf("cancelled during publish: inv=%+v err=%v, want zero Inventory and context.Canceled", inv, err)
	}
}

func TestRefreshPublish_StartedAtIsBeforeTheListingCall(t *testing.T) {
	s := newStore(t)
	var seen time.Time
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m"}}, onModels: func() {
		seen = time.Now()
		time.Sleep(25 * time.Millisecond)
	}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", ""}})
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
		t.Fatal(err)
	}
	l, err := s.CurrentListing(t.Context(), canonical(t, "http://a.test:1", "openai-compat"), adapterOf("openai-compat"), "")
	if err != nil || l == nil {
		t.Fatal(l, err)
	}
	if l.StartedAt.After(seen) {
		t.Fatalf("StartedAt %v is after the listing call began at %v", l.StartedAt, seen)
	}
}

func TestRefreshPublish_OllamaFactExpiry(t *testing.T) {
	s := newStore(t)
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m", Capabilities: []string{"completion"}}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "", ""}}) // empty format is ollama
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.EvidenceRows(t.Context(), canonical(t, "http://a.test:1", ""), "m")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Capability] = true
		switch r.State {
		case fingerprint.CapProbeYes:
			if !r.FreshUntil.Equal(r.ObservedAt.Add(30*24*time.Hour)) || !r.ExpiresAt.IsZero() {
				t.Errorf("%s yes: fresh_until %v expires %v, want observed+30d and none", r.Capability, r.FreshUntil, r.ExpiresAt)
			}
		case fingerprint.CapProbeNo:
			if !r.ExpiresAt.Equal(r.ObservedAt.Add(7*24*time.Hour)) || !r.FreshUntil.IsZero() {
				t.Errorf("%s no: fresh_until %v expires %v, want expires observed+7d", r.Capability, r.FreshUntil, r.ExpiresAt)
			}
		}
	}
	if !seen["chat"] || !seen["tool_call"] {
		t.Fatalf("rows for %v, want both a yes (chat) and a no (tool_call)", seen)
	}
}

func TestRefreshPublish_PublishesUnderTheCanonicalEndpoint(t *testing.T) {
	s := newStore(t)
	raw := "HTTP://Host.Test:1/"
	endpoint := canonical(t, raw, "openai-compat")
	if endpoint == raw || endpoint == "" {
		t.Fatalf("test needs a base URL that canonicalizes differently, got %q", endpoint)
	}
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m"}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfgFor(map[string][3]string{"a": {raw, "openai-compat", ""}}))); err != nil {
		t.Fatal(err)
	}
	if got := listedNames(t, s, endpoint, fingerprint.AdapterOpenAICompat, ""); len(got) != 1 {
		t.Fatalf("listing under the canonical key = %v", got)
	}
}

func TestRefreshPublish_OnlyOllamaListingsCarryFacts(t *testing.T) {
	s := newStore(t)
	p := &fakeProvider{name: "a", models: []provider.ModelInfo{{Name: "m", Capabilities: []string{"completion", "tools"}, Template: ".Suffix"}}}
	lister := &fakeLister{names: []string{"a"}, providers: map[string]provider.Provider{"a": p}}
	cfg := cfgFor(map[string][3]string{"a": {"http://a.test:1", "openai-compat", ""}})
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.EvidenceRows(t.Context(), canonical(t, "http://a.test:1", "openai-compat"), "m")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows = %+v, %v, want none for an openai-compat provider", rows, err)
	}
}

func TestRefreshPublish_OwnedByIsPublished(t *testing.T) {
	srv := httptest.NewServer(status(200, `{"data":[{"id":"m","owned_by":"acme"}]}`))
	defer srv.Close()
	s := newStore(t)
	lister := &fakeLister{names: []string{"p"}, providers: map[string]provider.Provider{"p": openaicompat.NewProvider(openaicompat.NewClient(srv.URL))}}
	if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfgFor(map[string][3]string{"p": {srv.URL, "openai-compat", ""}}))); err != nil {
		t.Fatal(err)
	}
	l, err := s.CurrentListing(t.Context(), canonical(t, srv.URL, "openai-compat"), adapterOf("openai-compat"), "")
	if err != nil || l == nil || len(l.Members) != 1 || l.Members[0].OwnedBy != "acme" {
		t.Fatalf("listing = %+v, %v, want owned_by acme", l, err)
	}
}

// A 200 whose body is not a listing must not wipe the previous snapshot; an
// explicit empty or null list is a real empty listing.
func TestRefreshPublish_NonListingBodyKeepsPreviousSnapshot(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		body := `{"data":[{"id":"m"}]}`
		if format == "ollama" {
			body = `{"models":[{"name":"m"}]}`
		}
		cur := &body
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/show" {
				_, _ = fmt.Fprint(w, `{}`)
				return
			}
			_, _ = fmt.Fprint(w, *cur)
		}))
		defer srv.Close()
		var p provider.Provider = openaicompat.NewProvider(openaicompat.NewClient(srv.URL))
		if format == "ollama" {
			p = provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
		}
		lister := &fakeLister{names: []string{"p"}, providers: map[string]provider.Provider{"p": p}}
		s := newStore(t)
		cfg := cfgFor(map[string][3]string{"p": {srv.URL, format, ""}})
		endpoint := canonical(t, srv.URL, format)
		if _, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg)); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`{}`, `{"error":{"message":"backend down"}}`} {
			*cur = bad
			inv, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg))
			if err != nil {
				t.Fatal(err)
			}
			if got := inv.Providers[0]; got.Reachable || got.Reason != "malformed_response" {
				t.Errorf("%s %s: provider = %+v", format, bad, got)
			}
			if got := listedNames(t, s, endpoint, adapterOf(format), ""); len(got) != 1 {
				t.Errorf("%s %s: snapshot = %v, want the previous one kept", format, bad, got)
			}
		}
		empty := `{"data":[]}`
		null := `{"data":null}`
		if format == "ollama" {
			empty, null = `{"models":[]}`, `{"models":null}`
		}
		for _, ok := range []string{empty, null} {
			*cur = ok
			inv, err := RefreshInventory(t.Context(), lister, nil, WithPublish(s, cfg))
			if err != nil || !inv.Providers[0].Reachable {
				t.Fatalf("%s %s: %+v, %v, want a reachable empty listing", format, ok, inv.Providers, err)
			}
			if l, _ := s.CurrentListing(t.Context(), endpoint, adapterOf(format), ""); l == nil || len(l.Members) != 0 {
				t.Errorf("%s %s: listing = %+v, want recorded empty", format, ok, l)
			}
		}
	}
}

type nilListing struct{ *fakeProvider }

func (nilListing) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }

func TestRefresh_NilListingWithoutPublishingIsUnchanged(t *testing.T) {
	lister := &fakeLister{names: []string{"p"}, providers: map[string]provider.Provider{"p": nilListing{&fakeProvider{name: "p"}}}}
	inv, err := RefreshInventory(t.Context(), lister, newFakeProjector())
	if err != nil || !inv.Providers[0].Reachable || inv.Providers[0].Reason != "" {
		t.Fatalf("%+v, %v, want reachable as before", inv.Providers, err)
	}
}

// adapterOf is the adapter a provider format publishes under.
func adapterOf(format string) string {
	if format == "" || format == "ollama" {
		return fingerprint.AdapterOllama
	}
	return fingerprint.AdapterOpenAICompat
}
