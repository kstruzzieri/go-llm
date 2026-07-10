package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// ---------------------------------------------------------------------------
// Name and Capabilities
// ---------------------------------------------------------------------------

func TestOllamaProvider_Name(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	if got := p.Name(); got != "ollama" {
		t.Errorf("Name() = %q, want %q", got, "ollama")
	}
}

func TestOllamaProvider_WithProviderName(t *testing.T) {
	t.Run("override applied", func(t *testing.T) {
		c := ollama.NewClient()
		p := NewOllamaProvider(c, WithProviderName("shared-ollama-vm"))
		if got := p.Name(); got != "shared-ollama-vm" {
			t.Errorf("Name() = %q, want %q", got, "shared-ollama-vm")
		}
	})

	t.Run("default preserved when option omitted", func(t *testing.T) {
		c := ollama.NewClient()
		p := NewOllamaProvider(c)
		if got := p.Name(); got != "ollama" {
			t.Errorf("Name() = %q, want %q", got, "ollama")
		}
	})

	// Fail-fast on empty: silent ignore would let a misconfigured factory
	// produce a provider whose Registry.Register later fails with a less
	// actionable error.
	t.Run("empty name panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on empty name, got nil")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected string panic value, got %T: %v", r, r)
			}
			if !strings.Contains(msg, "WithProviderName") || !strings.Contains(msg, "empty") {
				t.Errorf("panic message should explain the misuse, got: %q", msg)
			}
		}()
		_ = WithProviderName("")
	})
}

