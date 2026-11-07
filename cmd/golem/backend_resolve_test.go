package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func TestValidateBaseURLOverride(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string // substring; "" => no error
	}{
		{"valid root", "http://127.0.0.1:8081", "http://127.0.0.1:8081", ""},
		{"valid with path prefix (reverse proxy)", "http://127.0.0.1:8081/llm", "http://127.0.0.1:8081/llm", ""},
		{"trims whitespace", "  http://127.0.0.1:8081  ", "http://127.0.0.1:8081", ""},
		{"empty", "", "", "value is empty"},
		{"whitespace only", "   ", "", "value is empty"},
		{"no scheme", "127.0.0.1:8081", "", "scheme and host"},
		{"garbage", "http://", "", "scheme and host"},
		{"v1-prefixed segment allowed", "http://127.0.0.1:8081/v1x", "http://127.0.0.1:8081/v1x", ""},
		{"query rejected", "http://127.0.0.1:8081?x=1", "", "query or fragment"},
		{"bare query rejected", "http://127.0.0.1:8081?", "", "query or fragment"},
		{"fragment rejected", "http://127.0.0.1:8081/llm#f", "", "query or fragment"},
		{"v1 suffix", "http://127.0.0.1:8081/v1", "", "without the /v1 suffix"},
		{"v1 suffix trailing slash", "http://127.0.0.1:8081/v1/", "", "without the /v1 suffix"},
		{"proxy path ending in v1", "http://127.0.0.1:8081/llm/v1", "", "without the /v1 suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBaseURLOverride(tt.raw, "-base-url")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateBaseURLOverride_RedactsUserinfoInError(t *testing.T) {
	_, err := validateBaseURLOverride("http://u:sekret@127.0.0.1:8081/v1", "-base-url")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "sekret") {
		t.Fatalf("error leaks userinfo: %v", err)
	}
}

func TestParseFlags_BaseURLAndNoProbe(t *testing.T) {
	f, err := parseFlags([]string{"-base-url", "http://127.0.0.1:8083", "-no-probe"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.baseURL != "http://127.0.0.1:8083" {
		t.Fatalf("baseURL = %q", f.baseURL)
	}
	if !f.baseURLSet {
		t.Fatal("baseURLSet = false, want true")
	}
	if !f.noProbe {
		t.Fatal("noProbe = false, want true")
	}
	// -base-url plus -no-probe is allowed (no-probe is then redundant).
	if err := validateFlags(f); err != nil {
		t.Fatalf("validateFlags: %v", err)
	}
	// Unset flag => baseURLSet false (distinguishes explicit empty).
	f2, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if f2.baseURLSet {
		t.Fatal("baseURLSet = true for absent flag, want false")
	}
}

// --- Task 4 tests: resolveBackend policy ---

func ocConfig(agentProvider, agentModel, baseURL string) *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			agentProvider: {APIFormat: "openai-compat", BaseURL: baseURL},
		},
		Models: map[string]config.ModelConfig{
			"agent-role": {Name: agentModel, Provider: agentProvider, Type: "dense"},
		},
		Defaults: map[string]string{"agent": "agent-role"},
	}
}

// fakeProber records calls and returns a scripted result.
type fakeProber struct {
	calls     int
	gotCands  []string
	gotModel  string
	gotOpts   int
	returnURL string
	returnErr error
}

func (fp *fakeProber) probe(_ context.Context, cands []string, model string, opts ...openaicompat.ClientOption) (string, error) {
	fp.calls++
	fp.gotCands = cands
	fp.gotModel = model
	fp.gotOpts = len(opts)
	return fp.returnURL, fp.returnErr
}

func noEnv(string) (string, bool) { return "", false }

