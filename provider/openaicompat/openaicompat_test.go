package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// ---------------------------------------------------------------------------
// Construction, Name, Capabilities, options
// ---------------------------------------------------------------------------

func TestProvider_Name_Default(t *testing.T) {
	p := NewProvider(NewClient("http://localhost:8080"))
	if got := p.Name(); got != "openai-compat" {
		t.Errorf("Name() = %q, want %q", got, "openai-compat")
	}
}

func TestProvider_WithProviderName(t *testing.T) {
	t.Run("override applied", func(t *testing.T) {
		p := NewProvider(NewClient("http://x"), WithProviderName("vllm-workstation"))
		if got := p.Name(); got != "vllm-workstation" {
			t.Errorf("Name() = %q, want %q", got, "vllm-workstation")
		}
	})
	t.Run("empty name panics", func(t *testing.T) {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected panic on empty name, got nil")
			}
			msg, ok := r.(string)
			if !ok {
				t.Fatalf("expected string panic value, got %T", r)
			}
			if !strings.Contains(msg, "WithProviderName") || !strings.Contains(msg, "empty") {
				t.Errorf("panic message should explain misuse, got: %q", msg)
			}
		}()
		_ = WithProviderName("")
	})
}

func TestProvider_Capabilities_DefaultExcludesInsert(t *testing.T) {
	// CapInsert is intentionally NOT advertised by default because
	// OpenAI-compat servers don't expose a probe equivalent to Ollama's
	// template inspection. Users must opt in via WithCapabilities when
	// the target server is known to support native FIM.
	p := NewProvider(NewClient("http://x"))
	caps := p.Capabilities()
	for _, want := range []provider.Capability{
		provider.CapChat, provider.CapGenerate, provider.CapStream,
		provider.CapEmbed, provider.CapToolCall,
	} {
		if !caps.Has(want) {
			t.Errorf("default Capabilities missing %v, got %v", want, caps)
		}
	}
	if caps.Has(provider.CapInsert) {
		t.Errorf("default Capabilities should NOT include CapInsert, got %v", caps)
	}
}

func TestProvider_WithCapabilities_Override(t *testing.T) {
	p := NewProvider(NewClient("http://x"), WithCapabilities(provider.CapChat|provider.CapInsert))
	if !p.Capabilities().Has(provider.CapInsert) {
		t.Error("WithCapabilities did not propagate CapInsert")
	}
	if p.Capabilities().Has(provider.CapEmbed) {
		t.Error("WithCapabilities should replace, not merge — default CapEmbed leaked")
	}
}

// ---------------------------------------------------------------------------
// Client: baseURL hygiene + auth header + error envelope
// ---------------------------------------------------------------------------

func TestClient_BaseURL_TrimsTrailingSlash(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{})
	defer srv.close()

	// With trailing slash on baseURL the request URL must NOT be
	// "//v1/models" (double slash); strip-once at construction time.
	c := NewClient(srv.url + "/")
	p := NewProvider(c)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health failed with trailing-slash baseURL: %v", err)
	}
	if got, want := srv.lastPath.Load().(string), "/v1/models"; got != want {
		t.Errorf("request path = %q, want %q", got, want)
	}
}

func TestClient_BearerAuth_HeaderSet(t *testing.T) {
	const token = "sk-test-token"
	srv := newMockServer(t, mockServerOpts{})
	defer srv.close()

	c := NewClient(srv.url, WithAPIKey(token))
	p := NewProvider(c)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := srv.lastAuth.Load().(string); got != "Bearer "+token {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer "+token)
	}
}

func TestClient_BearerAuth_OmittedWhenEmpty(t *testing.T) {
	// Local servers without auth (vanilla llama.cpp --api) shouldn't see
	// a Bearer header at all; sending one could trip overly-strict
	// reverse proxies.
	srv := newMockServer(t, mockServerOpts{})
	defer srv.close()

	c := NewClient(srv.url) // no APIKey
	p := NewProvider(c)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got := srv.lastAuth.Load().(string); got != "" {
		t.Errorf("Authorization header should be absent, got %q", got)
	}
}

