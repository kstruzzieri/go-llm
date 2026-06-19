package probers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func newOpenAICompatTestProber(handler http.Handler, opts ...OpenAICompatProberOption) (*OpenAICompatProber, *httptest.Server) {
	srv := httptest.NewServer(handler)
	p := openaicompat.NewProvider(openaicompat.NewClient(srv.URL))
	return NewOpenAICompatProber(p, opts...), srv
}

func TestOpenAICompatProber_DetectKind_UsesCapabilitiesHint(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("DetectKind with capabilities hint must not make live probe request to %s", r.URL.Path)
	}), WithOpenAICompatCapabilities([]string{"completion", "embedding"}))
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
	if !containsCapability(det.Capabilities, "completion") || !containsCapability(det.Capabilities, "embedding") {
		t.Fatalf("Capabilities = %v, want completion and embedding", det.Capabilities)
	}
}

func TestOpenAICompatProber_DetectKind_Chat(t *testing.T) {
	prober, srv := newOpenAICompatTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]any{"role": "assistant", "content": "hi"}},
				},
				"usage": map[string]any{"completion_tokens": 1},
			})
			return
		}
		http.NotFound(w, r)
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
	prober, srv := newOpenAICompatTestProber(http.NotFoundHandler())
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
