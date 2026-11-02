package probers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func newOpenAICompatTestProber(handler http.Handler, opts ...OpenAICompatProberOption) (*OpenAICompatProber, *httptest.Server) {
	srv := httptest.NewServer(handler)
	p := openaicompat.NewProvider(openaicompat.NewClient(srv.URL))
	return NewOpenAICompatProber(p, opts...), srv
}

func TestOpenAICompatProber_DetectKind_UsesCapabilitiesHint(t *testing.T) {
	var chatRequests int
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "llama-3"}},
			})
		case "/v1/chat/completions", "/v1/embeddings":
			chatRequests++
			t.Fatalf("DetectKind with capabilities hint must not make probe request to %s", r.URL.Path)
		default:
			http.NotFound(w, r)
		}
	}), WithOpenAICompatCapabilities([]string{"chat", "embed"}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "llama-3")
	if err != nil {
		t.Fatalf("DetectKind() error = %v", err)
	}
	if det.Kind != fingerprint.ModelKindEmbedding {
		t.Errorf("Kind = %q, want %q", det.Kind, fingerprint.ModelKindEmbedding)
	}
	if det.Source != "capabilities" {
		t.Errorf("Source = %q, want capabilities", det.Source)
	}
	if !containsCapability(det.Capabilities, "chat") || !containsCapability(det.Capabilities, "embed") {
		t.Fatalf("Capabilities = %v, want chat and embed", det.Capabilities)
	}
	if chatRequests != 0 {
		t.Fatalf("live probe requests = %d, want 0", chatRequests)
	}
}

func TestOpenAICompatProber_DetectKind_ErrorsWhenModelMissingFromModelsEndpoint(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "other-model"}},
			})
			return
		}
		t.Fatalf("DetectKind must not probe %s when /v1/models does not list the model", r.URL.Path)
	}))
	defer srv.Close()

	if _, err := prober.DetectKind(context.Background(), "missing-model"); err == nil {
		t.Fatal("DetectKind() error = nil, want missing model error")
	}
}

func TestOpenAICompatProber_DetectKind_Chat(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "llama-3"}},
			})
		case "/v1/chat/completions":
			var body struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if body.MaxTokens != 1 {
				t.Fatalf("DetectKind max_tokens = %d, want 1", body.MaxTokens)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"role": "assistant", "content": "hi"}},
				},
				"usage": map[string]any{"completion_tokens": 1},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "llama-3")
	if err != nil {
		t.Fatalf("DetectKind() error = %v", err)
	}
	if det.Kind != fingerprint.ModelKindChat {
		t.Errorf("Kind = %q, want %q", det.Kind, fingerprint.ModelKindChat)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want probe", det.Source)
	}
}

func TestOpenAICompatProber_DetectKind_Embedding(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "embed-model"}},
			})
		case "/v1/chat/completions":
			http.NotFound(w, r)
		case "/v1/embeddings":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"embedding": []float64{0.1, 0.2}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "embed-model")
	if err != nil {
		t.Fatalf("DetectKind() error = %v", err)
	}
	if det.Kind != fingerprint.ModelKindEmbedding {
		t.Errorf("Kind = %q, want %q", det.Kind, fingerprint.ModelKindEmbedding)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want probe", det.Source)
	}
}

func TestOpenAICompatProber_DetectKind_Unknown(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "unknown-model"}},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "unknown-model")
	if err != nil {
		t.Fatalf("DetectKind() error = %v", err)
	}
	if det.Kind != fingerprint.ModelKindUnknown {
		t.Errorf("Kind = %q, want %q", det.Kind, fingerprint.ModelKindUnknown)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want probe", det.Source)
	}
}

func containsCapability(caps []string, want string) bool {
	return slices.Contains(caps, want)
}

func TestOpenAICompatProber_ProbeChat(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			var body struct {
				MaxTokens int `json:"max_tokens"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode chat request: %v", err)
			}
			if body.MaxTokens != 16 {
				t.Fatalf("ProbeChat max_tokens = %d, want 16", body.MaxTokens)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"role": "assistant", "content": "hello"}},
				},
				"usage": map[string]any{"completion_tokens": 8},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	m, err := prober.ProbeChat(context.Background(), "llama-3", nil)
	if err != nil {
		t.Fatalf("ProbeChat() error = %v", err)
	}
	if m.TokensPerSecond <= 0 {
		t.Errorf("TokensPerSecond = %v, want > 0 (8 completion tokens over measured elapsed)", m.TokensPerSecond)
	}
}

func TestOpenAICompatProber_ProbeEmbedding(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{"embedding": []float64{0.1, 0.2, 0.3, 0.4}},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	m, err := prober.ProbeEmbedding(context.Background(), "embed-model")
	if err != nil {
		t.Fatalf("ProbeEmbedding() error = %v", err)
	}
	if m.Dim != 4 {
		t.Errorf("Dim = %d, want 4", m.Dim)
	}
}

func TestOpenAICompatProber_ProbeEmbedding_EmptyResponseErrors(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, err := prober.ProbeEmbedding(context.Background(), "embed-model"); err == nil {
		t.Fatal("ProbeEmbedding() error = nil, want error for empty embedding response")
	}
}

// Fingerprint profiling calls are real model calls too: DetectKind's chat
// probe and embed fallback, ProbeChat and ProbeEmbedding each report their
// usage once, and a capabilities hint (no live probe) reports nothing.
func TestOpenAICompatProberProfilingUsageObserver(t *testing.T) {
	chatUsage := provider.Usage{PromptTokens: 11, CompletionTokens: 2, TotalTokens: 13}
	embedUsage := provider.Usage{PromptTokens: 9, TotalTokens: 9}
	handler := func(chatOK bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/models":
				_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			case "/v1/chat/completions":
				if !chatOK {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`))
			case "/v1/embeddings":
				_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}],"usage":{"prompt_tokens":9,"total_tokens":9}}`))
			default:
				http.NotFound(w, r)
			}
		})
	}
	for _, tt := range []struct {
		name   string
		chatOK bool
		caps   []string
		call   func(*OpenAICompatProber) error
		want   []observedUsage
	}{
		{"detect kind chat", true, nil, func(p *OpenAICompatProber) error {
			_, err := p.DetectKind(context.Background(), "m")
			return err
		}, []observedUsage{{"m", chatUsage}}},
		{"detect kind embed fallback", false, nil, func(p *OpenAICompatProber) error {
			_, err := p.DetectKind(context.Background(), "m")
			return err
		}, []observedUsage{{"m", embedUsage}}},
		{"detect kind from capabilities", true, []string{"chat"}, func(p *OpenAICompatProber) error {
			_, err := p.DetectKind(context.Background(), "m")
			return err
		}, nil},
		{"probe chat", true, nil, func(p *OpenAICompatProber) error {
			_, err := p.ProbeChat(context.Background(), "m", nil)
			return err
		}, []observedUsage{{"m", chatUsage}}},
		{"probe embedding", true, nil, func(p *OpenAICompatProber) error {
			_, err := p.ProbeEmbedding(context.Background(), "m")
			return err
		}, []observedUsage{{"m", embedUsage}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got []observedUsage
			opts := []OpenAICompatProberOption{WithOpenAICompatUsageObserver(func(model string, u provider.Usage) {
				got = append(got, observedUsage{model, u})
			})}
			if tt.caps != nil {
				opts = append(opts, WithOpenAICompatCapabilities(tt.caps))
			}
			prober, srv := newOpenAICompatTestProber(handler(tt.chatOK), opts...)
			defer srv.Close()
			if err := tt.call(prober); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("observations = %+v; want %+v", got, tt.want)
			}
		})
	}
}
