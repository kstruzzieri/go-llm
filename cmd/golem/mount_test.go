package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/provider"
)

// Pins fragment ORDER and separators against the real fragment functions.
// Every fragment is asserted non-empty first so the order pin cannot be
// vacuous.
func TestComposeSystemFragmentOrder(t *testing.T) {
	in := systemInputs{allowWrite: true, allowExec: true, delegate: true, dispatch: true, memory: true,
		projectContext: "<<<PROJECT>>>", agentMemory: true, sessionUp: true}
	want := []string{
		buildSystemPrompt(true, true),
		delegateSystemFragment(true, true),
		dispatchSystemFragment(true),
		memorySystemFragment(true),
		"\n\n<<<PROJECT>>>",
		agentMemorySystemFragment(true, true),
	}
	for i, w := range want {
		if w == "" {
			t.Fatalf("fragment %d is empty; the order pin would be vacuous", i)
		}
	}
	if got := composeSystem(in); got != strings.Join(want, "") {
		t.Fatalf("composeSystem order/separators changed:\n got=%q\nwant=%q", got, strings.Join(want, ""))
	}
}

func TestComposeSystemDisabledInputsAreByteIdenticalToBasePrompt(t *testing.T) {
	if got := composeSystem(systemInputs{}); got != buildSystemPrompt(false, false) {
		t.Fatalf("zero inputs = %q, want the bare read-only prompt", got)
	}
}

// Headless (-allow-tool) uses the exact-set prompt and derives the delegate
// fragment's write-awareness from the mounted caps, as startup's buildWrite
// did; a nil headless pointer (every REPL session) must not panic.
func TestComposeSystemHeadlessDerivesWriteAwareness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      golemruntime.HeadlessToolCaps
		wantWrite bool
	}{
		{"write_file", golemruntime.HeadlessToolCaps{WriteFile: true}, true},
		{"edit_file", golemruntime.HeadlessToolCaps{EditFile: true}, true},
		{"exec only", golemruntime.HeadlessToolCaps{RunCommand: true}, false},
	} {
		caps := tc.caps
		got := composeSystem(systemInputs{headless: &caps, delegate: true})
		if !strings.HasPrefix(got, golemruntime.SystemPromptHeadless(caps)) {
			t.Fatalf("%s: headless prompt not used: %q", tc.name, got)
		}
		if !strings.HasSuffix(got, delegateSystemFragment(true, tc.wantWrite)) {
			t.Fatalf("%s: delegate write-awareness = %v not honored: %q", tc.name, tc.wantWrite, got)
		}
	}
	if got := composeSystem(systemInputs{delegate: true}); !strings.HasSuffix(got, delegateSystemFragment(true, false)) {
		t.Fatalf("nil headless with delegate: %q", got)
	}
}

// Flipping one input changes exactly that fragment: everything after the
// base+delegate pair is byte-identical before and after.
func TestComposeSystemFlipChangesOnlyThatFragment(t *testing.T) {
	base := systemInputs{delegate: true, dispatch: true, memory: true, projectContext: "P", agentMemory: true, sessionUp: true}
	before := composeSystem(base)
	flipped := base
	flipped.allowWrite = true
	after := composeSystem(flipped)
	tailBefore := strings.TrimPrefix(before, buildSystemPrompt(false, false)+delegateSystemFragment(true, false))
	tailAfter := strings.TrimPrefix(after, buildSystemPrompt(true, false)+delegateSystemFragment(true, true))
	if tailBefore == before || tailAfter == after {
		t.Fatal("prefix did not match; the composition head changed")
	}
	if tailBefore != tailAfter {
		t.Fatalf("flipping allowWrite changed more than the write fragment:\n before=%q\n after=%q", tailBefore, tailAfter)
	}
}

