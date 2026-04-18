package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newBlockedServer returns a test server that flags any received request.
// Callers assert !called to prove replay refused the trace before reaching
// the network.
func newBlockedServer(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

func TestReplayRefusesMultipleUserTurns(t *testing.T) {
	srv, called := newBlockedServer(t)
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
	if !errors.Is(err, errMultiUserTurn) {
		t.Fatalf("err = %v, want errMultiUserTurn", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}

func TestReplayRefusesTraceWithoutUserTurn(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{ID: "no-user", Turns: []Turn{{Role: "assistant", Content: "hi"}}}

	_, err := replay(context.Background(), client, "m", trace)
	if !errors.Is(err, errNoUserTurn) {
		t.Fatalf("err = %v, want errNoUserTurn", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}
