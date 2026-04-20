package compat

import (
	"bytes"
	"context"
	"encoding/json"
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

// newCompletionFixture returns a Server wired to a provider whose Generate
// handler records the last request received and replies with the supplied
// text. The returned pointer to the captured request is updated on every
// call. Registered model grants CapGenerate|CapInsert via the "insert" runtime
// capability label so both the generate and FIM branches route successfully.
func newCompletionFixture(t *testing.T, text string, opts ...Option) (*Server, *provider.GenerateRequest, *int32, func()) {
	t.Helper()
	var last provider.GenerateRequest
	var calls int32
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			// "insert" grants CapGenerate|CapStream|CapInsert; combined with
			// the catalog entry for qwen3 (completion+tools) the merged
			// profile satisfies FIM routing requirements.
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genFn: func(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
			atomic.AddInt32(&calls, 1)
			last = req
			return &provider.GenerateResponse{
				Model:    req.Model,
				Provider: "ollama",
				Response: text,
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

// doCompletion POSTs a JSON body to /v1/completions and returns the recorder.
func doCompletion(t *testing.T, srv *Server, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	srv.buildHandler().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Core paths
// ---------------------------------------------------------------------------

// TestCompletions_GenerateMode exercises the non-FIM branch: a plain prompt
// with no suffix yields a "generate" use case and CapGenerate-only required
// caps. The response must carry the qualified provider/model identifier and
// a "cmpl_" ID prefix.
func TestCompletions_GenerateMode(t *testing.T) {
	srv, last, calls, teardown := newCompletionFixture(t, "hello from mock")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "finish this",
	}
	rec := doCompletion(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("provider Generate called %d times, want 1", got)
	}
	if last.Prompt != "finish this" {
		t.Errorf("provider saw prompt = %q, want %q", last.Prompt, "finish this")
	}
	if last.Suffix != "" {
		t.Errorf("provider saw suffix = %q, want empty (generate branch)", last.Suffix)
	}

	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "text_completion" {
		t.Errorf("object = %q, want text_completion", out.Object)
	}
	if !strings.HasPrefix(out.ID, "cmpl_") {
		t.Errorf("id = %q, want cmpl_ prefix", out.ID)
	}
	if !strings.HasPrefix(out.CompletionID, "cmpl_") {
		t.Errorf("x_completion_id = %q, want cmpl_ prefix", out.CompletionID)
	}
	if out.Model != "ollama/qwen3:8b" {
		t.Errorf("response.model = %q, want ollama/qwen3:8b (qualified)", out.Model)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	if out.Choices[0].Text != "hello from mock" {
		t.Errorf("choices[0].text = %q", out.Choices[0].Text)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("choices[0].finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
}

// TestCompletions_FIMMode verifies that a non-empty Suffix triggers the FIM
// branch: UseCase="fim" and CapGenerate|CapInsert required (we prove the
// latter indirectly by successful routing to a model that only satisfies
// the gate when CapInsert is granted). The provider must see the suffix.
func TestCompletions_FIMMode(t *testing.T) {
	srv, last, calls, teardown := newCompletionFixture(t, "def foo(): pass")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "def foo(",
		"suffix": "\n    return None",
	}
	rec := doCompletion(t, srv, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("provider Generate called %d times, want 1", got)
	}
	if last.Prompt != "def foo(" {
		t.Errorf("provider saw prompt = %q", last.Prompt)
	}
	if last.Suffix != "\n    return None" {
		t.Errorf("provider saw suffix = %q, want FIM suffix forwarded", last.Suffix)
	}
}

// TestCompletions_FIMBudgetTruncatesBeforeProvider verifies that applyFIMBudget
// shrinks the prompt BEFORE the provider is invoked when max_tokens is set.
// Using a 2000-char prompt, a short suffix, and max_tokens=16, the computed
// budget for qwen3+python is 75% prefix − 10% lang = 65%, so the prefix
// budget is ~10 tokens (~40 chars). The provider must see a trimmed prompt.
func TestCompletions_FIMBudgetTruncatesBeforeProvider(t *testing.T) {
	srv, last, _, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	longPrompt := strings.Repeat("abcd", 500) // 2000 chars, tail-preserving truncation
	shortSuffix := "short"
	body := map[string]any{
		"model":      "ollama/qwen3:8b",
		"prompt":     longPrompt,
		"suffix":     shortSuffix,
		"max_tokens": 16,
		"x_language": "python",
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// Provider should have received a truncated prompt. Exact length depends
	// on budgetForProfile's rounding; we only assert strictly less than the
	// original so the test is robust to small budget-policy tweaks.
	if len(last.Prompt) >= len(longPrompt) {
		t.Errorf("provider saw prompt of len %d, expected < %d (budget truncation didn't fire)",
			len(last.Prompt), len(longPrompt))
	}
	// Tail-preserving: the LAST character of the original prompt must still
	// be present after truncation.
	if !strings.HasSuffix(last.Prompt, "abcd") {
		t.Errorf("provider saw prompt tail = %q, want tail preserved", last.Prompt[len(last.Prompt)-4:])
	}
	// Suffix is shorter than its budget share so it is preserved intact.
	if last.Suffix != shortSuffix {
		t.Errorf("provider saw suffix = %q, want %q (short enough to preserve)", last.Suffix, shortSuffix)
	}
}

// TestCompletions_PromptAsArray exercises the array-form prompt decoder: the
// handler must collapse ["first","second"] to its first non-empty element.
func TestCompletions_PromptAsArray(t *testing.T) {
	srv, last, _, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": []string{"first", "second"},
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if last.Prompt != "first" {
		t.Errorf("provider saw prompt = %q, want first", last.Prompt)
	}
}

// TestCompletions_PromptAsString exercises the single-string prompt decoder.
func TestCompletions_PromptAsString(t *testing.T) {
	srv, last, _, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "solo",
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if last.Prompt != "solo" {
		t.Errorf("provider saw prompt = %q, want solo", last.Prompt)
	}
}

// TestCompletions_StopAsSingleString verifies the shared StopSequences decoder
// accepts a bare string.
func TestCompletions_StopAsSingleString(t *testing.T) {
	srv, last, _, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stop":   "END",
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if len(last.Options.Stop) != 1 || last.Options.Stop[0] != "END" {
		t.Errorf("provider saw Options.Stop = %v, want [END]", last.Options.Stop)
	}
}

// TestCompletions_DryRunReturnsRouteMetadata verifies that x_dry_run=true
// short-circuits before the provider is invoked: the handler returns route
// metadata with empty choice text and the Generate mock is never called.
func TestCompletions_DryRunReturnsRouteMetadata(t *testing.T) {
	var calls int32
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genFn: func(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
			atomic.AddInt32(&calls, 1)
			// If this ever runs on a dry-run, we want the test to see it as
			// a failed assertion — panicking here surfaces cleanly via the
			// recovery middleware as a 500, making the wrong-behavior loud.
			panic("provider.Generate called on dry-run path")
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	body := map[string]any{
		"model":     "ollama/qwen3:8b",
		"prompt":    "should not execute",
		"x_dry_run": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Errorf("provider Generate called %d times on dry-run, want 0", got)
	}
	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1 (dry-run single empty choice)", len(out.Choices))
	}
	if out.Choices[0].Text != "" {
		t.Errorf("choices[0].text = %q, want empty on dry-run", out.Choices[0].Text)
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing on dry-run")
	}
	if out.RouteInfo.ActualModel != "ollama/qwen3:8b" {
		t.Errorf("route.actual_model = %q, want ollama/qwen3:8b", out.RouteInfo.ActualModel)
	}
	if out.RouteInfo.PlannedModel != out.RouteInfo.ActualModel {
		t.Errorf("dry-run planned=%q actual=%q, expected equal",
			out.RouteInfo.PlannedModel, out.RouteInfo.ActualModel)
	}
}

// TestCompletions_AffinityKeyForwarded verifies that a request carrying
// x_affinity_key executes successfully and produces a populated x_route_info
// block — proving the field was accepted by the handler and the request
// reached the router. Exact sticky-cache behavior is covered by provider-
// package tests.
func TestCompletions_AffinityKeyForwarded(t *testing.T) {
	srv, _, calls, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model":          "ollama/qwen3:8b",
		"prompt":         "hi",
		"x_affinity_key": "file.py",
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("provider Generate called %d times, want 1", got)
	}
	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing; affinity key request should still populate it")
	}
}

// TestCompletions_EmptyPromptAndSuffixRejected guards the empty-input
// validation: a request with no prompt and no suffix is meaningless (nothing
// to complete) and must fail with a clear 400 rather than propagating to the
// provider.
func TestCompletions_EmptyPromptAndSuffixRejected(t *testing.T) {
	srv, _, _, teardown := newCompletionFixture(t, "n/a")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "",
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "empty_prompt" {
		t.Errorf("error.code = %q, want empty_prompt", env.Error.Code)
	}
}

// newCompletionFixtureWithResponse lets the caller shape the GenerateResponse
// returned by the mock provider. Used for finish_reason tests that need
// specific usage counts.
func newCompletionFixtureWithResponse(
	t *testing.T,
	build func(provider.GenerateRequest) *provider.GenerateResponse,
	opts ...Option,
) (*Server, func()) {
	t.Helper()
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genFn: func(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
			return build(req), nil
		},
	}
	srv, teardown := newTestServer(t, mp, opts...)
	return srv, teardown
}

// TestCompletions_FinishReasonLength exercises the length-cap branch: when
// max_tokens was set and the mock's reported CompletionTokens meets the cap,
// finish_reason must be "length" rather than the default "stop".
func TestCompletions_FinishReasonLength(t *testing.T) {
	srv, teardown := newCompletionFixtureWithResponse(t, func(req provider.GenerateRequest) *provider.GenerateResponse {
		return &provider.GenerateResponse{
			Model:    req.Model,
			Provider: "ollama",
			Response: "truncated",
			Done:     true,
			Usage:    provider.Usage{PromptTokens: 3, CompletionTokens: 8, TotalTokens: 11},
		}
	})
	defer teardown()

	body := map[string]any{
		"model":      "ollama/qwen3:8b",
		"prompt":     "write a long essay",
		"max_tokens": 8,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", out.Choices[0].FinishReason)
	}
}

// TestCompletions_FinishReasonStop covers the default: usage well under the
// cap yields finish_reason="stop".
func TestCompletions_FinishReasonStop(t *testing.T) {
	srv, teardown := newCompletionFixtureWithResponse(t, func(req provider.GenerateRequest) *provider.GenerateResponse {
		return &provider.GenerateResponse{
			Model:    req.Model,
			Provider: "ollama",
			Response: "done",
			Done:     true,
			Usage:    provider.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8},
		}
	})
	defer teardown()

	body := map[string]any{
		"model":      "ollama/qwen3:8b",
		"prompt":     "short",
		"max_tokens": 100,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", out.Choices[0].FinishReason)
	}
}

// TestCompletions_RequestIDFallback verifies that when the handler is invoked
// through a path that bypasses requestIDMiddleware, the response ID gets a
// generated random-hex suffix rather than the bare "cmpl_" string. This also
// guards against the accidental "cmpl_cmpl_<hex>" double-prefix regression
// that would arise from reusing fallbackRequestID() (which has its own cmpl_
// prefix).
func TestCompletions_RequestIDFallback(t *testing.T) {
	srv, _, _, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	// Build a mux directly (no middleware chain) so requestIDMiddleware
	// never runs.
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+srv.basePath+"/completions", srv.handleCompletions)

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var out CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Exact shape: "cmpl_" + 16 hex chars (8 random bytes).
	idRE := regexp.MustCompile(`^cmpl_[0-9a-f]{16}$`)
	if !idRE.MatchString(out.ID) {
		t.Errorf("id = %q, want match %s", out.ID, idRE)
	}
}

// TestCompletions_StreamStubReturns501 pins the current streaming behavior:
// stream=true yields 501 not_implemented until Task 13 replaces the stub with
// the real SSE writer. Task 13 will delete this test as it wires in the real
// streaming path.
func TestCompletions_StreamStubReturns501(t *testing.T) {
	srv, _, calls, teardown := newCompletionFixture(t, "ok")
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(calls); got != 0 {
		t.Errorf("provider Generate called %d times for stream stub, want 0", got)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode 501 body: %v", err)
	}
	if env.Error.Code != "not_implemented" {
		t.Errorf("error.code = %q, want not_implemented", env.Error.Code)
	}
}

// ---------------------------------------------------------------------------
// Unit tests for helpers
// ---------------------------------------------------------------------------

func TestPromptUnion_UnmarshalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"array", `["a","b"]`, []string{"a", "b"}},
		{"single", `"solo"`, []string{"solo"}},
		{"empty-string", `""`, nil},
		{"null", `null`, nil},
		{"array-with-empties", `["", "b", ""]`, []string{"b"}},
		{"array-all-empty", `[""]`, nil},
		{"array-empty", `[]`, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var p PromptUnion
			if err := json.Unmarshal([]byte(tc.in), &p); err != nil {
				t.Fatalf("unmarshal %q: %v", tc.in, err)
			}
			if !equalStrings([]string(p), tc.want) {
				t.Errorf("got %v, want %v", []string(p), tc.want)
			}
		})
	}
}

func TestPromptUnion_UnmarshalJSON_Invalid(t *testing.T) {
	var p PromptUnion
	err := json.Unmarshal([]byte(`123`), &p)
	if err == nil {
		t.Fatal("want error on numeric input")
	}
	if !strings.Contains(err.Error(), "prompt must be string or") {
		t.Errorf("error = %v, want message about string or []string", err)
	}
}

func TestPromptUnion_String(t *testing.T) {
	cases := []struct {
		name string
		p    PromptUnion
		want string
	}{
		{"empty", nil, ""},
		{"single", PromptUnion{"solo"}, "solo"},
		{"first non-empty", PromptUnion{"", "", "third"}, "third"},
		{"all empty", PromptUnion{"", ""}, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}
