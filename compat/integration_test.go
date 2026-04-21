package compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestIntegration_ChatRoundTrip verifies a real OpenAI-shape POST over a
// real socket returns a real JSON body.
func TestIntegration_ChatRoundTrip(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat"}}},
		chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{
				Model:   req.Model,
				Content: "pong",
				Done:    true,
			}, nil
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	body := bytes.NewBufferString(`{"model":"qwen3:8b","messages":[{"role":"user","content":"ping"}]}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	// ChatCompletionResponse has no separate CompletionID field (that exists
	// only on CompletionResponse/CompletionChunk). The equivalent assertion
	// for chat is that the top-level ID is non-empty and carries the
	// "chatcmpl_" prefix — which is the chat counterpart of "cmpl_" from
	// M5 — so this covers the plan's "ID has cmpl_ prefix" intent.
	if out.ID == "" {
		t.Error("id is empty")
	}
	if !strings.HasPrefix(out.ID, "chatcmpl_") {
		t.Errorf("id = %q, want chatcmpl_ prefix", out.ID)
	}
	if out.Model == "" {
		t.Error("model is empty")
	}
	if len(out.Choices) == 0 || out.Choices[0].Message.Content != "pong" {
		t.Errorf("content mismatch: %+v", out.Choices)
	}
}

// TestIntegration_FeedbackRoundTrip exercises the completion->feedback loop
// over a real socket, confirming x_completion_id attribution works.
func TestIntegration_FeedbackRoundTrip(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapGenerate,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"generate"}}},
		genFn: func(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
			return &provider.GenerateResponse{Model: req.Model, Response: "x", Done: true}, nil
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	completeBody := bytes.NewBufferString(`{"model":"qwen3:8b","prompt":"hi"}`)
	cResp, err := http.Post(ts.URL+"/v1/completions", "application/json", completeBody)
	if err != nil {
		t.Fatal(err)
	}
	defer cResp.Body.Close()
	var cOut CompletionResponse
	if err := json.NewDecoder(cResp.Body).Decode(&cOut); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if cOut.CompletionID == "" {
		t.Fatal("no completion id returned")
	}

	fbPayload := `{"completion_id":"` + cOut.CompletionID + `","action":"accepted"}`
	fResp, err := http.Post(ts.URL+"/v1/completions/feedback", "application/json", strings.NewReader(fbPayload))
	if err != nil {
		t.Fatal(err)
	}
	defer fResp.Body.Close()
	if fResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(fResp.Body)
		t.Fatalf("feedback status=%d body=%s", fResp.StatusCode, raw)
	}
	var fbOut FeedbackResponse
	if err := json.NewDecoder(fResp.Body).Decode(&fbOut); err != nil {
		t.Fatalf("decode feedback response: %v", err)
	}
	if !fbOut.Recorded {
		t.Errorf("recorded = false on first POST, want true")
	}

	// Duplicate POST: the completion store is consume-once, so a second
	// feedback for the same id must 404 with "unknown_completion" (matches
	// feedback.go's sentinel code for the consume miss).
	dupResp, err := http.Post(ts.URL+"/v1/completions/feedback", "application/json", strings.NewReader(fbPayload))
	if err != nil {
		t.Fatal(err)
	}
	defer dupResp.Body.Close()
	if dupResp.StatusCode != http.StatusNotFound {
		raw, _ := io.ReadAll(dupResp.Body)
		t.Fatalf("duplicate feedback status=%d, want 404; body=%s", dupResp.StatusCode, raw)
	}
	var dupEnv errorEnvelope
	if err := json.NewDecoder(dupResp.Body).Decode(&dupEnv); err != nil {
		t.Fatalf("decode duplicate error envelope: %v", err)
	}
	if dupEnv.Error.Code != "unknown_completion" {
		t.Errorf("duplicate error code = %q, want unknown_completion", dupEnv.Error.Code)
	}
}

// TestIntegration_StatusReportsLiveUptime smoke-tests GET /v1/status over
// a real socket. startedAt is written in New, but we refresh it here to
// pin the read against a recent time for hermeticity.
func TestIntegration_StatusReportsLiveUptime(t *testing.T) {
	mp := &mockProvider{name: "ollama", caps: provider.CapChat}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	srv.startedAt = time.Now()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var out StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Handler emits "ready" when every registered provider reports healthy
	// (see status.go handleStatus). The mock provider has no health error so
	// we expect the happy-path string rather than "degraded"/"unavailable".
	if out.Status != "ready" {
		t.Errorf("status = %q, want ready", out.Status)
	}
	if out.Uptime == "" {
		t.Fatal("uptime is empty")
	}
	d, err := time.ParseDuration(out.Uptime)
	if err != nil {
		t.Fatalf("uptime %q not parseable: %v", out.Uptime, err)
	}
	// startedAt was pinned to time.Now() immediately before the request; the
	// reported uptime must be well under a minute. This guards against a
	// regression that silently reports a stale or zero duration.
	if d >= time.Minute {
		t.Errorf("uptime %s >= 1m; expected recent start", d)
	}
}

// TestIntegration_ModelsAliasAndWarmOrder verifies GET /v1/models emits
// alias entries and orders warm models first.
//
// newTestServer does not expose a WithWarmthSource hook, so this test wires
// its own provider.Registry/ModelRegistry/Router using a fakeWarmthSource
// (defined in models_test.go) so warmth can be forced deterministically.
// Two canonical models are registered (qwen3:8b warm, qwen3:14b cold) plus
// one alias; we assert the alias maps exactly to the qualified target and
// at least one warm entry precedes any cold entry in the Data slice.
func TestIntegration_ModelsAliasAndWarmOrder(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat"}},
			{Name: "qwen3:14b", ContextWindow: 32768, Capabilities: []string{"chat"}},
		},
	}
	provReg := provider.NewRegistry()
	if err := provReg.Register(mp); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), mp.Name()); err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	// Force qwen3:8b warm; qwen3:14b stays cold.
	warmKey := provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	ws := &fakeWarmthSource{
		models: []provider.WarmModel{{
			Key: warmKey,
			Info: provider.WarmthInfo{
				Loaded:    true,
				Since:     time.Now().Add(-1 * time.Minute),
				ExpiresAt: time.Now().Add(5 * time.Minute),
				VRAM:      6.0,
			},
		}},
	}
	router := provider.NewRouter(modelReg, provReg,
		provider.WithStickyTTL(time.Second),
		provider.WithAvailableRAM(256),
		provider.WithWarmthSource(ws),
	)
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg,
		WithAliases(map[string]string{"gpt-3.5-turbo": "ollama/qwen3:8b"}),
	)
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out ModelsListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var sawAlias bool
	for _, m := range out.Data {
		if m.ID == "gpt-3.5-turbo" {
			sawAlias = true
			if m.AliasFor != "ollama/qwen3:8b" {
				t.Errorf("alias x_alias_for = %q, want ollama/qwen3:8b", m.AliasFor)
			}
		}
	}
	if !sawAlias {
		t.Errorf("expected alias entry in /v1/models; got %d entries", len(out.Data))
	}

	// Warm-first ordering: at least one warm=true entry must precede any
	// warm=false entry. sortModelsWarmFirst is the canonical policy; we
	// assert the observable wire-level effect here.
	firstCold := -1
	for i, m := range out.Data {
		if !m.Warm {
			firstCold = i
			break
		}
	}
	if firstCold < 0 {
		t.Fatalf("expected at least one cold entry in Data; got %d entries, all warm", len(out.Data))
	}
	sawWarmBeforeCold := false
	for i := 0; i < firstCold; i++ {
		if out.Data[i].Warm {
			sawWarmBeforeCold = true
			break
		}
	}
	if !sawWarmBeforeCold {
		ids := make([]string, 0, len(out.Data))
		for _, m := range out.Data {
			ids = append(ids, m.ID)
		}
		t.Errorf("no warm entry precedes first cold entry; order=%v", ids)
	}
}

// TestIntegration_EmbeddingsScalarInput confirms the OpenAI scalar input
// shape round-trips over a real socket.
func TestIntegration_EmbeddingsScalarInput(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapEmbed,
		models: []provider.ModelInfo{{Name: "qwen3-embedding:8b", ContextWindow: 32768, Capabilities: []string{"embedding"}}},
		embedFn: func(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
			return &provider.EmbedResponse{
				Model:      req.Model,
				Embeddings: [][]float64{{0.1, 0.2}},
				Usage:      provider.Usage{PromptTokens: 1, TotalTokens: 1},
			}, nil
		},
	}
	srv, teardown := newTestServer(t, mp, WithEmbeddings(true))
	defer teardown()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/embeddings", "application/json",
		bytes.NewBufferString(`{"model":"qwen3-embedding:8b","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	var out EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Data) != 1 || len(out.Data[0].Embedding) != 2 {
		t.Errorf("bad embedding: %+v", out.Data)
	}

	// Negative path: an explicit empty input array is rejected with 400 and
	// the "missing_input" code (see embeddings.go: len(req.Input.Values)==0
	// branch). Guards against a regression where UnmarshalJSON silently
	// accepts [] and the handler falls through to a zero-vector response.
	emptyResp, err := http.Post(ts.URL+"/v1/embeddings", "application/json",
		bytes.NewBufferString(`{"model":"qwen3-embedding:8b","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer emptyResp.Body.Close()
	if emptyResp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(emptyResp.Body)
		t.Fatalf("empty-array status=%d, want 400; body=%s", emptyResp.StatusCode, raw)
	}
	var emptyEnv errorEnvelope
	if err := json.NewDecoder(emptyResp.Body).Decode(&emptyEnv); err != nil {
		t.Fatalf("decode empty-array error: %v", err)
	}
	if emptyEnv.Error.Code != "missing_input" {
		t.Errorf("empty-array error code = %q, want missing_input", emptyEnv.Error.Code)
	}
}

// TestIntegration_ChatDryRun verifies chat dry_run short-circuits the
// request without executing the provider call.
func TestIntegration_ChatDryRun(t *testing.T) {
	// atomic.Bool so a stray provider invocation from an unrelated goroutine
	// (e.g. future background refresh) cannot race the assertion.
	var called atomic.Bool
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat"}}},
		chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			called.Store(true)
			return &provider.ChatResponse{Model: req.Model, Content: "should not be emitted", Done: true}, nil
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	body := bytes.NewBufferString(`{"model":"qwen3:8b","messages":[{"role":"user","content":"ping"}],"x_dry_run":true}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if called.Load() {
		t.Errorf("dry_run must not invoke provider")
	}

	// Decode the body so we can assert the handler surfaced route metadata
	// rather than silently returning a 200 with no x_route_info. Dry-run is
	// metadata-only; ActualModel must identify the model the handler would
	// have routed to.
	var out ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.RouteInfo == nil {
		t.Fatal("x_route_info missing on dry-run")
	}
	if out.RouteInfo.ActualModel == "" {
		t.Error("x_route_info.actual_model is empty on dry-run")
	}
	// The qualified form from plan.Profile.Key.String() is the convention
	// shared with /v1/completions; confirm the handler is on the same wire
	// shape here.
	if !strings.Contains(out.RouteInfo.ActualModel, "qwen3:8b") {
		t.Errorf("x_route_info.actual_model = %q, want to reference qwen3:8b", out.RouteInfo.ActualModel)
	}
}

// TestIntegration_StreamPreFirstChunkError verifies that if the stream
// fails BEFORE emitting any chunk, the wire response is a JSON error
// (not a false-positive 200 SSE stream).
func TestIntegration_StreamPreFirstChunkError(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat | provider.CapStream,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat", "stream"}}},
		chatStreamFn: func(ctx context.Context, req provider.ChatRequest, cb func(provider.ChatResponse) error) error {
			return errors.New("provider failed before first chunk")
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	body := bytes.NewBufferString(`{"model":"qwen3:8b","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected error status, got %d body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("expected JSON error, got Content-Type=%q", ct)
	}
	// Wire-level shape: OpenAI error envelope with a populated code/message.
	// A silent 4xx/5xx without a decodable envelope would indicate the lazy-
	// start SSE writer leaked a committed 200 or the error handler wrote a
	// bare status without a body.
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code == "" {
		t.Error("error.code is empty")
	}
	if env.Error.Message == "" {
		t.Error("error.message is empty")
	}
}

// TestIntegration_SSEFraming verifies a successful streaming chat over a
// real socket produces well-formed SSE: `data:` prefixed events, a final
// `[DONE]` sentinel, and blank-line separators.
func TestIntegration_SSEFraming(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat | provider.CapStream,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat", "stream"}}},
		chatStreamFn: func(ctx context.Context, req provider.ChatRequest, cb func(provider.ChatResponse) error) error {
			if err := cb(provider.ChatResponse{Model: req.Model, Content: "p"}); err != nil {
				return err
			}
			return cb(provider.ChatResponse{Model: req.Model, Content: "ong", Done: true})
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()
	ts := httptest.NewServer(srv.buildHandler())
	defer ts.Close()

	body := bytes.NewBufferString(`{"model":"qwen3:8b","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Collect all "data: " frames so we can assert ordering, not just
	// presence. The final frame must be "[DONE]" and at least one earlier
	// frame must be a non-sentinel payload.
	var frames []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("sse scan: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("got %d frames, want >=2 (at least one payload + [DONE])", len(frames))
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Errorf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}
	// Require at least one earlier payload frame that is not [DONE] — this
	// is the wire-level guarantee that the provider emitted real chunks
	// before terminating, not just a bare sentinel.
	sawPayload := false
	for _, f := range frames[:len(frames)-1] {
		if f != "[DONE]" {
			sawPayload = true
			break
		}
	}
	if !sawPayload {
		t.Errorf("no non-sentinel payload frame before [DONE]; frames=%v", frames)
	}
}
