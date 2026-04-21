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
// generated random-hex suffix rather than the bare "cmpl_" string. The regex
// also pins the exact "cmpl_<16 hex>" shape so a future change to
// fallbackRequestID that reintroduces a prefix would break the assertion.
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

// ---------------------------------------------------------------------------
// Streaming path
// ---------------------------------------------------------------------------

// newStreamingCompletionServer wires a server whose provider scripts
// GenerateStream directly. The `build` callback lets each test drive its own
// chunk sequence. The registered model grants CapGenerate|CapInsert|CapStream
// so both generate and FIM streaming routes resolve.
func newStreamingCompletionServer(
	t *testing.T,
	build func(ctx context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error,
	opts ...Option,
) (*Server, func()) {
	t.Helper()
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genStreamFn: build,
	}
	return newTestServer(t, mp, opts...)
}

// decodeCompletionSSEChunks parses an SSE body into individual CompletionChunk
// payloads. It skips the [DONE] sentinel and any non-"data:" lines.
func decodeCompletionSSEChunks(t *testing.T, body string) []CompletionChunk {
	t.Helper()
	var chunks []CompletionChunk
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var c CompletionChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}
	return chunks
}

// TestCompletions_Streaming exercises the happy path: two chunks "4" then "2"
// from the provider (the second with Done=true) produce two SSE events
// followed by "data: [DONE]". The first chunk must carry text="4" with no
// finish_reason, the second must carry text="2" and finish_reason="stop".
func TestCompletions_Streaming(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		if err := fn(provider.GenerateResponse{Model: req.Model, Response: "4"}); err != nil {
			return err
		}
		return fn(provider.GenerateResponse{Model: req.Model, Response: "2", Done: true})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "what is 4+38?",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
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
	if !strings.Contains(text, `"text":"4"`) {
		t.Errorf("first chunk text %q not found in body: %q", "4", text)
	}
	if !strings.Contains(text, `"text":"2"`) {
		t.Errorf("second chunk text %q not found in body: %q", "2", text)
	}

	chunks := decodeCompletionSSEChunks(t, text)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(chunks), text)
	}
	c0 := chunks[0]
	if c0.Object != "text_completion" {
		t.Errorf("chunk[0].object = %q, want text_completion", c0.Object)
	}
	if !strings.HasPrefix(c0.ID, "cmpl_") {
		t.Errorf("chunk[0].id = %q, want cmpl_ prefix", c0.ID)
	}
	if c0.Model != "ollama/qwen3:8b" {
		t.Errorf("chunk[0].model = %q, want ollama/qwen3:8b (qualified)", c0.Model)
	}
	if len(c0.Choices) != 1 {
		t.Fatalf("chunk[0].choices = %d", len(c0.Choices))
	}
	if c0.Choices[0].Text != "4" {
		t.Errorf("chunk[0].text = %q, want 4", c0.Choices[0].Text)
	}
	if c0.Choices[0].FinishReason != nil {
		t.Errorf("chunk[0].finish_reason = %v, want nil", *c0.Choices[0].FinishReason)
	}
	c1 := chunks[1]
	if c1.Choices[0].Text != "2" {
		t.Errorf("chunk[1].text = %q, want 2", c1.Choices[0].Text)
	}
	if c1.Choices[0].FinishReason == nil || *c1.Choices[0].FinishReason != "stop" {
		t.Errorf("chunk[1].finish_reason = %v, want stop", c1.Choices[0].FinishReason)
	}
	if c1.Model != "ollama/qwen3:8b" {
		t.Errorf("chunk[1].model = %q, want ollama/qwen3:8b (qualified)", c1.Model)
	}
}

// TestCompletions_Streaming_FinishReasonLength covers the length-cap branch of
// streaming finish_reason derivation: max_tokens=8 and usage reporting
// CompletionTokens=8 on the Done chunk must yield "length".
func TestCompletions_Streaming_FinishReasonLength(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		return fn(provider.GenerateResponse{
			Model:    req.Model,
			Response: "truncated",
			Done:     true,
			Usage:    provider.Usage{PromptTokens: 3, CompletionTokens: 8, TotalTokens: 11},
		})
	})
	defer teardown()

	body := map[string]any{
		"model":      "ollama/qwen3:8b",
		"prompt":     "write a long essay",
		"stream":     true,
		"max_tokens": 8,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeCompletionSSEChunks(t, rec.Body.String())
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	final := chunks[0]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", final.Choices[0].FinishReason)
	}
}

