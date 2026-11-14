package fingerprint

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
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

// TestOllamaProbeToolCall_CapabilitiesWithTools verifies a curated yes when
// /api/show lists "tools".
func TestOllamaProbeToolCall_CapabilitiesWithTools(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{"completion", "tools"},
		})
	}))
	defer srv.Close()

	out, err := prober.ProbeToolCall(context.Background(), "qwen2.5:72b")
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != CapProbeYes {
		t.Errorf("State = %q, want %q", out.State, CapProbeYes)
	}
	if out.TTL != 0 {
		t.Errorf("TTL = %v, want 0", out.TTL)
	}
}

// TestOllamaProbeToolCall_CapabilitiesWithoutTools verifies a curated no when
// the capabilities array is present but lacks "tools".
func TestOllamaProbeToolCall_CapabilitiesWithoutTools(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{"completion"},
		})
	}))
	defer srv.Close()

	out, err := prober.ProbeToolCall(context.Background(), "gemma3:27b")
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != CapProbeNo {
		t.Errorf("State = %q, want %q", out.State, CapProbeNo)
	}
	if out.TTL != 0 {
		t.Errorf("TTL = %v, want 0", out.TTL)
	}
}

// TestOllamaProbeToolCall_EmptyCapabilitiesArray verifies an empty-but-present
// array is still a curated no, not inconclusive.
func TestOllamaProbeToolCall_EmptyCapabilitiesArray(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{},
		})
	}))
	defer srv.Close()

	out, err := prober.ProbeToolCall(context.Background(), "gemma3:27b")
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != CapProbeNo {
		t.Errorf("State = %q, want %q", out.State, CapProbeNo)
	}
	if out.TTL != 0 {
		t.Errorf("TTL = %v, want 0", out.TTL)
	}
}

// TestOllamaProbeToolCall_MissingCapabilitiesArray verifies older Ollama
// (no capabilities field) yields inconclusive with a bounded TTL, never a
// hard no.
func TestOllamaProbeToolCall_MissingCapabilitiesArray(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"details": map[string]any{"family": "llama"},
		})
	}))
	defer srv.Close()

	out, err := prober.ProbeToolCall(context.Background(), "llama2:7b")
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != CapProbeInconclusive {
		t.Errorf("State = %q, want %q", out.State, CapProbeInconclusive)
	}
	if out.TTL != CapProbeInconclusiveTTL {
		t.Errorf("TTL = %v, want %v", out.TTL, CapProbeInconclusiveTTL)
	}
}

