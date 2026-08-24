package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// --- fixtures (reuse the manager test fakes; no second fake family) ---

// newStartCommandFixture builds a StartCommand over a real temp Workspace with
// a resolvable "mycmd" on a sanitized PATH, wired to a manager driven by the
// given fake starter.
func newStartCommandFixture(t *testing.T, starter *fakeStarter) (*StartCommand, *BackgroundManager) {
	t.Helper()
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")
	m := newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(m.Shutdown)
	return NewStartCommand(ws, m), m
}

// bgJobFixture drives one fake-backed job directly through the manager so the
// read-side tools (status/tail/stop) can be tested without a Workspace.
type bgJobFixture struct {
	m      *BackgroundManager
	proc   *fakeProc
	stdout io.Writer
	stderr io.Writer
	handle string
}

func newBGJobFixture(t *testing.T, argv []string, cwd string) *bgJobFixture {
	t.Helper()
	f := &bgJobFixture{proc: newFakeProc(4242)}
	starter := &fakeStarter{fn: func(_ execSpec, out, errw io.Writer) (backgroundProcess, error) {
		f.stdout, f.stderr = out, errw
		return f.proc, nil
	}}
	f.m = newBackgroundManager(starter, &countingRandom{})
	t.Cleanup(f.m.Shutdown)
	st, err := f.m.start(context.Background(), execSpec{Path: "/bin/cmd", Argv: argv, Dir: "/w"}, cwd)
	if err != nil {
		t.Fatalf("start fixture job: %v", err)
	}
	f.handle = st.Handle
	return f
}

// finish releases the fake process and waits for completion publication.
func (f *bgJobFixture) finish(t *testing.T) {
	t.Helper()
	done := jobDoneChan(t, f.m, f.handle)
	f.proc.release()
	<-done
}

func (f *bgJobFixture) write(t *testing.T, w io.Writer, s string) {
	t.Helper()
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("fixture write: %v", err)
	}
}

func mustPlan(t *testing.T, pt agent.PlanningTool, raw json.RawMessage) agent.ToolPlan {
	t.Helper()
	plan, err := pt.Plan(context.Background(), raw)
	if err != nil {
		t.Fatalf("Plan(%s): %v", raw, err)
	}
	return plan
}

func mustInvoke(t *testing.T, tool agent.Tool, raw json.RawMessage) agent.ToolResult {
	t.Helper()
	res, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke(%s): %v", raw, err)
	}
	return res
}

// tailPayload splits a command_tail result into the raw stream bytes after the
// frozen output delimiter.
func tailPayload(t *testing.T, content string) string {
	t.Helper()
	const delim = "--- output ---\n"
	i := strings.Index(content, delim)
	if i < 0 {
		t.Fatalf("no output delimiter in %q", content)
	}
	return content[i+len(delim):]
}

const fixtureHandle = "bg-01010101010101010101010101010101"

// --- Step 5.1: contract (names, schemas, effects, approvals, planning shape) ---