// TestCompletions_Streaming_ErrorBeforeFirstChunk verifies the lazy-start
// guarantee: when the provider's GenerateStream fails before invoking fn even
// once, the client sees a normal JSON error envelope (not a partial SSE
// stream). The status is 502 because an unclassified provider error maps
// through statusForCompatError to upstream_error.
func TestCompletions_Streaming_ErrorBeforeFirstChunk(t *testing.T) {
	wantErr := errors.New("synthetic upstream failure")
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, _ provider.GenerateRequest, _ func(provider.GenerateResponse) error) error {
		// Never invoke fn — fail immediately. The SSE writer's lazy-start
		// means no "data: ..." bytes have been committed yet.
		return wantErr
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (JSON error envelope)", ct)
	}
	if strings.Contains(rec.Body.String(), "data:") {
		t.Errorf("body unexpectedly contains SSE data line: %q", rec.Body.String())
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != "upstream_error" {
		t.Errorf("error.code = %q, want upstream_error", env.Error.Code)
	}
}

// TestCompletions_Streaming_ErrorAfterFirstChunk guards the HIGH-severity
// silent-truncation fix: when the provider emits one chunk successfully then
// fails mid-stream (non-cancellation error), the handler MUST NOT emit
// "data: [DONE]". The [DONE] sentinel is OpenAI's success signal — emitting
// it under error conditions silently masks the truncation. Its absence lets
// OpenAI SDKs detect premature EOS.
func TestCompletions_Streaming_ErrorAfterFirstChunk(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		if err := fn(provider.GenerateResponse{Model: req.Model, Response: "partial"}); err != nil {
			return err
		}
		return errors.New("upstream collapsed")
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)

	// Status 200 + event-stream headers are committed because the first chunk
	// was written successfully. The error occurs after commit.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers committed on first chunk), body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	text := rec.Body.String()
	if !strings.Contains(text, `"text":"partial"`) {
		t.Errorf("body missing first chunk payload: %q", text)
	}
	// Critical assertion: no [DONE] sentinel. Its absence is the wire-level
	// signal of premature EOS that clients rely on.
	if strings.Contains(text, "data: [DONE]") {
		t.Errorf("body unexpectedly contains data: [DONE] after mid-stream error: %q", text)
	}
}

