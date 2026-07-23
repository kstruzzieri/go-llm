package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestNewCandidateTransport_OllamaDefault(t *testing.T) {
	tr, err := newCandidateTransport(ModelTarget{Display: "m", Provider: "ollama", Model: "m"}, candidateTransportOptions{
		ollamaURL: "http://localhost:11434",
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}
	if tr.providerName != defaultBenchProvider {
		t.Fatalf("providerName = %q; want %q", tr.providerName, defaultBenchProvider)
	}
	if _, ok := tr.chat.(ollamaCandidateClient); !ok {
		t.Fatalf("chat = %T; want ollamaCandidateClient", tr.chat)
	}
}

// The Ollama candidate transport must honor candidateTransportOptions.timeout
// the same way the openai-compat branch does. The Ollama client defaults to a
// 5-minute HTTP timeout, so a dropped opts.timeout silently caps every Ollama
// candidate at 5 minutes regardless of -timeout. That bit gemma4:31b on a long
// RAG-baked replay prefill: the request exceeded the default header timeout and
// the cell was dropped even though -timeout was 20m. See real-workflow shakedown.
func TestOllamaCandidateClient_HonorsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Sleep well past opts.timeout before writing any headers. With the
		// timeout honored, the client aborts the request; with it dropped, the
		// client waits out the default 5-minute window and the request succeeds.
		time.Sleep(1 * time.Second)
		_, _ = w.Write([]byte(`{"model":"served","message":{"role":"assistant","content":"late"},"done":true}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "ollama/served", Provider: defaultBenchProvider, Model: "served"}, candidateTransportOptions{
		ollamaURL: srv.URL,
		timeout:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	start := time.Now()
	_, err = tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "served",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Chat returned nil error after %v; want a timeout error (opts.timeout was ignored)", elapsed)
	}
	// Assert it was the HTTP client timeout that fired, not some unrelated
	// error that happens to be non-nil — otherwise the test could pass for the
	// wrong reason and stop guarding the timeout wiring.
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("Chat error = %v; want an HTTP client timeout (deadline exceeded / Client.Timeout)", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Chat took %v; want it aborted near the 50ms opts.timeout (timeout not honored)", elapsed)
	}
}

func TestNewCandidateTransport_OpenAICompatRequiresBaseURL(t *testing.T) {
	_, err := newCandidateTransport(ModelTarget{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}, candidateTransportOptions{})
	if err == nil || !strings.Contains(err.Error(), "-candidate-base-url") {
		t.Fatalf("err = %v; want missing -candidate-base-url error", err)
	}
}

func TestNewCandidateTransport_UnknownProviderRejected(t *testing.T) {
	_, err := newCandidateTransport(ModelTarget{Display: "x/m", Provider: "x", Model: "m"}, candidateTransportOptions{})
	if !errors.Is(err, errUnsupportedProv) {
		t.Fatalf("err = %v; want errUnsupportedProv", err)
	}
}

func TestOpenAICompatCandidateClient_ChatTranslatesReplayRequestAndResponse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q; want /v1/chat/completions", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served-model",
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"content":"answer needle",
					"tool_calls":[{
						"id":"call_1",
						"type":"function",
						"function":{"name":"read_file","arguments":"{\"path\":\"provider/router.go\"}"}
					}]
				},
				"finish_reason":"tool_calls"
			}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
		openAICompatAPIKey:  "secret",
		timeout:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model: "fake",
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "tool", ToolName: "read_file", ToolCallID: "call_1", Content: "package provider"},
		},
		Tools: []ollama.Tool{{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "read_file",
				Description: "Read a file",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q; want Bearer secret", gotAuth)
	}
	if gotBody["model"] != "fake" {
		t.Fatalf("model = %v; want fake", gotBody["model"])
	}
	if _, ok := gotBody["keep_alive"]; ok {
		t.Fatalf("openai-compat request leaked Ollama keep_alive: %#v", gotBody)
	}
	if resp.Message.Content != "answer needle" {
		t.Fatalf("content = %q; want answer needle", resp.Message.Content)
	}
	if len(resp.Message.ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d; want 1", len(resp.Message.ToolCalls))
	}
	if resp.Message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool name = %q; want read_file", resp.Message.ToolCalls[0].Function.Name)
	}
	if got := resp.Message.ToolCalls[0].Function.Arguments["path"]; got != "provider/router.go" {
		t.Fatalf("tool arg path = %v; want provider/router.go", got)
	}
	if resp.PromptEvalCount != 11 || resp.EvalCount != 7 {
		t.Fatalf("usage = prompt %d gen %d; want 11/7", resp.PromptEvalCount, resp.EvalCount)
	}
	if resp.ThinkingTokensComputed {
		t.Fatalf("ThinkingTokensComputed = true; want false until provider exposes dedicated reasoning token counts")
	}
}

// When the server reports usage.completion_tokens_details.reasoning_tokens,
// the candidate transport must surface it as ThinkingTokens with
// ThinkingTokensComputed = true, closing the gap with the Ollama path (which
// folds reasoning into eval_count). See #158.
func TestOpenAICompatCandidateClient_ChatSurfacesReasoningTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"final answer"},
				"finish_reason":"stop"
			}],
			"usage":{
				"prompt_tokens":11,
				"completion_tokens":20,
				"total_tokens":31,
				"completion_tokens_details":{"reasoning_tokens":13}
			}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "fake",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.ThinkingTokens != 13 {
		t.Errorf("ThinkingTokens = %d; want 13 (mapped from completion_tokens_details.reasoning_tokens)", resp.ThinkingTokens)
	}
	if !resp.ThinkingTokensComputed {
		t.Error("ThinkingTokensComputed = false; want true when the server reports reasoning tokens")
	}
}

// A present-but-zero reasoning count is still a measured value. The benchmark
// report uses ThinkingTokensComputed to distinguish "measured zero reasoning"
// from "provider does not isolate reasoning", so a zero count from an explicit
// completion_tokens_details block must stay computed.
func TestOpenAICompatCandidateClient_ChatZeroReasoningTokensAreComputed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"final answer"},
				"finish_reason":"stop"
			}],
			"usage":{
				"prompt_tokens":11,
				"completion_tokens":7,
				"total_tokens":18,
				"completion_tokens_details":{"reasoning_tokens":0}
			}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "fake",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.ThinkingTokens != 0 {
		t.Errorf("ThinkingTokens = %d; want 0", resp.ThinkingTokens)
	}
	if !resp.ThinkingTokensComputed {
		t.Error("ThinkingTokensComputed = false; want true for a reported zero reasoning count")
	}
}

// When the server separates reasoning into the structured reasoning_content
// field (rather than inline <think> tags), the candidate must capture that text
// as candidateChatResponse.Thinking while keeping it OUT of Message — the
// reply Message feeds back into conversation history and must stay free of
// reasoning so replay determinism is preserved. See #160.
func TestOpenAICompatCandidateClient_CapturesReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"final answer","reasoning_content":"native chain of thought"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "fake",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Thinking != "native chain of thought" {
		t.Errorf("Thinking = %q, want native reasoning_content captured", resp.Thinking)
	}
	if resp.Message.Content != "final answer" {
		t.Errorf("Message.Content = %q, want %q", resp.Message.Content, "final answer")
	}
	if resp.Message.Thinking != "" {
		t.Errorf("Message.Thinking = %q, want empty (reasoning is carried separately, not in the history message)", resp.Message.Thinking)
	}
}

// The Ollama candidate uses the raw client, which now deserializes the separate
// message.thinking field. The transport must lift that text into
// candidateChatResponse.Thinking and clear it from Message so conversation
// history stays free of reasoning. See #160.
func TestOllamaCandidateClient_CapturesNativeThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"model":"served",
			"message":{"role":"assistant","content":"final answer","thinking":"native reasoning"},
			"done":true,
			"prompt_eval_count":5,
			"eval_count":3
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "ollama/served", Provider: defaultBenchProvider, Model: "served"}, candidateTransportOptions{
		ollamaURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "served",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Thinking != "native reasoning" {
		t.Errorf("Thinking = %q, want native message.thinking captured", resp.Thinking)
	}
	if resp.Message.Content != "final answer" {
		t.Errorf("Message.Content = %q, want %q", resp.Message.Content, "final answer")
	}
	if resp.Message.Thinking != "" {
		t.Errorf("Message.Thinking = %q, want it cleared so conversation history stays free of reasoning text", resp.Message.Thinking)
	}
}

// A reasoning model served via llama.cpp can emit <think>...</think> inline in
// content. The candidate transport must strip it so the scored transcript holds
// the final answer, not serving-stack-specific reasoning formatting.
func TestOpenAICompatCandidateClient_StripsInlineThinkTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"<think>secret chain of thought</think>final answer"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`))
	}))
	defer srv.Close()

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	resp, err := tr.chat.Chat(context.Background(), ollama.ChatRequest{
		Model:    "fake",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "final answer" {
		t.Fatalf("content = %q; want %q (inline <think> reasoning must be stripped)", resp.Message.Content, "final answer")
	}
	// The reasoning stripped from Content is now captured (it used to be
	// discarded), so a backend that inlines <think> tags reaches transcript
	// parity with one that uses a structured reasoning field. See #160.
	if resp.Thinking != "secret chain of thought" {
		t.Errorf("Thinking = %q; want the stripped reasoning captured rather than dropped", resp.Thinking)
	}
}
