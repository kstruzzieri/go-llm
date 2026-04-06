package fingerprint

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newTestProber creates a prober backed by a test server.
func newTestProber(handler http.Handler) (*OllamaProber, *httptest.Server) {
	srv := httptest.NewServer(handler)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	return NewProber(client), srv
}

// TestDetectKind_Capabilities verifies Tier 1: capabilities array.
func TestDetectKind_Capabilities(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"details":      map[string]any{"family": "qwen2"},
			"template":     "{{ .System }}\n{{ .Prompt }}",
			"capabilities": []string{"completion", "tools"},
		})
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "qwen2.5:72b")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindChat {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindChat)
	}
	if det.Source != "capabilities" {
		t.Errorf("Source = %q, want %q", det.Source, "capabilities")
	}
	if det.Family != "qwen2" {
		t.Errorf("Family = %q, want %q", det.Family, "qwen2")
	}
}

// TestDetectKind_Embedding verifies embedding detection from capabilities.
func TestDetectKind_Embedding(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"details":      map[string]any{"family": "nomic-bert"},
			"capabilities": []string{"embedding"},
		})
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "nomic-embed-text")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindEmbedding {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindEmbedding)
	}
	if det.Source != "capabilities" {
		t.Errorf("Source = %q, want %q", det.Source, "capabilities")
	}
}

// TestDetectKind_Heuristic verifies Tier 2: family/template fallback.
func TestDetectKind_Heuristic(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No capabilities, but has template (chat model).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"details":  map[string]any{"family": "llama"},
			"template": "{{ .System }}\n{{ .Prompt }}",
		})
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "llama3:8b")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindChat {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindChat)
	}
	if det.Source != "heuristic" {
		t.Errorf("Source = %q, want %q", det.Source, "heuristic")
	}
}

// TestDetectKind_HeuristicEmbedding verifies heuristic for known embedding families.
func TestDetectKind_HeuristicEmbedding(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Known embedding family, no capabilities.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"details": map[string]any{"family": "bert"},
		})
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "bert-base")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindEmbedding {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindEmbedding)
	}
	if det.Source != "heuristic" {
		t.Errorf("Source = %q, want %q", det.Source, "heuristic")
	}
}

// TestDetectKind_Probe verifies Tier 3: live probe when heuristics fail.
func TestDetectKind_Probe(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			// No capabilities, no template, unknown family
			_ = json.NewEncoder(w).Encode(map[string]any{
				"details": map[string]any{"family": "custom"},
			})
		case "/api/chat":
			// Chat succeeds — this model is a chat model
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":   "custom-model",
				"message": map[string]any{"role": "assistant", "content": "hi"},
				"done":    true,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "custom-model")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindChat {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindChat)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want %q", det.Source, "probe")
	}
}

// TestDetectKind_ProbeEmbedding verifies probe falls through to embedding.
func TestDetectKind_ProbeEmbedding(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"details": map[string]any{"family": "custom"},
			})
		case "/api/chat":
			// Chat fails
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"not a chat model"}`))
		case "/api/embed":
			// Embedding succeeds
			_ = json.NewEncoder(w).Encode(map[string]any{
				"embeddings": [][]float64{{0.1, 0.2, 0.3}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "custom-embed")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindEmbedding {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindEmbedding)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want %q", det.Source, "probe")
	}
}

// TestDetectKind_ProbeUnknown verifies probe returns unknown when both fail.
func TestDetectKind_ProbeUnknown(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"details": map[string]any{"family": "custom"},
			})
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"not supported"}`))
		}
	}))
	defer srv.Close()

	det, err := prober.DetectKind(context.Background(), "mystery-model")
	if err != nil {
		t.Fatalf("DetectKind() error: %v", err)
	}
	if det.Kind != ModelKindUnknown {
		t.Errorf("Kind = %q, want %q", det.Kind, ModelKindUnknown)
	}
	if det.Source != "probe" {
		t.Errorf("Source = %q, want %q", det.Source, "probe")
	}
}

// TestDetectKind_ShowError verifies error propagation from ShowModel.
func TestDetectKind_ShowError(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	_, err := prober.DetectKind(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error from ShowModel failure")
	}
}

// TestProbeChat verifies chat metric extraction.
func TestProbeChat(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":                "test-model",
			"message":              map[string]any{"role": "assistant", "content": "Hello!"},
			"done":                 true,
			"eval_count":           10,
			"eval_duration":        500_000_000, // 500ms → 20 tok/s
			"prompt_eval_duration": 200_000_000, // 200ms
			"load_duration":        50_000_000,  // 50ms (warm — below threshold)
			"total_duration":       800_000_000,
		})
	}))
	defer srv.Close()

	metrics, err := prober.ProbeChat(context.Background(), "test-model", nil)
	if err != nil {
		t.Fatalf("ProbeChat() error: %v", err)
	}

	// 10 tokens / 0.5s = 20 tok/s
	if metrics.TokensPerSecond < 19.9 || metrics.TokensPerSecond > 20.1 {
		t.Errorf("TokensPerSecond = %f, want ~20.0", metrics.TokensPerSecond)
	}
	if metrics.PromptLatency != 200_000_000 {
		t.Errorf("PromptLatency = %v, want 200ms", metrics.PromptLatency)
	}
	if metrics.ColdStartLatency != 0 {
		t.Errorf("ColdStartLatency = %v, want 0 (warm model)", metrics.ColdStartLatency)
	}
}

// TestProbeChat_ColdStart verifies cold start detection.
func TestProbeChat_ColdStart(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":          "test-model",
			"message":        map[string]any{"role": "assistant", "content": "Hi"},
			"done":           true,
			"eval_count":     5,
			"eval_duration":  250_000_000,
			"load_duration":  3_000_000_000, // 3 seconds — cold load
			"total_duration": 3_500_000_000,
		})
	}))
	defer srv.Close()

	metrics, err := prober.ProbeChat(context.Background(), "test-model", nil)
	if err != nil {
		t.Fatalf("ProbeChat() error: %v", err)
	}
	if metrics.ColdStartLatency == 0 {
		t.Error("ColdStartLatency = 0, want non-zero for cold model load")
	}
}

// TestProbeChat_Error verifies error propagation.
func TestProbeChat_Error(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"model unavailable"}`))
	}))
	defer srv.Close()

	_, err := prober.ProbeChat(context.Background(), "bad-model", nil)
	if err == nil {
		t.Fatal("expected error from chat failure")
	}
}

// TestProbeEmbedding verifies embedding metric extraction.
func TestProbeEmbedding(t *testing.T) {
	vec := make([]float64, 768)
	for i := range vec {
		vec[i] = float64(i) * 0.001
	}

	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{vec},
		})
	}))
	defer srv.Close()

	metrics, err := prober.ProbeEmbedding(context.Background(), "nomic-embed-text")
	if err != nil {
		t.Fatalf("ProbeEmbedding() error: %v", err)
	}
	if metrics.Dim != 768 {
		t.Errorf("Dim = %d, want 768", metrics.Dim)
	}
	if metrics.Latency <= 0 {
		t.Errorf("Latency = %v, want > 0", metrics.Latency)
	}
}

// TestProbeEmbedding_Error verifies error propagation.
func TestProbeEmbedding_Error(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"not an embedding model"}`))
	}))
	defer srv.Close()

	_, err := prober.ProbeEmbedding(context.Background(), "chat-only-model")
	if err == nil {
		t.Fatal("expected error from embedding failure")
	}
}
