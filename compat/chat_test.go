package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestChatCompletions_StreamReturns501(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "n/a")
	defer teardown()

	body := map[string]any{
		"model": "ollama/qwen3:8b",
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"stream": true,
	}
	rec := doChat(t, srv, body)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("provider Chat called %d times, want 0", got)
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
	if out.ID == "chatcmpl_" {
		t.Errorf("id = %q, want non-empty suffix via fallback", out.ID)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl_") {
		t.Errorf("id = %q, want chatcmpl_ prefix", out.ID)
	}
	if len(out.ID) <= len("chatcmpl_") {
		t.Errorf("id = %q, want suffix longer than empty", out.ID)
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