// TestOllamaProbeToolCall_ShowError verifies a transport/API failure is
// surfaced as an error, not persisted as a verdict.
func TestOllamaProbeToolCall_ShowError(t *testing.T) {
	prober, srv := newTestProber(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	out, err := prober.ProbeToolCall(context.Background(), "missing:latest")
	if err == nil {
		t.Fatal("ProbeToolCall() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "fingerprint: ollama tool probe") {
		t.Errorf("error = %q, want wrap prefix %q", err, "fingerprint: ollama tool probe")
	}
	if out != (CapProbeOutcome{}) {
		t.Errorf("outcome = %+v, want zero value", out)
	}
}

// showStub serves /api/show with body and counts the requests it receives.
type showStub struct {
	srv  *httptest.Server
	hits atomic.Int64
}

func newShowStub(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*OllamaProber, *showStub) {
	t.Helper()
	s := &showStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		// Drain the request so the server notices when the client goes away.
		_, _ = io.Copy(io.Discard, r.Body)
		handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return NewProber(ollama.NewClient(ollama.WithBaseURL(s.srv.URL))), s
}

func showJSON(body map[string]any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(body) }
}

func showRecipe(t *testing.T) ProbeRecipe {
	t.Helper()
	recipe, ok := RecipeFor(MethodOllamaShow)
	if !ok {
		t.Fatal("no ollama-show recipe")
	}
	return recipe
}

func providerAnswer(capability string, state CapProbeState) ProbeAnswer {
	if state == CapProbeInconclusive {
		return ProbeAnswer{Capability: capability, Source: EvidenceSourceProvider, Kind: EvidenceKindAttempt, State: state, TTL: CapProbeInconclusiveTTL}
	}
	return ProbeAnswer{Capability: capability, Source: EvidenceSourceProvider, Kind: EvidenceKindAnswer, State: state}
}

func TestOllamaBoundedRequest_ResponseLimit(t *testing.T) {
	recipe := showRecipe(t)
	huge := strings.Repeat("x", int(recipe.MaxResponseBytes))
	prober, stub := newShowStub(t, showJSON(map[string]any{"capabilities": []string{"completion"}, "template": huge}))
	res, err := prober.ProbeCapabilities(context.Background(), "m", CapabilityProbeRequest{Method: MethodOllamaShow, Capabilities: []string{"chat"}})
	if !errors.Is(err, responsebound.ErrExceeded) {
		t.Fatalf("err = %v, want ErrExceeded", err)
	}
	if len(res.Answers) != 0 {
		t.Errorf("answers = %+v, want none", res.Answers)
	}
	if res.Requests != 1 || stub.hits.Load() != 1 {
		t.Errorf("Requests = %d, stub saw %d, want 1 and 1", res.Requests, stub.hits.Load())
	}
}

func TestOllamaProbeCapabilities_AllMetadataAndMissingFields(t *testing.T) {
	recipe := showRecipe(t)
	capabilities := []string{"completion", "tools", "thinking"}
	template := "{{ .Prompt }}{{ .Suffix }}"
	prober, stub := newShowStub(t, showJSON(map[string]any{"capabilities": capabilities, "template": template}))

	res, err := prober.ProbeCapabilities(context.Background(), "m", CapabilityProbeRequest{Method: MethodOllamaShow, Capabilities: recipe.Capabilities})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodOllamaShow || res.Requests != 1 || stub.hits.Load() != 1 {
		t.Fatalf("Method %q, Requests %d, stub saw %d; want %q, 1, 1", res.Method, res.Requests, stub.hits.Load(), MethodOllamaShow)
	}
	facts := OllamaCapabilityFacts(capabilities, template)
	want := []ProbeAnswer{
		providerAnswer("chat", CapProbeYes), providerAnswer("generate", CapProbeYes),
		providerAnswer("insert", CapProbeYes), providerAnswer("embed", CapProbeInconclusive),
		providerAnswer("stream", CapProbeYes), providerAnswer("tool_call", CapProbeYes),
		providerAnswer("thinking", CapProbeYes),
	}
	got := map[string]ProbeAnswer{}
	for _, a := range res.Answers {
		if _, dup := got[a.Capability]; dup {
			t.Errorf("capability %q answered twice", a.Capability)
		}
		got[a.Capability] = a
		if state, ok := facts[a.Capability]; ok && a.State != state {
			t.Errorf("%s = %s, the listing mapping says %s", a.Capability, a.State, state)
		}
	}
	if len(got) != len(want) {
		t.Errorf("answers = %+v, want %+v", res.Answers, want)
	}
	for _, w := range want {
		if got[w.Capability] != w {
			t.Errorf("%s = %+v, want %+v", w.Capability, got[w.Capability], w)
		}
	}

	// Only the facts reported, plus the requested capabilities with no fact.
	prober, _ = newShowStub(t, showJSON(map[string]any{"capabilities": []string{"embedding"}}))
	res, err = prober.ProbeCapabilities(context.Background(), "m", CapabilityProbeRequest{Method: MethodOllamaShow, Capabilities: []string{"chat"}})
	if err != nil {
		t.Fatal(err)
	}
	wantSmall := []ProbeAnswer{providerAnswer("chat", CapProbeInconclusive), providerAnswer("embed", CapProbeYes), providerAnswer("tool_call", CapProbeNo)}
	if !slices.Equal(res.Answers, wantSmall) {
		t.Errorf("answers = %+v, want %+v", res.Answers, wantSmall)
	}
}

func TestOllamaProbeCapabilities_TimeoutAndRedirect(t *testing.T) {
	req := CapabilityProbeRequest{Method: MethodOllamaShow, Capabilities: []string{"tool_call"}}
	t.Run("timeout", func(t *testing.T) {
		prober, stub := newShowStub(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		})
		prober.requestTimeout = 100 * time.Millisecond
		start := time.Now()
		res, err := prober.ProbeCapabilities(context.Background(), "m", req)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want a deadline", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Errorf("took %v; the overridden timeout did not apply", time.Since(start))
		}
		if len(res.Answers) != 0 || res.Requests != 1 || stub.hits.Load() != 1 {
			t.Errorf("answers %+v, Requests %d, stub saw %d; want none, 1, 1", res.Answers, res.Requests, stub.hits.Load())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		_, target := newShowStub(t, showJSON(map[string]any{"capabilities": []string{"tools"}}))
		prober, origin := newShowStub(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
		})
		res, err := prober.ProbeCapabilities(context.Background(), "m", req)
		if err == nil {
			t.Fatal("a redirect was followed")
		}
		if len(res.Answers) != 0 || res.Requests != 1 || origin.hits.Load() != 1 || target.hits.Load() != 0 {
			t.Errorf("answers %+v, Requests %d, origin %d, target %d; want none, 1, 1, 0", res.Answers, res.Requests, origin.hits.Load(), target.hits.Load())
		}
	})
}

func TestOllamaProbe_CapabilitiesArrayCases(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want CapProbeState
	}{
		{"empty array", map[string]any{"capabilities": []string{}}, CapProbeNo},
		{"missing array", map[string]any{"details": map[string]any{"family": "llama"}}, CapProbeInconclusive},
		{"template only", map[string]any{"template": "{{ .Prompt }}{{ .Suffix }}"}, CapProbeInconclusive},
		{"Tools", map[string]any{"capabilities": []string{"completion", "Tools"}}, CapProbeYes},
		{"tool_call", map[string]any{"capabilities": []string{"tool_call"}}, CapProbeYes},
	}
	for _, tt := range tests {
		t.Run(tt.name+" ProbeToolCall", func(t *testing.T) {
			prober, _ := newShowStub(t, showJSON(tt.body))
			out, err := prober.ProbeToolCall(context.Background(), "m")
			if err != nil {
				t.Fatal(err)
			}
			if out.State != tt.want {
				t.Errorf("State = %q, want %q", out.State, tt.want)
			}
			wantTTL := time.Duration(0)
			if tt.want == CapProbeInconclusive {
				wantTTL = CapProbeInconclusiveTTL
			}
			if out.TTL != wantTTL {
				t.Errorf("TTL = %v, want %v", out.TTL, wantTTL)
			}
		})
		t.Run(tt.name+" ProbeCapabilities", func(t *testing.T) {
			prober, _ := newShowStub(t, showJSON(tt.body))
			res, err := prober.ProbeCapabilities(context.Background(), "m", CapabilityProbeRequest{Method: MethodOllamaShow, Capabilities: []string{"tool_call"}})
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(res.Answers, func(a ProbeAnswer) bool { return a.Capability == "tool_call" })
			if i < 0 {
				t.Fatalf("no tool_call answer in %+v", res.Answers)
			}
			if want := providerAnswer("tool_call", tt.want); res.Answers[i] != want {
				t.Errorf("tool_call = %+v, want %+v", res.Answers[i], want)
			}
		})
	}
}