// TestOllamaProvider_ResponseProviderField verifies the configured instance
// name is stamped on every response variant, not just Name(). This matters
// because RouteOutcome attribution and downstream RAG vsid synthesis read
// the response's Provider field, not a separate registry lookup.
func TestOllamaProvider_ResponseProviderField(t *testing.T) {
	const instance = "my-llama-instance"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":   "qwen3:8b",
				"message": map[string]string{"role": "assistant", "content": "hi"},
				"done":    true,
			})
		case "/api/embed":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"embeddings": [][]float64{{0.1, 0.2}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithProviderName(instance))

	chatResp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if chatResp.Provider != instance {
		t.Errorf("Chat response Provider = %q, want %q", chatResp.Provider, instance)
	}

	embResp, err := p.Embed(context.Background(), EmbedRequest{
		Model: "qwen3-embedding:8b",
		Input: []string{"text"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if embResp.Provider != instance {
		t.Errorf("Embed response Provider = %q, want %q", embResp.Provider, instance)
	}
}

// TestOllamaProvider_ResponseProviderField_AllPaths covers the response.Provider
// stamping on the paths the basic ResponseProviderField test misses: Generate
// (non-streaming), GenerateStream, and ChatStream. All four call sites in
// ChatStream are exercised by streaming a content chunk followed by a done.
func TestOllamaProvider_ResponseProviderField_AllPaths(t *testing.T) {
	const instance = "my-renamed-instance"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			w.Header().Set("Content-Type", "application/x-ndjson")
			// Single non-streaming response if Stream is false; ndjson stream otherwise.
			// The ollama client distinguishes via the request payload; emit a
			// streaming sequence either way and let the parser dispatch.
			enc := json.NewEncoder(w)
			_ = enc.Encode(map[string]any{"model": "qwen3:8b", "response": "hel", "done": false})
			_ = enc.Encode(map[string]any{"model": "qwen3:8b", "response": "lo", "done": true, "eval_count": 2})
		case "/api/chat":
			w.Header().Set("Content-Type", "application/x-ndjson")
			enc := json.NewEncoder(w)
			_ = enc.Encode(map[string]any{
				"model":   "qwen3:8b",
				"message": map[string]string{"role": "assistant", "content": "hello"},
				"done":    false,
			})
			_ = enc.Encode(map[string]any{
				"model":   "qwen3:8b",
				"message": map[string]string{"role": "assistant", "content": ""},
				"done":    true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithProviderName(instance))

	// Generate (non-streaming)
	t.Run("Generate", func(t *testing.T) {
		resp, err := p.Generate(context.Background(), GenerateRequest{
			Model:  "qwen3:8b",
			Prompt: "say hi",
		})
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}
		if resp.Provider != instance {
			t.Errorf("Provider = %q, want %q", resp.Provider, instance)
		}
	})

	// GenerateStream
	t.Run("GenerateStream", func(t *testing.T) {
		var providers []string
		err := p.GenerateStream(context.Background(), GenerateRequest{
			Model:  "qwen3:8b",
			Prompt: "say hi",
			Stream: true,
		}, func(resp GenerateResponse) error {
			providers = append(providers, resp.Provider)
			return nil
		})
		if err != nil {
			t.Fatalf("GenerateStream: %v", err)
		}
		if len(providers) == 0 {
			t.Fatal("no chunks received")
		}
		for i, got := range providers {
			if got != instance {
				t.Errorf("chunk %d Provider = %q, want %q", i, got, instance)
			}
		}
	})

	// ChatStream emits multiple response shapes (thinking, content, done,
	// tool calls, partial-on-cancel). Provider must be the configured name
	// for every one of them.
	t.Run("ChatStream", func(t *testing.T) {
		var providers []string
		err := p.ChatStream(context.Background(), ChatRequest{
			Model:    "qwen3:8b",
			Messages: []ChatMessage{{Role: "user", Content: "hi"}},
			Stream:   true,
		}, func(resp ChatResponse) error {
			providers = append(providers, resp.Provider)
			return nil
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		if len(providers) == 0 {
			t.Fatal("no chunks received")
		}
		for i, got := range providers {
			if got != instance {
				t.Errorf("chunk %d Provider = %q, want %q", i, got, instance)
			}
		}
	})
}

func TestOllamaProvider_Capabilities(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	caps := p.Capabilities()

	tests := []struct {
		name string
		cap  Capability
	}{
		{"CapChat", CapChat},
		{"CapGenerate", CapGenerate},
		{"CapInsert", CapInsert},
		{"CapEmbed", CapEmbed},
		{"CapStream", CapStream},
		{"CapToolCall", CapToolCall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !caps.Has(tt.cap) {
				t.Errorf("Capabilities() missing %s", tt.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestOllamaProvider_Health(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"healthy", http.StatusOK, false},
		{"unhealthy", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
			p := NewOllamaProvider(c)
			err := p.Health(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Health() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "provider: ollama:") {
				t.Errorf("error should contain 'provider: ollama:' prefix, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Chat (non-streaming)
// ---------------------------------------------------------------------------

func TestOllamaProvider_Chat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming chat")
		}
		if req.Model != "qwen3:8b" {
			t.Errorf("model = %q, want %q", req.Model, "qwen3:8b")
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "Hello" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}

		resp := ollama.ChatResponse{
			Model:              "qwen3:8b",
			Message:            ollama.ChatMessage{Role: "assistant", Content: "Hi there!"},
			Done:               true,
			PromptEvalCount:    5,
			EvalCount:          3,
			LoadDuration:       1000000,
			PromptEvalDuration: 2000000,
			EvalDuration:       3000000,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Content != "Hi there!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hi there!")
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "ollama")
	}
	if resp.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", resp.Model, "qwen3:8b")
	}
	if !resp.Done {
		t.Error("expected Done=true")
	}
	if resp.Usage.PromptTokens != 5 {
		t.Errorf("Usage.PromptTokens = %d, want 5", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 3 {
		t.Errorf("Usage.CompletionTokens = %d, want 3", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("Usage.TotalTokens = %d, want 8", resp.Usage.TotalTokens)
	}
	if resp.Latency.LoadDuration != time.Duration(1000000) {
		t.Errorf("Latency.LoadDuration = %v, want 1ms", resp.Latency.LoadDuration)
	}
}

func TestOllamaProvider_Chat_ThinkExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me reason about this.</think>The answer is 42."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "What is the meaning of life?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "Let me reason about this." {
		t.Errorf("Thinking = %q, want %q", resp.Thinking, "Let me reason about this.")
	}
	if resp.Content != "The answer is 42." {
		t.Errorf("Content = %q, want %q", resp.Content, "The answer is 42.")
	}
}

func TestOllamaProvider_Chat_NoThinkTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "Just a plain answer."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", resp.Thinking)
	}
	if resp.Content != "Just a plain answer." {
		t.Errorf("Content = %q, want %q", resp.Content, "Just a plain answer.")
	}
}

func TestOllamaProvider_Chat_ThinkToggleDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me reason about this.</think>The answer is 42."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkToggle))

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "What is the meaning of life?"}},
		Options: ModelOptions{
			Think: Ptr(false),
		},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", resp.Thinking)
	}
	want := "<think>Let me reason about this.</think>The answer is 42."
	if resp.Content != want {
		t.Errorf("Content = %q, want %q", resp.Content, want)
	}
}

// TestOllamaProvider_Chat_SurfacesNativeThinking verifies that Ollama's
// separate message.thinking field is surfaced through ChatResponse.Thinking,
// with Content left as the clean final answer — no inline tags to strip.
func TestOllamaProvider_Chat_SurfacesNativeThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "The answer is 42.", Thinking: "Let me reason about this."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "What is the meaning of life?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "Let me reason about this." {
		t.Errorf("Thinking = %q, want native message.thinking surfaced", resp.Thinking)
	}
	if resp.Content != "The answer is 42." {
		t.Errorf("Content = %q, want content unchanged when reasoning is in its own field", resp.Content)
	}
}

// TestOllamaProvider_Chat_NativeThinkingWinsOverInline locks the precedence:
// the structured message.thinking field is authoritative over inline <think>
// tags, which are still stripped from Content.
func TestOllamaProvider_Chat_NativeThinkingWinsOverInline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "<think>inline</think>The answer is 42.", Thinking: "structured reasoning"},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Q"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "structured reasoning" {
		t.Errorf("Thinking = %q, want structured message.thinking to win over inline tags", resp.Thinking)
	}
	if resp.Content != "The answer is 42." {
		t.Errorf("Content = %q, want inline tags still stripped", resp.Content)
	}
}

