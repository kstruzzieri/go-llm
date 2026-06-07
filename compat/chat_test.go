package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// ---------------------------------------------------------------------------
// Shared fixture helpers
// ---------------------------------------------------------------------------

// newChatFixture returns a Server wired to a provider whose Chat handler
// records the last request received and replies with the supplied content.
// The returned pointer to the captured request is updated on every call.
func newChatFixture(t *testing.T, content string, opts ...Option) (*Server, *provider.ChatRequest, *int32, func()) {
	t.Helper()
	var last provider.ChatRequest
	var calls int32
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
		},
		chatFn: func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			atomic.AddInt32(&calls, 1)
			last = req
			return &provider.ChatResponse{
				Model:    req.Model,
				Provider: "ollama",
				Content:  content,
				Done:     true,
				Usage: provider.Usage{
					PromptTokens:     3,
					CompletionTokens: 5,
					TotalTokens:      8,
				},
			}, nil
		},
	}
	srv, teardown := newTestServer(t, mp, opts...)
	return srv, &last, &calls, teardown
}

// doChat POSTs a JSON body to /v1/chat/completions and returns the recorder.
func doChat(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	srv.buildHandler().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Core non-streaming path
// ---------------------------------------------------------------------------

func TestChatCompletions_NonStreaming_Success(t *testing.T) {
	srv, last, calls, teardown := newChatFixture(t, "hello from mock")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi there"},
		},
	}
	rec := doChat(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("provider Chat called %d times, want 1", got)
	}
	if last.Model != "qwen3:8b" {
		t.Errorf("provider saw model = %q, want qwen3:8b", last.Model)
	}
	if len(last.Messages) != 1 || last.Messages[0].Content != "hi there" {
		t.Errorf("provider saw messages = %+v", last.Messages)
	}

	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl_") {
		t.Errorf("id = %q, want chatcmpl_ prefix", out.ID)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if out.Choices[0].Message.Role != "assistant" {
		t.Errorf("message role = %q, want assistant", out.Choices[0].Message.Role)
	}
	if out.Choices[0].Message.Content != "hello from mock" {
		t.Errorf("message content = %q", out.Choices[0].Message.Content)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
	if out.Usage.TotalTokens != 8 {
		t.Errorf("usage.total_tokens = %d, want 8", out.Usage.TotalTokens)
	}
	// Top-level "model" must be qualified (provider/model) to match the
	// dry-run branch; see chat.go — we take it from plan.Profile.Key.String()
	// rather than resp.Model so the wire is consistent even when the
	// provider reports a bare model name.
	if out.Model != "ollama/qwen3:8b" {
		t.Errorf("response.model = %q, want ollama/qwen3:8b", out.Model)
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing; router should populate it")
	}
	if out.RouteInfo.ActualModel != "ollama/qwen3:8b" {
		t.Errorf("route.actual_model = %q", out.RouteInfo.ActualModel)
	}
}

func TestChatCompletions_BodyTooLarge(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "unused")
	defer teardown()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		oversizedJSONStringReader(
			`{"model":"ollama/qwen3:8b","messages":[{"role":"user","content":"`,
			maxChatRequestBodyBytes+1,
			`"}]}`,
		))
	req.Header.Set("Content-Type", "application/json")
	srv.buildHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Fatalf("provider Chat called %d times, want 0", got)
	}
	env := decodeErrorEnvelope(t, rec)
	if env.Error.Code != "body_too_large" {
		t.Errorf("error.code = %q, want body_too_large", env.Error.Code)
	}
}