// TestCompletions_Streaming_ClientDisconnect verifies the handler returns
// cleanly when the caller's context is canceled mid-stream (IDE closing the
// completion, e.g., on the next keystroke). The semaphore slot must be
// released so subsequent requests are not blocked — we probe this by
// acquiring a fresh slot after the handler returns with MaxConcurrency=1.
func TestCompletions_Streaming_ClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		// Emit one chunk, then cancel the request context and return the
		// cancellation error — simulating a client disconnect detected by the
		// provider after it had already pushed a chunk.
		if err := fn(provider.GenerateResponse{Model: req.Model, Response: "tick"}); err != nil {
			return err
		}
		cancel()
		return context.Canceled
	}, WithMaxConcurrency(1))
	defer teardown()

	buf, err := json.Marshal(map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stream": true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", bytes.NewReader(buf)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	// Should not panic and should return (not hang).
	srv.buildHandler().ServeHTTP(rec, req)

	text := rec.Body.String()
	if !strings.Contains(text, `"text":"tick"`) {
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

// TestCompletions_Streaming_ModelQualified verifies that every streaming
// chunk carries the qualified "provider/model" form in the Model field,
// matching the non-streaming branch's plan.Profile.Key.String() convention.
// The provider returns a bare model name (the realistic Ollama shape); the
// compat layer must upgrade it to the qualified form.
func TestCompletions_Streaming_ModelQualified(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		return fn(provider.GenerateResponse{Model: req.Model, Response: "ok", Done: true})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "hi",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeCompletionSSEChunks(t, rec.Body.String())
	if len(chunks) == 0 {
		t.Fatalf("no chunks: %q", rec.Body.String())
	}
	for i, c := range chunks {
		if c.Model != "ollama/qwen3:8b" {
			t.Errorf("chunk[%d].model = %q, want ollama/qwen3:8b (qualified)", i, c.Model)
		}
	}
}

// TestCompletions_Streaming_NoDoneChunkSkipsDONE guards Fix 1 for the Task 13
// skeptic findings: when ExecuteGenerateStream returns nil (no error) but the
// provider never sent a chunk with Done=true, the handler must NOT emit
// "data: [DONE]". The sentinel is OpenAI's success signal — emitting it when
// the stream was never cleanly terminated makes SDKs silently report success
// on a truncated response. Withholding the sentinel preserves the wire-level
// premature-EOS signal clients depend on.
func TestCompletions_Streaming_NoDoneChunkSkipsDONE(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		// Clean return but the provider never set Done=true.
		_ = fn(provider.GenerateResponse{Model: req.Model, Response: "partial"})
		return nil
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "p",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	text := rec.Body.String()
	if !strings.Contains(text, `"text":"partial"`) {
		t.Errorf("body missing first chunk payload: %q", text)
	}
	if strings.Contains(text, "data: [DONE]") {
		t.Errorf("emitted [DONE] despite provider never sending Done chunk:\n%s", text)
	}
}

// TestCompletions_Streaming_StoreRecordOnCleanDone verifies that after a clean
// streaming completion (interim chunk + terminal Done chunk) the completion
// record store has a hit for the response's x_completion_id. This is the
// orphan-avoidance positive test.
func TestCompletions_Streaming_StoreRecordOnCleanDone(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		if err := fn(provider.GenerateResponse{Model: req.Model, Response: "a"}); err != nil {
			return err
		}
		return fn(provider.GenerateResponse{Model: req.Model, Response: "b", Done: true})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "p",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeCompletionSSEChunks(t, rec.Body.String())
	if len(chunks) == 0 {
		t.Fatalf("no chunks: %q", rec.Body.String())
	}
	last := chunks[len(chunks)-1]
	if last.CompletionID == "" {
		t.Fatalf("final chunk missing x_completion_id:\n%s", rec.Body.String())
	}
	rec2, ok := srv.completionStore.get(last.CompletionID)
	if !ok {
		t.Fatalf("completionStore.get(%q) miss; want hit after clean Done", last.CompletionID)
	}
	if rec2.Model != "ollama/qwen3:8b" {
		t.Errorf("record.Model = %q, want ollama/qwen3:8b", rec2.Model)
	}
}

// TestCompletions_Streaming_NoStoreRecordWithoutDone verifies that when the
// provider returns nil without ever sending a Done chunk, no completion
// record is written. The Task 15 feedback flow depends on a miss in this
// case (the stream was incomplete — feedback on a lost completion should
// not find an attribution record).
func TestCompletions_Streaming_NoStoreRecordWithoutDone(t *testing.T) {
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		_ = fn(provider.GenerateResponse{Model: req.Model, Response: "partial"})
		return nil
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "p",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// The handler doesn't expose the generated id on this path, so we assert
	// the store remained empty — equivalent and stricter.
	if got := srv.completionStore.list.Len(); got != 0 {
		t.Errorf("completionStore.list.Len() = %d; want 0 (no Done chunk = no record)", got)
	}
}

// TestCompletions_Streaming_ConfidenceOnDoneChunk guards Fix 2 for the Task 13
// skeptic findings: the streaming CompletionChunk must surface
// x_completion_id and x_confidence on the final (Done) frame, matching the
// non-streaming CompletionResponse. Interim frames must not carry either
// (omitempty + nil pointer).
func TestCompletions_Streaming_ConfidenceOnDoneChunk(t *testing.T) {
	score := 0.87
	srv, teardown := newStreamingCompletionServer(t, func(_ context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
		if err := fn(provider.GenerateResponse{Model: req.Model, Response: "a"}); err != nil {
			return err
		}
		return fn(provider.GenerateResponse{
			Model:      req.Model,
			Response:   "b",
			Done:       true,
			Confidence: &provider.CompletionConfidence{Score: score},
		})
	})
	defer teardown()

	body := map[string]any{
		"model":  "ollama/qwen3:8b",
		"prompt": "p",
		"stream": true,
	}
	rec := doCompletion(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	chunks := decodeCompletionSSEChunks(t, rec.Body.String())
	if len(chunks) < 2 {
		t.Fatalf("want >=2 chunks, got %d: %q", len(chunks), rec.Body.String())
	}
	// Interim chunk must NOT carry extensions — omitempty + nil pointer keeps
	// the wire frame minimal.
	if chunks[0].Confidence != nil {
		t.Errorf("interim chunk carried confidence: %v", *chunks[0].Confidence)
	}
	if chunks[0].CompletionID != "" {
		t.Errorf("interim chunk carried x_completion_id: %q", chunks[0].CompletionID)
	}
	last := chunks[len(chunks)-1]
	if last.Confidence == nil {
		t.Fatalf("done chunk missing x_confidence:\n%s", rec.Body.String())
	}
	if *last.Confidence != score {
		t.Errorf("confidence = %v, want %v", *last.Confidence, score)
	}
	if last.CompletionID == "" {
		t.Error("done chunk missing x_completion_id")
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

// TestCompletions_FIMBudgetZeroPrefixPreservesOriginal guards against a
// regression where applyFIMBudget silently emptied the FIM prefix when
// budgetForProfile returned a zero prefix budget (e.g. max_tokens=1): the old
// code computed keepChars=0 and sliced prompt[len(prompt)-0:] -> "". The fix
// preserves the original prefix and logs a breadcrumb.
func TestCompletions_FIMBudgetZeroPrefixPreservesOriginal(t *testing.T) {
	var seenPrompt, seenSuffix string
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genFn: func(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
			seenPrompt = req.Prompt
			seenSuffix = req.Suffix
			return &provider.GenerateResponse{Model: req.Model, Provider: "ollama", Response: "ok", Done: true}, nil
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	longPrefix := strings.Repeat("A", 400)
	body := `{"model":"ollama/qwen3:8b","prompt":"` + longPrefix + `","suffix":"B","max_tokens":1,"x_language":"python"}`
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seenPrompt == "" {
		t.Fatal("provider received empty prompt — budget truncated to zero silently")
	}
	if seenSuffix == "" {
		t.Fatal("provider received empty suffix")
	}
}

// TestCompletions_StopFiltersEmptyStrings covers the toModelOptions fix:
// {"stop": ""} and {"stop": ["",""]} must not forward "" to the provider,
// which Ollama interprets as "stop immediately" and silently truncates
// generation.
func TestCompletions_StopFiltersEmptyStrings(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"stop_as_empty_string", `{"model":"ollama/qwen3:8b","prompt":"p","stop":""}`},
		{"stop_as_array_of_empty", `{"model":"ollama/qwen3:8b","prompt":"p","stop":["",""]}`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var seenStop []string
			mp := &mockProvider{
				name: "ollama",
				caps: provider.CapGenerate | provider.CapStream,
				models: []provider.ModelInfo{
					{Name: "qwen3:8b", ContextWindow: 32768},
				},
				genFn: func(_ context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
					seenStop = req.Options.Stop
					return &provider.GenerateResponse{Model: req.Model, Provider: "ollama", Response: "x", Done: true}, nil
				},
			}
			srv, teardown := newTestServer(t, mp)
			defer teardown()
			rec := httptest.NewRecorder()
			srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(tc.body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(seenStop) != 0 {
				t.Errorf("provider received non-empty Stop after empty-only filter: %+v", seenStop)
			}
		})
	}
}

// TestCompletions_FIMDryRunDoesNotTruncate verifies that applyFIMBudget runs
// AFTER the dry-run short-circuit: on a dry-run, the mutation is skipped
// entirely (the provider never executes) and the metadata-only response
// carries an empty choice.
func TestCompletions_FIMDryRunDoesNotTruncate(t *testing.T) {
	var called int32
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapGenerate | provider.CapInsert | provider.CapStream,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"insert"}},
		},
		genFn: func(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
			atomic.AddInt32(&called, 1)
			return nil, nil
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	long := strings.Repeat("A", 2000)
	body := `{"model":"ollama/qwen3:8b","prompt":"` + long + `","suffix":"x","max_tokens":4,"x_dry_run":true}`
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&called); got != 0 {
		t.Fatalf("provider invoked on dry-run (calls=%d)", got)
	}
	var resp CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RouteInfo == nil {
		t.Fatal("dry-run missing route info")
	}
	// Sanity: dry-run should not itself write generated text.
	if len(resp.Choices) != 1 || resp.Choices[0].Text != "" {
		t.Errorf("dry-run emitted non-empty text: %+v", resp.Choices)
	}
}

// TestCompletions_DryRunReportsWasSticky verifies that writeCompletionDryRun
// surfaces RoutePlan.WasSticky() on the wire. The handler-level integration
// path (Router.Route with req.DryRun=true) deliberately bypasses the sticky
// cache in the Router (see router.go step 6: `req.AffinityKey != "" && !req.DryRun`),
// so integration testing is not possible today. We test the wire-shape
// responsibility of the compat layer directly: given a plan whose
// SetWasSticky(true) was called, writeCompletionDryRun must populate
// RouteInfo.WasSticky. Without the accessor+field wiring this fix added,
// RouteInfo.WasSticky would always be false regardless of plan state.
func TestCompletions_DryRunReportsWasSticky(t *testing.T) {
	key := provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	plan := &provider.RoutePlan{
		Profile: &provider.ModelProfile{Key: key, Family: "qwen3"},
		Score:   0.75,
		Reason:  "test",
	}
	plan.SetWasSticky(true)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
	writeCompletionDryRun(rec, r, plan)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp CompletionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.RouteInfo == nil {
		t.Fatal("dry-run missing route info")
	}
	if !resp.RouteInfo.WasSticky {
		t.Errorf("expected WasSticky=true, got RouteInfo=%+v", resp.RouteInfo)
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