// TestOllamaProvider_ChatStream_SurfacesNativeThinking verifies streaming
// message.thinking deltas are emitted as Thinking chunks, separate from content
// chunks. Ollama streams reasoning before the answer.
func TestOllamaProvider_ChatStream_SurfacesNativeThinking(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Thinking: "Let me "}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Thinking: "think."}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "The answer is 42."}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	var thinkingChunks, contentChunks []string
	var gotDone bool
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Q"}},
	}, func(resp ChatResponse) error {
		if resp.Thinking != "" {
			thinkingChunks = append(thinkingChunks, resp.Thinking)
		}
		if resp.Content != "" {
			contentChunks = append(contentChunks, resp.Content)
		}
		if resp.Done {
			gotDone = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}
	if !gotDone {
		t.Error("expected final chunk with Done=true")
	}
	if thinking := strings.Join(thinkingChunks, ""); thinking != "Let me think." {
		t.Errorf("accumulated thinking = %q, want %q", thinking, "Let me think.")
	}
	if content := strings.Join(contentChunks, ""); content != "The answer is 42." {
		t.Errorf("accumulated content = %q, want %q", content, "The answer is 42.")
	}
}

func TestOllamaProvider_Chat_WithOptions(t *testing.T) {
	var receivedOpts ollama.ModelOptions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Options != nil {
			receivedOpts = *req.Options
		}
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "ok"},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
		Options: ModelOptions{
			Temperature: Ptr(0.7),
			NumPredict:  100,
			NumCtx:      4096,
		},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if receivedOpts.Temperature != 0.7 {
		t.Errorf("Temperature = %f, want 0.7", receivedOpts.Temperature)
	}
	if receivedOpts.NumPredict != 100 {
		t.Errorf("NumPredict = %d, want 100", receivedOpts.NumPredict)
	}
	if receivedOpts.NumCtx != 4096 {
		t.Errorf("NumCtx = %d, want 4096", receivedOpts.NumCtx)
	}
}

// ---------------------------------------------------------------------------
// ChatStream
// ---------------------------------------------------------------------------

func TestOllamaProvider_ChatStream(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "Hello "}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "world!"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true, EvalCount: 4, PromptEvalCount: 2},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	// Use ThinkNone to get passthrough behavior for plain text.
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	var received []ChatResponse
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(resp ChatResponse) error {
		received = append(received, resp)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	// With ThinkNone passthrough, we expect: content("Hello "), content("world!"), done.
	if len(received) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(received))
	}

	// All should have provider set.
	for i, r := range received {
		if r.Provider != "ollama" {
			t.Errorf("chunk[%d].Provider = %q, want %q", i, r.Provider, "ollama")
		}
	}

	// Last chunk should be Done with usage.
	last := received[len(received)-1]
	if !last.Done {
		t.Error("last chunk should have Done=true")
	}
	if last.Usage.CompletionTokens != 4 {
		t.Errorf("last chunk Usage.CompletionTokens = %d, want 4", last.Usage.CompletionTokens)
	}
	if last.Usage.PromptTokens != 2 {
		t.Errorf("last chunk Usage.PromptTokens = %d, want 2", last.Usage.PromptTokens)
	}
}