func TestChatCompletions_MissingMessages_400(t *testing.T) {
	srv, _, _, teardown := newChatFixture(t, "n/a")
	defer teardown()

	body := map[string]any{
		"model":    "ollama/qwen3:8b",
		"messages": []any{},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "missing_messages" {
		t.Errorf("code = %q, want missing_messages", env.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// Streaming path
// ---------------------------------------------------------------------------

// doChatRequest is doChat's streaming cousin — it keeps the raw recorder so
// callers can inspect the SSE body for framing as well as individual chunks.
func doChatRequest(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	srv.buildHandler().ServeHTTP(rec, req)
	return rec
}

// decodeSSEChunks parses an SSE body into individual ChatCompletionChunk
// payloads. It skips the [DONE] sentinel and any non-"data:" lines.
func decodeSSEChunks(t *testing.T, body string) []ChatCompletionChunk {
	t.Helper()
	var chunks []ChatCompletionChunk
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var c ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// newStreamingServer wires a server whose provider scripts ChatStream directly
// — distinct from newChatFixture which scripts the non-streaming path. The
// `build` callback lets each test drive its own chunk sequence.
func newStreamingServer(
	t *testing.T,
	build func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error,
	opts ...Option,
) (*Server, func()) {
	t.Helper()
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapStream | provider.CapToolCall,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
		},
		chatStreamFn: build,
	}
	return newTestServer(t, mp, opts...)
}

// TestChatCompletions_Streaming exercises the happy path: two chunks from the
// provider, the final one with Done=true, produce two SSE events followed by
// "data: [DONE]". The first chunk must carry role=assistant and content="hi ",
// the second must carry finish_reason="stop".
func TestChatCompletions_Streaming(t *testing.T) {
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		if err := fn(provider.ChatResponse{Model: req.Model, Content: "hi "}); err != nil {
			return err
		}
		return fn(provider.ChatResponse{Model: req.Model, Content: "there", Done: true})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	text := rec.Body.String()
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Errorf("missing DONE sentinel: %q", text)
	}
	chunks := decodeSSEChunks(t, text)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(chunks), text)
	}
	// First chunk: content delta, no finish_reason.
	c0 := chunks[0]
	if c0.Object != "chat.completion.chunk" {
		t.Errorf("chunk[0].object = %q", c0.Object)
	}
	if !strings.HasPrefix(c0.ID, "chatcmpl_") {
		t.Errorf("chunk[0].id = %q, want chatcmpl_ prefix", c0.ID)
	}
	if len(c0.Choices) != 1 {
		t.Fatalf("chunk[0].choices = %d", len(c0.Choices))
	}
	if c0.Choices[0].Delta.Role != "assistant" {
		t.Errorf("chunk[0].delta.role = %q", c0.Choices[0].Delta.Role)
	}
	if c0.Choices[0].Delta.Content != "hi " {
		t.Errorf("chunk[0].delta.content = %q", c0.Choices[0].Delta.Content)
	}
	if c0.Choices[0].FinishReason != nil {
		t.Errorf("chunk[0].finish_reason = %v, want nil", *c0.Choices[0].FinishReason)
	}
	// Second chunk: content delta, finish_reason=stop.
	c1 := chunks[1]
	if c1.Choices[0].Delta.Content != "there" {
		t.Errorf("chunk[1].delta.content = %q", c1.Choices[0].Delta.Content)
	}
	if c1.Choices[0].FinishReason == nil || *c1.Choices[0].FinishReason != "stop" {
		t.Errorf("chunk[1].finish_reason = %v, want stop", c1.Choices[0].FinishReason)
	}
}

func TestChatCompletions_StreamingIncludeUsage(t *testing.T) {
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		if err := fn(provider.ChatResponse{Model: req.Model, Content: "hi"}); err != nil {
			return err
		}
		return fn(provider.ChatResponse{
			Model: req.Model,
			Done:  true,
			Usage: provider.Usage{
				PromptTokens:     3,
				CompletionTokens: 5,
				TotalTokens:      8,
			},
		})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeSSEChunks(t, rec.Body.String())
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3: %q", len(chunks), rec.Body.String())
	}
	final := chunks[1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %v, want stop", final.Choices[0].FinishReason)
	}
	usageChunk := chunks[2]
	if len(usageChunk.Choices) != 0 {
		t.Fatalf("usage chunk choices = %d, want 0", len(usageChunk.Choices))
	}
	if usageChunk.Usage == nil {
		t.Fatal("usage chunk missing usage")
	}
	if usageChunk.Usage.PromptTokens != 3 || usageChunk.Usage.CompletionTokens != 5 || usageChunk.Usage.TotalTokens != 8 {
		t.Fatalf("usage = %+v, want 3/5/8", *usageChunk.Usage)
	}
}

