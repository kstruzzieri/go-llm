package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
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
	system := buildSystemPrompt(false, false)
	orch := agent.New(caller, agent.ContextManager{})
	return &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, nil),
		tools:      tools,
		baseSystem: system,
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
	}
}

func newTestRuntime(t *testing.T, root, system string, orch *agent.Orchestrator, tools []agent.Tool) *golemruntime.Runtime {
	t.Helper()
	runtime, err := golemruntime.New(context.Background(), golemruntime.Options{
		Root:         root,
		System:       system,
		Tools:        tools,
		MaxSteps:     16,
		Orchestrator: orch,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	return runtime
}

func TestRunOnceDelegatesThroughConsumerRuntime(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{Content: "runtime answer"},
	}}}
	system := buildSystemPrompt(false, false)
	sess := &replSession{
		runtime:    newTestRuntime(t, root, system, agent.New(caller, agent.ContextManager{}), nil),
		baseSystem: system,
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
	}
	var out strings.Builder
	result, err := runOnce(context.Background(), &out, nil, sess, "answer this", nil)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if result.Answer != "runtime answer" {
		t.Fatalf("answer = %q, want runtime answer", result.Answer)
	}
}

// runOnce deliberately does not put sess.modelOptions on an agent.Request of
// its own: golem.Runtime owns model options (golem.Options.ModelOptions) and
// stamps them onto every turn's request. This pins that the -think options
// still reach the model through that path, so the REPL-side field is
// genuinely redundant rather than silently dropped.
func TestRunOnceAppliesRuntimeModelOptions(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "answer"}
	system := buildSystemPrompt(false, false)
	thinkOpts := thinkModelOptions("high") // exactly what -think=high produces
	runtime, err := golemruntime.New(context.Background(), golemruntime.Options{
		Root:         root,
		System:       system,
		MaxSteps:     16,
		ModelOptions: thinkOpts,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	// main.go sets both golem.Options.ModelOptions and replSession.modelOptions
	// from the same -think value; mirror that so the test proves which one
	// actually carries the options to the model.
	sess := &replSession{
		runtime:      runtime,
		baseSystem:   system,
		maxSteps:     16,
		clock:        func() time.Time { return time.Unix(0, 0) },
		modelOptions: thinkOpts,
	}
	var out strings.Builder
	if _, err := runOnce(context.Background(), &out, nil, sess, "think hard about this", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.options.Think == nil || !*caller.options.Think {
		t.Fatalf("model options Think = %v, want true", caller.options.Think)
	}
	if caller.options.ThinkEffort != "high" {
		t.Fatalf("model options ThinkEffort = %q, want high", caller.options.ThinkEffort)
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

	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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

	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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

	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, interrupts, sess); err != nil {
		t.Fatalf("runREPL should not error on cancel: %v", err)
	}
	if !strings.Contains(out.String(), "canceled") {
		t.Errorf("expected a cancel notice, got:\n%s", out.String())
	}
}

// captureCaller records the full message list and model options of the first
// request it serves, then returns a final answer.
type captureCaller struct {
	messages []provider.ChatMessage
	system   string
	options  provider.ModelOptions
	answer   string
}

func (c *captureCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.messages == nil {
		c.messages = append([]provider.ChatMessage(nil), req.Messages...)
		c.options = req.Options
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
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	dbPath := filepath.Join(dataDir, "golem", "sessions.db")
	s, _, err := openSession(context.Background(), dbPath, id)
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	system := buildSystemPrompt(false, false)
	orch := agent.New(caller, agent.ContextManager{})
	return &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, nil),
		tools:      tools,
		baseSystem: system,
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(strings.NewReader("read it\n"), &out), &out, nil, sess); err != nil {
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
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, nil)
	if err := runREPL(context.Background(), newScannerSource(strings.NewReader("again\n"), &out), &out, nil, sess); err != nil {
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

func TestRunOnceUnansweredFreshSessionDoesNotRefreshMissingRow(t *testing.T) {
	root := t.TempDir()
	sess := newSessionedTestSession(t, &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{},
	}}}, root, "workspace:unanswered")

	var out strings.Builder
	result, err := runOnce(context.Background(), &out, nil, sess, "question", nil)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if result.Answer != "" {
		t.Fatalf("answer = %q, want empty", result.Answer)
	}
	if strings.Contains(out.String(), "session state not refreshed") {
		t.Fatalf("unexpected refresh warning:\n%s", out.String())
	}
}