func TestBackgroundExecToolsContract(t *testing.T) {
	m := newBackgroundManager(starterOf(), &countingRandom{})
	t.Cleanup(m.Shutdown)
	start := NewStartCommand(nil, m)
	status := NewCommandStatus(m)
	tail := NewCommandTail(m)
	stop := NewStopCommand(m)

	cases := []struct {
		tool       agent.Tool
		name       string
		schemaHas  []string
		class      agent.EffectClass
		exactClass bool
		approval   agent.ApprovalPolicy
		timeout    bool // must be the frozen 10s
		outputCap  int
		planning   bool
	}{
		{
			tool: start, name: "start_command",
			schemaHas:  []string{`"argv"`, `"dir"`, `"required":["argv"]`},
			class:      agent.Read | agent.Write | agent.Exec | agent.Network,
			exactClass: true, approval: agent.ApprovalAlways,
			timeout: true, outputCap: 4096, planning: true,
		},
		{
			tool: status, name: "command_status",
			schemaHas:  []string{`"handle"`, `"required":["handle"]`},
			class:      agent.Read,
			exactClass: true, approval: agent.ApprovalNever,
			outputCap: 4096,
		},
		{
			tool: tail, name: "command_tail",
			schemaHas:  []string{`"handle"`, `"stream"`, `"cursor"`, `"max_bytes"`, `"stdout"`, `"stderr"`, `"required":["handle"]`},
			class:      agent.Read,
			exactClass: true, approval: agent.ApprovalNever,
			outputCap: 20 * 1024,
		},
		{
			tool: stop, name: "stop_command",
			schemaHas:  []string{`"handle"`, `"required":["handle"]`},
			class:      agent.Exec,
			exactClass: true, approval: agent.ApprovalAlways,
			timeout: true, outputCap: 4096, planning: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.tool.Spec()
			if s.Name != tc.name {
				t.Errorf("name = %q, want %q", s.Name, tc.name)
			}
			for _, want := range tc.schemaHas {
				if !strings.Contains(string(s.Parameters), want) {
					t.Errorf("schema missing %s: %s", want, s.Parameters)
				}
			}
			e := tc.tool.Effect()
			if tc.exactClass && e.Class != tc.class {
				t.Errorf("effect class = %v, want exactly %v", e.Class, tc.class)
			}
			if e.Approval != tc.approval {
				t.Errorf("approval = %v, want %v", e.Approval, tc.approval)
			}
			if tc.timeout && e.Timeout != bgToolTimeout {
				t.Errorf("timeout = %v, want %v", e.Timeout, bgToolTimeout)
			}
			if e.OutputCap != tc.outputCap {
				t.Errorf("OutputCap = %d, want %d", e.OutputCap, tc.outputCap)
			}
			_, isPlanning := tc.tool.(agent.PlanningTool)
			if isPlanning != tc.planning {
				t.Errorf("PlanningTool = %v, want %v", isPlanning, tc.planning)
			}
		})
	}
}

func TestBackgroundExecToolsConstructor(t *testing.T) {
	if _, err := NewExecToolsWithBackground(t.TempDir(), nil); err == nil {
		t.Error("nil manager: want error")
	}
	m := newBackgroundManager(starterOf(), &countingRandom{})
	t.Cleanup(m.Shutdown)
	tools, err := NewExecToolsWithBackground(t.TempDir(), m)
	if err != nil {
		t.Fatalf("NewExecToolsWithBackground: %v", err)
	}
	wantOrder := []string{"run_command", "start_command", "command_status", "command_tail", "stop_command"}
	if len(tools) != len(wantOrder) {
		t.Fatalf("got %d tools, want %d", len(tools), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got := tools[i].Spec().Name; got != want {
			t.Errorf("tools[%d] = %q, want %q", i, got, want)
		}
	}
	// One shared Workspace across foreground and background preparation.
	rc, ok := tools[0].(*RunCommand)
	if !ok {
		t.Fatal("tools[0] is not *RunCommand")
	}
	sc, ok := tools[1].(*StartCommand)
	if !ok {
		t.Fatal("tools[1] is not *StartCommand")
	}
	if rc.ws != sc.ws {
		t.Error("run_command and start_command must share one Workspace")
	}
	// Compat: the foreground-only constructor is unchanged.
	fg, err := NewExecTools(t.TempDir())
	if err != nil {
		t.Fatalf("NewExecTools: %v", err)
	}
	if len(fg) != 1 || fg[0].Spec().Name != "run_command" {
		t.Errorf("NewExecTools = %d tools (first %q), want the single run_command", len(fg), fg[0].Spec().Name)
	}
}

// --- Step 5.2/5.3: start planning ---

func TestStartCommandPlanPreview(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, _ := newStartCommandFixture(t, starterOf())
	plan := mustPlan(t, sc, json.RawMessage(`{"argv":["mycmd","a b"]}`))
	for _, want := range []string{
		`mycmd "a b"`,      // argv rendered via renderArgvForPreview
		"mycmd -> ",        // original argv0 -> resolved path
		"(workspace root)", // cwd display
		"(parent)",         // env source markers
		"id:",              // short fingerprint id line
		"lifetime:",        // manager-owned lifetime line
		"session shutdown", // ...naming the shutdown bound
	} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("preview missing %q:\n%s", want, plan.Preview)
		}
	}
	if strings.Contains(plan.Preview, "timeout") {
		t.Errorf("start preview must not carry a timeout line:\n%s", plan.Preview)
	}
	// The id line is the short display form of the full-digest key suffix.
	suffix := strings.TrimPrefix(plan.ApprovalKey, "exec-bg:v1:")
	if !strings.Contains(plan.Preview, suffix[:fingerprintLen]) {
		t.Errorf("preview id is not the short form of the key digest:\n%s", plan.Preview)
	}
}

