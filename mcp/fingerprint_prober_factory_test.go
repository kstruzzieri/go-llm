package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

type mcpFingerprintStore struct {
	profiles map[string]*fingerprint.Profile
}

func newMCPFingerprintStore() *mcpFingerprintStore {
	return &mcpFingerprintStore{profiles: map[string]*fingerprint.Profile{}}
}

func (s *mcpFingerprintStore) key(backendID, modelName string) string {
	return backendID + "\x00" + modelName
}

func (s *mcpFingerprintStore) Get(_ context.Context, backendID, modelName string) (*fingerprint.Profile, error) {
	p, ok := s.profiles[s.key(backendID, modelName)]
	if !ok {
		return nil, fingerprint.ErrNotFound
	}
	return p, nil
}

func (s *mcpFingerprintStore) GetFailure(context.Context, string, string) (*fingerprint.FailureInfo, error) {
	return nil, fingerprint.ErrNotFound
}

func (s *mcpFingerprintStore) Save(_ context.Context, profile fingerprint.Profile) error {
	p := profile
	s.profiles[s.key(profile.BackendID, profile.ModelName)] = &p
	return nil
}

func (s *mcpFingerprintStore) NeedsFingerprint(_ context.Context, backendID, modelName, _ string) (bool, error) {
	_, ok := s.profiles[s.key(backendID, modelName)]
	return !ok, nil
}

func (s *mcpFingerprintStore) SaveFailure(context.Context, string, string, string, string) error {
	return nil
}

func TestEnsureModelRegistry_ProfilesOpenAICompatWithConfiguredCapabilities(t *testing.T) {
	ctx := context.Background()
	var chatProbeRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "llama-local"}},
			})
		case "/v1/chat/completions":
			chatProbeRequests++
			var body struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode chat probe request: %v", err)
			}
			if body.MaxTokens != 16 {
				t.Fatalf("chat probe max_tokens = %d, want 16", body.MaxTokens)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"role": "assistant", "content": "hello"}},
				},
				"usage": map[string]any{"completion_tokens": 7},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := newMCPFingerprintStore()
	s := &Server{
		ollamaURL:        defaultOllamaURL,
		client:           ollama.NewClient(ollama.WithBaseURL(defaultOllamaURL)),
		fingerprintStore: store,
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"local-openai": {BaseURL: srv.URL, APIFormat: "openai-compat"},
			},
			Models: map[string]config.ModelConfig{
				"chat": {Name: "llama-local", Provider: "local-openai", Type: "dense"},
			},
			Defaults: map[string]string{"chat": "chat"},
		},
	}

	if err := s.ensureModelRegistry(); err != nil {
		t.Fatalf("ensureModelRegistry() error = %v", err)
	}
	profile, err := s.modelRegistry.Lookup(ctx, provider.ModelKey{Provider: "local-openai", Model: "llama-local"})
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if !profile.Caps.Has(provider.CapChat) {
		t.Fatalf("profile Caps = %v, want CapChat from configured canonical capabilities", profile.Caps)
	}
	if chatProbeRequests != 1 {
		t.Fatalf("chat probe requests = %d, want 1", chatProbeRequests)
	}
	saved, err := store.Get(ctx, "local-openai", "llama-local")
	if err != nil {
		t.Fatalf("fingerprint profile not saved: %v", err)
	}
	if saved.GenerationTokensPerSecond <= 0 {
		t.Fatalf("saved GenerationTokensPerSecond = %v, want measured throughput", saved.GenerationTokensPerSecond)
	}
	if !fingerprintCapabilitiesContainAll(saved.Capabilities, "chat", "generate", "stream") {
		t.Fatalf("saved Capabilities = %v, want configured canonical capabilities", saved.Capabilities)
	}
}

func fingerprintCapabilitiesContainAll(got []string, wants ...string) bool {
	seen := make(map[string]bool, len(got))
	for _, v := range got {
		seen[v] = true
	}
	for _, want := range wants {
		if !seen[want] {
			return false
		}
	}
	return true
}