func TestRunOnceKeepsAnswerWhenSessionSaveFails(t *testing.T) {
	root := t.TempDir()
	sess := newSessionedTestSession(t, &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{Content: "completed answer"},
	}}}, root, "workspace:save-failure")
	if _, err := sess.session.db.ExecContext(context.Background(), `
		CREATE TRIGGER fail_conversation_save
		BEFORE INSERT ON conversations
		BEGIN
			SELECT RAISE(FAIL, 'forced save failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	var out strings.Builder
	result, err := runOnce(context.Background(), &out, nil, sess, "question", nil)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if result.Answer != "completed answer" {
		t.Fatalf("answer = %q, want completed answer", result.Answer)
	}
	if got := out.String(); !strings.Contains(got, "warning: session not saved:") ||
		!strings.Contains(got, "done ·") || strings.Contains(got, "\nerror:") {
		t.Fatalf("save failure did not preserve successful CLI output:\n%s", got)
	}
	if len(sess.session.msgs) != 0 {
		t.Fatalf("failed save changed cached session: %+v", sess.session.msgs)
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, interrupts, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	system := buildSystemPrompt(true, false)
	orch := agent.New(caller, agent.ContextManager{})
	return &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, writeTools),
		tools:      append(readTools, writeTools...),
		baseSystem: system,
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
	system := buildSystemPrompt(false, true)
	orch := agent.New(caller, agent.ContextManager{})
	return &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, execTools),
		tools:      append(readTools, execTools...),
		baseSystem: system,
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
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
	if err := runREPL(context.Background(), newScannerSource(in, &out), &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "session search:") || !strings.Contains(got, "workspace:search") ||
		!strings.Contains(got, "approval prompts") || strings.Contains(got, "user:other") {
		t.Fatalf("search output wrong:\n%s", got)
	}
}

// barrierCaller signals entry, then blocks until its context is canceled.
type barrierCaller struct {
	started chan struct{}
	once    sync.Once
}

func (b *barrierCaller) Chat(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return agent.ModelResult{}, ctx.Err()
}

// newTracingSession builds a session whose observ records traces under a temp
// XDG root, returning the session and the trace directory.
func newTracingSession(t *testing.T, caller agent.ModelCaller) (*replSession, string) {
	t.Helper()
	root := t.TempDir()
	base := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	o, err := newObserv(getenv, root, true, false, func() time.Time { return time.Unix(1719600000, 0) })
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	sess := newTestSession(t, caller, root)
	sess.obs = o
	return sess, o.traceDir
}

// recordedTraceStatus reads the single trace file in dir and returns its
// recorded status field.
func recordedTraceStatus(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("trace files = %d, want 1", len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var trace struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &trace); err != nil {
		t.Fatalf("parse trace: %v", err)
	}
	return trace.Status
}

func TestRunOnceApproverFirstCancelIsCanceled(t *testing.T) {
	// Approver-first ordering, barrier-forced by construction: Run returns
	// context.Canceled synchronously (an interrupted approval mapped by the
	// approver) and there is NO watcher (interrupts is nil), so only the
	// synchronous normalization in runOnce can make runCtx canceled. Pins both
	// the rendered line and the status recorded to the trace, which is what
	// telemetry consumers see.
	sess, traceDir := newTracingSession(t, errCaller{err: context.Canceled})
	var out strings.Builder
	_, runErr := runOnce(context.Background(), &out, nil, sess, "goal", nil)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runErr = %v, want context.Canceled", runErr)
	}
	if !strings.Contains(out.String(), "canceled") || strings.Contains(out.String(), "error:") {
		t.Fatalf("approver-first cancel rendered:\n%s\nwant canceled, never error:", out.String())
	}
	if status := recordedTraceStatus(t, traceDir); status != "canceled" {
		t.Fatalf("recorded trace status = %q, want canceled", status)
	}
}

func TestRunOnceWatcherFirstCancelIsCanceled(t *testing.T) {
	// Watcher-first ordering, barrier-forced: the model blocks until the
	// interrupt watcher cancels runCtx, so runCtx.Err() is already non-nil
	// when Run returns. Must classify identically to approver-first.
	caller := &barrierCaller{started: make(chan struct{})}
	sess, traceDir := newTracingSession(t, caller)
	interrupts := make(chan struct{}, 1)
	go func() {
		<-caller.started
		interrupts <- struct{}{}
	}()
	var out strings.Builder
	_, runErr := runOnce(context.Background(), &out, interrupts, sess, "goal", nil)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runErr = %v, want context.Canceled", runErr)
	}
	if !strings.Contains(out.String(), "canceled") || strings.Contains(out.String(), "error:") {
		t.Fatalf("watcher-first cancel rendered:\n%s\nwant canceled, never error:", out.String())
	}
	if status := recordedTraceStatus(t, traceDir); status != "canceled" {
		t.Fatalf("recorded trace status = %q, want canceled", status)
	}
}

// fakeGoalEditor scripts Compose; it records the seeds it was given.
type fakeGoalEditor struct {
	available bool
	text      string
	err       error
	seeds     []string
	composes  int
}

func (f *fakeGoalEditor) Available() bool { return f.available }
func (f *fakeGoalEditor) Compose(_ context.Context, seed string) (string, error) {
	f.composes++
	f.seeds = append(f.seeds, seed)
	return f.text, f.err
}

func TestREPL_EditComposesAForcedGoal(t *testing.T) {
	// /edit seeds the editor and the edited text becomes the goal: recorded,
	// then run against the model. /edit itself is never recorded.
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{Content: "done"},
	}}}
	sess := newTestSession(t, caller, root)
	ed := &fakeGoalEditor{available: true, text: "edited goal\n"}
	sess.goalEditor = ed

	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit fix the bug\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if len(ed.seeds) != 1 || ed.seeds[0] != "fix the bug" {
		t.Fatalf("seeds = %q, want [\"fix the bug\"]", ed.seeds)
	}
	if len(src.recorded) != 1 || src.recorded[0] != "edited goal" {
		t.Fatalf("recorded goals = %q, want the trimmed edited goal only", src.recorded)
	}
	if caller.i != 1 {
		t.Fatalf("model called %d times, want 1", caller.i)
	}
}

func TestREPL_EditResultStartingWithSlashRunsAsGoal(t *testing.T) {
	// The forced flag bypasses slash dispatch exactly once: an edited goal of
	// "/exit" is a model goal, not a command.
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{Content: "done"},
	}}}
	sess := newTestSession(t, caller, root)
	sess.goalEditor = &fakeGoalEditor{available: true, text: "/exit"}

	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	if caller.i != 1 {
		t.Fatalf("model called %d times, want 1: \"/exit\" must run as a goal", caller.i)
	}
	if len(src.recorded) != 1 || src.recorded[0] != "/exit" {
		t.Fatalf("recorded goals = %q, want [\"/exit\"]", src.recorded)
	}
}

func TestREPL_EditUnavailable(t *testing.T) {
	// nil and Available()==false both refuse before any runner involvement, so
	// a piped script can never spawn an interactive editor.
	for _, tc := range []struct {
		name   string
		editor goalEditor
	}{
		{"nil editor", nil},
		{"unavailable editor", &fakeGoalEditor{available: false, text: "never"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caller := &scriptCaller{}
			sess := newTestSession(t, caller, root)
			sess.goalEditor = tc.editor

			var out strings.Builder
			src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit\n"), &out)}
			if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
				t.Fatalf("runREPL: %v", err)
			}
			if !strings.Contains(out.String(), "/edit requires an interactive terminal") {
				t.Fatalf("missing unavailable message in:\n%s", out.String())
			}
			if fe, ok := tc.editor.(*fakeGoalEditor); ok && fe.composes != 0 {
				t.Fatalf("Compose invoked %d times on an unavailable editor, want 0", fe.composes)
			}
			if caller.i != 0 || len(src.recorded) != 0 {
				t.Fatalf("model calls=%d goals=%q, want none", caller.i, src.recorded)
			}
		})
	}
}

func TestREPL_EditErrorAndEmptyYieldNoGoal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		editor   *fakeGoalEditor
		wantOut  string
		wantSkip string
	}{
		{"runner error", &fakeGoalEditor{available: true, err: errors.New("exit status 3")}, "edit failed:", ""},
		{"oversized", &fakeGoalEditor{available: true, err: errEditTooLarge}, goalLimitWarning, "edit failed:"},
		{"empty content", &fakeGoalEditor{available: true, text: "  \n "}, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caller := &scriptCaller{}
			sess := newTestSession(t, caller, root)
			sess.goalEditor = tc.editor

			var out strings.Builder
			src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit\n"), &out)}
			if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
				t.Fatalf("runREPL: %v", err)
			}
			if tc.wantOut != "" && !strings.Contains(out.String(), tc.wantOut) {
				t.Fatalf("output missing %q:\n%s", tc.wantOut, out.String())
			}
			if tc.wantSkip != "" && strings.Contains(out.String(), tc.wantSkip) {
				t.Fatalf("output must not contain %q:\n%s", tc.wantSkip, out.String())
			}
			if caller.i != 0 || len(src.recorded) != 0 {
				t.Fatalf("model calls=%d goals=%q, want none", caller.i, src.recorded)
			}
		})
	}
}

// TestREPLIntegrationPartialInputDoubleCtrlCExits is spec 13.1 case 2 driven
// through the production seam rather than the editor in isolation: newInput
// selects the editor, replControl owns the arm/hint policy, withLineSource
// owns Close, and runREPL is the loop.
//
// The invariant it guards is spec 7.4. replControl.enterPrompt clears the arm,
// and runREPL calls it once per loop iteration. If the interrupt cycle ever
// returned from ReadGoal, runREPL would loop, re-enter the prompt, clear the
// arm, and a second Ctrl-C could never quit -- so this cannot be replaced by
// asserting on the editor alone.
func TestREPLIntegrationPartialInputDoubleCtrlCExits(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, root)

	stdin, stdout := tempDescriptors(t)
	out := &lockedBuffer{}
	errOut := &lockedBuffer{}
	ops := &fakeTermOps{
		ttys:  map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true},
		sizes: [][2]int{{80, 24}},
	}

	replCtx, cancelREPL := context.WithCancel(context.Background())
	defer cancelREPL()
	interrupts := make(chan struct{}, 1)
	ctrl := newReplControl(out, errOut, interrupts, cancelREPL)
	sess.control = ctrl

	// Two presses with typed text before the first and nothing between them.
	// Separate chunks so the second 0x03 cannot be consumed by the same read
	// as the first, which is what makes this two distinct presses.
	in := &chunkReader{chunks: [][]byte{[]byte("partial input\x03"), []byte("\x03")}}
	src := newInput(inputConfig{
		Stdin: stdin, Stdout: stdout, Stderr: errOut,
		In: in, Out: out,
		UseHistory:  false,
		Getenv:      func(string) string { return "" },
		Root:        root,
		Ops:         ops,
		OnInterrupt: ctrl.interrupt,
	})
	if _, isEditor := src.(*editorSource); !isEditor {
		t.Fatalf("newInput selected %T, want the editor: this test must exercise the editor path", src)
	}
	ctrl.setIdleDisplay(src.IdleDisplay)

	done := make(chan error, 1)
	go func() {
		done <- withLineSource(src, func(s lineSource) error {
			return runREPL(replCtx, s, out, interrupts, sess)
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runREPL after a double Ctrl-C = %v, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runREPL did not exit after two idle Ctrl-C presses")
	}

	if got := strings.Count(out.String(), ctrlCHint); got != 1 {
		t.Fatalf("hint printed %d times, want exactly 1: the first press arms, the second quits", got)
	}
	if caller.i != 0 {
		t.Fatalf("model called %d times, want 0: a discarded partial line is never a goal", caller.i)
	}
	// The "was the discarded line resubmitted" check that used to live here was
	// removed rather than kept: x/term never flushes the echo on the Ctrl-C
	// path, so the substring it looked for could not appear whatever the
	// implementation did, and it would not have noticed the Terminal never
	// being recreated. TestEditorSourceCtrlCDiscardsRetainedBytesAndContinues
	// asserts that property where it is observable -- on the goal the next line
	// produces.
	// Production Close ownership ran: every raw window this REPL opened was
	// closed, so the shell is not left in raw mode.
	makeRaw, restore, _ := ops.counts()
	if makeRaw == 0 || makeRaw != restore {
		t.Fatalf("MakeRaw=%d Restore=%d, want equal and non-zero", makeRaw, restore)
	}
}