// newMountSession mirrors main.go's REPL session: file tools first, then
// any host tools the test injects (the runtime receives the same extras),
// the #372 bookkeeping fields, a lateVerifier wired into the orchestrator,
// and checkpoint/trace data isolated under a temp XDG_DATA_HOME. Tool order
// is read with the existing order-preserving names() helper.
func newMountSession(t *testing.T, caller agent.ModelCaller, root string, host ...agent.Tool) *replSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	fileTools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	var in systemInputs
	system := composeSystem(in)
	slot := &lateVerifier{}
	orch := agent.New(caller, agent.ContextManager{}, agent.WithVerifier(slot))
	sess := &replSession{
		orch:          orch,
		runtime:       newTestRuntime(t, root, system, orch, host),
		tools:         append(append([]agent.Tool(nil), fileTools...), host...),
		baseSystem:    system,
		sysInputs:     in,
		root:          root,
		readToolCount: len(fileTools),
		mountAt:       len(fileTools),
		maxSteps:      16,
		clock:         func() time.Time { return time.Unix(0, 0) },
		grants:        newApprovalGrants(),
		verifier:      slot,
	}
	t.Cleanup(func() { _ = sess.closeLateMounts() })
	return sess
}

func TestAllowWriteMountsWriteToolsAndPrompt(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes enabled") {
		t.Fatalf("out = %q", out.String())
	}
	want := append(names(sess.tools[:sess.readToolCount]), "write_file", "edit_file")
	if got := names(sess.tools); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if !sess.allowWrite || sess.journal == nil || sess.lateStore == nil || sess.writeToolCount != 2 {
		t.Fatalf("session not committed: allowWrite=%v journal=%v store=%v count=%d", sess.allowWrite, sess.journal != nil, sess.lateStore != nil, sess.writeToolCount)
	}
	if sess.baseSystem != composeSystem(sess.sysInputs) || !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(true, false)) {
		t.Fatalf("baseSystem not recomposed for writes: %q", sess.baseSystem)
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.system != sess.baseSystem {
		t.Fatalf("runtime sent %q, session holds %q", caller.system, sess.baseSystem)
	}
	if got := strings.Join(caller.tools, ","); got != strings.Join(names(sess.tools), ",") {
		t.Fatalf("provider received tools %s, session holds %s", got, strings.Join(names(sess.tools), ","))
	}
	// The REPL surfaces follow the mount: /tools lists the new tools and the
	// auto-edits line, /checkpoints is live instead of the disabled message.
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/tools")
	if !strings.Contains(out.String(), "write_file (write)") || !strings.Contains(out.String(), "edit_file (write)") || !strings.Contains(out.String(), "auto-edits: off") {
		t.Fatalf("/tools after mount = %q", out.String())
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/checkpoints")
	if strings.Contains(out.String(), "writes disabled") {
		t.Fatalf("/checkpoints still disabled after mount: %q", out.String())
	}
}

func TestAllowWriteIsIdempotent(t *testing.T) {
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	store, journal, n, system := sess.lateStore, sess.journal, len(sess.tools), sess.baseSystem
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if out.String() != "writes already enabled\n" {
		t.Fatalf("second = %q", out.String())
	}
	if sess.lateStore != store || sess.journal != journal || len(sess.tools) != n || sess.baseSystem != system {
		t.Fatal("a repeated /allow-write changed session state")
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write now")
	if out.String() != "usage: /allow-write\n" {
		t.Fatalf("usage = %q", out.String())
	}
}

// Mounting never grants: the write-class grant stays absent and the first
// write prompts, so a scripted "n" denies it.
func TestAllowWriteDoesNotGrantApproval(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{
		writeToolCallResponse("w1", "w.txt", "W\n"),
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	sess := newMountSession(t, caller, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if sess.grants.count() != 0 || autoEditState(sess) != "off" {
		t.Fatalf("mount granted approval: grants=%d auto-edits=%s", sess.grants.count(), autoEditState(sess))
	}
	src := &stubAnswerSource{line: "n", ok: true}
	_, _ = runOnce(context.Background(), &out, nil, sess, "write w", src)
	if _, err := os.Stat(filepath.Join(root, "w.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a denied write landed (stat err=%v); output:\n%s", err, out.String())
	}
}

// A mid-session write goes through the same journal as a startup one, so
// /undo restores the pre-turn content.
func TestAllowWriteMidSessionWritesAreUndoable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "w.txt")
	if err := os.WriteFile(target, []byte("OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	caller := &scriptCaller{responses: []agent.ModelResult{
		writeToolCallResponse("w1", "w.txt", "NEW\n"),
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	sess := newMountSession(t, caller, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	src := &stubAnswerSource{line: "y", ok: true}
	if _, err := runOnce(context.Background(), &out, nil, sess, "write w", src); err != nil {
		t.Fatalf("runOnce: %v\n%s", err, out.String())
	}
	if got, _ := os.ReadFile(target); string(got) != "NEW\n" {
		t.Fatalf("write did not land: %q", got)
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/undo")
	if got, _ := os.ReadFile(target); string(got) != "OLD\n" {
		t.Fatalf("/undo left %q (out=%q)", got, out.String())
	}
}

// The checkpoint lease is exclusive per workspace; holding it first makes
// the mount fail closed before anything is built, with the session,
// runtime snapshot, and approval state untouched.
func TestAllowWriteFailsClosedBeforeConstruction(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root)
	held, err := openCheckpointStore(context.Background(), os.Getenv, root)
	if err != nil {
		t.Fatalf("hold lease: %v", err)
	}
	defer func() { _ = held.Close() }()
	before, system := strings.Join(names(sess.tools), ","), sess.baseSystem
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes not enabled: checkpoint store:") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.allowWrite || sess.journal != nil || sess.lateStore != nil || sess.baseSystem != system || strings.Join(names(sess.tools), ",") != before {
		t.Fatal("a failed mount changed the session")
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.system != system {
		t.Fatalf("runtime snapshot changed on a failed mount: %q", caller.system)
	}
}

// Store, journal and write tools are all built, then Replace rejects the
// mount because a host tool already owns the name edit_file. Every session
// field, the verifier slot, the approval state, and the runtime snapshot
// stay unchanged, and the checkpoint lease is released.
func TestAllowWriteFailsClosedAfterConstruction(t *testing.T) {
	// A leaked store is unreachable once the handler returns, and the
	// os.File finalizer would release its flock at the next GC, making a
	// leak look like a release. No GC, no finalizer: the lease check below
	// observes the handler's own Close, or its absence.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	root := t.TempDir()
	// A real verifier config makes the slot assertion live: a stray set()
	// before the failed mount would install a runner here.
	writeGolemJSON(t, root, `{"verify":{"argv":["/bin/sh","-c","true"]}}`)
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root, fakeNamedTool{name: "edit_file"})
	before, system := strings.Join(names(sess.tools), ","), sess.baseSystem
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes not enabled: runtime:") || !strings.Contains(out.String(), "edit_file") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.allowWrite || sess.journal != nil || sess.lateStore != nil || sess.writeToolCount != 0 ||
		sess.baseSystem != system || strings.Join(names(sess.tools), ",") != before ||
		sess.verifier.runner != nil || sess.grants.count() != 0 {
		t.Fatal("a rejected replacement changed the session")
	}
	store, err := openCheckpointStore(context.Background(), os.Getenv, root)
	if err != nil {
		t.Fatalf("checkpoint lease not released after the failed mount: %v", err)
	}
	_ = store.Close()
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.system != system {
		t.Fatalf("runtime snapshot changed on a rejected replacement: %q", caller.system)
	}
}

func TestCloseLateMountsReleasesTheCheckpointLease(t *testing.T) {
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if _, err := openCheckpointStore(context.Background(), os.Getenv, root); err == nil {
		t.Fatal("lease must be held while the mount is live")
	}
	if err := sess.closeLateMounts(); err != nil {
		t.Fatalf("closeLateMounts: %v", err)
	}
	if sess.lateStore != nil {
		t.Fatal("lateStore not cleared")
	}
	store, err := openCheckpointStore(context.Background(), os.Getenv, root)
	if err != nil {
		t.Fatalf("lease still held after closeLateMounts: %v", err)
	}
	_ = store.Close()
	if err := sess.closeLateMounts(); err != nil {
		t.Fatalf("second closeLateMounts: %v", err)
	}
}

func TestAllowExecMountsExecToolsAndPrompt(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/jobs")
	if !strings.Contains(out.String(), "exec disabled") {
		t.Fatalf("precondition: %q", out.String())
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	if !strings.HasPrefix(out.String(), "exec enabled") {
		t.Fatalf("out = %q", out.String())
	}
	want := append(names(sess.tools[:sess.readToolCount]), "run_command", "start_command", "command_status", "command_tail", "stop_command")
	if got := strings.Join(names(sess.tools), ","); got != strings.Join(want, ",") {
		t.Fatalf("tools = %s, want %s", got, strings.Join(want, ","))
	}
	if !sess.allowExec || sess.bgManager == nil {
		t.Fatal("session not committed")
	}
	if sess.grants.count() != 0 {
		t.Fatalf("mount granted approval: grants=%d", sess.grants.count())
	}
	if sess.baseSystem != composeSystem(sess.sysInputs) || !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(false, true)) {
		t.Fatalf("baseSystem not recomposed for exec: %q", sess.baseSystem)
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/jobs")
	if out.String() != "no background jobs\n" {
		t.Fatalf("/jobs after mount = %q", out.String())
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.system != sess.baseSystem {
		t.Fatalf("runtime sent %q, session holds %q", caller.system, sess.baseSystem)
	}
	if got := strings.Join(caller.tools, ","); got != strings.Join(names(sess.tools), ",") {
		t.Fatalf("provider received tools %s, session holds %s", got, strings.Join(names(sess.tools), ","))
	}
}

func TestAllowExecIsIdempotent(t *testing.T) {
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	mgr, n, system := sess.bgManager, len(sess.tools), sess.baseSystem
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	if out.String() != "exec already enabled\n" || sess.bgManager != mgr || len(sess.tools) != n || sess.baseSystem != system {
		t.Fatalf("repeat changed state: %q", out.String())
	}
	out.Reset()
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec x")
	if out.String() != "usage: /allow-exec\n" {
		t.Fatalf("usage = %q", out.String())
	}
}

// The manager and exec set are built, then Replace rejects the mount because
// a host tool already owns the name run_command. Session, approval state,
// and the runtime snapshot stay unchanged; the unpublished manager is shut
// down by the handler (not observed here: no test-only manager factory).
func TestAllowExecFailsClosedAfterConstruction(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root, fakeNamedTool{name: "run_command"})
	before, system := strings.Join(names(sess.tools), ","), sess.baseSystem
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
	if !strings.HasPrefix(out.String(), "exec not enabled: runtime:") || !strings.Contains(out.String(), "run_command") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.allowExec || sess.bgManager != nil || sess.baseSystem != system || strings.Join(names(sess.tools), ",") != before ||
		sess.grants.count() != 0 || sess.lateStore != nil || sess.journal != nil || sess.allowWrite {
		t.Fatal("a rejected replacement changed the session")
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	if caller.system != system {
		t.Fatalf("runtime snapshot changed on a rejected replacement: %q", caller.system)
	}
}

// Exec after write and write after exec both end in the startup order:
// [file tools][write_file edit_file][run_command ... stop_command].
func TestAllowCommandsKeepStartupOrderInEitherSequence(t *testing.T) {
	want := "write_file,edit_file,run_command,start_command,command_status,command_tail,stop_command"
	for _, order := range [][]string{{"/allow-write", "/allow-exec"}, {"/allow-exec", "/allow-write"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			sess := newMountSession(t, &captureCaller{answer: "ok"}, t.TempDir())
			var out strings.Builder
			for _, c := range order {
				_, _ = dispatchSlash(context.Background(), &out, sess, c)
			}
			got := strings.Join(names(sess.tools[sess.readToolCount:]), ",")
			if got != want {
				t.Fatalf("gated order = %s, want %s (out=%q)", got, want, out.String())
			}
			if !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(true, true)) {
				t.Fatalf("prompt = %q", sess.baseSystem)
			}
		})
	}
}

// Runtime System and trace metadata are byte-identical after a replacement:
// what the provider received is what the trace file records.
func TestAllowWriteRuntimeSystemMatchesTrace(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newMountSession(t, caller, root)
	obs, err := newObserv(os.Getenv, root, true, false, time.Now)
	if err != nil {
		t.Fatalf("newObserv: %v", err)
	}
	sess.obs = obs
	before := sess.baseSystem
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	sent := caller.system
	if sent == before {
		t.Fatal("the runtime prompt did not change after /allow-write")
	}
	// The hash below is computed from sess.tools on both sides, so the
	// provider-received names are what actually pin the mounted set.
	if got := strings.Join(caller.tools, ","); got != strings.Join(names(sess.tools), ",") {
		t.Fatalf("provider received tools %s, session holds %s", got, strings.Join(names(sess.tools), ","))
	}
	files, _ := filepath.Glob(filepath.Join(obs.traceDir, "*.json"))
	if len(files) == 0 {
		t.Fatalf("no trace written under %s", obs.traceDir)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var rec agenttrace.TraceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if rec.Request.System != sent {
			t.Fatalf("%s records a different System than the runtime sent:\n trace=%q\n sent=%q", f, rec.Request.System, sent)
		}
		if rec.Request.ToolSchemaHash != toolSchemaHash(sess.tools) {
			t.Fatalf("%s tool hash %q != mounted set %q", f, rec.Request.ToolSchemaHash, toolSchemaHash(sess.tools))
		}
	}
}

// After a mid-session mount, #347 verification runs: the .golem.json command
// leaves a marker file, which only a wired verifier can produce.
func TestAllowWriteInstallsPostWriteVerifier(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":["/bin/sh","-c","echo ran > verified.txt"]}}`)
	caller := &scriptCaller{responses: []agent.ModelResult{
		writeToolCallResponse("w1", "w.txt", "W\n"),
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	sess := newMountSession(t, caller, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if strings.Contains(out.String(), "verification disabled") {
		t.Fatalf("verifier not built: %q", out.String())
	}
	src := &stubAnswerSource{line: "y", ok: true} // approves the write and the verify prompt
	if _, err := runOnce(context.Background(), &out, nil, sess, "write w", src); err != nil {
		t.Fatalf("runOnce: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "verified.txt")); err != nil {
		t.Fatalf("post-write verification did not run after the mid-session mount: %v\n%s", err, out.String())
	}
}

// Recovery is durable and one-shot: the interrupted turn is sealed into a
// checkpoint by recoverStartup itself. Its notice must therefore reach the
// user even when a later step of the mount fails, or the recovered work
// becomes invisible (a retry finds nothing left to recover).
func TestAllowWriteReportsRecoveryEvenWhenTheMountFails(t *testing.T) {
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root, fakeNamedTool{name: "edit_file"})
	ctx := context.Background()
	staged, err := openCheckpointStore(ctx, os.Getenv, root)
	if err != nil {
		t.Fatalf("stage store: %v", err)
	}
	target := filepath.Join(root, "c.txt")
	if err := os.WriteFile(target, []byte("C0"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fc, err := staged.prepareIntent(ctx, "crashed", testNow, testRec("c.txt", "C0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.WriteFile(target, []byte("C1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.commitIntent(ctx, fc); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatalf("release staged lease: %v", err)
	}
	var out strings.Builder
	_, _ = dispatchSlash(ctx, &out, sess, "/allow-write")
	if !strings.Contains(out.String(), "recovered an interrupted turn") {
		t.Fatalf("recovery notice lost behind the failed mount:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "writes not enabled: runtime:") {
		t.Fatalf("mount must still fail on the collision:\n%s", out.String())
	}
}

// A workspace verifier that cannot be installed (no slot in this session
// mode) is reported, never silently dropped.
func TestAllowWriteReportsMissingVerifierSlot(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":["/bin/sh","-c","true"]}}`)
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	sess.verifier = nil
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes enabled") || !strings.Contains(out.String(), "verification disabled: this session has no post-write verifier slot") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestNonNilVerifierNormalizesTypedNils(t *testing.T) {
	if got := nonNilVerifier((*verifyRunner)(nil)); got != nil {
		t.Fatalf("typed-nil *verifyRunner leaked as %#v", got)
	}
	if got := nonNilVerifier((*lateVerifier)(nil)); got != nil {
		t.Fatalf("typed-nil *lateVerifier leaked as %#v", got)
	}
	slot := &lateVerifier{}
	if got := nonNilVerifier(slot); got != agent.Verifier(slot) {
		t.Fatal("a real verifier must pass through unchanged")
	}
	if got := nonNilVerifier(nil); got != nil {
		t.Fatal("nil must stay nil")
	}
}

// Reachable D9 state: startup was -allow-exec -scratch without -allow-write,
// so the exec set carries scratch tools and no promotion. A later
// /allow-write keeps that set and its manager exactly, inserts the write
// tools before it, and says promotion stays startup-bound.
func TestAllowWriteKeepsStartupScratchExecSet(t *testing.T) {
	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	opts, _ := scratchExecOptions(true, nil)
	mgr, execTools, err := buildExecMount(root, opts)
	if err != nil {
		t.Fatalf("scratch exec set: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	sess := newMountSession(t, caller, root, execTools...)
	sess.allowExec, sess.bgManager, sess.scratch = true, mgr, true
	sess.sysInputs.allowExec = true
	sess.baseSystem = composeSystem(sess.sysInputs)
	before := strings.Join(names(execTools), ",")
	if !strings.Contains(before, "scratch_changes") || strings.Contains(before, "promote_artifact") {
		t.Fatalf("precondition: scratch set without promotion expected, got %s", before)
	}
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes enabled") || !strings.Contains(out.String(), "scratch: promote_artifact stays unavailable") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.bgManager != mgr {
		t.Fatal("/allow-write replaced the startup background manager")
	}
	got := strings.Join(names(sess.tools[sess.readToolCount:]), ",")
	if got != "write_file,edit_file,"+before {
		t.Fatalf("gated tools = %s, want write tools inserted before the untouched scratch exec set", got)
	}
	if !strings.HasPrefix(sess.baseSystem, buildSystemPrompt(true, true)) {
		t.Fatalf("prompt = %q", sess.baseSystem)
	}
}

// An interrupted undo left by a previous process is reported by /allow-write
// exactly as startup -allow-write reports it.
func TestAllowWriteReportsInterruptedUndo(t *testing.T) {
	root := t.TempDir()
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	ctx := context.Background()
	staged, err := openCheckpointStore(ctx, os.Getenv, root)
	if err != nil {
		t.Fatalf("stage store: %v", err)
	}
	target := filepath.Join(root, "u.txt")
	if err := os.WriteFile(target, []byte("U0"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, fc, err := staged.prepareIntent(ctx, "crashed", testNow, testRec("u.txt", "U0", true))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := staged.commitIntent(ctx, fc); err != nil {
		t.Fatalf("commit: %v", err)
	}
	_, journal, err := buildWriteTools(root, staged)
	if err != nil {
		t.Fatalf("buildWriteTools: %v", err)
	}
	if _, err := journal.recoverStartup(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	groups, err := staged.newestCompleted(ctx, 1)
	if err != nil || len(groups) != 1 {
		t.Fatalf("newestCompleted: %v (%d groups)", err, len(groups))
	}
	if err := staged.markUndoing(ctx, []int64{groups[0].id}); err != nil {
		t.Fatalf("markUndoing: %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatalf("release staged lease: %v", err)
	}
	var out strings.Builder
	_, _ = dispatchSlash(ctx, &out, sess, "/allow-write")
	if !strings.Contains(out.String(), "an interrupted undo exists (1 checkpoint(s)); /undo resumes it") {
		t.Fatalf("interrupted undo not reported:\n%s", out.String())
	}
}

// A workspace verifier that cannot be armed is reported, and the slot stays
// empty, so the user never believes verification is active when it is not.
func TestAllowWriteReportsVerifierWarning(t *testing.T) {
	root := t.TempDir()
	writeGolemJSON(t, root, `{"verify":{"argv":[]}}`)
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
	if !strings.HasPrefix(out.String(), "writes enabled") || !strings.Contains(out.String(), "verification disabled") {
		t.Fatalf("out = %q", out.String())
	}
	if sess.verifier.runner != nil {
		t.Fatal("a verifier that could not be armed must not be installed")
	}
}

func TestBuildExecMountFailsClosedOnBadRoot(t *testing.T) {
	mgr, tools, err := buildExecMount(filepath.Join(t.TempDir(), "missing"), agenttools.ExecToolsOptions{})
	if err == nil || mgr != nil || tools != nil {
		t.Fatalf("buildExecMount on a missing root = %v, %v, %v; want nil, nil, error", mgr, tools, err)
	}
}

func TestHelpListsAllowCommands(t *testing.T) {
	for _, want := range []string{"/allow-write", "/allow-exec"} {
		if !strings.Contains(golemHelp, want) {
			t.Fatalf("help must document %s:\n%s", want, golemHelp)
		}
	}
}