func TestOllamaProvider_ChatStream_ThinkExtraction(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " think.</think>The answer"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " is 42."}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))

	var thinkingChunks []string
	var contentChunks []string
	var gotDone bool

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Think about this"}},
	}, func(resp ChatResponse) error {
		if resp.Thinking != "" {
			thinkingChunks = append(thinkingChunks, resp.Thinking)
		}
		if resp.Content != "" {
			contentChunks = append(contentChunks, resp.Content)
		}
		if resp.Done {
			gotDone = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	if !gotDone {
		t.Error("expected final chunk with Done=true")
	}

	thinking := strings.Join(thinkingChunks, "")
	content := strings.Join(contentChunks, "")

	if thinking != "Let me think." {
		t.Errorf("accumulated thinking = %q, want %q", thinking, "Let me think.")
	}
	if content != "The answer is 42." {
		t.Errorf("accumulated content = %q, want %q", content, "The answer is 42.")
	}
}

func TestOllamaProvider_ChatStream_FragmentedTags(t *testing.T) {
	// The <think> tag is split across two chunks: "<thi" then "nk>..."
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "<thi"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "nk>reasoning</think>content"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))

	var thinkingChunks []string
	var contentChunks []string

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "test"}},
	}, func(resp ChatResponse) error {
		if resp.Thinking != "" {
			thinkingChunks = append(thinkingChunks, resp.Thinking)
		}
		if resp.Content != "" {
			contentChunks = append(contentChunks, resp.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	thinking := strings.Join(thinkingChunks, "")
	content := strings.Join(contentChunks, "")

	if thinking != "reasoning" {
		t.Errorf("thinking = %q, want %q", thinking, "reasoning")
	}
	if content != "content" {
		t.Errorf("content = %q, want %q", content, "content")
	}
}

func TestOllamaProvider_ChatStream_ThinkToggle(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " think.</think>The answer"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " is 42."}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}

	for _, tt := range []struct {
		name         string
		think        bool
		wantThinking string
		wantContent  string
	}{
		{
			name:         "enabled",
			think:        true,
			wantThinking: "Let me think.",
			wantContent:  "The answer is 42.",
		},
		{
			name:         "disabled",
			think:        false,
			wantThinking: "",
			wantContent:  "<think>Let me think.</think>The answer is 42.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, chunk := range chunks {
					data, _ := json.Marshal(chunk)
					_, _ = fmt.Fprintf(w, "%s\n", data)
				}
			}))
			defer srv.Close()

			c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
			p := NewOllamaProvider(c, WithThinkMode(ThinkToggle))

			var thinkingChunks []string
			var contentChunks []string
			err := p.ChatStream(context.Background(), ChatRequest{
				Model:    "qwen3:8b",
				Messages: []ChatMessage{{Role: "user", Content: "What is the meaning of life?"}},
				Options: ModelOptions{
					Think: Ptr(tt.think),
				},
			}, func(resp ChatResponse) error {
				if resp.Thinking != "" {
					thinkingChunks = append(thinkingChunks, resp.Thinking)
				}
				if resp.Content != "" {
					contentChunks = append(contentChunks, resp.Content)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("ChatStream() error: %v", err)
			}

			if got := strings.Join(thinkingChunks, ""); got != tt.wantThinking {
				t.Errorf("thinking = %q, want %q", got, tt.wantThinking)
			}
			if got := strings.Join(contentChunks, ""); got != tt.wantContent {
				t.Errorf("content = %q, want %q", got, tt.wantContent)
			}
		})
	}
}

func TestOllamaProvider_ChatStream_GracefulCancellation(t *testing.T) {
	// Server sends one chunk then blocks until the serverDone channel is closed,
	// simulating a long-running stream that gets cancelled by the client.
	ready := make(chan struct{})
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "partial content"},
			Done:    false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
		// Flush to ensure the client sees the chunk.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Signal that the chunk was sent.
		close(ready)
		// Block until test cleanup signals us to exit.
		<-serverDone
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	ctx, cancel := context.WithCancel(context.Background())

	var received []ChatResponse
	done := make(chan error, 1)
	go func() {
		done <- p.ChatStream(ctx, ChatRequest{
			Model:    "qwen3:8b",
			Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
		}, func(resp ChatResponse) error {
			received = append(received, resp)
			return nil
		})
	}()

	// Wait for the first chunk to be processed, then cancel.
	<-ready
	// Small delay to ensure the chunk is processed by the stream handler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	// Unblock the server handler so httptest.Server.Close() returns promptly.
	close(serverDone)

	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	if !strings.Contains(err.Error(), "provider: ollama:") {
		t.Errorf("error should contain 'provider: ollama:' prefix, got: %v", err)
	}

	// Should have at least the content chunk and the partial result.
	gotPartial := false
	contentDeltas := ""
	for _, r := range received {
		contentDeltas += r.Content
		if r.Partial && r.Done {
			gotPartial = true
			if r.Content != "" {
				t.Errorf("partial Content = %q, want empty delta-preserving partial chunk", r.Content)
			}
			if r.Thinking != "" {
				t.Errorf("partial Thinking = %q, want empty delta-preserving partial chunk", r.Thinking)
			}
		}
	}
	if !gotPartial {
		t.Error("expected a partial result chunk after cancellation")
	}
	if contentDeltas != "partial content" {
		t.Errorf("combined content deltas = %q, want %q", contentDeltas, "partial content")
	}
}