func TestStartCommandApprovalKeyNamespaceAndDigest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, _ := newStartCommandFixture(t, starterOf())
	plan := mustPlan(t, sc, json.RawMessage(`{"argv":["mycmd"]}`))
	if !strings.HasPrefix(plan.ApprovalKey, "exec-bg:v1:") {
		t.Fatalf("key %q must be namespaced with \"exec-bg:v1:\"", plan.ApprovalKey)
	}
	// Step 5.3: the digest suffix must equal the CANONICAL fingerprint function
	// applied with zero effective and requested timeouts (manager-owned
	// lifetime) — compared against commandFingerprint directly, never against a
	// second instance of the background helper.
	env, _ := buildExecEnv(os.LookupEnv)
	path, _, err := resolveExecutable(sc.ws, sc.ws.root, "mycmd", pathFromEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	want := commandFingerprint([]string{"mycmd"}, sc.ws.root, env, 0, 0, path)
	if got := strings.TrimPrefix(plan.ApprovalKey, "exec-bg:v1:"); got != want {
		t.Errorf("key digest = %q, want canonical zero-timeout fingerprint %q", got, want)
	}
}

func TestStartCommandKeySeparateFromRunCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, _ := newStartCommandFixture(t, starterOf())
	rc := NewRunCommand(sc.ws, nil)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	startKey := mustPlan(t, sc, raw).ApprovalKey
	runKey := mustPlan(t, rc, raw).ApprovalKey
	if startKey == runKey {
		t.Fatalf("same argv/dir produced one key for foreground and background: %q", startKey)
	}
	if !strings.HasPrefix(startKey, "exec-bg:v1:") || strings.HasPrefix(startKey, "exec:v2:") {
		t.Errorf("start key %q must live in the exec-bg:v1: namespace, never exec:v2:", startKey)
	}
	if !strings.HasPrefix(runKey, "exec:v2:") {
		t.Errorf("run key %q must keep the exec:v2: namespace", runKey)
	}
}

func TestStartCommandPlanRejects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, _ := newStartCommandFixture(t, starterOf())
	for name, raw := range map[string]string{
		"empty argv":  `{"argv":[]}`,
		"blank argv0": `{"argv":["  "]}`,
		"unknown dir": `{"argv":["mycmd"],"dir":"missing"}`,
		"bad json":    `{"argv":`,
	} {
		if _, err := sc.Plan(context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("%s: Plan accepted %s", name, raw)
		}
	}
}

func TestStartCommandInvokeWithoutMatchingPlan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	starter := starterOf(newFakeProc(1))
	sc, _ := newStartCommandFixture(t, starter)
	mustPlan(t, sc, json.RawMessage(`{"argv":["mycmd"]}`))
	// Different raw args -> hash mismatch -> fail closed without spawning.
	res := mustInvoke(t, sc, json.RawMessage(`{"argv":["mycmd","x"]}`))
	if !res.IsError || res.Content != "start preview missing; retry" {
		t.Errorf("result = (%q, IsError=%v), want the frozen fail-closed message", res.Content, res.IsError)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times without a matching plan, want 0", starter.callCount())
	}
}

