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
	if !strings.Contains(got, "< 1 line") {
		t.Errorf("missing tool-result summary line in:\n%s", got)
	}
	if !strings.Contains(got, "done · 2 steps") {
		t.Errorf("missing/incorrect final footer in:\n%s", got)
	}
}

func TestREPL_SlashCommands(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	var out strings.Builder
	in := strings.NewReader("/help\n/tools\n/model\n/clear\n/new\n/bogus\n/exit\n")
	sess := newTestSession(t, caller, root)

	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"read_file", "(read)", "not yet routed", "session disabled (--no-session)", "unknown command"} {
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

// captureCaller records the full message list of the first request it serves,
// then returns a final answer.
type captureCaller struct {
	messages []provider.ChatMessage
	system   string
	answer   string
}

func (c *captureCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.messages == nil {
		c.messages = append([]provider.ChatMessage(nil), req.Messages...)
		for _, m := range req.Messages {
			if m.Role == "system" {
				c.system = m.Content
				break
			}
		}
	}
	resp := provider.ChatResponse{Content: c.answer}
	if onToken != nil {
		_ = onToken(resp)
	}
	return agent.ModelResult{Response: resp}, nil
}

func newSessionedTestSession(t *testing.T, caller agent.ModelCaller, root, id string) *replSession {
	t.Helper()
	tools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "golem", "sessions.db")
	s, _, err := openSession(context.Background(), dbPath, id)
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &replSession{
		orch:       agent.New(caller, agent.ContextManager{}),
		tools:      tools,
		baseSystem: golemSystemPrompt,
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
		session:    s,
	}
}

func TestREPL_HistoryReachesModelAsRealRoles(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "second answer"}
	sess := newSessionedTestSession(t, caller, root, "workspace:repl")

	// Seed a prior turn so it is fed as History on the next prompt.
	if err := sess.session.record(context.Background(), "earlier question", "earlier answer"); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	in := strings.NewReader("new question\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}

	// The system prompt is exactly the base prompt — no rendered preamble.
	if caller.system != golemSystemPrompt {
		t.Errorf("system must equal baseSystem with no preamble, got:\n%s", caller.system)
	}
	// Prior turn reaches the model as real user/assistant messages, ahead of the
	// current goal: system, user(prior), assistant(prior), user(goal).
	wantRoles := []string{"system", "user", "assistant", "user"}
	wantContent := []string{golemSystemPrompt, "earlier question", "earlier answer", "new question"}
	if len(caller.messages) != len(wantRoles) {
		t.Fatalf("messages = %+v, want %d entries", caller.messages, len(wantRoles))
	}
	for i := range wantRoles {
		if caller.messages[i].Role != wantRoles[i] || caller.messages[i].Content != wantContent[i] {
			t.Errorf("message %d = {%q,%q}, want {%q,%q}",
				i, caller.messages[i].Role, caller.messages[i].Content, wantRoles[i], wantContent[i])
		}
	}
	// The successful turn must still be persisted: 2 seeded + 2 new = 4.
	if len(sess.session.msgs) != 4 || sess.session.msgs[3].Content != "second answer" {
		t.Errorf("turn not persisted: %+v", sess.session.msgs)
	}
}

func TestREPL_DoesNotPersistCanceledRun(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{block: make(chan struct{})} // never unblocked
	sess := newSessionedTestSession(t, caller, root, "workspace:cancel")

	var out strings.Builder
	in := strings.NewReader("long running\n")
	interrupts := make(chan struct{}, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		interrupts <- struct{}{}
	}()
	if err := runREPL(context.Background(), in, &out, interrupts, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if len(sess.session.msgs) != 0 {
		t.Errorf("canceled run must not persist, got %+v", sess.session.msgs)
	}
}

func TestREPL_ClearAndNew(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newSessionedTestSession(t, caller, root, "workspace:cn")
	if err := sess.session.record(context.Background(), "q", "a"); err != nil {
		t.Fatal(err)
	}
	oldID := sess.session.id

	var out strings.Builder
	in := strings.NewReader("/clear\n/new\n/exit\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "session cleared") {
		t.Errorf("/clear output missing in:\n%s", got)
	}
	if !strings.Contains(got, "session: golem:") {
		t.Errorf("/new must announce a new golem: id in:\n%s", got)
	}
	if sess.session.id == oldID || !strings.HasPrefix(sess.session.id, "golem:") {
		t.Errorf("/new did not switch session id: %q", sess.session.id)
	}
}