// TestChatCompletions_Streaming_ModelQualified verifies that every streaming
// chunk carries the qualified "provider/model" form in the Model field,
// matching the non-streaming branch's plan.Profile.Key.String() convention.
func TestChatCompletions_Streaming_ModelQualified(t *testing.T) {
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		// Provider returns bare model name (this is the realistic Ollama shape).
		return fn(provider.ChatResponse{Model: req.Model, Content: "ok", Done: true})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeSSEChunks(t, rec.Body.String())
	if len(chunks) == 0 {
		t.Fatalf("no chunks: %q", rec.Body.String())
	}
	for i, c := range chunks {
		if c.Model != "ollama/qwen3:8b" {
			t.Errorf("chunk[%d].model = %q, want ollama/qwen3:8b (qualified)", i, c.Model)
		}
	}
}

// TestChatCompletions_Streaming_FinishReasonFromToolCalls asserts that when
// the provider emits tool_calls on the final chunk, the wire delta carries
// them AND finish_reason is "tool_calls" — matching the non-streaming Task 9
// fix's derivation rules.
func TestChatCompletions_Streaming_FinishReasonFromToolCalls(t *testing.T) {
	args := json.RawMessage(`{"city":"Paris"}`)
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		return fn(provider.ChatResponse{
			Model: req.Model,
			Done:  true,
			ToolCalls: []provider.ToolCall{{
				ID:   "call_abc",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      "get_weather",
					Arguments: args,
				},
			}},
		})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "weather in Paris?"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeSSEChunks(t, rec.Body.String())
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	final := chunks[0]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", final.Choices[0].FinishReason)
	}
	calls := final.Choices[0].Delta.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("delta.tool_calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_abc" {
		t.Errorf("tool_call.id = %q, want call_abc", calls[0].ID)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call.function.name = %q, want get_weather", calls[0].Function.Name)
	}
	if !bytes.Equal(calls[0].Function.Arguments, args) {
		t.Errorf("tool_call.function.arguments = %s, want %s", calls[0].Function.Arguments, args)
	}
}

// TestChatCompletions_Streaming_FinishReasonLength covers the length-cap
// branch of streaming finish_reason derivation: MaxTokens=5 and usage
// reporting CompletionTokens=5 on the Done chunk must yield "length".
func TestChatCompletions_Streaming_FinishReasonLength(t *testing.T) {
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		return fn(provider.ChatResponse{
			Model:   req.Model,
			Content: "truncated",
			Done:    true,
			Usage:   provider.Usage{CompletionTokens: 5, TotalTokens: 8, PromptTokens: 3},
		})
	})
	defer teardown()

	body := map[string]any{
		"model":      "ollama/qwen3:8b",
		"stream":     true,
		"max_tokens": 5,
		"messages": []map[string]string{
			{"role": "user", "content": "write a long essay"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeSSEChunks(t, rec.Body.String())
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	final := chunks[0]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", final.Choices[0].FinishReason)
	}
}

// TestChatCompletions_Streaming_ErrorBeforeFirstChunk verifies the lazy-start
// guarantee: when the provider's ChatStream fails before invoking fn even
// once, the client sees a normal JSON error envelope (not a partial SSE
// stream). The status is 502 because an unclassified provider error maps
// through statusForCompatError to upstream_error.
func TestChatCompletions_Streaming_ErrorBeforeFirstChunk(t *testing.T) {
	wantErr := errors.New("synthetic upstream failure")
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		// Never invoke fn — fail immediately. The SSE writer's lazy-start
		// means no "data: ..." bytes have been committed yet.
		return wantErr
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (JSON error envelope)", ct)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != "upstream_error" {
		t.Errorf("error.code = %q, want upstream_error", env.Error.Code)
	}
}

