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
	"github.com/kstruzzieri/go-llm/conversation"
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
		baseSystem: buildSystemPrompt(false, false),
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
	in := strings.NewReader("/help\n/tools\n/model\n/clear\n/new\n/undo\n/bogus\n/exit\n")
	sess := newTestSession(t, caller, root)

	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"read_file", "(read)", "not yet routed", "session disabled (--no-session)", "unknown command", "writes disabled"} {
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
		baseSystem: buildSystemPrompt(false, false),
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

	// The system prompt is exactly the base prompt — history is sent as real messages, not appended.
	if caller.system != buildSystemPrompt(false, false) {
		t.Errorf("system must equal baseSystem (history is sent as real messages, not appended), got:\n%s", caller.system)
	}
	// Prior turn reaches the model as real user/assistant messages, ahead of the
	// current goal: system, user(prior), assistant(prior), user(goal).
	wantRoles := []string{"system", "user", "assistant", "user"}
	wantContent := []string{buildSystemPrompt(false, false), "earlier question", "earlier answer", "new question"}
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

func TestREPL_PersistsToolTranscriptButResumesPlainHistory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCall := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "c1", Type: "function",
		Function: provider.ToolCallFunction{
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path":"hello.txt"}`),
		},
	}}}
	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: toolCall},
		{Response: provider.ChatResponse{Content: "it says hi"}},
	}}
	sess := newSessionedTestSession(t, caller, root, "workspace:toolhist")

	var out strings.Builder
	if err := runREPL(context.Background(), strings.NewReader("read it\n"), &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if len(sess.session.msgs) != 4 {
		t.Fatalf("persisted messages = %+v, want user/tool-call/tool/final transcript", sess.session.msgs)
	}
	if sess.session.msgs[1].Role != "assistant" || len(sess.session.msgs[1].ToolCalls) == 0 {
		t.Fatalf("assistant tool call not persisted: %+v", sess.session.msgs[1])
	}
	if sess.session.msgs[2].Role != "tool" || sess.session.msgs[2].ToolName != "read_file" ||
		sess.session.msgs[2].ToolCallID != "c1" || sess.session.msgs[2].Content != "hi" {
		t.Fatalf("tool result not persisted: %+v", sess.session.msgs[2])
	}

	resume := &captureCaller{answer: "second"}
	sess.orch = agent.New(resume, agent.ContextManager{})
	if err := runREPL(context.Background(), strings.NewReader("again\n"), &out, nil, sess); err != nil {
		t.Fatalf("second runREPL: %v", err)
	}
	roles := make([]string, len(resume.messages))
	for i, m := range resume.messages {
		roles[i] = m.Role
		if len(m.ToolCalls) != 0 || m.ToolName != "" || m.ToolCallID != "" {
			t.Fatalf("history message %d leaked tool metadata: %+v", i, m)
		}
	}
	wantRoles := []string{"system", "user", "assistant", "user"}
	if strings.Join(roles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("resume roles = %v, want %v", roles, wantRoles)
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

func TestREPL_ReadOnlyDeniesWriteAttempt(t *testing.T) {
	root := t.TempDir()
	// Model attempts a write in a read-only session, then answers.
	writeCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "w1", Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "write_file",
				Arguments: json.RawMessage(`{"path":"out.txt","content":"x\n"}`),
			},
		}},
	}
	finalAns := provider.ChatResponse{Content: "could not write"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: writeCall}, {Response: finalAns}}}
	sess := newTestSession(t, caller, root) // read-only: no write tools, nil approver
	var out strings.Builder
	in := strings.NewReader("please write out.txt\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if strings.Contains(out.String(), "Apply this change?") {
		t.Fatalf("read-only session must not show an approval prompt:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("read-only session must not create a file")
	}
}

func TestREPL_AllowWriteApprovedWriteApplies(t *testing.T) {
	root := t.TempDir()
	writeCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "w1", Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "write_file",
				Arguments: json.RawMessage(`{"path":"out.txt","content":"hello\n"}`),
			},
		}},
	}
	finalAns := provider.ChatResponse{Content: "wrote it"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: writeCall}, {Response: finalAns}}}
	sess := newWriteEnabledTestSession(t, caller, root)

	var out strings.Builder
	in := strings.NewReader("write out.txt\ny\n") // goal, then approve the write
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("approved write not applied: got %q err %v\nout:\n%s", got, err, out.String())
	}
	if !strings.Contains(out.String(), "Apply this change?") {
		t.Fatalf("approval prompt missing:\n%s", out.String())
	}
}

func TestREPL_AllowWriteDeniedWriteSkips(t *testing.T) {
	root := t.TempDir()
	writeCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "w1", Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "write_file",
				Arguments: json.RawMessage(`{"path":"out.txt","content":"hello\n"}`),
			},
		}},
	}
	finalAns := provider.ChatResponse{Content: "ok, skipped"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: writeCall}, {Response: finalAns}}}
	sess := newWriteEnabledTestSession(t, caller, root)

	var out strings.Builder
	in := strings.NewReader("write out.txt\nn\n") // goal, then deny the write
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("denied write must not create the file")
	}
}

