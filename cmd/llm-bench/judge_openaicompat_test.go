package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
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
		Options: &ollama.ModelOptions{Temperature: 0.1, NumPredict: 512},
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
	if tr.providerName != "openai-compat" {
		t.Fatalf("providerName = %q; want openai-compat", tr.providerName)
	}
	if _, ok := tr.chat.(*openAICompatJudgeClient); !ok {
		t.Fatalf("chat is %T; want *openAICompatJudgeClient", tr.chat)
	}
	if _, ok := tr.checker.(*openAICompatJudgeClient); !ok {
		t.Fatalf("checker is %T; want *openAICompatJudgeClient", tr.checker)
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
// records JudgeProvider="openai-compat".
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
	if js.JudgeProvider != "openai-compat" {
		t.Fatalf("JudgeProvider = %q; want openai-compat", js.JudgeProvider)
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