func TestOllamaProvider_ChatStream_CallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "hello"},
			Done:    false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	callbackErr := fmt.Errorf("consumer stopped")
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(_ ChatResponse) error {
		return callbackErr
	})
	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !strings.Contains(err.Error(), "consumer stopped") {
		t.Errorf("expected callback error in chain, got: %v", err)
	}
}

func TestOllamaProvider_ChatStream_NilCallback(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil callback")
	}
	if !strings.Contains(err.Error(), "callback function is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Functional options
// ---------------------------------------------------------------------------

func TestOllamaProvider_WithThinkMode(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))
	if p.thinkMode != ThinkAlways {
		t.Errorf("thinkMode = %v, want ThinkAlways", p.thinkMode)
	}
}

func TestOllamaProvider_WithThinkTags(t *testing.T) {
	customTags := ThinkTags{Open: "<reasoning>", Close: "</reasoning>"}
	c := ollama.NewClient()
	p := NewOllamaProvider(c, WithThinkTags(customTags))
	if p.thinkTags != customTags {
		t.Errorf("thinkTags = %+v, want %+v", p.thinkTags, customTags)
	}
}

func TestOllamaProvider_DefaultThinkMode(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	if p.thinkMode != ThinkAuto {
		t.Errorf("default thinkMode = %v, want ThinkAuto", p.thinkMode)
	}
	if p.thinkTags != DefaultThinkTags() {
		t.Errorf("default thinkTags = %+v, want %+v", p.thinkTags, DefaultThinkTags())
	}
}

// ---------------------------------------------------------------------------
// Type mapping helpers
// ---------------------------------------------------------------------------

func TestToOllamaChatRequest(t *testing.T) {
	req := ChatRequest{
		Model: "qwen3:8b",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
		},
		Options: ModelOptions{
			Temperature: Ptr(0.5),
			NumCtx:      8192,
		},
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "search",
					Description: "Search the web",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}

	oReq := toOllamaChatRequest(req)

	if oReq.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", oReq.Model, "qwen3:8b")
	}
	if len(oReq.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(oReq.Messages))
	}
	if oReq.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want %q", oReq.Messages[0].Role, "system")
	}
	if oReq.Options == nil {
		t.Fatal("Options should not be nil")
	}
	if oReq.Options.Temperature != 0.5 {
		t.Errorf("Options.Temperature = %f, want 0.5", oReq.Options.Temperature)
	}
	if oReq.Options.NumCtx != 8192 {
		t.Errorf("Options.NumCtx = %d, want 8192", oReq.Options.NumCtx)
	}
	if len(oReq.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(oReq.Tools))
	}
	if oReq.Tools[0].Function.Name != "search" {
		t.Errorf("Tools[0].Function.Name = %q, want %q", oReq.Tools[0].Function.Name, "search")
	}
}

func TestToOllamaOptions_NilOnZero(t *testing.T) {
	opts := toOllamaOptions(ModelOptions{})
	if opts != nil {
		t.Errorf("expected nil for zero-value options, got %+v", opts)
	}
}

func TestToProviderToolCalls(t *testing.T) {
	calls := []ollama.ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: ollama.ToolCallFunction{
				Index:     0,
				Name:      "get_weather",
				Arguments: map[string]any{"city": "Tokyo"},
			},
		},
	}

	result := toProviderToolCalls(calls)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	if result[0].ID != "call_1" {
		t.Errorf("ID = %q, want %q", result[0].ID, "call_1")
	}
	if result[0].Function.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", result[0].Function.Name, "get_weather")
	}
	// Arguments should be valid JSON.
	var args map[string]any
	if err := json.Unmarshal(result[0].Function.Arguments, &args); err != nil {
		t.Fatalf("failed to unmarshal arguments: %v", err)
	}
	if args["city"] != "Tokyo" {
		t.Errorf("args[city] = %v, want %q", args["city"], "Tokyo")
	}
}

func TestToProviderToolCalls_Empty(t *testing.T) {
	result := toProviderToolCalls(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestToOllamaToolCalls_RoundTrip(t *testing.T) {
	providerCalls := []ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: ToolCallFunction{
				Index:     0,
				Name:      "search",
				Arguments: json.RawMessage(`{"query":"test"}`),
			},
		},
	}

	ollamaCalls := toOllamaToolCalls(providerCalls)
	if len(ollamaCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(ollamaCalls))
	}
	if ollamaCalls[0].Function.Name != "search" {
		t.Errorf("Name = %q, want %q", ollamaCalls[0].Function.Name, "search")
	}
	if ollamaCalls[0].Function.Arguments["query"] != "test" {
		t.Errorf("Arguments[query] = %v, want %q", ollamaCalls[0].Function.Arguments["query"], "test")
	}

	// Round-trip back to provider.
	back := toProviderToolCalls(ollamaCalls)
	if len(back) != 1 {
		t.Fatalf("round-trip: expected 1 call, got %d", len(back))
	}
	if back[0].Function.Name != "search" {
		t.Errorf("round-trip: Name = %q, want %q", back[0].Function.Name, "search")
	}
}

