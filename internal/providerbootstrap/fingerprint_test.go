package providerbootstrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func TestProberFactory_OllamaKeyBuildsProber(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("ollama"))
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "ollama", Model: "qwen"}, &provider.ModelInfo{Digest: "sha256:real"}, op)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	if spec == nil || spec.Prober == nil || spec.ModelDigest != "sha256:real" {
		t.Fatalf("spec = %+v, want a prober under the runtime digest", spec)
	}
}

// With no runtime digest, an Ollama lookup has no real identity to profile under.
func TestProberFactory_OllamaWithoutDigestHasNoSpec(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	factory := proberFactory(cfg, map[string]*ollama.Client{"ollama": ollama.NewClient()}, nil)
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("ollama"))
	for name, runtime := range map[string]*provider.ModelInfo{"nil runtime": nil, "empty digest": {Name: "qwen"}} {
		spec, err := factory(context.Background(), provider.ModelKey{Provider: "ollama", Model: "qwen"}, runtime, op)
		if err != nil || spec != nil {
			t.Errorf("%s: spec = %+v, err = %v; want none", name, spec, err)
		}
	}
}

// An openai-compat server reports no digest at all, so its fallback is its identity.
func TestProberFactory_OpenAICompatWithoutDigestKeepsFallback(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc": {APIFormat: "openai-compat", BaseURL: "http://localhost:8080"},
	}}
	factory := proberFactory(cfg, nil, nil)
	oc := openaicompat.NewProvider(openaicompat.NewClient("http://localhost:8080"), openaicompat.WithProviderName("lc"))
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "lc", Model: "m"}, nil, oc)
	if err != nil {
		t.Fatal(err)
	}
	if spec == nil || spec.Prober == nil || spec.ModelDigest != "lc/m" {
		t.Fatalf("spec = %+v, want a prober under the key", spec)
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

// One failed /api/tags must not re-run the benchmark under a fallback identity,
// neither during the failure nor when the real digest comes back.
func TestTagsFailureDoesNotReprofile(t *testing.T) {
	const digest = "sha256:real"
	var tagsDown atomic.Bool
	var chats atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if tagsDown.Load() {
				http.Error(w, "overloaded", http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"models":[{"name":"m:7b","digest":%q}]}`, digest)
		case "/api/show":
			_, _ = fmt.Fprint(w, `{"capabilities":["completion"],"template":"{{ .Prompt }}"}`)
		case "/api/chat":
			chats.Add(1)
			_, _ = fmt.Fprint(w, `{"model":"m:7b","message":{"role":"assistant","content":"hi"},"done":true,"eval_count":1,"eval_duration":1000}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	db, err := memory.OpenHardenedDB(ctx, filepath.Join(t.TempDir(), "fingerprints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	inner, err := fingerprint.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	store := &countingProfiles{SQLiteStore: inner}

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	reg := provider.NewRegistry()
	if err := reg.Register(provider.NewOllamaProvider(client)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"ollama": {APIFormat: "ollama", BaseURL: srv.URL}}}
	mr, err := provider.NewModelRegistry(reg, store,
		provider.WithFingerprintProberFactory(proberFactory(cfg, map[string]*ollama.Client{"ollama": client}, nil)))
	if err != nil {
		t.Fatal(err)
	}

	key := provider.ModelKey{Provider: "ollama", Model: "m:7b"}
	for _, step := range []struct {
		name string
		down bool
	}{{"healthy", false}, {"healthy again", false}, {"tags down", true}, {"tags back", false}, {"healthy after", false}} {
		tagsDown.Store(step.down)
		if _, err := mr.Refresh(ctx, key); err != nil {
			t.Fatalf("%s: Refresh: %v", step.name, err)
		}
		if got := store.saves.Load(); got != 1 {
			t.Fatalf("%s: profile written %d times, want 1", step.name, got)
		}
		if got := chats.Load(); got != 1 {
			t.Fatalf("%s: benchmark ran %d times, want 1", step.name, got)
		}
		profile, err := store.Get(ctx, "ollama", "m:7b")
		if err != nil || profile.ModelDigest != digest {
			t.Fatalf("%s: stored profile = %+v, %v; want digest %q", step.name, profile, err, digest)
		}
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