// TestChatCompletions_Streaming_ErrorAfterFirstChunk guards the HIGH-severity
// silent-truncation fix: when the provider emits one chunk successfully then
// fails mid-stream (non-cancellation error), the handler MUST NOT emit
// "data: [DONE]". The [DONE] sentinel is OpenAI's success signal — emitting
// it under error conditions silently masks the truncation. Its absence lets
// OpenAI SDKs detect premature EOS.
func TestChatCompletions_Streaming_ErrorAfterFirstChunk(t *testing.T) {
	srv, teardown := newStreamingServer(t, func(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		if err := fn(provider.ChatResponse{Model: req.Model, Content: "partial"}); err != nil {
			return err
		}
		return errors.New("upstream collapsed")
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)

	// Status 200 + event-stream headers are committed because the first chunk
	// was written successfully. The error occurs after commit.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers committed on first chunk), body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	text := rec.Body.String()
	// First chunk was delivered before the failure.
	if !strings.Contains(text, `"content":"partial"`) {
		t.Errorf("body missing first chunk payload %q: %q", `"content":"partial"`, text)
	}
	// Critical assertion: no [DONE] sentinel. Its absence is the wire-level
	// signal of premature EOS that clients rely on.
	if strings.Contains(text, "data: [DONE]") {
		t.Errorf("body unexpectedly contains data: [DONE] after mid-stream error: %q", text)
	}
}

// TestChatCompletions_Streaming_ClientDisconnect verifies the handler returns
// cleanly when the caller's context is canceled mid-stream (IDE closing the
// completion, e.g., on the next keystroke). The semaphore slot must be
// released so subsequent requests are not blocked — we probe this by
// acquiring all slots after the handler returns.
func TestChatCompletions_Streaming_ClientDisconnect(t *testing.T) {
	// Use a single-slot semaphore so we can prove the slot was released by
	// re-acquiring it after the handler returns.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, teardown := newStreamingServer(t, func(streamCtx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
		// Emit one chunk, then cancel the request context and return the
		// cancellation error — simulating a client disconnect detected by the
		// provider after it had already pushed a chunk.
		if err := fn(provider.ChatResponse{Model: req.Model, Content: "tick"}); err != nil {
			return err
		}
		cancel()
		return context.Canceled
	}, WithMaxConcurrency(1))
	defer teardown()

	buf, err := json.Marshal(map[string]any{
		"model":  "ollama/qwen3:8b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	// Should not panic and should return (not hang).
	srv.buildHandler().ServeHTTP(rec, req)

	// The first chunk made it onto the wire; no [DONE] should follow because
	// the stream was aborted.
	text := rec.Body.String()
	if !strings.Contains(text, `"content":"tick"`) {
		t.Errorf("body missing first chunk: %q", text)
	}
	if strings.Contains(text, "data: [DONE]") {
		t.Errorf("body contains data: [DONE] after cancellation: %q", text)
	}

	// Probe semaphore release: with max concurrency 1, we must be able to
	// acquire a fresh slot now that the handler has returned.
	release, ok := srv.semaphore.acquire(provider.PriorityNormal)
	if !ok {
		t.Fatal("semaphore slot not released after client-disconnect — handler leaked a slot")
	}
	release()
}

// TestChatCompletions_Streaming_ProviderWithoutCapStream verifies that a
// streaming request routed through a model that lacks CapStream is rejected
// cleanly — the router's capability gate eliminates the candidate BEFORE
// newSSEWriter is called, so the client sees a JSON error envelope (not a
// partial SSE stream). This proves the capability gate closes before any SSE
// bytes are committed.
//
// The model profile's Caps come from the static catalog or runtime
// ModelInfo.Capabilities (merged in model_registry.go). Using a model that is
// NOT in the catalog and providing no runtime Capabilities yields profile.Caps
// == 0, which fails the gate when RequiredCaps includes CapChat | CapStream.
func TestChatCompletions_Streaming_ProviderWithoutCapStream(t *testing.T) {
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat,
		models: []provider.ModelInfo{
			// No Capabilities set, no catalog match for this family — so
			// profile.Caps stays 0 and the capability gate rejects when the
			// router requires CapStream.
			{Name: "unlisted-model:1b", ContextWindow: 32768},
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	body := map[string]any{
		"model":  "ollama/unlisted-model:1b",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChatRequest(t, srv, body)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want non-200 (router should reject before SSE headers): body=%s", rec.Body.String())
	}
	// JSON envelope, not text/event-stream — the lazy-start SSE writer was
	// never constructed because Route returned an error first.
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (JSON error envelope, not partial SSE)", ct)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code == "" {
		t.Errorf("error.code is empty, want non-empty")
	}
	// Pin the specific code: the router returns ErrNoViableCandidate for
	// capability-gated rejections, which statusForCompatError maps to
	// "no_viable_candidate" with status 400.
	if env.Error.Code != "no_viable_candidate" {
		t.Errorf("error.code = %q, want no_viable_candidate", env.Error.Code)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (ErrNoViableCandidate)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Compatibility tests
// ---------------------------------------------------------------------------

func TestChatCompletions_StopAsSingleString(t *testing.T) {
	srv, last, _, teardown := newChatFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stop": "END",
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(last.Options.Stop) != 1 || last.Options.Stop[0] != "END" {
		t.Errorf("provider saw Options.Stop = %v, want [END]", last.Options.Stop)
	}
}

func TestChatCompletions_StopAsArray(t *testing.T) {
	srv, last, _, teardown := newChatFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stop": []string{"END1", "END2"},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(last.Options.Stop) != 2 || last.Options.Stop[0] != "END1" || last.Options.Stop[1] != "END2" {
		t.Errorf("provider saw Options.Stop = %v, want [END1 END2]", last.Options.Stop)
	}
}

func TestChatCompletions_ToolsForwarded(t *testing.T) {
	srv, last, _, teardown := newChatFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "get_time",
					"description": "Return the current time",
					"parameters":  map[string]any{"type": "object"},
				},
			},
		},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(last.Tools) != 1 {
		t.Fatalf("provider saw %d tools, want 1", len(last.Tools))
	}
	if last.Tools[0].Function.Name != "get_time" {
		t.Errorf("tool name = %q, want get_time", last.Tools[0].Function.Name)
	}
	if last.Tools[0].Type != "function" {
		t.Errorf("tool type = %q, want function", last.Tools[0].Type)
	}
	// Parameters should round-trip as JSON; we do not assert exact bytes
	// because encoding/json map key order is stable but we only need the
	// "type" field to show through.
	if !bytes.Contains(last.Tools[0].Function.Parameters, []byte(`"type"`)) {
		t.Errorf("tool parameters = %s, missing type field", last.Tools[0].Function.Parameters)
	}
}