// ---------------------------------------------------------------------------
// Generate (non-streaming)
// ---------------------------------------------------------------------------

func TestOllamaProvider_Generate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming generate")
		}
		if req.Model != "qwen3:8b" {
			t.Errorf("model = %q, want %q", req.Model, "qwen3:8b")
		}
		if req.Prompt != "Complete this:" {
			t.Errorf("prompt = %q, want %q", req.Prompt, "Complete this:")
		}

		resp := ollama.GenerateResponse{
			Model:         "qwen3:8b",
			Response:      "Generated text here.",
			Done:          true,
			EvalCount:     10,
			EvalDuration:  5000000,
			TotalDuration: 6000000,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Generate(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Complete this:",
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Response != "Generated text here." {
		t.Errorf("Response = %q, want %q", resp.Response, "Generated text here.")
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "ollama")
	}
	if resp.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", resp.Model, "qwen3:8b")
	}
	if !resp.Done {
		t.Error("expected Done=true")
	}
	if resp.Usage.CompletionTokens != 10 {
		t.Errorf("Usage.CompletionTokens = %d, want 10", resp.Usage.CompletionTokens)
	}
	if resp.Latency.GenerationDuration != time.Duration(5000000) {
		t.Errorf("Latency.GenerationDuration = %v, want 5ms", resp.Latency.GenerationDuration)
	}
}

