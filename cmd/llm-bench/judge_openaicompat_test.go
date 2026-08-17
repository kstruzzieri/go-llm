package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// TestOpenAICompatJudgeClient_ChatTranslatesRequestAndResponse drives the
// adapter through a real *openaicompat.Provider over httptest: the ollama
// judge request must translate to an OpenAI /v1/chat/completions call
// (messages, temperature, max_tokens preserved) and the OpenAI response
// content must map back onto ollama.ChatResponse.Message.Content so the
// existing judge parser sees an unchanged contract.
func TestOpenAICompatJudgeClient_ChatTranslatesRequestAndResponse(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q; want /v1/chat/completions", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer_quality\":1.0,\"justification\":\"good\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer srv.Close()

	judge := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))

	resp, err := judge.Chat(context.Background(), ollama.ChatRequest{
		Model: "claude-x",
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "you are a judge"},
			{Role: "user", Content: "evaluate this"},
		},
		Format:  "json",
		Options: &ollama.ModelOptions{Temperature: provider.Ptr(0.1), NumPredict: 512},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp == nil || resp.Message.Content != `{"answer_quality":1.0,"justification":"good"}` {
		t.Fatalf("unexpected response: %+v", resp)
	}

	if gotBody["model"] != "claude-x" {
		t.Fatalf("model = %v; want claude-x", gotBody["model"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d; want 2 (system+user)", len(msgs))
	}
	if gotBody["temperature"] != 0.1 {
		t.Fatalf("temperature = %v; want 0.1", gotBody["temperature"])
	}
	if gotBody["max_tokens"] != float64(512) {
		t.Fatalf("max_tokens = %v; want 512", gotBody["max_tokens"])
	}
}

// TestOpenAICompatJudgeClient_AvailableModels confirms the model-checker seam
// lists models via /v1/models so newScorer's judge-model validation works on
// the openai-compat transport.
func TestOpenAICompatJudgeClient_AvailableModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q; want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"},{"id":"gpt-4o"}]}`))
	}))
	defer srv.Close()

	judge := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))
	models, err := judge.AvailableModels(context.Background())
	if err != nil {
		t.Fatalf("AvailableModels: %v", err)
	}
	if len(models) != 2 || models[0] != "claude-x" || models[1] != "gpt-4o" {
		t.Fatalf("models = %v; want [claude-x gpt-4o]", models)
	}
}

