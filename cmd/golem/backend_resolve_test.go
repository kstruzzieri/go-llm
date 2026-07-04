package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
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

func TestResolveBackend_ScanPassesAPIKeyOpt(t *testing.T) {
	fp := &fakeProber{returnURL: "http://127.0.0.1:8083"}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080", APIKey: "k123"},
		},
		Models:   map[string]config.ModelConfig{"agent-role": {Name: "gemma4:31b", Provider: "llamacpp", Type: "dense"}},
		Defaults: map[string]string{"agent": "agent-role"},
	}
	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv: noEnv, prober: fp.probe,
	})
	if err != nil {
		t.Fatalf("resolveBackend: %v", err)
	}
	if fp.calls != 1 {
		t.Fatalf("prober calls = %d, want 1", fp.calls)
	}
	if fp.gotOpts != 2 {
		t.Fatalf("gotOpts = %d, want 2 (base HTTP-client opt + APIKey opt)", fp.gotOpts)
	}
	if res.baseURL != "http://127.0.0.1:8083" {
		t.Fatalf("res = %+v", res)
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
