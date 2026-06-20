package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// scriptCaller returns queued responses in order; each Chat call pops one.
type scriptCaller struct {
	responses []agent.ModelResult
	i         int
	block     chan struct{} // when non-nil, Chat waits on ctx or this before responding
}

func (s *scriptCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if s.block != nil {
		select {
		case <-ctx.Done():
			return agent.ModelResult{}, ctx.Err()
		case <-s.block:
		}
	}
	if s.i >= len(s.responses) {
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done"}}, nil
	}
	r := s.responses[s.i]
	s.i++
	if onToken != nil {
		_ = onToken(r.Response)
	}
	return r, nil
}

func newTestSession(t *testing.T, caller agent.ModelCaller, root string) *replSession {
	t.Helper()
	tools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	return &replSession{
		orch:       agent.New(caller, agent.ContextManager{}),
		tools:      tools,
		baseSystem: golemSystemPrompt,
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
	}
}

func TestREPL_EndToEndReadOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Turn 1: model asks to read hello.txt. Turn 2: model gives the final answer.
	toolCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"hello.txt"}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "the file says hi there"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: toolCall}, {Response: finalAns}}}

	var out strings.Builder
	in := strings.NewReader("read hello.txt\n")
	sess := newTestSession(t, caller, root)

	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `> read_file {"path":"hello.txt"}`) {
		t.Errorf("missing tool-call line in:\n%s", got)
	}
	if !strings.Contains(got, "the file says hi there") {
		t.Errorf("missing final answer in:\n%s", got)
	}
	if !strings.Contains(got, "done · 2 steps") {
		t.Errorf("missing/incorrect final footer in:\n%s", got)
	}
}

func TestREPL_SlashCommands(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	var out strings.Builder
	in := strings.NewReader("/help\n/tools\n/model\n/clear\n/bogus\n/exit\n")
	sess := newTestSession(t, caller, root)

	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"read_file", "(read)", "not yet routed", "no session history in this build", "unknown command"} {
		if !strings.Contains(got, want) {
			t.Errorf("slash output missing %q in:\n%s", want, got)
		}
	}
	if caller.i != 0 {
		t.Errorf("model called %d times for slash-only input, want 0", caller.i)
	}
}

func TestREPL_CtrlCCancelsRunKeepsREPL(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{block: make(chan struct{})} // never unblocked => Chat waits on ctx
	var out strings.Builder
	in := strings.NewReader("long running\n")
	interrupts := make(chan struct{}, 1)
	sess := newTestSession(t, caller, root)

	go func() {
		time.Sleep(20 * time.Millisecond)
		interrupts <- struct{}{}
	}()

	if err := runREPL(context.Background(), in, &out, interrupts, sess); err != nil {
		t.Fatalf("runREPL should not error on cancel: %v", err)
	}
	if !strings.Contains(out.String(), "canceled") {
		t.Errorf("expected a cancel notice, got:\n%s", out.String())
	}
}