// TestLLMJudgeScorer_OpenAICompatTransport_ProducesVerdictPreservingContract
// drives the whole judge path through the openai-compat adapter: Score must
// produce a parsed verdict, AND the judge system prompt must reach the wire
// unchanged. This is the integration the unit tests skip — the adapter is
// tested alone and Score is tested with a fake client, but their seam (and the
// preserved prompt contract) is only exercised here.
func TestLLMJudgeScorer_OpenAICompatTransport_ProducesVerdictPreservingContract(t *testing.T) {
	var sawSystemPrompt bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		for _, m := range body.Messages {
			if m.Role == "system" && m.Content == judgeSystemPrompt {
				sawSystemPrompt = true
			}
		}
		_, _ = w.Write([]byte(`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer_quality\":1.0,\"justification\":\"matches rubric\"}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	adapter := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))
	scorer := &LLMJudgeScorer{Client: adapter, JudgeModel: "claude-x", JudgeProvider: "openai-compat"}
	trace := Trace{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "answer must be correct"}}
	actual := Result{Model: "ollama/cand", Transcript: []Turn{{Role: "assistant", Content: "the answer"}}}

	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want 1.0", score.AnswerQuality)
	}
	if !sawSystemPrompt {
		t.Fatalf("judge system prompt not delivered through openai-compat transport (prompt contract not preserved)")
	}
}

// newReasoningFallbackFixture builds a scorer wired through a real
// openai-compat provider against a server that always answers with respBody,
// plus a minimal trace/actual pair that reaches the judge call. Used by the
// reasoning_content fallback tests so each asserts on the final Score — not on
// any helper that shares code with the adapter under test.
func newReasoningFallbackFixture(t *testing.T, respBody string) (*LLMJudgeScorer, Trace, Result, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	adapter := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))
	scorer := &LLMJudgeScorer{Client: adapter, JudgeModel: "claude-x", JudgeProvider: "openai-compat"}
	trace := Trace{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "answer must be correct"}}
	actual := Result{Model: "ollama/cand", Transcript: []Turn{{Role: "assistant", Content: "the answer"}}}
	return scorer, trace, actual, srv.Close
}

// TestOpenAICompatJudgeClient_ForwardsThinkFalseToWire pins that the judge's
// request-level Think=false directive reaches the wire as
// chat_template_kwargs.enable_thinking=false. Without it a thinking-tuned
// judge model reasons freely, burns the judge token budget, and returns its
// verdict in reasoning_content — measured live 2026-08-16 as 13/24
// "malformed judge response: empty response" failures (gemma4:31b thinking
// via llama-swap). Asserted on the raw request bytes captured server-side so
// no shared translation helper can make this test blind.
func TestOpenAICompatJudgeClient_ForwardsThinkFalseToWire(t *testing.T) {
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		rawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer_quality\":1.0,\"justification\":\"good\"}"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	judge := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))
	think := false
	_, err := judge.Chat(context.Background(), ollama.ChatRequest{
		Model:    "claude-x",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "evaluate"}},
		Think:    &think,
		Options:  &ollama.ModelOptions{Temperature: provider.Ptr(0.1), NumPredict: 512},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.Contains(rawBody, `"chat_template_kwargs":{"enable_thinking":false}`) {
		t.Fatalf("wire request missing enable_thinking:false; body = %s", rawBody)
	}
}

// TestOpenAICompatJudgeClient_NilThinkOmitsThinkControls pins the converse:
// with no Think directive the wire request must carry neither
// chat_template_kwargs nor reasoning_effort, preserving applyOptionsChat's
// pre-#220 byte-identical guarantee for requests that never opted in.
func TestOpenAICompatJudgeClient_NilThinkOmitsThinkControls(t *testing.T) {
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		rawBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer srv.Close()

	judge := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient(srv.URL)))
	_, err := judge.Chat(context.Background(), ollama.ChatRequest{
		Model:    "claude-x",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "evaluate"}},
		Options:  &ollama.ModelOptions{Temperature: provider.Ptr(0.1), NumPredict: 512},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(rawBody, "chat_template_kwargs") || strings.Contains(rawBody, "reasoning_effort") {
		t.Fatalf("wire request carries think controls without a Think directive; body = %s", rawBody)
	}
}

// TestLLMJudgeScorer_OpenAICompat_FallsBackToReasoningContentWhenContentEmpty
// pins the verdict-in-reasoning fallback: templates that ignore
// enable_thinking (gemma4, measured 2026-08-16) can leave content empty with
// the whole verdict in reasoning_content. The fixture verdict is 0.5 so any
// mutant that "succeeds" through a zero-value path scores 0 and fails.
func TestLLMJudgeScorer_OpenAICompat_FallsBackToReasoningContentWhenContentEmpty(t *testing.T) {
	scorer, trace, actual, done := newReasoningFallbackFixture(t,
		`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"the verdict: {\"answer_quality\":0.5,\"justification\":\"partial\"}"},"finish_reason":"stop"}],"usage":{}}`)
	defer done()

	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %v; want 0.5 (verdict parsed from reasoning_content)", score.AnswerQuality)
	}
}

// TestLLMJudgeScorer_OpenAICompat_ContentWinsOverReasoningContent pins
// precedence: non-empty content is always the verdict source, even when the
// reasoning carries a differently-scored verdict-shaped object (deliberation
// text routinely does). Fixture scores differ (content 1.0 vs reasoning 0.0)
// so a mutant preferring reasoning fails.
func TestLLMJudgeScorer_OpenAICompat_ContentWinsOverReasoningContent(t *testing.T) {
	scorer, trace, actual, done := newReasoningFallbackFixture(t,
		`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"{\"answer_quality\":1.0,\"justification\":\"matches\"}","reasoning_content":"maybe {\"answer_quality\":0.0,\"justification\":\"draft\"}"},"finish_reason":"stop"}],"usage":{}}`)
	defer done()

	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want 1.0 (content must win over reasoning_content)", score.AnswerQuality)
	}
}

// TestLLMJudgeScorer_OpenAICompat_WhitespaceContentTriggersFallback pins that
// whitespace-only content counts as empty for fallback purposes — the parser
// would reject it anyway, so the reasoning verdict must be consulted.
func TestLLMJudgeScorer_OpenAICompat_WhitespaceContentTriggersFallback(t *testing.T) {
	scorer, trace, actual, done := newReasoningFallbackFixture(t,
		`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"  \n","reasoning_content":"{\"answer_quality\":0.5,\"justification\":\"partial\"}"},"finish_reason":"stop"}],"usage":{}}`)
	defer done()

	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %v; want 0.5 (whitespace content must fall back to reasoning_content)", score.AnswerQuality)
	}
}

// TestLLMJudgeScorer_OpenAICompat_EmptyContentAndReasoningIsEmptyResponse pins
// that when BOTH content and reasoning_content are absent the failure mode is
// unchanged: errMalformedJudgeResponse with the "empty response" shape, not a
// substituted default.
func TestLLMJudgeScorer_OpenAICompat_EmptyContentAndReasoningIsEmptyResponse(t *testing.T) {
	scorer, trace, actual, done := newReasoningFallbackFixture(t,
		`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{}}`)
	defer done()

	_, err := scorer.Score(context.Background(), trace, actual)
	if !errors.Is(err, errMalformedJudgeResponse) {
		t.Fatalf("Score err = %v; want errMalformedJudgeResponse", err)
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Fatalf("Score err = %v; want the empty-response shape", err)
	}
}

// TestLLMJudgeScorer_OpenAICompat_ReasoningWithoutVerdictIsMissingJSONObject
// pins that a truncated deliberation with no verdict object stays a hard
// failure — but now accurately diagnosed as "missing JSON object" instead of
// "empty response". Guards against the fallback ever inventing a score.
func TestLLMJudgeScorer_OpenAICompat_ReasoningWithoutVerdictIsMissingJSONObject(t *testing.T) {
	scorer, trace, actual, done := newReasoningFallbackFixture(t,
		`{"id":"x","model":"claude-x","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"still weighing the rubric against the answer, no conclusion yet"},"finish_reason":"stop"}],"usage":{}}`)
	defer done()

	_, err := scorer.Score(context.Background(), trace, actual)
	if !errors.Is(err, errMalformedJudgeResponse) {
		t.Fatalf("Score err = %v; want errMalformedJudgeResponse", err)
	}
	if !strings.Contains(err.Error(), "missing JSON object") {
		t.Fatalf("Score err = %v; want the missing-JSON-object shape (fallback surfaced reasoning to the parser)", err)
	}
}