func TestChatCompletions_DryRun(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "should-not-be-generated")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"x_dry_run": true,
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("provider Chat called %d times on dry-run, want 0", got)
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) != 0 {
		t.Errorf("choices = %d, want 0 on dry-run", len(out.Choices))
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing on dry-run")
	}
	if out.RouteInfo.ActualModel == "" {
		t.Errorf("route.actual_model is empty on dry-run")
	}
	if out.RouteInfo.PlannedModel != out.RouteInfo.ActualModel {
		t.Errorf("dry-run planned=%q actual=%q, expected equal",
			out.RouteInfo.PlannedModel, out.RouteInfo.ActualModel)
	}
}

func TestChatCompletions_DryRunWinsOverStream(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "should-not-be-generated")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream":    true,
		"x_dry_run": true,
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("provider Chat called %d times on stream dry-run, want 0", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing on stream dry-run")
	}
}

// newChatFixtureWithResponse is like newChatFixture but lets the caller shape
// the response returned by the mock provider. The builder receives the request
// the provider observed and returns the desired ChatResponse — this is how
// tests for finish_reason, tool_calls forwarding, etc. scriptedly inject
// response state the mock would not otherwise produce.
func newChatFixtureWithResponse(
	t *testing.T,
	build func(provider.ChatRequest) *provider.ChatResponse,
	opts ...Option,
) (*Server, *provider.ChatRequest, func()) {
	t.Helper()
	var last provider.ChatRequest
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
		},
		chatFn: func(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			last = req
			return build(req), nil
		},
	}
	srv, teardown := newTestServer(t, mp, opts...)
	return srv, &last, teardown
}

