package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestReplayRefusesMultipleUserTurns(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "multi-turn",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "ack"},
			{Role: "user", Content: "second"},
		},
	}

	_, err := replay(context.Background(), client, "some-model", trace)
	if err == nil {
		t.Fatal("expected error for multi-user-turn trace, got nil")
	}
	if !strings.Contains(err.Error(), "multi-turn replay not yet supported") {
		t.Errorf("error = %v, want multi-turn guard message", err)
	}
	if called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}

func TestReplayRefusesTraceWithoutUserTurn(t *testing.T) {
	client := ollama.NewClient(ollama.WithBaseURL("http://127.0.0.1:1"))
	trace := Trace{ID: "no-user", Turns: []Turn{{Role: "assistant", Content: "hi"}}}

	_, err := replay(context.Background(), client, "m", trace)
	if err == nil {
		t.Fatal("expected error for trace with no user turn, got nil")
	}
	if !strings.Contains(err.Error(), "no user turn") {
		t.Errorf("error = %v, want no-user-turn guard message", err)
	}
}