// TestNewJudgeTransport_OllamaDefault pins that an empty or "ollama" transport
// resolves to the existing Ollama client and a provider identity of "ollama".
func TestNewJudgeTransport_OllamaDefault(t *testing.T) {
	for _, name := range []string{"", "ollama", "OLLAMA"} {
		tr, err := newJudgeTransport(scorerOptions{judgeTransport: name, ollamaURL: "http://localhost:11434"})
		if err != nil {
			t.Fatalf("newJudgeTransport(%q): %v", name, err)
		}
		if tr.providerName != defaultBenchProvider {
			t.Fatalf("newJudgeTransport(%q) providerName = %q; want %q", name, tr.providerName, defaultBenchProvider)
		}
		if _, ok := tr.chat.(*ollama.Client); !ok {
			t.Fatalf("newJudgeTransport(%q) chat is %T; want *ollama.Client", name, tr.chat)
		}
	}
}

// TestNewJudgeTransport_OpenAICompat pins that the openai-compat transport
// resolves to the adapter and the provider instance name.
func TestNewJudgeTransport_OpenAICompat(t *testing.T) {
	tr, err := newJudgeTransport(scorerOptions{judgeTransport: "openai-compat", judgeBaseURL: "https://api.example.com"})
	if err != nil {
		t.Fatalf("newJudgeTransport: %v", err)
	}
	if want := openAICompatJudgeProviderName("https://api.example.com"); tr.providerName != want {
		t.Fatalf("providerName = %q; want %q", tr.providerName, want)
	}
	if _, ok := tr.chat.(*openAICompatJudgeClient); !ok {
		t.Fatalf("chat is %T; want *openAICompatJudgeClient", tr.chat)
	}
	if _, ok := tr.checker.(*openAICompatJudgeClient); !ok {
		t.Fatalf("checker is %T; want *openAICompatJudgeClient", tr.checker)
	}
}