func TestResolveBackend_ExplicitFlagSkipsDiscovery(t *testing.T) {
	fp := &fakeProber{}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		flagBaseURL: "http://127.0.0.1:8083", flagSet: true,
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if fp.calls != 0 {
		t.Fatalf("prober called %d times, want 0 (explicit override is absolute)", fp.calls)
	}
	if res.providerKey != "llamacpp" || res.baseURL != "http://127.0.0.1:8083" || res.source != "-base-url" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.notice, "discovery disabled") || !strings.Contains(res.notice, "-base-url") {
		t.Fatalf("notice = %q", res.notice)
	}
}

func TestResolveBackend_FlagBeatsEnv(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "GO_LLM_BASE_URL" {
			return "http://127.0.0.1:9999", true
		}
		return "", false
	}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		flagBaseURL: "http://127.0.0.1:8083", flagSet: true,
		lookupEnv: env, prober: (&fakeProber{}).probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if res.baseURL != "http://127.0.0.1:8083" || res.source != "-base-url" {
		t.Fatalf("res = %+v, want flag to beat env", res)
	}
}

func TestResolveBackend_EnvUsedWhenFlagAbsent(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "GO_LLM_BASE_URL" {
			return "http://127.0.0.1:9999", true
		}
		return "", false
	}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: env, prober: (&fakeProber{}).probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if res.baseURL != "http://127.0.0.1:9999" || res.source != "GO_LLM_BASE_URL" {
		t.Fatalf("res = %+v, want env override", res)
	}
}

func TestResolveBackend_SetButEmptyEnvErrors(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "GO_LLM_BASE_URL" {
			return "", true // set but empty
		}
		return "", false
	}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	if _, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: env, prober: (&fakeProber{}).probe,
	}); err == nil {
		t.Fatal("want validation error for set-but-empty env, got nil")
	}
}

func TestResolveBackend_NoProbeSkipsScan(t *testing.T) {
	fp := &fakeProber{}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		noProbe: true, lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if fp.calls != 0 {
		t.Fatalf("prober called with -no-probe, want 0 calls")
	}
	if res.providerKey != "" || res.baseURL != "" {
		t.Fatalf("res = %+v, want zero resolution (configured URL used as-is)", res)
	}
}

func TestResolveBackend_NonLoopbackConfiguredSkipsScan(t *testing.T) {
	fp := &fakeProber{}
	cfg := ocConfig("hosted", "gemma4:31b", "https://api.example.com")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if fp.calls != 0 {
		t.Fatal("prober called for non-loopback configured URL; scan must not arm")
	}
	if res.providerKey != "" {
		t.Fatalf("res = %+v, want zero resolution", res)
	}
}

func TestResolveBackend_ScanCandidatesShape(t *testing.T) {
	fp := &fakeProber{returnURL: "http://127.0.0.1:8083"}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if fp.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", fp.calls)
	}
	if fp.gotModel != "gemma4:31b" {
		t.Fatalf("prober model = %q", fp.gotModel)
	}
	// Configured URL first, then 8080..8090; configured == :8080 dedupes to 11 candidates.
	if len(fp.gotCands) != 11 {
		t.Fatalf("candidates = %d, want 11 (deduped): %v", len(fp.gotCands), fp.gotCands)
	}
	if fp.gotCands[0] != "http://127.0.0.1:8080" {
		t.Fatalf("first candidate = %q, want configured URL", fp.gotCands[0])
	}
	if fp.gotCands[10] != "http://127.0.0.1:8090" {
		t.Fatalf("last candidate = %q, want :8090", fp.gotCands[10])
	}
	if res.providerKey != "llamacpp" || res.baseURL != "http://127.0.0.1:8083" || res.source != "discovered" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.notice, "resolved to http://127.0.0.1:8083") {
		t.Fatalf("notice = %q", res.notice)
	}
	if fp.gotOpts != 1 {
		t.Fatalf("gotOpts = %d, want 1 (base HTTP-client opt only, no APIKey configured)", fp.gotOpts)
	}
}