// --- Step 5.4: start invocation ---

func TestStartCommandInvokeSuccessPublishesHandle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, m := newStartCommandFixture(t, starterOf(newFakeProc(4242)))
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	res := mustInvoke(t, sc, raw)
	if res.IsError {
		t.Fatalf("Invoke: %s", res.Content)
	}
	want := "handle: " + fixtureHandle + "\npid: 4242\nstate: running\n"
	if res.Content != want {
		t.Errorf("content = %q, want golden %q", res.Content, want)
	}
	st, ok := m.status(fixtureHandle)
	if !ok || st.State != backgroundStateRunning {
		t.Errorf("published job = (%+v, %v), want running under the returned handle", st, ok)
	}
	if st.Cwd != "(workspace root)" {
		t.Errorf("cwd display = %q, want workspace-root label, not the host path", st.Cwd)
	}
}

func TestStartCommandInvokeDirDisplayUsesLabel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	sc, m := newStartCommandFixture(t, starterOf(newFakeProc(7)))
	if err := os.Mkdir(filepath.Join(sc.ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"argv":["mycmd"],"dir":"sub"}`)
	mustPlan(t, sc, raw)
	if res := mustInvoke(t, sc, raw); res.IsError {
		t.Fatalf("Invoke: %s", res.Content)
	}
	if st, _ := m.status(fixtureHandle); st.Cwd != "sub" {
		t.Errorf("cwd display = %q, want the workspace-relative label", st.Cwd)
	}
}

func TestStartCommandInvokeDirIdentitySwapFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	starter := starterOf(newFakeProc(1))
	sc, _ := newStartCommandFixture(t, starter)
	subDir := filepath.Join(sc.ws.root, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"argv":["mycmd"],"dir":"sub"}`)
	mustPlan(t, sc, raw)
	// Same path, new inode: sibling + rename (remove+recreate can reuse the inode).
	newSub := filepath.Join(sc.ws.root, "sub.new")
	if err := os.Mkdir(newSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newSub, subDir); err != nil {
		t.Fatal(err)
	}
	res := mustInvoke(t, sc, raw)
	if !res.IsError || !strings.Contains(res.Content, "working directory changed") {
		t.Errorf("result = (%q, IsError=%v), want the dir-changed fail-closed error", res.Content, res.IsError)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times after a failed recheck, want 0", starter.callCount())
	}
}

func TestStartCommandInvokeExecutableSwapFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	starter := starterOf(newFakeProc(1))
	sc, _ := newStartCommandFixture(t, starter)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	// White-box the pending plan's resolved path, then swap the binary at a
	// guaranteed-new inode via sibling + rename.
	sc.mu.Lock()
	resolved := sc.plan.path
	sc.mu.Unlock()
	pathDir := filepath.Dir(resolved)
	swapped := filepath.Join(pathDir, "mycmd.new")
	writeExecutable(t, swapped, "#!/bin/sh\n# inode B\n")
	if err := os.Rename(swapped, filepath.Join(pathDir, "mycmd")); err != nil {
		t.Fatal(err)
	}
	res := mustInvoke(t, sc, raw)
	if !res.IsError || !strings.Contains(res.Content, "executable changed") {
		t.Errorf("result = (%q, IsError=%v), want the exe-changed fail-closed error", res.Content, res.IsError)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times after a failed recheck, want 0", starter.callCount())
	}
}

func TestStartCommandInvokeCancelBeforeSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	starter := starterOf(newFakeProc(1))
	sc, m := newStartCommandFixture(t, starter)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := sc.Invoke(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Content != "start canceled" {
		t.Errorf("result = (%q, IsError=%v), want start canceled", res.Content, res.IsError)
	}
	if starter.callCount() != 0 {
		t.Errorf("starter called %d times for a pre-canceled start, want 0", starter.callCount())
	}
	if len(m.List()) != 0 {
		t.Error("canceled start must publish nothing")
	}
}

