package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		w.Write([]byte(`{"model":"m","done":true,"message":{"role":"assistant","content":"` + reply + `"}}`))
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
