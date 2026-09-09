package golem_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// sessionRecorder records the SessionID of every ChatRequest it serves, so a
// test can assert what identity the runtime gave each provider call.
type sessionRecorder struct {
	mu  sync.Mutex
	ids []string
}

func (s *sessionRecorder) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	s.mu.Lock()
	s.ids = append(s.ids, req.SessionID)
	s.mu.Unlock()
	if err := onToken(provider.ChatResponse{Content: "ok"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "ok", Done: true}}, nil
}

func (s *sessionRecorder) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// TestSessionIDIsStablePerThread pins the caching contract from #533: every
// turn of one thread must reach the provider under one session id, and a
// different thread must use a different one. Without this, opencode sees each
// turn as a new conversation and multi-turn prompt caching never hits.
func TestSessionIDIsStablePerThread(t *testing.T) {
	rec := &sessionRecorder{}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(rec, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	turns := []struct{ runID, threadID, msg string }{
		{"run-1", "thread-alpha", "first"},
		{"run-2", "thread-alpha", "second"},
		{"run-3", "thread-beta", "third"},
	}
	for _, turn := range turns {
		if _, err := runtime.Run(context.Background(), golem.Turn{
			RunID:    turn.runID,
			ThreadID: turn.threadID,
			Message:  turn.msg,
		}, func(golem.Event) error { return nil }); err != nil {
			t.Fatalf("Run %s: %v", turn.runID, err)
		}
	}

	got := rec.seen()
	if len(got) != len(turns) {
		t.Fatalf("recorded %d session ids (%q), want %d", len(got), got, len(turns))
	}
	if got[0] != got[1] {
		t.Errorf("same thread produced different session ids: %q then %q", got[0], got[1])
	}
	if got[0] != "thread-alpha" {
		t.Errorf("session id = %q, want the thread id %q", got[0], "thread-alpha")
	}
	if got[2] == got[0] {
		t.Errorf("different threads shared session id %q", got[2])
	}
}

// TestThreadIDWithUnsafeHeaderValueIsRejected covers IDs HTTP would reject or
// trim, which would make distinct runtime threads share an upstream session.
func TestThreadIDWithUnsafeHeaderValueIsRejected(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&sessionRecorder{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	tests := []struct {
		name     string
		threadID string
		wantErr  bool
	}{
		{name: "carriage return and newline", threadID: "abc\r\nX-Injected: evil", wantErr: true},
		{name: "bare newline", threadID: "abc\ndef", wantErr: true},
		{name: "null byte", threadID: "abc\x00def", wantErr: true},
		{name: "del", threadID: "abc\x7fdef", wantErr: true},
		{name: "leading space", threadID: " thread-abc", wantErr: true},
		{name: "trailing space", threadID: "thread-abc ", wantErr: true},
		{name: "leading tab", threadID: "\tthread-abc", wantErr: true},
		{name: "trailing tab", threadID: "thread-abc\t", wantErr: true},
		{name: "whitespace only", threadID: " \t", wantErr: true},
		{name: "ordinary id", threadID: "thread-abc_123.4"},
		{name: "empty is allowed (stateless turn)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runtime.Run(context.Background(), golem.Turn{
				RunID:    "run-" + tt.name,
				ThreadID: tt.threadID,
				Message:  "hi",
			}, func(golem.Event) error { return nil })

			if tt.wantErr {
				if !errors.Is(err, golem.ErrInvalidRequest) {
					t.Fatalf("Run error = %v, want ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
		})
	}
}