// recProber records each call's candidates and whether the client options it
// received carry an API key. It applies the options to a real openaicompat
// probe against an httptest recorder and reads the Authorization header the
// recorder got (the key field is unexported). No port in 8080-8090 is bound.
type recProber struct {
	calls   [][]string
	sawAuth []string // Authorization header seen per call ("" = none)
	serves  string   // candidate URL that "serves" the model; "" = none
}

func (rp *recProber) probe(ctx context.Context, cands []string, model string, opts ...openaicompat.ClientOption) (string, error) {
	rp.calls = append(rp.calls, append([]string(nil), cands...))
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
	}))
	defer srv.Close()
	if _, err := openaicompat.DiscoverBaseURL(ctx, []string{srv.URL}, model, opts...); err != nil {
		return "", err
	}
	rp.sawAuth = append(rp.sawAuth, auth)
	for _, c := range cands {
		if c == rp.serves {
			return c, nil
		}
	}
	return "", fmt.Errorf("no candidate serves %q", model)
}

func keyedConfig(key string) *config.Config {
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	pc := cfg.Providers["llamacpp"]
	pc.APIKey = key
	cfg.Providers["llamacpp"] = pc
	return cfg
}

func TestResolveBackendKeyedSkipsScan(t *testing.T) {
	const canary = "CANARY-776-a1b2c3d4e5f6"
	rp := &recProber{}
	res, err := resolveBackend(context.Background(), keyedConfig(canary), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(rp.calls) != 1 || len(rp.calls[0]) != 1 || rp.calls[0][0] != "http://127.0.0.1:8080" {
		t.Fatalf("probe calls = %v, want exactly [[configured]]", rp.calls)
	}
	if len(rp.sawAuth) != 1 || rp.sawAuth[0] != "Bearer "+canary {
		t.Fatal("key must reach the configured URL probe")
	}
	want := `openai-compat backend discovery skipped: provider "llamacpp" has an api_key; pass -base-url to use another port`
	if len(res.warns) != 1 || res.warns[0] != want {
		t.Fatalf("warns = %q, want [%q]", res.warns, want)
	}
	if res.baseURL != "" || res.providerKey != "" {
		t.Fatalf("res = %+v, want no override", res)
	}
	if strings.Contains(res.warns[0], canary) || strings.Contains(res.notice, canary) {
		t.Fatal("canary leaked into notice output")
	}
}

func TestResolveBackendKeyedConfiguredServes(t *testing.T) {
	rp := &recProber{serves: "http://127.0.0.1:8080"}
	res, err := resolveBackend(context.Background(), keyedConfig("CANARY-776-0f9e8d7c"), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(res.warns) != 0 || res.baseURL != "" || res.providerKey != "" || res.notice != "" {
		t.Fatalf("res = %+v, want zero resolution", res)
	}
	if len(rp.calls) != 1 || len(rp.calls[0]) != 1 || rp.calls[0][0] != "http://127.0.0.1:8080" {
		t.Fatalf("probe calls = %v, want one call with [configured]", rp.calls)
	}
	if len(rp.sawAuth) != 1 || rp.sawAuth[0] != "Bearer CANARY-776-0f9e8d7c" {
		t.Fatal("key must reach the configured URL probe")
	}
}

func TestResolveBackendKeylessStillScans(t *testing.T) {
	rp := &recProber{serves: "http://127.0.0.1:8083"}
	res, err := resolveBackend(context.Background(), keyedConfig(""), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(rp.calls) != 1 || len(rp.calls[0]) != 11 {
		t.Fatalf("probe calls = %v, want one call with 8080..8090", rp.calls)
	}
	if len(rp.sawAuth) != 1 || rp.sawAuth[0] != "" {
		t.Fatal("keyless scan must make one probe without Authorization")
	}
	if res.providerKey != "llamacpp" || res.baseURL != "http://127.0.0.1:8083" || res.source != "discovered" {
		t.Fatalf("res = %+v", res)
	}
}

func TestResolveBackendGuardedScanSendsNoKey(t *testing.T) {
	rp := &recProber{serves: "http://127.0.0.1:8083"}
	res, err := resolveBackend(context.Background(), keyedConfig(""), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
		guardCandidate: func(string) (*http.Client, context.Context, error) {
			return &http.Client{}, context.Background(), nil
		},
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(rp.sawAuth) == 0 {
		t.Fatal("guarded scan made no probes")
	}
	for i, a := range rp.sawAuth {
		if a != "" {
			t.Fatalf("guarded candidate %d carried an Authorization header", i)
		}
	}
	if res.baseURL != "http://127.0.0.1:8083" {
		t.Fatalf("res = %+v", res)
	}
}

// The production configuration: guardCandidate is always set. A keyed
// provider asks the guard only for the configured URL, probes nothing else,
// and a miss yields the skip notice.
func TestResolveBackendKeyedGuardedProbesOnlyConfigured(t *testing.T) {
	const canary = "CANARY-776-77aa11bb22cc"
	rp := &recProber{}
	var guarded []string
	res, err := resolveBackend(context.Background(), keyedConfig(canary), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
		guardCandidate: func(c string) (*http.Client, context.Context, error) {
			guarded = append(guarded, c)
			return &http.Client{}, context.Background(), nil
		},
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(guarded) != 1 || guarded[0] != "http://127.0.0.1:8080" {
		t.Fatalf("guard asked for %v, want only the configured URL", guarded)
	}
	if len(rp.calls) != 1 || len(rp.calls[0]) != 1 || rp.calls[0][0] != "http://127.0.0.1:8080" {
		t.Fatalf("probe calls = %v, want exactly [[configured]]", rp.calls)
	}
	if len(rp.sawAuth) != 1 || rp.sawAuth[0] != "Bearer "+canary {
		t.Fatal("key must reach the configured URL probe")
	}
	if len(res.warns) != 1 || !strings.Contains(res.warns[0], "discovery skipped") || strings.Contains(res.warns[0], canary) {
		t.Fatalf("warns = %q, want the skip notice without the key", res.warns)
	}
	if res.baseURL != "" || res.providerKey != "" {
		t.Fatalf("res = %+v, want no override", res)
	}
}

// A guard refusal for the configured URL skips discovery rather than probing
// through an unguarded client.
func TestResolveBackendKeyedGuardRefusalSkips(t *testing.T) {
	rp := &recProber{serves: "http://127.0.0.1:8080"}
	res, err := resolveBackend(context.Background(), keyedConfig("CANARY-776-5566aabb"), backendResolveOpts{
		lookupEnv: noEnv, prober: rp.probe,
		guardCandidate: func(string) (*http.Client, context.Context, error) {
			return nil, nil, fmt.Errorf("refused")
		},
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(rp.calls) != 0 {
		t.Fatalf("probed despite guard refusal: %v", rp.calls)
	}
	if len(res.warns) != 1 || !strings.Contains(res.warns[0], "discovery skipped") {
		t.Fatalf("warns = %q", res.warns)
	}
}

// Real guard + a configured URL that redirects to a second loopback port:
// the key must not follow the redirect.
func TestResolveBackendKeyedRealGuardDoesNotFollowRedirect(t *testing.T) {
	const canary = "CANARY-776-c0ffee123456"
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		_, _ = fmt.Fprint(w, `{"data":[{"id":"gemma4:31b"}]}`)
	}))
	defer target.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer front.Close()

	cfg := ocConfig("llamacpp", "gemma4:31b", front.URL)
	pc := cfg.Providers["llamacpp"]
	pc.APIKey = canary
	cfg.Providers["llamacpp"] = pc
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: openaicompat.DiscoverBaseURL,
		guardCandidate: discoveryCandidateGuard(context.Background(), "llamacpp"),
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target received %d request(s); the key followed a redirect", targetHits)
	}
	if len(res.warns) != 1 || !strings.Contains(res.warns[0], "discovery skipped") {
		t.Fatalf("warns = %q", res.warns)
	}
}

func TestResolveBackend_DistinctConfiguredURLPrepended(t *testing.T) {
	fp := &fakeProber{returnURL: "http://127.0.0.1:8080"}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://localhost:9090")
	if _, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	}); err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(fp.gotCands) != 12 {
		t.Fatalf("candidates = %d, want 12 (configured :9090 + 8080..8090)", len(fp.gotCands))
	}
	if fp.gotCands[0] != "http://localhost:9090" {
		t.Fatalf("first candidate = %q, want configured URL first", fp.gotCands[0])
	}
}

func TestResolveBackend_HitEqualsConfiguredNoOverride(t *testing.T) {
	fp := &fakeProber{returnURL: "http://127.0.0.1:8080"}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if res.providerKey != "" || res.baseURL != "" || res.notice != "" {
		t.Fatalf("res = %+v, want zero resolution when the configured URL serves", res)
	}
}

func TestResolveBackend_NoHitWarnsNonFatal(t *testing.T) {
	fp := &fakeProber{returnErr: fmt.Errorf("openaicompat: discover: no candidate serves model %q; tried a, b", "gemma4:31b")}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend must be non-fatal on discovery miss: %v", err)
	}
	if res.providerKey != "" {
		t.Fatalf("res = %+v, want zero resolution", res)
	}
	if len(res.warns) != 1 || !strings.Contains(res.warns[0], "no candidate serves") {
		t.Fatalf("warns = %v", res.warns)
	}
}

