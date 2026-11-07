package providerbootstrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestProberFactory_OllamaKeyBuildsProber(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("ollama"))
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "ollama", Model: "qwen"}, nil, op)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	if spec == nil || spec.Prober == nil {
		t.Fatalf("expected non-nil prober spec")
	}
}

func TestProberFactory_UnknownProviderReturnsNil(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "missing", Model: "m"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected nil spec for unknown provider")
	}
}

func TestProberFactory_OpenAICompatWrongTypeErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc": {APIFormat: "openai-compat", BaseURL: "http://localhost:8080"},
	}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("lc"))
	if _, err := factory(context.Background(), provider.ModelKey{Provider: "lc", Model: "m"}, nil, op); err == nil {
		t.Fatalf("expected type-mismatch error when openai-compat provider is not *openaicompat.Provider")
	}
}

func TestFingerprintDigestForModel(t *testing.T) {
	if got := fingerprintDigestForModel(provider.ModelKey{Provider: "p", Model: "m"}, nil, []string{"chat"}); got != "config-caps:chat" {
		t.Fatalf("digest = %q", got)
	}
	if got := fingerprintDigestForModel(provider.ModelKey{Provider: "p", Model: "m"}, nil, nil); got != "p/m" {
		t.Fatalf("digest = %q, want p/m", got)
	}
}

// An empty runtime digest is not a reason to skip a model: it keeps its spec under
// the fallback identity.
func TestProberFactory_OllamaWithoutDigestKeepsFallback(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("ollama"))
	for name, runtime := range map[string]*provider.ModelInfo{"nil runtime": nil, "empty digest": {Name: "qwen"}} {
		spec, err := factory(context.Background(), provider.ModelKey{Provider: "ollama", Model: "qwen"}, runtime, op)
		if err != nil || spec == nil || spec.Prober == nil || spec.ModelDigest != "ollama/qwen" {
			t.Errorf("%s: spec = %+v, err = %v; want a prober under the key", name, spec, err)
		}
	}
}

type countingProfiles struct {
	*fingerprint.SQLiteStore
	saves atomic.Int64
}

func (s *countingProfiles) Save(ctx context.Context, p fingerprint.Profile) error {
	s.saves.Add(1)
	return s.SQLiteStore.Save(ctx, p)
}

const stubDigest = "sha256:real"

// ollamaStub is an Ollama server listing one model. Its show response has no
// digest and no tools capability, so a tool-call resolution has to reach the probe.
type ollamaStub struct {
	tagsDown atomic.Bool
	chats    atomic.Int64
}

// newOllamaBundle wires the stub through the real bootstrap, so the registry uses
// the real factory for both profiling and tool-call probing.
func newOllamaBundle(t *testing.T, configured, listed string) (*ollamaStub, *Bundle, *countingProfiles) {
	t.Helper()
	stub := &ollamaStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if stub.tagsDown.Load() {
				http.Error(w, "overloaded", http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"models":[{"name":%q,"digest":%q}]}`, listed, stubDigest)
		case "/api/show":
			_, _ = fmt.Fprint(w, `{"capabilities":["completion"],"template":"{{ .Prompt }}"}`)
		case "/api/chat":
			stub.chats.Add(1)
			_, _ = fmt.Fprintf(w, `{"model":%q,"message":{"role":"assistant","content":"hi"},"done":true,"eval_count":1,"eval_duration":1000}`, listed)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	store := &countingProfiles{SQLiteStore: newTestFingerprintStore(t)}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"ollama": {APIFormat: "ollama", BaseURL: srv.URL}},
		Models:    map[string]config.ModelConfig{"chat": {Provider: "ollama", Name: configured, Type: "dense"}},
	}
	bundle, err := New(context.Background(), Options{Config: cfg, FingerprintStore: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = bundle.Close() })
	return stub, bundle, store
}

// A model configured with one spelling and listed with the other is profiled and
// resolved under its real digest, once.
func TestOllamaNameSpellingsGetProfileAndToolCallResolution(t *testing.T) {
	for _, tc := range []struct{ configured, listed string }{
		{"m", "m:latest"},
		{"m:latest", "m"},
	} {
		t.Run(tc.configured+" listed as "+tc.listed, func(t *testing.T) {
			stub, bundle, store := newOllamaBundle(t, tc.configured, tc.listed)
			ctx := context.Background()
			key := provider.ModelKey{Provider: "ollama", Model: tc.configured}
			for range 2 {
				if _, err := bundle.Models.Refresh(ctx, key); err != nil {
					t.Fatalf("Refresh: %v", err)
				}
			}
			if saves, chats := store.saves.Load(), stub.chats.Load(); saves != 1 || chats != 1 {
				t.Fatalf("profile written %d times and benchmark run %d times, want 1 and 1", saves, chats)
			}
			profile, err := store.Get(ctx, "ollama", tc.configured)
			if err != nil || profile.ModelDigest != stubDigest {
				t.Fatalf("stored profile = %+v, %v; want digest %q", profile, err, stubDigest)
			}
			state, err := bundle.Models.ResolveToolCall(ctx, key)
			if err != nil || state != fingerprint.CapProbeNo {
				t.Fatalf("ResolveToolCall = %q, %v; want %q", state, err, fingerprint.CapProbeNo)
			}
			row, err := store.GetCapProbe(ctx, "ollama", tc.configured, "tool_call")
			if err != nil || row == nil || row.State != fingerprint.CapProbeNo || row.ModelDigest != stubDigest {
				t.Fatalf("probe row = %+v, %v; want %q under %q", row, err, fingerprint.CapProbeNo, stubDigest)
			}
		})
	}
}

// A known limit, pinned so it stays bounded: a failed /api/tags during ModelInfo
// leaves no digest, so the fingerprint re-runs under the fallback on that lookup
// and again when the real digest returns, and then settles.
func TestTagsFailureReprofilesOnThatLookupAndOnRecovery(t *testing.T) {
	stub, bundle, store := newOllamaBundle(t, "m:7b", "m:7b")
	ctx := context.Background()
	key := provider.ModelKey{Provider: "ollama", Model: "m:7b"}
	for _, step := range []struct {
		name   string
		down   bool
		saves  int64
		digest string
	}{
		{"healthy", false, 1, stubDigest},
		{"healthy again", false, 1, stubDigest},
		{"tags down", true, 2, "config-caps:chat,generate,stream"},
		{"tags back", false, 3, stubDigest},
		{"healthy after", false, 3, stubDigest},
	} {
		stub.tagsDown.Store(step.down)
		if _, err := bundle.Models.Refresh(ctx, key); err != nil {
			t.Fatalf("%s: Refresh: %v", step.name, err)
		}
		if got := store.saves.Load(); got != step.saves {
			t.Fatalf("%s: profile written %d times, want %d", step.name, got, step.saves)
		}
		profile, err := store.Get(ctx, "ollama", "m:7b")
		if err != nil || profile.ModelDigest != step.digest {
			t.Fatalf("%s: stored profile = %+v, %v; want digest %q", step.name, profile, err, step.digest)
		}
	}
}
