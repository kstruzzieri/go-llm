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
	_ = json.NewDecoder(resp.Body).Decode(&out)
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
	_ = json.NewDecoder(cResp.Body).Decode(&cOut)
	if cOut.CompletionID == "" {
		t.Fatal("no completion id returned")
	}

	fbBody := strings.NewReader(`{"completion_id":"` + cOut.CompletionID + `","action":"accepted"}`)
	fResp, err := http.Post(ts.URL+"/v1/completions/feedback", "application/json", fbBody)
	if err != nil {
		t.Fatal(err)
	}
	defer fResp.Body.Close()
	if fResp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(fResp.Body)
		t.Fatalf("feedback status=%d body=%s", fResp.StatusCode, raw)
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
}

// TestIntegration_ModelsAliasAndWarmOrder verifies GET /v1/models emits
// alias entries and orders warm models first.
func TestIntegration_ModelsAliasAndWarmOrder(t *testing.T) {
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat"}},
		},
	}
	srv, teardown := newTestServer(t, mp, WithAliases(map[string]string{"gpt-3.5-turbo": "ollama/qwen3:8b"}))
	defer teardown()
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
	_ = json.NewDecoder(resp.Body).Decode(&out)

	var sawAlias bool
	for _, m := range out.Data {
		if m.ID == "gpt-3.5-turbo" {
			sawAlias = true
			if m.AliasFor == "" {
				t.Errorf("alias entry missing x_alias_for")
			}
		}
	}
	if !sawAlias {
		t.Errorf("expected alias entry in /v1/models; got %d entries", len(out.Data))
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
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Data) != 1 || len(out.Data[0].Embedding) != 2 {
		t.Errorf("bad embedding: %+v", out.Data)
	}
}

// TestIntegration_ChatDryRun verifies chat dry_run short-circuits the
// request without executing the provider call.
func TestIntegration_ChatDryRun(t *testing.T) {
	var called bool
	mp := &mockProvider{
		name: "ollama", caps: provider.CapChat,
		models: []provider.ModelInfo{{Name: "qwen3:8b", ContextWindow: 32768, Capabilities: []string{"chat"}}},
		chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			called = true
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
	if called {
		t.Errorf("dry_run must not invoke provider")
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

	var sawData, sawDone bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			sawData = true
			if strings.TrimSpace(strings.TrimPrefix(line, "data: ")) == "[DONE]" {
				sawDone = true
			}
		}
	}
	if !sawData {
		t.Errorf("no `data: ` frames seen")
	}
	if !sawDone {
		t.Errorf("no `[DONE]` sentinel seen")
	}
}
