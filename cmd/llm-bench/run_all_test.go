package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newFakeChatServer responds to /api/chat with a fixed assistant reply.
func newFakeChatServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"model":"m","done":true,"message":{"role":"assistant","content":"` + reply + `"}}`)); err != nil {
			t.Errorf("fake server write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunAllReturnsPartialResultsOnCtxCancel(t *testing.T) {
	srv := newFakeChatServer(t, "answer with needle inside")
	runner := &Runner{
		OllamaURL: srv.URL,
		Timeout:   5 * time.Second,
		Scorer:    &ExactMatchScorer{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel up front

	targets := []ModelTarget{{Display: "m1", Provider: "ollama", Model: "m1"}}
	traces := []Trace{{
		ID:     "t1",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerSubstring: "needle"},
	}}

	results, err := runner.RunAll(ctx, targets, traces)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results on pre-cancelled ctx, want 0", len(results))
	}
}

func TestRunAllRecordsClientConstructionFailureAsResults(t *testing.T) {
	runner := &Runner{
		OllamaURL: "   ", // whitespace triggers errEmptyBaseURL
		Timeout:   time.Second,
		Scorer:    &ExactMatchScorer{},
	}

	targets := []ModelTarget{{Display: "m1", Provider: "ollama", Model: "m1"}}
	traces := []Trace{
		{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerSubstring: "x"}},
		{ID: "t2", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerSubstring: "x"}},
	}

	results, err := runner.RunAll(context.Background(), targets, traces)
	if err != nil {
		t.Fatalf("RunAll() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (one per trace, all errored)", len(results))
	}
	for _, r := range results {
		if !errors.Is(r.Err, errEmptyBaseURL) {
			t.Errorf("result %q: err = %v, want errEmptyBaseURL", r.TraceID, r.Err)
		}
	}
}

func TestRunAllSucceedsEndToEnd(t *testing.T) {
	srv := newFakeChatServer(t, "answer with needle inside")
	runner := &Runner{
		OllamaURL: srv.URL,
		Timeout:   5 * time.Second,
		Scorer:    &ExactMatchScorer{},
	}

	targets := []ModelTarget{{Display: "m1", Provider: "ollama", Model: "m1"}}
	traces := []Trace{{
		ID:     "t1",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerSubstring: "needle"},
	}}

	results, err := runner.RunAll(context.Background(), targets, traces)
	if err != nil {
		t.Fatalf("RunAll() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.Score.AnswerQuality != 1.0 {
		t.Errorf("AnswerQuality = %f, want 1.0", r.Score.AnswerQuality)
	}
}

type latencyPoisonScorer struct{}

func (s latencyPoisonScorer) Score(context.Context, Trace, Result) (Score, error) {
	time.Sleep(20 * time.Millisecond)
	return Score{
		AnswerQuality: 1.0,
		LatencyMs:     int64((24 * time.Hour) / time.Millisecond),
	}, nil
}

func TestRunAllLatencyExcludesScorerWork(t *testing.T) {
	srv := newFakeChatServer(t, "answer")
	runner := &Runner{
		OllamaURL: srv.URL,
		Timeout:   5 * time.Second,
		Scorer:    latencyPoisonScorer{},
	}

	targets := []ModelTarget{{Display: "m1", Provider: "ollama", Model: "m1"}}
	traces := []Trace{{
		ID:     "t1",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerSubstring: "answer"},
	}}

	results, err := runner.RunAll(context.Background(), targets, traces)
	if err != nil {
		t.Fatalf("RunAll() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected result error: %v", results[0].Err)
	}
	if results[0].Score.LatencyMs >= int64(time.Hour/time.Millisecond) {
		t.Fatalf("LatencyMs = %d, scorer latency leaked into replay latency", results[0].Score.LatencyMs)
	}
	if results[0].Score.ScorerLatencyMs <= 0 {
		t.Fatalf("ScorerLatencyMs = %d, want scorer work to be visible", results[0].Score.ScorerLatencyMs)
	}
}

// fakeChatResp is a scripted chat reply for newSequencedFakeChatServer.
// PromptEvalCount + EvalCount are surfaced into the JSON response so
// callers can verify token-sum accounting.
type fakeChatResp struct {
	Content         string
	PromptEvalCount int
	EvalCount       int
}

// newSequencedFakeChatServer returns a fake /api/chat server that
// answers consecutive requests with replies[0], replies[1], … When
// requests outnumber replies, the server returns the LAST reply for
// every overflow call so the test fails loudly via wrong content rather
// than via a 5xx response. Token counts are JSON-omitempty: a zero
// PromptEvalCount / EvalCount produces a response with the field
// absent, matching providers that omit counts entirely.
func newSequencedFakeChatServer(t *testing.T, replies ...fakeChatResp) *httptest.Server {
	t.Helper()
	if len(replies) == 0 {
		t.Fatalf("newSequencedFakeChatServer: at least one reply required")
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		idx := int(calls.Add(1) - 1)
		if idx >= len(replies) {
			idx = len(replies) - 1
		}
		body := struct {
			Model           string             `json:"model"`
			Done            bool               `json:"done"`
			Message         ollama.ChatMessage `json:"message"`
			PromptEvalCount int                `json:"prompt_eval_count,omitempty"`
			EvalCount       int                `json:"eval_count,omitempty"`
		}{
			Model:           "m",
			Done:            true,
			Message:         ollama.ChatMessage{Role: "assistant", Content: replies[idx].Content},
			PromptEvalCount: replies[idx].PromptEvalCount,
			EvalCount:       replies[idx].EvalCount,
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("fake server encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunnerSumsTokenCountsAcrossTurns(t *testing.T) {
	srv := newSequencedFakeChatServer(t,
		fakeChatResp{Content: "hi", PromptEvalCount: 11, EvalCount: 7},
		fakeChatResp{Content: "bye", PromptEvalCount: 13, EvalCount: 5},
	)

	trace := Trace{
		ID:     "t1",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "ping"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "again"},
			{Role: "assistant", Content: "bye"},
		},
		Golden: Golden{FinalAnswerSubstring: "bye"},
	}
	r := &Runner{OllamaURL: srv.URL, Timeout: 5 * time.Second, Scorer: &ExactMatchScorer{}}
	results, err := r.RunAll(context.Background(),
		[]ModelTarget{{Display: "fake", Provider: "ollama", Model: "fake"}},
		[]Trace{trace})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results=%d; want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected result error: %v", results[0].Err)
	}
	if got, want := results[0].Score.TotalTokens, 36; got != want {
		t.Fatalf("TotalTokens=%d; want %d", got, want)
	}
}

func TestRunnerLeavesTotalTokensZeroWhenUnreported(t *testing.T) {
	srv := newSequencedFakeChatServer(t, fakeChatResp{Content: "hi"})

	trace := Trace{
		ID:     "t2",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "ping"},
			{Role: "assistant", Content: "hi"},
		},
		Golden: Golden{FinalAnswerSubstring: "hi"},
	}
	r := &Runner{OllamaURL: srv.URL, Timeout: 5 * time.Second, Scorer: &ExactMatchScorer{}}
	results, err := r.RunAll(context.Background(),
		[]ModelTarget{{Display: "fake", Provider: "ollama", Model: "fake"}},
		[]Trace{trace})
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	if results[0].Err != nil {
		t.Fatalf("unexpected result error: %v", results[0].Err)
	}
	if got := results[0].Score.TotalTokens; got != 0 {
		t.Fatalf("TotalTokens=%d; want 0 (unavailable)", got)
	}
}