func TestClient_ErrorEnvelope_Unwrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(errorEnvelope{
			Error: errorPayload{Message: "model not found", Type: "invalid_request_error"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	p := NewProvider(c)
	_, err := p.Chat(context.Background(), provider.ChatRequest{Model: "missing"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "model not found") {
		t.Errorf("error should surface the OpenAI message, got: %v", err)
	}
}

func TestClient_NonJSONError_FallsBackToBody(t *testing.T) {
	// HTML error pages (proxy / load balancer) must still produce a
	// useful error message rather than swallowing the failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>502 Bad Gateway</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	p := NewProvider(c)
	_, err := p.Chat(context.Background(), provider.ChatRequest{Model: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should mention status, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Health + Models
// ---------------------------------------------------------------------------

func TestProvider_Health_Success(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{})
	defer srv.close()

	p := NewProvider(NewClient(srv.url))
	if err := p.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}
}

func TestProvider_Models_ListsServerIDs(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{
		models: []modelEntry{
			{ID: "qwen3:8b"},
			{ID: "gpt-oss-7b"},
		},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url))
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].Name != "qwen3:8b" || models[1].Name != "gpt-oss-7b" {
		t.Errorf("Models = %+v, want IDs qwen3:8b, gpt-oss-7b", models)
	}
}

// ---------------------------------------------------------------------------
// Chat (non-streaming)
// ---------------------------------------------------------------------------

func TestProvider_Chat_Success(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{
		chatResponse: chatResponse{
			ID:    "chatcmpl-1",
			Model: "qwen3:8b",
			Choices: []chatChoice{{
				Index: 0,
				Message: chatMessage{
					Role:    "assistant",
					Content: "hello world",
				},
				FinishReason: "stop",
			}},
			Usage: usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
		},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url), WithProviderName("vllm-test"))
	resp, err := p.Chat(context.Background(), provider.ChatRequest{
		Model:    "qwen3:8b",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello world" {
		t.Errorf("Content = %q, want %q", resp.Content, "hello world")
	}
	if resp.Provider != "vllm-test" {
		t.Errorf("Provider = %q, want %q (instance name must be stamped on response)", resp.Provider, "vllm-test")
	}
	if resp.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want qwen3:8b", resp.Model)
	}
	if resp.Usage.TotalTokens != 6 {
		t.Errorf("Usage.TotalTokens = %d, want 6", resp.Usage.TotalTokens)
	}
	if !resp.Done {
		t.Error("Done should be true on non-streaming response")
	}
}

func TestProvider_Chat_ExtractsInlineThinking(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{
		chatResponse: chatResponse{
			Model: "deepseek-r1",
			Choices: []chatChoice{{
				Message: chatMessage{Role: "assistant", Content: "<think>let me think</think>final answer"},
			}},
		},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url))
	resp, err := p.Chat(context.Background(), provider.ChatRequest{Model: "deepseek-r1"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "final answer" {
		t.Errorf("Content = %q, want %q (think tags must be stripped)", resp.Content, "final answer")
	}
	if !strings.Contains(resp.Thinking, "let me think") {
		t.Errorf("Thinking = %q, want it to contain extracted reasoning", resp.Thinking)
	}
}

func TestProvider_Chat_ThinkNone_KeepsTagsInline(t *testing.T) {
	// When thinking is disabled at construction, raw content (including
	// tags) must pass through unchanged — the caller has opted out of
	// the parse pass and expects byte-for-byte fidelity.
	srv := newMockServer(t, mockServerOpts{
		chatResponse: chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Content: "<think>x</think>y"}}},
		},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url), WithThinkMode(provider.ThinkNone))
	resp, err := p.Chat(context.Background(), provider.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "<think>x</think>y" {
		t.Errorf("Content = %q, want raw bytes preserved with ThinkNone", resp.Content)
	}
	if resp.Thinking != "" {
		t.Errorf("Thinking = %q, want empty with ThinkNone", resp.Thinking)
	}
}

func TestProvider_Chat_NoChoices_ReturnsError(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{
		chatResponse: chatResponse{ID: "empty", Choices: nil},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url))
	_, err := p.Chat(context.Background(), provider.ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' error, got: %v", err)
	}
}