func TestStartCommandInvokeCancelDuringGatedSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	proc := newFakeProc(7)
	entered := make(chan struct{})
	gate := make(chan struct{})
	starter := &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		close(entered)
		<-gate
		return proc, nil
	}}
	sc, m := newStartCommandFixture(t, starter)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan agent.ToolResult, 1)
	go func() {
		res, _ := sc.Invoke(ctx, raw)
		resCh <- res
	}()
	<-entered
	cancel()
	close(gate)
	res := <-resCh
	if !res.IsError || res.Content != "start canceled" {
		t.Errorf("result = (%q, IsError=%v), want start canceled", res.Content, res.IsError)
	}
	// Spawned but never registered: reaped before Invoke returned.
	if proc.killCount() == 0 || !proc.waitDone() {
		t.Errorf("late process killed=%d waited=%v, want both before return", proc.killCount(), proc.waitDone())
	}
	if len(m.List()) != 0 {
		t.Error("canceled start must publish nothing")
	}
}

func TestStartCommandInvokeCancelAfterRegistrationLeavesJobAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	proc := newFakeProc(9)
	sc, m := newStartCommandFixture(t, starterOf(proc))
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	ctx, cancel := context.WithCancel(context.Background())
	res, err := sc.Invoke(ctx, raw)
	if err != nil || res.IsError {
		t.Fatalf("Invoke = (%q, %v)", res.Content, err)
	}
	// Step 5.9: cancellation past the registration linearization point must
	// never touch the published process — the manager owns its lifetime.
	cancel()
	st, ok := m.status(fixtureHandle)
	if !ok || st.State != backgroundStateRunning {
		t.Errorf("job after dispatch-ctx cancel = (%+v, %v), want still running", st, ok)
	}
	if proc.killCount() != 0 {
		t.Errorf("kill count = %d after dispatch-ctx cancel, want 0", proc.killCount())
	}
}

func TestStartCommandInvokeSpawnFailureReleasesSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	fail := true
	var procs []*fakeProc
	for i := 0; i < backgroundActiveCap; i++ {
		procs = append(procs, newFakeProc(100+i))
	}
	i := 0
	starter := &fakeStarter{fn: func(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
		if fail {
			fail = false
			return nil, fmt.Errorf("spawn boom")
		}
		p := procs[i]
		i++
		return p, nil
	}}
	sc, _ := newStartCommandFixture(t, starter)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	mustPlan(t, sc, raw)
	res := mustInvoke(t, sc, raw)
	if !res.IsError || !strings.Contains(res.Content, "spawn boom") {
		t.Errorf("result = (%q, IsError=%v), want the wrapped spawn failure", res.Content, res.IsError)
	}
	// The reserved slot must be restored: the full active cap still starts.
	for j := 0; j < backgroundActiveCap; j++ {
		mustPlan(t, sc, raw)
		if res := mustInvoke(t, sc, raw); res.IsError {
			t.Fatalf("start %d after spawn failure: %s", j, res.Content)
		}
	}
}

// --- Step 5.5: status and tail ---

func TestCommandStatusGoldenRendering(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd", "a b"}, "~/w")
	cs := NewCommandStatus(f.m)
	raw := json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle))
	f.write(t, f.stdout, "hello")
	f.write(t, f.stderr, "oops")

	head := "handle: " + fixtureHandle + "\n"
	tailLines := "pid: 4242\ncwd: \"~/w\"\nargv: [\"cmd\",\"a b\"]\n" +
		"stdout_floor: 0\nstdout_cursor: 5\nstderr_floor: 0\nstderr_cursor: 4\n"

	res := mustInvoke(t, cs, raw)
	if want := head + "state: running\n" + tailLines; res.Content != want {
		t.Errorf("running content = %q, want %q", res.Content, want)
	}

	f.proc.code = 5
	f.finish(t)
	res = mustInvoke(t, cs, raw)
	if want := head + "state: exited\n" + tailLines + "exit_code: 5\n"; res.Content != want {
		t.Errorf("exited content = %q, want %q", res.Content, want)
	}
}