func newWriteEnabledTestSession(t *testing.T, caller agent.ModelCaller, root string) *replSession {
	t.Helper()
	readTools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	writeTools, journal, err := buildWriteTools(root)
	if err != nil {
		t.Fatalf("buildWriteTools: %v", err)
	}
	return &replSession{
		orch:       agent.New(caller, agent.ContextManager{}),
		tools:      append(readTools, writeTools...),
		baseSystem: buildSystemPrompt(true, false),
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
		journal:    journal,
		allowWrite: true,
	}
}

func newExecOnlyTestSession(t *testing.T, caller agent.ModelCaller, root string) *replSession {
	t.Helper()
	readTools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	execTools, err := buildExecTools(root)
	if err != nil {
		t.Fatalf("buildExecTools: %v", err)
	}
	return &replSession{
		orch:       agent.New(caller, agent.ContextManager{}),
		tools:      append(readTools, execTools...),
		baseSystem: buildSystemPrompt(false, true),
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
		allowExec:  true,
	}
}

// TestRunOnceExecOnlyWiresApprover verifies that -allow-exec alone (no -allow-write)
// wires the approval gate. The model emits a run_command call; the user denies it with
// "n"; the test asserts the "Run this command?" prompt was shown and no file was
// written (i.e. the approver was actually invoked).
func TestRunOnceExecOnlyWiresApprover(t *testing.T) {
	root := t.TempDir()
	execCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "e1", Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "run_command",
				Arguments: json.RawMessage(`{"argv":["echo","hello"]}`),
			},
		}},
	}
	finalAns := provider.ChatResponse{Content: "ok, skipped"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: execCall}, {Response: finalAns}}}
	sess := newExecOnlyTestSession(t, caller, root)

	var out strings.Builder
	in := strings.NewReader("run something\nn\n") // goal, then deny the exec
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if !strings.Contains(out.String(), "Run this command?") {
		t.Errorf("exec-only session must show 'Run this command?' approval prompt:\n%s", out.String())
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

func TestREPL_SessionsListsStoredSessions(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newSessionedTestSession(t, caller, root, "workspace:list")
	if err := sess.session.record(context.Background(), "current question", "current answer"); err != nil {
		t.Fatal(err)
	}
	if err := sess.session.store.Save(context.Background(), conversation.Conversation{
		ID:       "user:other",
		Title:    "other title",
		Messages: []conversation.Message{{Role: "user", Content: "other"}},
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	in := strings.NewReader("/sessions\n/exit\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sessions:", "workspace:list", "2 messages", "current question", "user:other", "1 message", "other title"} {
		if !strings.Contains(got, want) {
			t.Errorf("/sessions output missing %q in:\n%s", want, got)
		}
	}
}

func TestREPL_ResumeSwitchesActiveSession(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "new answer"}
	sess := newSessionedTestSession(t, caller, root, "workspace:current")
	if err := sess.session.record(context.Background(), "current question", "current answer"); err != nil {
		t.Fatal(err)
	}
	if err := sess.session.store.Save(context.Background(), conversation.Conversation{
		ID:    "user:other",
		Title: "other question",
		Messages: []conversation.Message{
			{Role: "user", Content: "other question"},
			{Role: "assistant", Content: "other answer"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	in := strings.NewReader("/resume user:other\nfollow up\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if sess.session.id != "user:other" {
		t.Fatalf("active session = %q, want user:other", sess.session.id)
	}
	if !strings.Contains(out.String(), "session: user:other resumed, 2 messages") {
		t.Fatalf("resume output missing in:\n%s", out.String())
	}
	wantRoles := []string{"system", "user", "assistant", "user"}
	wantContent := []string{buildSystemPrompt(false, false), "other question", "other answer", "follow up"}
	if len(caller.messages) != len(wantRoles) {
		t.Fatalf("messages = %+v, want %d entries", caller.messages, len(wantRoles))
	}
	for i := range wantRoles {
		if caller.messages[i].Role != wantRoles[i] || caller.messages[i].Content != wantContent[i] {
			t.Errorf("message %d = {%q,%q}, want {%q,%q}",
				i, caller.messages[i].Role, caller.messages[i].Content, wantRoles[i], wantContent[i])
		}
	}
}

func TestREPL_SearchSessions(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newSessionedTestSession(t, caller, root, "workspace:search")
	if err := sess.session.record(context.Background(), "approval prompts", "writes require approval"); err != nil {
		t.Fatal(err)
	}
	if err := sess.session.store.Save(context.Background(), conversation.Conversation{
		ID:       "user:other",
		Title:    "other",
		Messages: []conversation.Message{{Role: "user", Content: "quantum notes"}},
	}); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	in := strings.NewReader("/search-sessions approval\n/exit\n")
	if err := runREPL(context.Background(), in, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "session search:") || !strings.Contains(got, "workspace:search") ||
		!strings.Contains(got, "approval prompts") || strings.Contains(got, "user:other") {
		t.Fatalf("search output wrong:\n%s", got)
	}
}