// TestChatCompletions_ToolCallsPreserved verifies that a tool_calls response
// from the provider surfaces in the response body's message.tool_calls array
// AND drives finish_reason="tool_calls" — before this fix, tool_calls were
// silently dropped and finish_reason was hard-coded to "stop".
func TestChatCompletions_ToolCallsPreserved(t *testing.T) {
	args := json.RawMessage(`{"city":"Paris"}`)
	srv, _, teardown := newChatFixtureWithResponse(t, func(req provider.ChatRequest) *provider.ChatResponse {
		return &provider.ChatResponse{
			Model:    req.Model,
			Provider: "ollama",
			// Content may be empty when the model emits only tool calls;
			// OpenAI SDK clients tolerate that.
			Content: "",
			Done:    true,
			ToolCalls: []provider.ToolCall{{
				ID:   "call_abc",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      "get_weather",
					Arguments: args,
				},
			}},
			Usage: provider.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
		}
	})
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "weather in Paris?"},
		},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", out.Choices[0].FinishReason)
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("message.tool_calls = %d, want 1", len(calls))
	}
	if calls[0].ID != "call_abc" {
		t.Errorf("tool_call.id = %q, want call_abc", calls[0].ID)
	}
	if calls[0].Type != "function" {
		t.Errorf("tool_call.type = %q, want function", calls[0].Type)
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("tool_call.function.name = %q, want get_weather", calls[0].Function.Name)
	}
	if !bytes.Equal(calls[0].Function.Arguments, args) {
		t.Errorf("tool_call.function.arguments = %s, want %s", calls[0].Function.Arguments, args)
	}
}

// TestChatCompletions_FinishReasonLength exercises the length-cap branch of
// finish_reason derivation — when MaxTokens is set and the response hit the
// cap, we return "length" instead of "stop".
func TestChatCompletions_FinishReasonLength(t *testing.T) {
	srv, _, teardown := newChatFixtureWithResponse(t, func(req provider.ChatRequest) *provider.ChatResponse {
		return &provider.ChatResponse{
			Model:    req.Model,
			Provider: "ollama",
			Content:  "truncated",
			Done:     true,
			Usage:    provider.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
		}
	})
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "write a long essay"},
		},
		"max_tokens": 5,
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", out.Choices[0].FinishReason)
	}
}

// TestChatCompletions_ToolRoleMessageRoundTrip verifies that a tool-role
// message's Name and ToolCallID flow through to the provider — they name the
// tool whose result this message carries and correlate with the assistant's
// prior invocation, so dropping them breaks multi-turn tool use.
func TestChatCompletions_ToolRoleMessageRoundTrip(t *testing.T) {
	srv, last, _, teardown := newChatFixture(t, "thanks")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]any{
			{"role": "user", "content": "weather?"},
			{
				"role":         "tool",
				"name":         "search",
				"tool_call_id": "call_1",
				"content":      `{"results":[]}`,
			},
		},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(last.Messages) != 2 {
		t.Fatalf("provider saw %d messages, want 2", len(last.Messages))
	}
	m := last.Messages[1]
	if m.Role != "tool" {
		t.Errorf("message[1].role = %q, want tool", m.Role)
	}
	if m.ToolName != "search" {
		t.Errorf("message[1].ToolName = %q, want search", m.ToolName)
	}
	if m.ToolCallID != "call_1" {
		t.Errorf("message[1].ToolCallID = %q, want call_1", m.ToolCallID)
	}
	if m.Content != `{"results":[]}` {
		t.Errorf("message[1].Content = %q", m.Content)
	}
}

// TestChatCompletions_InvalidToolParameters rejects non-object parameter
// payloads at the edge rather than forwarding garbage to the provider.
func TestChatCompletions_InvalidToolParameters(t *testing.T) {
	srv, _, _, teardown := newChatFixture(t, "n/a")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"tools": []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":       "broken",
					"parameters": "not an object",
				},
			},
		},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "invalid_tool_parameters" {
		t.Errorf("code = %q, want invalid_tool_parameters", env.Error.Code)
	}
}