func TestCommandStatusKilledRendering(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	if _, err := f.m.Stop(context.Background(), f.handle); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res := mustInvoke(t, NewCommandStatus(f.m), json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	want := "handle: " + fixtureHandle + "\nstate: killed\npid: 4242\ncwd: \"/w\"\nargv: [\"cmd\"]\n" +
		"stdout_floor: 0\nstdout_cursor: 0\nstderr_floor: 0\nstderr_cursor: 0\nexit_code: -1\n"
	if res.Content != want {
		t.Errorf("killed content = %q, want %q", res.Content, want)
	}
}

func TestCommandStatusUnknownHandle(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	cs := NewCommandStatus(f.m)
	res := mustInvoke(t, cs, json.RawMessage(`{"handle":"bg-nope"}`))
	if !res.IsError || res.Content != `unknown background job handle "bg-nope"` {
		t.Errorf("unknown handle = (%q, IsError=%v), want the frozen error", res.Content, res.IsError)
	}
	if strings.Contains(res.Content, "state:") {
		t.Error("unknown handle must not fabricate a state block")
	}
	if res := mustInvoke(t, cs, json.RawMessage(`{}`)); !res.IsError {
		t.Error("missing handle must be a model-visible error")
	}
}

func TestCommandTailGoldenRendering(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	f.write(t, f.stdout, "hello")
	res := mustInvoke(t, NewCommandTail(f.m), json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	want := "handle: " + fixtureHandle + "\nstream: stdout\nnext_cursor: 5\ndropped_bytes: 0\neof: false\n--- output ---\nhello"
	if res.Content != want {
		t.Errorf("content = %q, want golden %q", res.Content, want)
	}
}

func TestCommandTailStreamIsolationAndDefault(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	ct := NewCommandTail(f.m)
	f.write(t, f.stdout, "outdata")
	f.write(t, f.stderr, "errdata")

	res := mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	if got := tailPayload(t, res.Content); got != "outdata" {
		t.Errorf("default stream payload = %q, want stdout bytes only", got)
	}
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"stream":"stderr"}`, f.handle)))
	if got := tailPayload(t, res.Content); got != "errdata" {
		t.Errorf("stderr payload = %q, want stderr bytes only", got)
	}
	if !strings.Contains(res.Content, "stream: stderr\n") {
		t.Errorf("stderr tail must label its stream:\n%s", res.Content)
	}
}

func TestCommandTailOmittedCursorIsNewestTail(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	f.write(t, f.stdout, "0123456789")
	res := mustInvoke(t, NewCommandTail(f.m),
		json.RawMessage(fmt.Sprintf(`{"handle":%q,"max_bytes":4}`, f.handle)))
	// Newest max_bytes, NOT the oldest prefix; dropped stays 0 in this mode.
	if got := tailPayload(t, res.Content); got != "6789" {
		t.Errorf("newest-tail payload = %q, want the newest 4 bytes", got)
	}
	for _, want := range []string{"next_cursor: 10\n", "dropped_bytes: 0\n"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q:\n%s", want, res.Content)
		}
	}
}

func TestCommandTailExplicitCursorIncremental(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	ct := NewCommandTail(f.m)
	f.write(t, f.stdout, "hello world")

	res := mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":0,"max_bytes":5}`, f.handle)))
	if got := tailPayload(t, res.Content); got != "hello" {
		t.Errorf("cursor 0 payload = %q, want the OLDEST 5 bytes (cursor mode, not newest-tail)", got)
	}
	if !strings.Contains(res.Content, "next_cursor: 5\n") {
		t.Errorf("cursor 0 content missing next_cursor 5:\n%s", res.Content)
	}
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":5}`, f.handle)))
	if got := tailPayload(t, res.Content); got != " world" {
		t.Errorf("cursor 5 payload = %q, want the remainder", got)
	}
	// Cursor at/ahead of end: no bytes, current end cursor.
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":99}`, f.handle)))
	if got := tailPayload(t, res.Content); got != "" {
		t.Errorf("ahead-of-end payload = %q, want empty", got)
	}
	if !strings.Contains(res.Content, "next_cursor: 11\n") {
		t.Errorf("ahead-of-end content missing clamped end cursor:\n%s", res.Content)
	}
}

