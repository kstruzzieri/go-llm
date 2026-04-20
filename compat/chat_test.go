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