func TestProvider_Chat_RequestBodyShape(t *testing.T) {
	// Confirm tool definitions, ModelOptions, and messages are translated
	// to the OpenAI wire shape — this is the contract that lets stock
	// OpenAI SDKs respond.
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Chat(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
		Options: provider.ModelOptions{
			Temperature: provider.Ptr(0.7),
			NumPredict:  100,
			Stop:        []string{"\n\n"},
		},
		Tools: []provider.Tool{{
			Type: "function",
			Function: provider.ToolFunction{
				Name: "search", Description: "search the web",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.Model != "m" || len(captured.Messages) != 1 {
		t.Errorf("model/messages not propagated: %+v", captured)
	}
	if captured.Temperature == nil || *captured.Temperature != 0.7 {
		t.Errorf("Temperature not propagated: %+v", captured.Temperature)
	}
	if captured.MaxTokens != 100 {
		t.Errorf("MaxTokens = %d, want 100 (NumPredict -> max_tokens)", captured.MaxTokens)
	}
	if len(captured.Stop) != 1 || captured.Stop[0] != "\n\n" {
		t.Errorf("Stop = %v, want [\\n\\n]", captured.Stop)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "search" {
		t.Errorf("Tools not propagated: %+v", captured.Tools)
	}
}

// TestProvider_Chat_RequestBody_OmitsToolCallIndex verifies that the
// outbound wire does NOT carry an `index` field on tool_calls entries.
// OpenAI treats index as response-only — strict server-side schema
// validators (some vLLM and llama.cpp builds) reject requests that
// include it. The omission is enforced by toWireToolCalls deliberately
// not copying Function.Index and by chatToolCall.Index using
// `*int` + omitempty. A regression that starts sending `"index":0` on
// every request would break those servers silently; this test pins
// the contract.
func TestProvider_Chat_RequestBody_OmitsToolCallIndex(t *testing.T) {
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Chat(context.Background(), provider.ChatRequest{
		Model: "m",
		Messages: []provider.ChatMessage{{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: provider.ToolCallFunction{
					Index:     3, // non-zero on purpose — caller might have it set
					Name:      "search",
					Arguments: json.RawMessage(`{"q":"go"}`),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(string(rawBody), `"index"`) {
		t.Errorf("outbound body must not include tool_call index field, body was: %s", string(rawBody))
	}
}

func TestProvider_Chat_RequestBody_ToolCallArgumentsEncodedAsString(t *testing.T) {
	var captured chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []chatChoice{{Message: chatMessage{Content: "ok"}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Chat(context.Background(), provider.ChatRequest{
		Model: "m",
		Messages: []provider.ChatMessage{{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      "search",
					Arguments: json.RawMessage(`{"q":"go"}`),
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(captured.Messages) != 1 || len(captured.Messages[0].ToolCalls) != 1 {
		t.Fatalf("captured tool calls = %+v", captured.Messages)
	}
	var args string
	if err := json.Unmarshal(captured.Messages[0].ToolCalls[0].Function.Arguments, &args); err != nil {
		t.Fatalf("arguments should be an OpenAI-style JSON string, got %s: %v", captured.Messages[0].ToolCalls[0].Function.Arguments, err)
	}
	if args != `{"q":"go"}` {
		t.Errorf("arguments string = %q, want raw object JSON", args)
	}
}

// TestNormalizeToolCallArguments_PreservesScalarLiterals locks the rule
// that scalar JSON literals wrapped in OpenAI's JSON-string envelope are
// preserved as strings rather than narrowed to bare scalars. Tool
// implementations declare argument shapes via JSON Schema — a string-typed
// parameter accidentally arriving as a JSON number would break runtime
// type checks downstream.
func TestNormalizeToolCallArguments_PreservesScalarLiterals(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Object / array unwrap to canonical tool-call shape.
		{"json object string -> raw object", `"{\"q\":\"go\"}"`, `{"q":"go"}`},
		{"json array string -> raw array", `"[1,2,3]"`, `[1,2,3]`},
		// Scalar literals stay wrapped — the inner value WAS a string and
		// downstream consumers must keep it that way.
		{"numeric literal stays wrapped", `"42"`, `"42"`},
		{"bool literal stays wrapped", `"true"`, `"true"`},
		{"null literal stays wrapped", `"null"`, `"null"`},
		{"non-json string stays wrapped", `"hello"`, `"hello"`},
		// Empty / nil / null collapse.
		{"empty input -> nil", ``, ``},
		{"null literal -> nil", `null`, ``},
		{"empty string envelope -> nil", `""`, ``},
		// Raw inputs (no envelope) pass through.
		{"raw object passes through", `{"q":"go"}`, `{"q":"go"}`},
		{"raw array passes through", `[1,2]`, `[1,2]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeToolCallArguments(json.RawMessage(tt.in))
			if string(got) != tt.want {
				t.Errorf("normalize(%q) = %q, want %q", tt.in, string(got), tt.want)
			}
		})
	}
}

func TestProvider_Chat_ToolCallArgumentsStringDecoded(t *testing.T) {
	srv := newMockServer(t, mockServerOpts{
		chatResponse: chatResponse{
			Choices: []chatChoice{{Message: chatMessage{
				ToolCalls: []chatToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: chatToolCallFunction{
						Name:      "search",
						Arguments: rawJSONString(t, `{"q":"go"}`),
					},
				}},
			}}},
		},
	})
	defer srv.close()

	p := NewProvider(NewClient(srv.url))
	resp, err := p.Chat(context.Background(), provider.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(resp.ToolCalls))
	}
	if got := string(resp.ToolCalls[0].Function.Arguments); got != `{"q":"go"}` {
		t.Errorf("Arguments = %s, want decoded JSON object", got)
	}
	if resp.ToolCalls[0].Function.Index != 0 {
		t.Errorf("Index = %d, want 0", resp.ToolCalls[0].Function.Index)
	}
}

// ---------------------------------------------------------------------------
// ChatStream
// ---------------------------------------------------------------------------

func TestProvider_ChatStream_DeliversChunksThenDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "hello"}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: " world"}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("stop"),
			}}, Usage: &usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
		})
	}))
	defer srv.Close()

	// ThinkNone disables the think-parser's lookahead buffering so each
	// content delta surfaces as a discrete chunk — the chunk-cadence
	// contract is what this test asserts. The think-extraction path has
	// its own coverage in TestProvider_Chat_ExtractsInlineThinking.
	p := NewProvider(NewClient(srv.URL),
		WithProviderName("inst-1"),
		WithThinkMode(provider.ThinkNone),
	)
	var got []provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	// 2 content chunks (one per delta) + 1 done.
	if len(got) < 3 {
		t.Fatalf("got %d chunks, want >= 3: %+v", len(got), got)
	}
	last := got[len(got)-1]
	if !last.Done {
		t.Errorf("last chunk should be Done, got %+v", last)
	}
	if last.Usage.TotalTokens != 3 {
		t.Errorf("last.Usage = %+v, want TotalTokens=3", last.Usage)
	}
	for _, chunk := range got {
		if chunk.Provider != "inst-1" {
			t.Errorf("chunk.Provider = %q, want inst-1 (instance name stamp must hold across stream)", chunk.Provider)
		}
	}
}

// TestProvider_ChatStream_SingleToolCall_AssembledOnDone verifies the
// simplest tool-call streaming shape: one delta carrying a complete call,
// followed by a finish delta. The call must appear on the Done chunk
// (not on the opening delta — the accumulator deliberately suppresses
// per-delta tool-call emission so consumers see one authoritative
// payload, matching OpenAI streaming semantics).
func TestProvider_ChatStream_SingleToolCall_AssembledOnDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					ID: "call_1", Type: "function",
					Function: chatToolCallFunction{Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var (
		preDoneSeenCall bool
		doneCall        provider.ToolCall
	)
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done && len(r.ToolCalls) > 0 {
			doneCall = r.ToolCalls[0]
		}
		if !r.Done && len(r.ToolCalls) > 0 {
			preDoneSeenCall = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if preDoneSeenCall {
		t.Error("tool calls were emitted before Done; the contract is assemble-on-Done so consumers see one authoritative payload")
	}
	if doneCall.Function.Name != "search" {
		t.Errorf("Done chunk missing assembled tool call, got %+v", doneCall)
	}
}

func TestProvider_ChatStream_FragmentedToolCall_AssembledOnDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Index: intPtr(0),
					ID:    "call_1",
					Type:  "function",
					Function: chatToolCallFunction{
						Name: "search",
					},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Index: intPtr(0),
					Function: chatToolCallFunction{
						Arguments: rawJSONString(t, `{"q"`),
					},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Index: intPtr(0),
					Function: chatToolCallFunction{
						Arguments: rawJSONString(t, `:"go"}`),
					},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var final provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			final = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !final.Done {
		t.Fatal("missing final Done chunk")
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("final ToolCalls len = %d, want 1: %+v", len(final.ToolCalls), final.ToolCalls)
	}
	call := final.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "search" || call.Function.Index != 0 {
		t.Errorf("assembled call metadata = %+v", call)
	}
	if got := string(call.Function.Arguments); got != `{"q":"go"}` {
		t.Errorf("assembled arguments = %s, want decoded JSON object", got)
	}
}

// TestProvider_ChatStream_FragmentedToolCall_LockedEncodingMode verifies
// that argument-fragment decoding stays in the mode locked by the FIRST
// non-empty fragment. A server that sent the first fragment as a
// JSON-string envelope cannot mid-stream switch to raw partial JSON —
// the accumulator would otherwise produce an unparseable concatenation.
// Test: server emits encoded first fragment, then a raw-looking second
// fragment; the accumulator unwraps both consistently and assembles
// valid final JSON.
func TestProvider_ChatStream_FragmentedToolCall_LockedEncodingMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Index: intPtr(0), ID: "call_1", Type: "function",
					Function: chatToolCallFunction{Name: "search"},
				}},
			}}}},
			// First non-empty fragment is JSON-string-encoded — locks mode.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(0),
					Function: chatToolCallFunction{Arguments: rawJSONString(t, `{"q":`)}}},
			}}}},
			// Second fragment is also JSON-string-encoded (the locked mode);
			// decoding under the locked mode yields the inner string.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(0),
					Function: chatToolCallFunction{Arguments: rawJSONString(t, `"go"}`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var final provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			final = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(final.ToolCalls))
	}
	if got := string(final.ToolCalls[0].Function.Arguments); got != `{"q":"go"}` {
		t.Errorf("locked-mode assembled arguments = %s, want %s", got, `{"q":"go"}`)
	}
}

// TestProvider_ChatStream_FragmentedToolCall_NilIndexUsesLastKnown verifies
// that arg-only deltas with no Index field (a real shape vLLM emits after
// the opening tool delta) attach to the most-recently-active call rather
// than the per-chunk loop position. Without the lastIndex fallback the
// fragments would silently land on slot 0 even after the active call moved
// to slot 1.
func TestProvider_ChatStream_FragmentedToolCall_NilIndexUsesLastKnown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			// Opening delta names the call at index 0.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Index: intPtr(0), ID: "call_1", Type: "function",
					Function: chatToolCallFunction{Name: "search"},
				}},
			}}}},
			// Subsequent arg-only deltas omit Index — must still attach to call_1.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Function: chatToolCallFunction{Arguments: rawJSONString(t, `{"q"`)},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{
					Function: chatToolCallFunction{Arguments: rawJSONString(t, `:"go"}`)},
				}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var final provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			final = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(final.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1 (nil-Index fragments must attach to last-known call, not spawn new slots): %+v", len(final.ToolCalls), final.ToolCalls)
	}
	if got := string(final.ToolCalls[0].Function.Arguments); got != `{"q":"go"}` {
		t.Errorf("assembled arguments = %s, want fragments concatenated under last-known index", got)
	}
}

// TestProvider_ChatStream_MultiToolFragmented_AssembledIndependently
// covers two concurrent tool_calls with interleaved arg fragments. The
// accumulator must keep slot 0 and slot 1 fragments separate even when
// they arrive in mixed order.
func TestProvider_ChatStream_MultiToolFragmented_AssembledIndependently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			// Both tools opened in the same delta.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{
					{Index: intPtr(0), ID: "call_a", Type: "function", Function: chatToolCallFunction{Name: "search"}},
					{Index: intPtr(1), ID: "call_b", Type: "function", Function: chatToolCallFunction{Name: "fetch"}},
				},
			}}}},
			// Interleaved arg fragments.
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(1), Function: chatToolCallFunction{Arguments: rawJSONString(t, `{"url":"`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(0), Function: chatToolCallFunction{Arguments: rawJSONString(t, `{"q":"go"}`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(1), Function: chatToolCallFunction{Arguments: rawJSONString(t, `x.com"}`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var final provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			final = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(final.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2: %+v", len(final.ToolCalls), final.ToolCalls)
	}
	// Snapshot order follows first-appearance, so call_a (index 0) comes first.
	if final.ToolCalls[0].ID != "call_a" || final.ToolCalls[1].ID != "call_b" {
		t.Errorf("ToolCalls order = %q,%q; want call_a,call_b (first-appearance ordering)",
			final.ToolCalls[0].ID, final.ToolCalls[1].ID)
	}
	if got := string(final.ToolCalls[0].Function.Arguments); got != `{"q":"go"}` {
		t.Errorf("call_a arguments = %s, want isolated to slot 0", got)
	}
	if got := string(final.ToolCalls[1].Function.Arguments); got != `{"url":"x.com"}` {
		t.Errorf("call_b arguments = %s, want fragments correctly concatenated under slot 1", got)
	}
}

// TestProvider_ChatStream_FragmentedToolCall_OrderByFirstAppearance
// confirms snapshot order tracks first-appearance even when indexes arrive
// out of natural order (e.g. index 1 before index 0). This is the contract
// downstream consumers depend on for stable iteration.
func TestProvider_ChatStream_FragmentedToolCall_OrderByFirstAppearance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(1), ID: "second-but-first-seen", Type: "function",
					Function: chatToolCallFunction{Name: "b", Arguments: rawJSONString(t, `{}`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{
				ToolCalls: []chatToolCall{{Index: intPtr(0), ID: "first-but-second-seen", Type: "function",
					Function: chatToolCallFunction{Name: "a", Arguments: rawJSONString(t, `{}`)}}},
			}}}},
			{Model: "m", Choices: []chatChunkChoice{{
				Delta: chatMessage{}, FinishReason: stringPtr("tool_calls"),
			}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var final provider.ChatResponse
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		if r.Done {
			final = r
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if len(final.ToolCalls) != 2 {
		t.Fatalf("ToolCalls len = %d, want 2", len(final.ToolCalls))
	}
	if final.ToolCalls[0].ID != "second-but-first-seen" {
		t.Errorf("first ToolCall.ID = %q, want %q (snapshot order should track first-appearance, not numeric index)",
			final.ToolCalls[0].ID, "second-but-first-seen")
	}
}

func TestProvider_ChatStream_CancelMidStream_EmitsPartial(t *testing.T) {
	// Server sends one chunk, then blocks. We cancel from INSIDE the
	// callback (right after observing chunk 1), eliminating the race
	// between "did the client see chunk 1?" and "did cancel fire?".
	// Without ThinkNone the partial-content delta would sit in the
	// parser's lookahead buffer and never reach fn — so the callback
	// would never fire and the cancel signal would never trigger.
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("response writer does not flush")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, chatChunk{
			Model:   "m",
			Choices: []chatChunkChoice{{Delta: chatMessage{Content: "partial-content"}}},
		}))
		flusher.Flush()
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
	var (
		mu        sync.Mutex
		responses []provider.ChatResponse
	)
	err := p.ChatStream(ctx, provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		mu.Lock()
		responses = append(responses, r)
		// Cancel after observing the first content chunk. The subsequent
		// reader.Next() will fail with the ctx error, triggering the
		// partial-emission code path.
		if r.Content == "partial-content" {
			cancel()
		}
		mu.Unlock()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "context") {
		t.Errorf("expected ctx error, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(responses) < 2 {
		t.Fatalf("expected >= 2 chunks (content + synthetic Done+Partial), got %d: %+v", len(responses), responses)
	}
	last := responses[len(responses)-1]
	if !last.Done || !last.Partial {
		t.Errorf("last chunk should be Done+Partial after mid-stream cancel, got %+v", last)
	}
}

func TestProvider_ChatStream_MalformedChunk_Skipped(t *testing.T) {
	// Proxies and middleboxes sometimes inject keepalive frames or
	// oddly-shaped JSON; the stream should tolerate them rather than abort.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, ": keepalive comment\n\n")
		_, _ = fmt.Fprintf(w, "data: {not json}\n\n")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, chatChunk{
			Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "good"}}},
		}))
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, chatChunk{
			Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{}, FinishReason: stringPtr("stop")}},
		}))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var saw string
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		saw += r.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if saw != "good" {
		t.Errorf("Content = %q, want %q (malformed chunk should be skipped, good one preserved)", saw, "good")
	}
}

func TestProvider_ChatStream_SSEReadError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 1024*1024+1))
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(provider.ChatResponse) error {
		t.Fatal("callback should not be invoked for an unreadable SSE frame")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "sse read") {
		t.Fatalf("expected propagated SSE read error, got %v", err)
	}
}

// TestProvider_ChatStream_EOFMidFrame_PropagatesError covers the
// connection-reset / abrupt-close failure mode: a server begins writing
// an SSE frame, flushes a partial "data: " prefix without the closing
// blank line, then closes the connection. The reader must surface this
// as a stream error rather than silently terminating via errStreamDone,
// otherwise consumers would see "success with no Done chunk" and not
// know to retry. Distinct from cancel-mid-stream (which has its own
// chunksReceived > 0 + partial-emission path) because here NO complete
// chunk was ever delivered.
func TestProvider_ChatStream_EOFMidFrame_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not flush")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Partial frame: prefix only, no closing blank line, then close.
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chunk-")
		flusher.Flush()
		// Hijack to slam the connection shut so the client sees an
		// unterminated frame, not a clean [DONE]. httptest's default
		// behavior on handler return is to close cleanly which surfaces
		// as scanner.Err == nil. Hijacking guarantees an abrupt close.
		if hj, hjOK := w.(http.Hijacker); hjOK {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				_ = conn.Close()
			}
		}
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(provider.ChatResponse) error {
		t.Fatal("callback must not fire — no complete chunk was delivered")
		return nil
	})
	if err == nil {
		t.Fatal("expected error from mid-frame EOF, got nil")
	}
	// Either bufio detects the truncation (sse read err) or the http
	// client surfaces the connection close — both are valid surfacing
	// paths; the contract is "do not silently succeed".
	if !strings.Contains(err.Error(), "chat stream") {
		t.Errorf("error should be wrapped with chat-stream context, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Generate (non-streaming /v1/completions) + FIM via suffix
// ---------------------------------------------------------------------------

func TestProvider_Generate_FIM_SuffixPropagated(t *testing.T) {
	var captured completionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("expected /v1/completions, got %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(completionResponse{
			Model: "m", Choices: []completionChoice{{Text: "middle"}},
			Usage: usage{TotalTokens: 1},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	resp, err := p.Generate(context.Background(), provider.GenerateRequest{
		Model:  "m",
		Prompt: "before",
		Suffix: "after",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.Suffix != "after" {
		t.Errorf("Suffix not propagated: %q", captured.Suffix)
	}
	if captured.Prompt != "before" {
		t.Errorf("Prompt = %q, want %q", captured.Prompt, "before")
	}
	if resp.Response != "middle" {
		t.Errorf("Response = %q, want middle", resp.Response)
	}
}

func TestProvider_Generate_System_PrependedToPrompt(t *testing.T) {
	// /v1/completions has no system field. Provider prepends System to
	// Prompt with a newline boundary so the directive isn't silently lost.
	var captured completionRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(completionResponse{
			Choices: []completionChoice{{Text: "ok"}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Generate(context.Background(), provider.GenerateRequest{
		Model: "m", System: "You are concise.", Prompt: "explain bayes",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(captured.Prompt, "You are concise.\n") {
		t.Errorf("System should be prepended to Prompt, got: %q", captured.Prompt)
	}
}

func TestProvider_GenerateStream_DeliversChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, txt := range []string{"a", "b", "c"} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, completionChunk{
				Model: "m", Choices: []completionChunkChoice{{Text: txt}},
			}))
			f.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, completionChunk{
			Model: "m", Choices: []completionChunkChoice{{Text: "", FinishReason: stringPtr("stop")}},
		}))
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	var combined string
	var lastDone bool
	err := p.GenerateStream(context.Background(), provider.GenerateRequest{Model: "m", Prompt: "x"},
		func(r provider.GenerateResponse) error {
			combined += r.Response
			lastDone = r.Done
			return nil
		})
	if err != nil {
		t.Fatalf("GenerateStream: %v", err)
	}
	if combined != "abc" {
		t.Errorf("combined = %q, want abc", combined)
	}
	if !lastDone {
		t.Error("last chunk should be Done")
	}
}

func TestProvider_GenerateStream_SSEReadError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.Repeat("x", 1024*1024+1))
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	err := p.GenerateStream(context.Background(), provider.GenerateRequest{Model: "m", Prompt: "x"}, func(provider.GenerateResponse) error {
		t.Fatal("callback should not be invoked for an unreadable SSE frame")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "sse read") {
		t.Fatalf("expected propagated SSE read error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Embed (singleflight dedup + index-based reorder + error paths)
// ---------------------------------------------------------------------------

func TestProvider_Embed_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Model: "embed-m",
			Data: []embeddingDoc{
				{Index: 0, Embedding: []float64{0.1, 0.2}},
				{Index: 1, Embedding: []float64{0.3, 0.4}},
			},
			Usage: usage{PromptTokens: 4},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	resp, err := p.Embed(context.Background(), provider.EmbedRequest{
		Model: "embed-m", Input: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Embeddings) != 2 || resp.Embeddings[0][0] != 0.1 {
		t.Errorf("Embeddings = %+v", resp.Embeddings)
	}
}

func TestProvider_Embed_ReordersByIndex(t *testing.T) {
	// Spec says data array order is unspecified; provider must reorder
	// using Index so callers can rely on positional alignment with Input.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Data: []embeddingDoc{
				{Index: 1, Embedding: []float64{2}},
				{Index: 0, Embedding: []float64{1}},
			},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	resp, err := p.Embed(context.Background(), provider.EmbedRequest{Model: "m", Input: []string{"a", "b"}})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if resp.Embeddings[0][0] != 1 || resp.Embeddings[1][0] != 2 {
		t.Errorf("not reordered by index: %+v", resp.Embeddings)
	}
}

func TestProvider_Embed_MismatchedCount_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Data: []embeddingDoc{{Index: 0, Embedding: []float64{1}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Embed(context.Background(), provider.EmbedRequest{Model: "m", Input: []string{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "1 embeddings for 2 inputs") {
		t.Errorf("expected count-mismatch error, got: %v", err)
	}
}

func TestProvider_Embed_OutOfRangeIndex_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(embedResponse{
			Data: []embeddingDoc{
				{Index: 0, Embedding: []float64{1}},
				{Index: 5, Embedding: []float64{2}}, // out of range
			},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	_, err := p.Embed(context.Background(), provider.EmbedRequest{Model: "m", Input: []string{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "out-of-range index") {
		t.Errorf("expected out-of-range error, got: %v", err)
	}
}

func TestProvider_Embed_EmptyInput_Errors(t *testing.T) {
	p := NewProvider(NewClient("http://x"))
	_, err := p.Embed(context.Background(), provider.EmbedRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "at least one input") {
		t.Errorf("expected empty-input error, got: %v", err)
	}
}

func TestProvider_Embed_Singleflight_Dedups(t *testing.T) {
	// Concurrent identical embed requests must collapse to ONE upstream
	// call — this is the optimization that makes batch RAG indexing
	// affordable.
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&calls, 1)
		time.Sleep(20 * time.Millisecond) // hold so the second caller piggybacks
		_ = json.NewEncoder(w).Encode(embedResponse{
			Data: []embeddingDoc{{Index: 0, Embedding: []float64{42}}},
		})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	req := provider.EmbedRequest{Model: "m", Input: []string{"same-text"}}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			if _, err := p.Embed(context.Background(), req); err != nil {
				t.Errorf("Embed: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (singleflight should have deduped)", got)
	}
}

func TestProvider_Embed_SingleflightKey_NULInputsDoNotCollide(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		time.Sleep(40 * time.Millisecond)
		data := make([]embeddingDoc, len(req.Input))
		for i := range req.Input {
			data[i] = embeddingDoc{Index: i, Embedding: []float64{float64(len(req.Input)), float64(i)}}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Data: data})
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	reqs := []provider.EmbedRequest{
		{Model: "m", Input: []string{"a\x00b"}},
		{Model: "m", Input: []string{"a", "b"}},
	}
	type embedResult struct {
		count int
		err   error
	}
	results := make([]embedResult, len(reqs))
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(len(reqs))
	for i := range reqs {
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := p.Embed(context.Background(), reqs[i])
			results[i].err = err
			if resp != nil {
				results[i].count = len(resp.Embeddings)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, res := range results {
		if res.err != nil {
			t.Fatalf("Embed[%d]: %v", i, res.err)
		}
		if res.count != len(reqs[i].Input) {
			t.Fatalf("Embed[%d] returned %d embeddings for %d inputs", i, res.count, len(reqs[i].Input))
		}
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 distinct calls for NUL-containing inputs", got)
	}
}

// ---------------------------------------------------------------------------
// SSE reader unit tests
// ---------------------------------------------------------------------------

func TestSSEReader_ParsesDataLines(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"))
	r := newSSEReader(body)
	defer func() { _ = r.Close() }()

	p1, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(p1) != `{"a":1}` {
		t.Errorf("first payload = %q, want %q", p1, `{"a":1}`)
	}
	p2, err := r.Next()
	if err != nil || string(p2) != `{"b":2}` {
		t.Errorf("second payload = %q err=%v", p2, err)
	}
	if _, err := r.Next(); err != errStreamDone {
		t.Errorf("[DONE] should produce errStreamDone, got %v", err)
	}
}

func TestSSEReader_SkipsCommentsAndNonDataLines(t *testing.T) {
	body := io.NopCloser(strings.NewReader(": keepalive\nevent: ping\nid: 1\ndata: {\"x\":1}\n\ndata: [DONE]\n\n"))
	r := newSSEReader(body)
	defer func() { _ = r.Close() }()

	p, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(p) != `{"x":1}` {
		t.Errorf("payload = %q, want %q", p, `{"x":1}`)
	}
}

func TestSSEReader_EOFWithoutDone_IsStreamDone(t *testing.T) {
	// Some servers close the stream without emitting [DONE]; the reader
	// must treat that as normal termination rather than an error.
	body := io.NopCloser(strings.NewReader("data: {\"x\":1}\n\n"))
	r := newSSEReader(body)
	defer func() { _ = r.Close() }()

	if _, err := r.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if _, err := r.Next(); err != errStreamDone {
		t.Errorf("EOF should produce errStreamDone, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Mock-server helper
// ---------------------------------------------------------------------------

type mockServerOpts struct {
	models       []modelEntry
	chatResponse chatResponse
}

type mockServer struct {
	url      string
	srv      *httptest.Server
	lastPath atomic.Value // string
	lastAuth atomic.Value // string
}

func (m *mockServer) close() { m.srv.Close() }

func newMockServer(t *testing.T, opts mockServerOpts) *mockServer {
	t.Helper()
	m := &mockServer{}
	m.lastPath.Store("")
	m.lastAuth.Store("")

	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.lastPath.Store(r.URL.Path)
		m.lastAuth.Store(r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(modelsResponse{Data: opts.models})
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(opts.chatResponse)
		default:
			http.NotFound(w, r)
		}
	}))
	m.url = m.srv.URL
	return m
}

// writeSSE writes a slice of chunks as SSE events terminated by [DONE].
func writeSSE(t *testing.T, w http.ResponseWriter, chunks []chatChunk) {
	t.Helper()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("response writer doesn't flush")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, c))
		flusher.Flush()
	}
	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func stringPtr(s string) *string { return &s }

func intPtr(i int) *int { return &i }

func rawJSONString(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal string: %v", err)
	}
	return json.RawMessage(b)
}