func TestNewJudgeTransport_OpenAICompatProviderNameDistinguishesBaseURL(t *testing.T) {
	first, err := newJudgeTransport(scorerOptions{judgeTransport: "openai-compat", judgeBaseURL: "https://judge-a.example.com"})
	if err != nil {
		t.Fatalf("newJudgeTransport first: %v", err)
	}
	second, err := newJudgeTransport(scorerOptions{judgeTransport: "openai-compat", judgeBaseURL: "https://judge-b.example.com"})
	if err != nil {
		t.Fatalf("newJudgeTransport second: %v", err)
	}
	if first.providerName == second.providerName {
		t.Fatalf("providerName reused across distinct base URLs: %q", first.providerName)
	}
	if !strings.HasPrefix(first.providerName, openAICompatTransport+":") {
		t.Fatalf("providerName = %q; want %s:<endpoint-id>", first.providerName, openAICompatTransport)
	}
}

// TestNewJudgeTransport_OpenAICompatRequiresBaseURL pins that the compat
// transport refuses to run without an endpoint rather than defaulting to one.
func TestNewJudgeTransport_OpenAICompatRequiresBaseURL(t *testing.T) {
	if _, err := newJudgeTransport(scorerOptions{judgeTransport: "openai-compat"}); err == nil {
		t.Fatalf("newJudgeTransport(openai-compat, no base url) error = nil; want error")
	}
}

// TestNewJudgeTransport_UnknownRejected pins that an unrecognized transport is
// a hard error, not a silent fallback.
func TestNewJudgeTransport_UnknownRejected(t *testing.T) {
	if _, err := newJudgeTransport(scorerOptions{judgeTransport: "bogus"}); err == nil {
		t.Fatalf("newJudgeTransport(bogus) error = nil; want error")
	}
}

// TestNewScorer_OpenAICompatSetsJudgeProvider drives newScorer end-to-end on
// the openai-compat path: /v1/models validation passes and the returned scorer
// records an endpoint-scoped JudgeProvider.
func TestNewScorer_OpenAICompatSetsJudgeProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-x"}]}`))
			return
		}
		t.Errorf("unexpected path %s", r.URL.Path)
	}))
	defer srv.Close()

	sc, err := newScorer(context.Background(), "llm-judge", scorerOptions{
		judgeTransport: "openai-compat",
		judgeBaseURL:   srv.URL,
		judgeModel:     "claude-x",
		judgeTimeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("newScorer: %v", err)
	}
	js, ok := sc.(*LLMJudgeScorer)
	if !ok {
		t.Fatalf("scorer type %T; want *LLMJudgeScorer", sc)
	}
	if !strings.HasPrefix(js.JudgeProvider, openAICompatTransport+":") {
		t.Fatalf("JudgeProvider = %q; want %s:<endpoint-id>", js.JudgeProvider, openAICompatTransport)
	}
}

// TestOpenAICompatJudgeClient_ShowModelReturnsNil pins that the openai-compat
// transport reports no model digest (there is no /api/show equivalent), so
// resolveJudgeDigest degrades to an empty digest rather than erroring. It must
// not touch the network.
func TestOpenAICompatJudgeClient_ShowModelReturnsNil(t *testing.T) {
	judge := newOpenAICompatJudge(openaicompat.NewProvider(openaicompat.NewClient("http://example.invalid")))
	info, err := judge.ShowModel(context.Background(), "claude-x")
	if err != nil {
		t.Fatalf("ShowModel err = %v; want nil", err)
	}
	if info != nil {
		t.Fatalf("ShowModel info = %+v; want nil (no digest concept on openai-compat)", info)
	}
}