func TestCommandTailDroppedBytesExact(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	data := make([]byte, backgroundRingCap+100)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	f.write(t, f.stdout, string(data))
	res := mustInvoke(t, NewCommandTail(f.m),
		json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":0,"max_bytes":10}`, f.handle)))
	head := res.Content
	if len(head) > 200 {
		head = head[:200]
	}
	if !strings.Contains(res.Content, "dropped_bytes: 100\n") {
		t.Errorf("content must report the exact evicted gap:\n%s", head)
	}
	if got, want := tailPayload(t, res.Content), string(data[100:110]); got != want {
		t.Errorf("payload = %q, want the 10 bytes at the floor %q", got, want)
	}
	if !strings.Contains(res.Content, "next_cursor: 110\n") {
		t.Errorf("content missing next_cursor 110:\n%s", head)
	}
}

func TestCommandTailMaxBytesValidationAndClamp(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	ct := NewCommandTail(f.m)
	f.write(t, f.stdout, strings.Repeat("x", 17000))

	for _, raw := range []string{
		fmt.Sprintf(`{"handle":%q,"max_bytes":0}`, f.handle),
		fmt.Sprintf(`{"handle":%q,"max_bytes":-5}`, f.handle),
	} {
		res := mustInvoke(t, ct, json.RawMessage(raw))
		if !res.IsError || !strings.Contains(res.Content, "max_bytes") {
			t.Errorf("%s = (%q, IsError=%v), want a model-visible max_bytes error", raw, res.Content, res.IsError)
		}
	}
	// Oversized clamps to 16384 (never reaches the ring un-clamped).
	res := mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"max_bytes":100000}`, f.handle)))
	if res.IsError {
		t.Fatalf("oversized max_bytes: %s", res.Content)
	}
	if got := len(tailPayload(t, res.Content)); got != 16384 {
		t.Errorf("payload length = %d, want clamped 16384", got)
	}
	// Omitted defaults to 8192.
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	if got := len(tailPayload(t, res.Content)); got != 8192 {
		t.Errorf("default payload length = %d, want 8192", got)
	}
}

func TestCommandTailEOFTiming(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	ct := NewCommandTail(f.m)
	f.write(t, f.stdout, "hello")

	res := mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	if !strings.Contains(res.Content, "eof: false\n") {
		t.Errorf("running tail must render eof false:\n%s", res.Content)
	}
	f.finish(t)
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":5}`, f.handle)))
	if !strings.Contains(res.Content, "eof: true\n") {
		t.Errorf("post-publication end-of-stream tail must render eof true:\n%s", res.Content)
	}
	// A read that has not reached the end stays eof false even after exit.
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"cursor":0,"max_bytes":2}`, f.handle)))
	if !strings.Contains(res.Content, "eof: false\n") {
		t.Errorf("mid-stream tail must render eof false even after exit:\n%s", res.Content)
	}
}

func TestCommandTailUnknownHandleAndBadStream(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	ct := NewCommandTail(f.m)
	res := mustInvoke(t, ct, json.RawMessage(`{"handle":"bg-nope"}`))
	if !res.IsError || res.Content != `unknown background job handle "bg-nope"` {
		t.Errorf("unknown handle = (%q, IsError=%v), want the frozen error", res.Content, res.IsError)
	}
	res = mustInvoke(t, ct, json.RawMessage(fmt.Sprintf(`{"handle":%q,"stream":"both"}`, f.handle)))
	if !res.IsError || !strings.Contains(res.Content, `unknown stream "both"`) {
		t.Errorf("bad stream = (%q, IsError=%v), want a stream error", res.Content, res.IsError)
	}
	if res := mustInvoke(t, ct, json.RawMessage(`{}`)); !res.IsError {
		t.Error("missing handle must be a model-visible error")
	}
}

// --- Step 5.6: stop ---

func TestStopCommandPlanUngrantable(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd", "a b"}, "/w")
	stop := NewStopCommand(f.m)
	plan := mustPlan(t, stop, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	// Frozen: every model stop prompts. Empty key = structurally ungrantable.
	if plan.ApprovalKey != "" {
		t.Fatalf("stop ApprovalKey = %q, want empty (ungrantable)", plan.ApprovalKey)
	}
	if plan.Effect.Approval != agent.ApprovalAlways {
		t.Errorf("plan approval = %v, want ApprovalAlways", plan.Effect.Approval)
	}
	for _, want := range []string{f.handle, `cmd "a b"`, "4242", "running"} {
		if !strings.Contains(plan.Preview, want) {
			t.Errorf("stop preview missing %q:\n%s", want, plan.Preview)
		}
	}
}

func TestStopCommandPlanUnknownHandleErrors(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	stop := NewStopCommand(f.m)
	if _, err := stop.Plan(context.Background(), json.RawMessage(`{"handle":"bg-nope"}`)); err == nil {
		t.Error("unknown handle: Plan must error so the user is never prompted")
	}
	if _, err := stop.Plan(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("missing handle: Plan must error")
	}
}

func TestStopCommandInvokeKillGolden(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	res := mustInvoke(t, NewStopCommand(f.m), json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	if res.IsError {
		t.Fatalf("stop: %s", res.Content)
	}
	want := "handle: " + fixtureHandle + "\nstate: killed\nexit_code: -1\n"
	if res.Content != want {
		t.Errorf("content = %q, want golden %q", res.Content, want)
	}
}

func TestStopCommandInvokeFinishedNoop(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	f.finish(t)
	res := mustInvoke(t, NewStopCommand(f.m), json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	want := "handle: " + fixtureHandle + "\nstate: exited\nexit_code: 0\n"
	if res.Content != want {
		t.Errorf("content = %q, want golden %q", res.Content, want)
	}
	if f.proc.killCount() != 0 {
		t.Errorf("kill count = %d for a finished job, want 0 (no-op)", f.proc.killCount())
	}
}

func TestStopCommandInvokeCanceledWaitContinuesReaping(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	f.proc.killReleases = false // hold the stop-vs-reap window open
	stop := NewStopCommand(f.m)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := stop.Invoke(ctx, json.RawMessage(fmt.Sprintf(`{"handle":%q}`, f.handle)))
	if err != nil {
		t.Fatal(err)
	}
	// Early return by contract: current snapshot, still running, no exit_code.
	want := "handle: " + fixtureHandle + "\nstate: running\n"
	if res.IsError || res.Content != want {
		t.Errorf("content = (%q, IsError=%v), want golden early-return %q", res.Content, res.IsError, want)
	}
	// The manager's own cleanup must still complete after the tool returned.
	f.finish(t)
	st, ok := f.m.status(f.handle)
	if !ok || st.State != backgroundStateKilled || !st.ExitKnown || st.ExitCode != -1 {
		t.Errorf("final status = (%+v, %v), want killed/-1/known", st, ok)
	}
}

func TestStopCommandInvokeUnknownHandle(t *testing.T) {
	f := newBGJobFixture(t, []string{"cmd"}, "/w")
	res := mustInvoke(t, NewStopCommand(f.m), json.RawMessage(`{"handle":"bg-nope"}`))
	if !res.IsError || res.Content != `unknown background job handle "bg-nope"` {
		t.Errorf("unknown handle = (%q, IsError=%v), want the frozen error", res.Content, res.IsError)
	}
	if strings.Contains(res.Content, "state:") {
		t.Error("unknown handle must not fabricate a state block")
	}
}