// TestChatCompletions_429WhenAtCapacity exercises the admission-control
// boundary: once the semaphore is full, new requests receive 429 with
// Retry-After: 1 and error code "capacity". Releasing the slot lets the
// next request succeed.
func TestChatCompletions_429WhenAtCapacity(t *testing.T) {
	srv, _, _, teardown := newChatFixture(t, "ok", WithMaxConcurrency(1))
	defer teardown()

	// Manually fill the single slot so the handler's acquire() returns
	// false. This mirrors what a real in-flight request would hold.
	release, ok := srv.semaphore.acquire(provider.PriorityNormal)
	if !ok {
		t.Fatal("could not acquire semaphore slot for test setup")
	}

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if env.Error.Code != "capacity" {
		t.Errorf("error.code = %q, want capacity", env.Error.Code)
	}

	// Release and retry — the next request should succeed.
	release()
	rec2 := doChat(t, srv, body)
	if rec2.Code != http.StatusOK {
		t.Fatalf("after release: status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
}

// TestChatCompletions_RequestIDFallback verifies that when the handler is
// invoked through a path that bypasses requestIDMiddleware, the response ID
// still gets a non-empty suffix. This guards against a bug where bypass
// paths would emit "chatcmpl_" verbatim.
func TestChatCompletions_RequestIDFallback(t *testing.T) {
	srv, _, _, teardown := newChatFixture(t, "ok")
	defer teardown()

	// Build a mux directly (no middleware chain) so requestIDMiddleware
	// never runs. This is the bypass path the fallback protects.
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+srv.basePath+"/chat/completions", srv.handleChatCompletions)

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Exact shape: "chatcmpl_" + 16 hex chars (8 random bytes). The regex
	// rejects the historical "chatcmpl_cmpl_<hex>" double-prefix bug that
	// arose when fallbackRequestID returned its own "cmpl_" prefix.
	idRE := regexp.MustCompile(`^chatcmpl_[0-9a-f]{16}$`)
	if !idRE.MatchString(out.ID) {
		t.Errorf("id = %q, want match %s", out.ID, idRE)
	}
}

// TestChatCompletions_AffinityKeyForwarded verifies that a request with
// x_affinity_key set completes successfully. The provider.ChatRequest that
// the mock receives does not carry AffinityKey (plumbing stays at the
// RoutingRequest layer) so we cannot assert the string directly at the HTTP
// boundary; tests in package provider cover the AffinityKey -> sticky
// cache plumbing. Here we assert only that the wire field is accepted and
// the request executes end-to-end.
func TestChatCompletions_AffinityKeyForwarded(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"x_affinity_key": "session-42",
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("provider Chat called %d times, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Unit tests for helpers
// ---------------------------------------------------------------------------

func TestStopSequences_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"array", `["a","b"]`, []string{"a", "b"}},
		{"single", `"END"`, []string{"END"}},
		{"empty-string", `""`, nil},
		{"null", `null`, nil},
		// Array branch: empty strings are filtered out so providers never
		// see a nil-like sentinel that would match at every token.
		{"array-with-empties", `["", "b", ""]`, []string{"b"}},
		{"array-all-empty", `[""]`, nil},
		{"array-empty", `[]`, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var s StopSequences
			if err := json.Unmarshal([]byte(tc.in), &s); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.in, err)
			}
			if !equalStrings([]string(s), tc.want) {
				t.Errorf("got %v, want %v", []string(s), tc.want)
			}
		})
	}
}

func TestStopSequences_UnmarshalJSON_Invalid(t *testing.T) {
	var s StopSequences
	err := json.Unmarshal([]byte(`123`), &s)
	if err == nil {
		t.Fatal("want error on numeric input")
	}
	if !strings.Contains(err.Error(), "stop must be string or") {
		t.Errorf("error = %v, want message about string or []string", err)
	}
}

func TestResolvePriority(t *testing.T) {
	cases := []struct {
		name string
		in   *int
		def  provider.Priority
		want provider.Priority
	}{
		{"nil uses default", nil, provider.PriorityNormal, provider.PriorityNormal},
		{"explicit high", intPtr(int(provider.PriorityHigh)), provider.PriorityNormal, provider.PriorityHigh},
		{"negative clamps to background", intPtr(-5), provider.PriorityNormal, provider.PriorityBackground},
		{"overflow clamps to critical", intPtr(999), provider.PriorityNormal, provider.PriorityCritical},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePriority(tc.in, tc.def)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSelectorFor(t *testing.T) {
	cases := []struct {
		in   provider.ModelKey
		want string
	}{
		{provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}, "ollama/qwen3:8b"},
		{provider.ModelKey{Provider: "", Model: "qwen3:8b"}, "qwen3:8b"},
	}
	for _, tc := range cases {
		got := selectorFor(tc.in)
		if got != tc.want {
			t.Errorf("selectorFor(%+v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "c"); got != "c" {
		t.Errorf("got %q, want c", got)
	}
	if got := firstNonEmpty("a", "b"); got != "a" {
		t.Errorf("got %q, want a", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// small utilities
// ---------------------------------------------------------------------------

func intPtr(v int) *int { return &v }

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