func TestResolveBackend_NoTargetNoOp(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{"nil config", nil},
		{"recommend mode (no defaults.agent)", &config.Config{
			Providers: map[string]config.ProviderConfig{
				"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			},
			Models:   map[string]config.ModelConfig{"r": {Name: "m", Provider: "llamacpp", Type: "dense"}},
			Defaults: map[string]string{},
		}},
		{"primary not openai-compat", &config.Config{
			Providers: map[string]config.ProviderConfig{
				"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
			},
			Models:   map[string]config.ModelConfig{"r": {Name: "m", Provider: "ollama", Type: "dense"}},
			Defaults: map[string]string{"agent": "r"},
		}},
		{"defaults.agent names missing role", &config.Config{
			Providers: map[string]config.ProviderConfig{
				"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			},
			Models:   map[string]config.ModelConfig{"agent-role": {Name: "m", Provider: "llamacpp", Type: "dense"}},
			Defaults: map[string]string{"agent": "typo"},
		}},
		{"chain resolution errors (RoleFallbackChain)", &config.Config{
			// defaults.agent points at a role whose fallback cycles back to
			// itself: RoleFallbackChain errors, which openAICompatAgentTarget
			// treats identically to an unparseable selector (ok=false).
			Providers: map[string]config.ProviderConfig{
				"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			},
			Models: map[string]config.ModelConfig{
				"agent-role": {Name: "m", Provider: "llamacpp", Type: "dense", Fallbacks: []string{"agent-role"}},
			},
			Defaults: map[string]string{"agent": "agent-role"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &fakeProber{}
			// Without an explicit override: silent no-op.
			res, err := resolveBackend(context.Background(), tt.cfg, backendResolveOpts{
				lookupEnv: noEnv, prober: fp.probe,
			})
			if err != nil {
				t.Fatalf("resolveBackend: %v", err)
			}
			if fp.calls != 0 || res.providerKey != "" || len(res.warns) != 0 {
				t.Fatalf("res = %+v calls=%d, want silent no-op", res, fp.calls)
			}
			// With an explicit override: warn that it has no target.
			res, err = resolveBackend(context.Background(), tt.cfg, backendResolveOpts{
				flagBaseURL: "http://127.0.0.1:8083", flagSet: true,
				lookupEnv: noEnv, prober: fp.probe,
			})
			if err != nil {
				t.Fatalf("resolveBackend: %v", err)
			}
			if res.providerKey != "" {
				t.Fatalf("res = %+v, want no override applied", res)
			}
			if len(res.warns) != 1 || !strings.Contains(res.warns[0], "no openai-compat agent provider") {
				t.Fatalf("warns = %v, want ignored-override warning", res.warns)
			}
		})
	}
}

func TestResolveBackend_IgnoredOverrideWarningNamesTheActiveUseCase(t *testing.T) {
	// A goal-mode user whose planning route has no openai-compat primary must
	// be pointed at the planning route, not at defaults.agent -- the wrong
	// key would send them to edit a default the active route never reads
	// (#476). Ollama-format provider so the openai-compat target derivation
	// fails and the override is ignored.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"ollama": {BaseURL: "http://127.0.0.1:11434", APIFormat: "ollama"}},
		Models:    map[string]config.ModelConfig{"planner": {Name: "big", Provider: "ollama"}},
		Defaults:  map[string]string{"planning": "planner"},
	}
	route := providerbootstrap.PlannedRoute{UseCase: config.UseCasePlanning, Chain: []string{"ollama/big"}}
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		flagBaseURL: "http://127.0.0.1:8083", flagSet: true,
		lookupEnv:   func(string) (string, bool) { return "", false },
		activeRoute: &route,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if len(res.warns) != 1 || !strings.Contains(res.warns[0], "no openai-compat planning provider") {
		t.Fatalf("warns = %v, want the warning to name the planning route", res.warns)
	}
	if strings.Contains(res.warns[0], "defaults.agent") {
		t.Fatalf("warns = %v, must not point a planning-mode user at defaults.agent", res.warns)
	}
}

func TestStartupNotices_BackendLine(t *testing.T) {
	lines := startupNotices(startupInfo{
		workspace:   "/w",
		backendLine: "openai-compat backend: resolved to http://127.0.0.1:8083 (configured http://127.0.0.1:8080 did not serve \"gemma4:31b\")",
	})
	if len(lines) < 2 {
		t.Fatalf("lines = %v", lines)
	}
	if lines[0] != "workspace: /w" {
		t.Fatalf("lines[0] = %q", lines[0])
	}
	if !strings.Contains(lines[1], "openai-compat backend: resolved to") {
		t.Fatalf("lines[1] = %q, want the backend line directly after workspace", lines[1])
	}
}

func TestBackendResolutionDiagSource(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   string
	}{
		{"flag source", "-base-url", "-base-url"},
		{"env source", "GO_LLM_BASE_URL", "GO_LLM_BASE_URL"},
		{"discovered source stays unlabeled", "discovered", ""},
		{"empty source", "", ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := backendResolution{source: tt.source}
			if got := r.diagSource(); got != tt.want {
				t.Fatalf("diagSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveBackend_InvalidFlagErrors(t *testing.T) {
	fp := &fakeProber{}
	cfg := ocConfig("llamacpp", "gemma4:31b", "http://127.0.0.1:8080")
	_, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		flagBaseURL: "http://127.0.0.1:8081/v1", flagSet: true,
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err == nil || !strings.Contains(err.Error(), "without the /v1 suffix") {
		t.Fatalf("err = %v, want substring %q", err, "without the /v1 suffix")
	}
	if fp.calls != 0 {
		t.Fatalf("prober called %d times, want 0 (flag validation fails before discovery)", fp.calls)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://Localhost:8080", true}, // hostnames are case-insensitive
		{"http://[::1]:8080", true},     // brackets stripped by Hostname()
		{"http://127.0.0.2:8080", true}, // whole 127/8 block is loopback
		{"https://api.example.com", false},
		{"http://192.168.1.10:8080", false},
		{"http://myhost.local:8080", false}, // no DNS lookups: not a literal
		{"", false},
		{"not a url", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			if got := isLoopbackURL(tt.raw); got != tt.want {
				t.Fatalf("isLoopbackURL(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