func TestOllamaProvider_Generate_WithOptions(t *testing.T) {
	var receivedOpts ollama.ModelOptions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Options != nil {
			receivedOpts = *req.Options
		}
		if req.System != "You are a code assistant." {
			t.Errorf("System = %q, want %q", req.System, "You are a code assistant.")
		}
		if req.Suffix != "// end" {
			t.Errorf("Suffix = %q, want %q", req.Suffix, "// end")
		}
		resp := ollama.GenerateResponse{
			Model:    "qwen3:8b",
			Response: "ok",
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	_, err := p.Generate(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "function add(",
		System: "You are a code assistant.",
		Suffix: "// end",
		Options: ModelOptions{
			Temperature: Ptr(0.3),
			TopK:        Ptr(0),
			NumPredict:  50,
		},
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if receivedOpts.Temperature != 0.3 {
		t.Errorf("Temperature = %f, want 0.3", receivedOpts.Temperature)
	}
	if receivedOpts.TopK == nil || *receivedOpts.TopK != 0 {
		t.Errorf("TopK = %v, want explicit zero", receivedOpts.TopK)
	}
	if receivedOpts.NumPredict != 50 {
		t.Errorf("NumPredict = %d, want 50", receivedOpts.NumPredict)
	}
}

// ---------------------------------------------------------------------------
// GenerateStream
// ---------------------------------------------------------------------------

func TestOllamaProvider_GenerateStream(t *testing.T) {
	chunks := []ollama.GenerateResponse{
		{Model: "qwen3:8b", Response: "Hello ", Done: false},
		{Model: "qwen3:8b", Response: "world!", Done: false},
		{Model: "qwen3:8b", Response: "", Done: true, EvalCount: 4, EvalDuration: 2000000},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	var received []GenerateResponse
	err := p.GenerateStream(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Complete:",
	}, func(resp GenerateResponse) error {
		received = append(received, resp)
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	if len(received) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(received))
	}
	if received[0].Response != "Hello " {
		t.Errorf("chunk[0].Response = %q, want %q", received[0].Response, "Hello ")
	}
	if received[0].Provider != "ollama" {
		t.Errorf("chunk[0].Provider = %q, want %q", received[0].Provider, "ollama")
	}
	if received[1].Response != "world!" {
		t.Errorf("chunk[1].Response = %q, want %q", received[1].Response, "world!")
	}
	if !received[2].Done {
		t.Error("last chunk should have Done=true")
	}
	if received[2].Usage.CompletionTokens != 4 {
		t.Errorf("last chunk Usage.CompletionTokens = %d, want 4", received[2].Usage.CompletionTokens)
	}
}

func TestOllamaProvider_GenerateStream_GracefulCancellation(t *testing.T) {
	ready := make(chan struct{})
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.GenerateResponse{
			Model:    "qwen3:8b",
			Response: "partial output",
			Done:     false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(ready)
		<-serverDone
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	ctx, cancel := context.WithCancel(context.Background())

	var received []GenerateResponse
	done := make(chan error, 1)
	go func() {
		done <- p.GenerateStream(ctx, GenerateRequest{
			Model:  "qwen3:8b",
			Prompt: "Generate something long",
		}, func(resp GenerateResponse) error {
			received = append(received, resp)
			return nil
		})
	}()

	<-ready
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	close(serverDone)

	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	if !strings.Contains(err.Error(), "provider: ollama:") {
		t.Errorf("error should contain 'provider: ollama:' prefix, got: %v", err)
	}

	gotPartial := false
	responseDeltas := ""
	for _, r := range received {
		responseDeltas += r.Response
		if r.Partial && r.Done {
			gotPartial = true
			if r.Response != "" {
				t.Errorf("partial Response = %q, want empty delta-preserving partial chunk", r.Response)
			}
		}
	}
	if !gotPartial {
		t.Error("expected a partial result chunk after cancellation")
	}
	if responseDeltas != "partial output" {
		t.Errorf("combined response deltas = %q, want %q", responseDeltas, "partial output")
	}
}

func TestOllamaProvider_GenerateStream_NilCallback(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)

	err := p.GenerateStream(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Hi",
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil callback")
	}
	if !strings.Contains(err.Error(), "callback function is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestOllamaProvider_GenerateStream_CallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.GenerateResponse{
			Model:    "qwen3:8b",
			Response: "hello",
			Done:     false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	callbackErr := fmt.Errorf("consumer stopped")
	err := p.GenerateStream(context.Background(), GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Hi",
	}, func(_ GenerateResponse) error {
		return callbackErr
	})
	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !strings.Contains(err.Error(), "consumer stopped") {
		t.Errorf("expected callback error in chain, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Embed
// ---------------------------------------------------------------------------

func TestOllamaProvider_Embed(t *testing.T) {
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&callCount, 1)
		resp := ollama.EmbedResponse{
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Embed(context.Background(), EmbedRequest{
		Model: "nomic-embed-text",
		Input: []string{"hello world", "goodbye world"},
	})
	if err != nil {
		t.Fatalf("Embed() error: %v", err)
	}
	if len(resp.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(resp.Embeddings))
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "ollama")
	}
	if resp.Model != "nomic-embed-text" {
		t.Errorf("Model = %q, want %q", resp.Model, "nomic-embed-text")
	}
	if resp.Usage.PromptTokens != 2 {
		t.Errorf("Usage.PromptTokens = %d, want 2", resp.Usage.PromptTokens)
	}
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Errorf("expected 2 API calls (one per input), got %d", got)
	}
	// Verify embedding values.
	for i, emb := range resp.Embeddings {
		if len(emb) != 3 {
			t.Errorf("embedding[%d] length = %d, want 3", i, len(emb))
		}
	}
}

func TestOllamaProvider_Embed_Validation(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)

	tests := []struct {
		name    string
		req     EmbedRequest
		wantErr string
	}{
		{
			name:    "empty model",
			req:     EmbedRequest{Input: []string{"hello"}},
			wantErr: "model name is required",
		},
		{
			name:    "empty input",
			req:     EmbedRequest{Model: "nomic-embed-text"},
			wantErr: "at least one input text is required",
		},
		{
			name:    "nil input",
			req:     EmbedRequest{Model: "nomic-embed-text", Input: nil},
			wantErr: "at least one input text is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.Embed(context.Background(), tt.req)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestOllamaProvider_Embed_Singleflight(t *testing.T) {
	// Track how many times the server is actually called. We use an atomic
	// counter because concurrent goroutines will hit the handler in parallel.
	var serverCalls int32

	// Add a small delay in the handler so concurrent requests overlap.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		time.Sleep(50 * time.Millisecond)
		resp := ollama.EmbedResponse{
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	req := EmbedRequest{
		Model: "nomic-embed-text",
		Input: []string{"identical text"},
	}

	const concurrency = 5
	var wg sync.WaitGroup
	errs := make([]error, concurrency)
	results := make([]*EmbedResponse, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			resp, err := p.Embed(context.Background(), req)
			errs[idx] = err
			results[idx] = resp
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Embed() error: %v", i, err)
		}
	}

	// With singleflight, only 1 request's worth of embed calls should hit
	// the server (1 input text = 1 server call), not 5.
	got := atomic.LoadInt32(&serverCalls)
	if got != 1 {
		t.Errorf("expected 1 server call (singleflight dedup), got %d", got)
	}

	// All results should be valid and identical (shared via singleflight).
	for i, resp := range results {
		if resp == nil {
			t.Errorf("goroutine %d: nil response", i)
			continue
		}
		if len(resp.Embeddings) != 1 {
			t.Errorf("goroutine %d: expected 1 embedding, got %d", i, len(resp.Embeddings))
		}
		if resp.Provider != "ollama" {
			t.Errorf("goroutine %d: Provider = %q, want %q", i, resp.Provider, "ollama")
		}
	}
}

func TestOllamaProvider_Embed_SingleflightLeaderCancellationDoesNotCancelFollowers(t *testing.T) {
	var serverCalls int32
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		select {
		case <-started:
		default:
			close(started)
		}
		time.Sleep(100 * time.Millisecond)
		resp := ollama.EmbedResponse{
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)
	req := EmbedRequest{
		Model: "nomic-embed-text",
		Input: []string{"identical text"},
	}

	leaderCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	leaderDone := make(chan error, 1)
	go func() {
		_, err := p.Embed(leaderCtx, req)
		leaderDone <- err
	}()

	<-started

	followerDone := make(chan struct {
		resp *EmbedResponse
		err  error
	}, 1)
	go func() {
		resp, err := p.Embed(context.Background(), req)
		followerDone <- struct {
			resp *EmbedResponse
			err  error
		}{resp: resp, err: err}
	}()

	if err := <-leaderDone; err == nil {
		t.Fatal("expected leader request to be canceled")
	}

	follower := <-followerDone
	if follower.err != nil {
		t.Fatalf("follower Embed() error: %v", follower.err)
	}
	if follower.resp == nil || len(follower.resp.Embeddings) != 1 {
		t.Fatalf("follower response = %#v, want one embedding", follower.resp)
	}
	if got := atomic.LoadInt32(&serverCalls); got != 1 {
		t.Errorf("expected 1 server call, got %d", got)
	}
}

func TestOllamaProvider_Embed_DifferentRequestsNotDeduplicated(t *testing.T) {
	var serverCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		resp := ollama.EmbedResponse{
			Embeddings: [][]float64{{0.1, 0.2, 0.3}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	// Two sequential requests with different inputs should both hit the server.
	_, err := p.Embed(context.Background(), EmbedRequest{
		Model: "nomic-embed-text",
		Input: []string{"first text"},
	})
	if err != nil {
		t.Fatalf("first Embed() error: %v", err)
	}

	_, err = p.Embed(context.Background(), EmbedRequest{
		Model: "nomic-embed-text",
		Input: []string{"second text"},
	})
	if err != nil {
		t.Fatalf("second Embed() error: %v", err)
	}

	got := atomic.LoadInt32(&serverCalls)
	if got != 2 {
		t.Errorf("expected 2 server calls for different inputs, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// toOllamaGenerateRequest helper
// ---------------------------------------------------------------------------

func TestToOllamaGenerateRequest(t *testing.T) {
	req := GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Complete this code:",
		System: "You are a code assistant.",
		Suffix: "// end of function",
		Options: ModelOptions{
			Temperature: Ptr(0.3),
			NumPredict:  200,
			NumCtx:      4096,
		},
	}

	oReq := toOllamaGenerateRequest(req)

	if oReq.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", oReq.Model, "qwen3:8b")
	}
	if oReq.Prompt != "Complete this code:" {
		t.Errorf("Prompt = %q, want %q", oReq.Prompt, "Complete this code:")
	}
	if oReq.System != "You are a code assistant." {
		t.Errorf("System = %q, want %q", oReq.System, "You are a code assistant.")
	}
	if oReq.Suffix != "// end of function" {
		t.Errorf("Suffix = %q, want %q", oReq.Suffix, "// end of function")
	}
	if oReq.Options == nil {
		t.Fatal("Options should not be nil")
	}
	if oReq.Options.Temperature != 0.3 {
		t.Errorf("Options.Temperature = %f, want 0.3", oReq.Options.Temperature)
	}
	if oReq.Options.NumPredict != 200 {
		t.Errorf("Options.NumPredict = %d, want 200", oReq.Options.NumPredict)
	}
	if oReq.Options.NumCtx != 4096 {
		t.Errorf("Options.NumCtx = %d, want 4096", oReq.Options.NumCtx)
	}
}

func TestToOllamaGenerateRequest_NoOptions(t *testing.T) {
	req := GenerateRequest{
		Model:  "qwen3:8b",
		Prompt: "Hello",
	}

	oReq := toOllamaGenerateRequest(req)

	if oReq.Options != nil {
		t.Errorf("expected nil Options for zero-value ModelOptions, got %+v", oReq.Options)
	}
}

func TestOllamaProvider_PullModel(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// Non-streaming pull (fn == nil) expects a single JSON object.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))

	// Compile-time: OllamaProvider must satisfy ModelPuller.
	var _ ModelPuller = p

	if err := p.PullModel(context.Background(), "qwen3:8b", nil); err != nil {
		t.Fatalf("PullModel() error = %v", err)
	}
	if gotPath != "/api/pull" {
		t.Errorf("request path = %q, want /api/pull", gotPath)
	}
}
