package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

// Golem native wiring proof (#576, spec section 10). agent/tools behavioral
// tests cannot see cmd/golem still building host seams, so these tests build
// native sessions through golem's own construction paths (run() for startup
// -allow-exec, -scratch and -allow-write; the /allow-exec and /allow-write
// handlers for late mounts) and run real commands. Every probe case first
// runs the identical probe in a host-policy session, where both escapes must
// land, so a native pass cannot come from a probe that sees nothing. The
// native CI jobs run these without -race; nothing here claims race coverage.

// canonicalDir resolves every symlink in dir (macOS temp dirs sit behind
// /var -> /private/var). Native backends bind canonical paths, so every probe
// path is spelled canonically.
func canonicalDir(t *testing.T, dir string) string {
	t.Helper()
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// nativePolicyForTest returns this OS's native policy (Seatbelt on darwin,
// Bubblewrap on linux) with network denied. A backend that cannot bound root
// here skips, or fails under GO_LLM_REQUIRE_SEATBELT=1 / GO_LLM_REQUIRE_BWRAP=1
// so a native CI job can never pass by skipping. Other OSes reject both native
// selections, so there is nothing to wire.
func nativePolicyForTest(t *testing.T, root string) agenttools.SandboxConfig {
	t.Helper()
	var policy agenttools.SandboxConfig
	var require string
	switch runtime.GOOS {
	case "darwin":
		policy, require = agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}, "GO_LLM_REQUIRE_SEATBELT"
	case "linux":
		policy, require = agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap}, "GO_LLM_REQUIRE_BWRAP"
	default:
		t.Skipf("golem rejects -sandbox seatbelt and -sandbox bwrap on %s", runtime.GOOS)
	}
	if err := agenttools.ValidateSandbox(policy, root); err != nil {
		if os.Getenv(require) == "1" {
			t.Fatalf("%s=1 but %s cannot bound %s: %v", require, policy.Runtime, root, err)
		}
		t.Skipf("%s unavailable on this host: %v", policy.Runtime, err)
	}
	return policy
}

// newNativeSession is newMountSession for a sandbox policy: the golem runtime
// (Profile Trusted, Sandbox policy), the VerifierSlot bound to the root and
// policy, the orchestrator verifying through that slot, and sess.sandbox, all
// built together over a fresh canonical root. The zero policy builds the host
// twin the positive controls run in.
func newNativeSession(t *testing.T, policy agenttools.SandboxConfig) *replSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := canonicalDir(t, t.TempDir())
	fileTools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	var in systemInputs
	system := composeSystem(in)
	slot, err := agenttools.NewVerifierSlot(root, policy)
	if err != nil {
		t.Fatalf("NewVerifierSlot: %v", err)
	}
	orch := agent.New(&scriptCaller{}, agent.ContextManager{}, agent.WithVerifier(slot))
	rt, err := golemruntime.New(context.Background(), golemruntime.Options{
		Root:           root,
		System:         system,
		Profile:        golemruntime.ProfileTrusted,
		Sandbox:        policy,
		MaxSteps:       16,
		Orchestrator:   orch,
		FailureMessage: runFailureMessage,
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	sess := &replSession{
		profile:       golemruntime.ProfileTrusted,
		orch:          orch,
		runtime:       rt,
		tools:         fileTools,
		baseSystem:    system,
		stdinTerminal: true,
		sysInputs:     in,
		root:          root,
		readToolCount: len(fileTools),
		mountAt:       len(fileTools),
		maxSteps:      16,
		clock:         func() time.Time { return time.Unix(0, 0) },
		grants:        newApprovalGrants(),
		verifier:      slot,
		sandbox:       policy,
	}
	t.Cleanup(func() { _ = sess.closeLateMounts() })
	return sess
}

// wiringProbe is one escape attempt: a control write the policy permits, a
// write to a sink directory outside the workspace (created outside any
// backend's private temp), and a TCP connect to a host listener.
type wiringProbe struct {
	sink string
	ln   net.Listener

	mu      sync.Mutex
	remotes []string // remote address of every accepted connection
}

func newWiringProbe(t *testing.T) *wiringProbe {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &wiringProbe{sink: canonicalDir(t, t.TempDir()), ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.remotes = append(p.remotes, conn.RemoteAddr().String())
			p.mu.Unlock()
			_ = conn.Close()
		}
	}()
	return p
}

// shQuote single-quotes s for bash.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }

// argv writes "ok" to control (absolute, or relative to the command's cwd),
// "x" to the sink, then connects to the listener. No step depends on the one
// before it succeeding.
func (p *wiringProbe) argv(control string) []string {
	port := p.ln.Addr().(*net.TCPAddr).Port
	return []string{"bash", "-c", fmt.Sprintf("printf ok > %s; printf x > %s; exec 3<>/dev/tcp/127.0.0.1/%d",
		shQuote(control), shQuote(filepath.Join(p.sink, "w.txt")), port)}
}

// connects reports how many probe connections reached the listener. It dials
// a sentinel and waits for it to be accepted: a probe connection was queued
// before its command returned, and the accept queue is FIFO, so it is counted
// by then.
func (p *wiringProbe) connects(t *testing.T) int {
	t.Helper()
	conn, err := net.Dial("tcp", p.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sentinel := conn.LocalAddr().String()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		seen, n := slices.Contains(p.remotes, sentinel), len(p.remotes)
		p.mu.Unlock()
		if seen {
			return n - 1
		}
	}
	t.Fatal("the listener never accepted the sentinel connection")
	return 0
}

// check asserts the control (skipped when control is ""), the sink write and
// the connect count against the expected literals.
func (p *wiringProbe) check(t *testing.T, label, control string, wantSink bool, wantConnects int) {
	t.Helper()
	if control != "" {
		if got, err := os.ReadFile(control); err != nil || string(got) != "ok" {
			t.Fatalf("%s: control %s = %q (err %v), want \"ok\"", label, control, got, err)
		}
	}
	_, err := os.Lstat(filepath.Join(p.sink, "w.txt"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if got := err == nil; got != wantSink {
		t.Fatalf("%s: sink write landed = %t, want %t", label, got, wantSink)
	}
	if got := p.connects(t); got != wantConnects {
		t.Fatalf("%s: listener accepted %d probe connection(s), want %d", label, got, wantConnects)
	}
}

// invokeTool runs the named session tool as dispatch does once a call is
// approved: Plan for a planning tool, then Invoke.
func invokeTool(ctx context.Context, t *testing.T, tools []agent.Tool, name string, args any) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	tool := toolByName(t, tools, name)
	if pt, ok := tool.(agent.PlanningTool); ok {
		if _, err := pt.Plan(ctx, raw); err != nil {
			t.Fatalf("%s Plan: %v", name, err)
		}
	}
	res, err := tool.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("%s Invoke: %v", name, err)
	}
	return res
}

// resultField returns the value of the first "label: value" line in content.
func resultField(t *testing.T, content, label string) string {
	t.Helper()
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, label+": "); ok {
			return v
		}
	}
	t.Fatalf("no %q line in:\n%s", label, content)
	return ""
}

// writeVerifyArgv declares argv as the workspace's post-write check.
func writeVerifyArgv(t *testing.T, root string, argv []string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"verify": map[string]any{"argv": argv, "timeout_seconds": 60}})
	if err != nil {
		t.Fatal(err)
	}
	writeGolemJSON(t, root, string(body))
}

// golemWorkspace is a run() fixture: a scripted model that writes note.txt on
// a turn's first step and answers on the next, and a fresh canonical root.
func golemWorkspace(t *testing.T) (configPath, root string) {
	t.Helper()
	configPath, root, _ = fenceWireHarness(t, func(req wireRequest) []string {
		if req.hasToolMessage() {
			return sseAnswer("done")
		}
		return sseToolCall("w1", "write_file", `{"path":"note.txt","content":"x"}`)
	})
	return configPath, canonicalDir(t, root)
}

// withGolem starts golem through run(), the production construction path,
// with -profile trusted, the policy's -sandbox flags and extra, then calls fn
// on the session run built and stops the run. It also pins the runtime's
// policy: run() must hand the golem runtime the selected backend, and its
// startup permissions: line must name it.
func withGolem(t *testing.T, configPath, root string, policy agenttools.SandboxConfig, extra []string, fn func(*replSession)) {
	t.Helper()
	args := []string{"-config", configPath, "-root", root, "-profile", "trusted",
		"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag",
		"-no-project-context", "-no-git-context", "-no-auto-index"}
	if nativeSandbox(policy) {
		args = append(args, "-sandbox", string(policy.Runtime))
		if policy.AllowNetwork {
			args = append(args, "-sandbox-network", "allowed")
		}
	}
	args = append(args, extra...)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after the wiring probe")
	runtimeName := "host"
	if nativeSandbox(policy) {
		runtimeName = string(policy.Runtime)
	}
	err := run(args, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(sess *replSession) error {
			if got := sess.runtime.EffectivePermissions().Command.Runtime; got != runtimeName {
				t.Errorf("runtime command policy = %q, want %q", got, runtimeName)
			}
			fn(sess)
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run %q = %v, want the probe stop\nstderr:\n%s", args, err, readRunTestFile(t, stderr))
	}
	var permissions []string
	for _, line := range strings.Split(readRunTestFile(t, stderr), "\n") {
		if strings.HasPrefix(line, "permissions: ") {
			t.Logf("%s", line)
			permissions = append(permissions, line)
		}
	}
	if len(permissions) == 0 || !strings.Contains(permissions[0], " commands="+runtimeName+" ") {
		t.Fatalf("startup permissions: lines %q, want the first to carry \" commands=%s \"", permissions, runtimeName)
	}
}

// wiringCase is one probe run first under the host policy and then under the
// native one.
type wiringCase struct {
	extra        []string // golem flags beyond -profile trusted and -sandbox
	allowNetwork bool     // native run adds -sandbox-network allowed
	// prepare, when set, runs before golem starts (startup reads .golem.json).
	prepare func(t *testing.T, root string, p *wiringProbe)
	// exercise runs the probe in the session and returns the control path to
	// check, or "" when it checked the control itself.
	exercise func(t *testing.T, sess *replSession, p *wiringProbe) string
}

// probeHostThenNative runs c in a host session, where the forbidden write and
// the connect must both land (the probe can see an escape), then in a native
// session, where only the control may land (plus the connect when the
// network is allowed).
func probeHostThenNative(t *testing.T, c wiringCase) {
	t.Helper()
	hostConfig, hostRoot := golemWorkspace(t)
	nativeConfig, nativeRoot := golemWorkspace(t)
	policy := nativePolicyForTest(t, nativeRoot)
	policy.AllowNetwork = c.allowNetwork
	nativeConnects := 0
	if c.allowNetwork {
		nativeConnects = 1
	}
	for _, leg := range []struct {
		label        string
		config, root string
		policy       agenttools.SandboxConfig
		wantSink     bool
		wantConnects int
	}{
		{"host", hostConfig, hostRoot, agenttools.SandboxConfig{}, true, 1},
		{string(policy.Runtime), nativeConfig, nativeRoot, policy, false, nativeConnects},
	} {
		p := newWiringProbe(t)
		if c.prepare != nil {
			c.prepare(t, leg.root, p)
		}
		control := ""
		withGolem(t, leg.config, leg.root, leg.policy, c.extra, func(sess *replSession) {
			control = c.exercise(t, sess, p)
		})
		p.check(t, leg.label, control, leg.wantSink, leg.wantConnects)
	}
}

// foregroundProbe runs the probe through the session's mounted run_command.
func foregroundProbe(t *testing.T, sess *replSession, p *wiringProbe) string {
	t.Helper()
	control := filepath.Join(sess.root, "in.txt")
	res := invokeTool(context.Background(), t, sess.tools, "run_command", map[string]any{"argv": p.argv(control)})
	t.Logf("run_command:\n%s", res.Content)
	return control
}

func TestNativeWiringForeground(t *testing.T) {
	probeHostThenNative(t, wiringCase{extra: []string{"-allow-exec"}, exercise: foregroundProbe})
}

func TestNativeWiringBackground(t *testing.T) {
	probeHostThenNative(t, wiringCase{extra: []string{"-allow-exec"}, exercise: func(t *testing.T, sess *replSession, p *wiringProbe) string {
		ctx := context.Background()
		control := filepath.Join(sess.root, "in.txt")
		started := invokeTool(ctx, t, sess.tools, "start_command", map[string]any{"argv": p.argv(control)})
		handle := resultField(t, started.Content, "handle")
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			status := invokeTool(ctx, t, sess.tools, "command_status", map[string]any{"handle": handle})
			if resultField(t, status.Content, "state") != "running" {
				t.Logf("command_status:\n%s", status.Content)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("background probe still running after 10s:\n%s", status.Content)
			}
		}
		return control
	}})
}

// Startup scratch (scratchExecOptions(true, journal)): the cwd-relative
// control lands in the snapshot, never the canonical root, under both
// policies; only the host scratch control reaches the sink and the listener.
func TestNativeWiringScratch(t *testing.T) {
	probeHostThenNative(t, wiringCase{extra: []string{"-allow-exec", "-scratch"}, exercise: func(t *testing.T, sess *replSession, p *wiringProbe) string {
		ctx := context.Background()
		res := invokeTool(ctx, t, sess.tools, "run_command", map[string]any{"argv": p.argv("in.txt")})
		t.Logf("scratched run_command:\n%s", res.Content)
		id := resultField(t, res.Content, "scratch")
		id = strings.TrimPrefix(strings.Fields(id)[0], "id=")
		changes := invokeTool(ctx, t, sess.tools, "scratch_changes", map[string]any{"id": id})
		if !strings.Contains(changes.Content, "\ncreate \"in.txt\" size=2") {
			t.Fatalf("control write not captured in the snapshot:\n%s", changes.Content)
		}
		if _, err := os.Lstat(filepath.Join(sess.root, "in.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("scratch control reached the canonical root: %v", err)
		}
		return ""
	}})
}

// Startup -allow-write without exec: the verifier startup built from
// .golem.json runs, after a scripted model write, under the session policy.
func TestNativeWiringStartupWriteOnlyVerification(t *testing.T) {
	probeHostThenNative(t, wiringCase{
		extra: []string{"-allow-write"},
		prepare: func(t *testing.T, root string, p *wiringProbe) {
			writeVerifyArgv(t, root, p.argv(filepath.Join(root, "verified.txt")))
		},
		exercise: func(t *testing.T, sess *replSession, _ *wiringProbe) string {
			if state, _ := sess.verifier.State(); state != "armed" {
				t.Fatalf("verifier slot = %s, want armed", state)
			}
			// Stands in for the user approving exactly the write and the check.
			sess.headlessApprover = newHeadlessApprover(allowToolSet{names: map[string]struct{}{"write_file": {}, verifyToolName: {}}})
			var out strings.Builder
			if _, err := runOnce(context.Background(), &out, nil, sess, "write the note", nil); err != nil {
				t.Fatalf("turn: %v\n%s", err, out.String())
			}
			t.Logf("turn:\n%s", out.String())
			if got, err := os.ReadFile(filepath.Join(sess.root, "note.txt")); err != nil || string(got) != "x" {
				t.Fatalf("scripted write = %q (err %v), want \"x\"\n%s", got, err, out.String())
			}
			return filepath.Join(sess.root, "verified.txt")
		},
	})
}

// lateMountProbes starts golem through run() under policy with nothing
// mounted, so both mounts take the REPL's late paths on the session run()
// built (its sess.sandbox from main.go), then runs the foreground probe
// through the late run_command and the verifier probe through the slot
// /allow-write armed.
func lateMountProbes(t *testing.T, policy agenttools.SandboxConfig, order []string, wantSink bool, wantConnects int) {
	t.Helper()
	ctx := context.Background()
	config, root := golemWorkspace(t)
	fg, verify := newWiringProbe(t), newWiringProbe(t)
	verifyControl := filepath.Join(root, "verified.txt")
	label := "host"
	if nativeSandbox(policy) {
		label = string(policy.Runtime)
	}
	fgControl := ""
	withGolem(t, config, root, policy, nil, func(sess *replSession) {
		// Stands in for the user at a terminal, which late mounts require.
		sess.stdinTerminal = true
		writeVerifyArgv(t, sess.root, verify.argv(verifyControl))
		for _, command := range order {
			var out strings.Builder
			_, _ = dispatchSlash(ctx, &out, sess, command)
			want := map[string]string{"/allow-exec": "exec enabled (", "/allow-write": "writes enabled ("}[command]
			if !strings.Contains(out.String(), want) {
				t.Fatalf("%s = %q, want it to report %q", command, out.String(), want)
			}
		}
		fgControl = foregroundProbe(t, sess, fg)
		obs, err := sess.verifier.Verify(ctx, allowAllApprover{})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		t.Logf("verification:\n%s", obs)
	})
	fg.check(t, label+" exec", fgControl, wantSink, wantConnects)
	verify.check(t, label+" verifier", verifyControl, wantSink, wantConnects)
}

func TestNativeWiringLateMountsBothOrders(t *testing.T) {
	policy := nativePolicyForTest(t, canonicalDir(t, t.TempDir()))
	for _, order := range [][]string{{"/allow-exec", "/allow-write"}, {"/allow-write", "/allow-exec"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			lateMountProbes(t, agenttools.SandboxConfig{}, order, true, 1)
			lateMountProbes(t, policy, order, false, 0)
		})
	}
}

// Allowing the network is a permission, not a widening of the write
// boundary: the native connect lands and the sink still does not.
func TestNativeWiringNetworkAllowedIsPermission(t *testing.T) {
	probeHostThenNative(t, wiringCase{extra: []string{"-allow-exec"}, allowNetwork: true, exercise: foregroundProbe})
}

func TestNativeWiringCancellation(t *testing.T) {
	config, root := golemWorkspace(t)
	policy := nativePolicyForTest(t, root)
	withGolem(t, config, root, policy, []string{"-allow-exec"}, func(sess *replSession) {
		t.Run("foreground", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(200*time.Millisecond, cancel)
			start := time.Now()
			res := invokeTool(ctx, t, sess.tools, "run_command", map[string]any{"argv": []string{"sleep", "30"}})
			if elapsed := time.Since(start); elapsed >= 5*time.Second {
				t.Fatalf("canceled foreground sleep returned after %s, want under 5s", elapsed)
			}
			if !strings.Contains(res.Content, "command canceled") {
				t.Fatalf("result = %q, want command canceled", res.Content)
			}
		})
		t.Run("background", func(t *testing.T) {
			ctx := context.Background()
			started := invokeTool(ctx, t, sess.tools, "start_command", map[string]any{"argv": []string{"sleep", "30"}})
			if state := resultField(t, started.Content, "state"); state != "running" {
				t.Fatalf("job state = %q before stop_command, want running", state)
			}
			handle := resultField(t, started.Content, "handle")
			stopped := invokeTool(ctx, t, sess.tools, "stop_command", map[string]any{"handle": handle})
			t.Logf("stop_command:\n%s", stopped.Content)
			if state := resultField(t, stopped.Content, "state"); state != "killed" {
				t.Fatalf("stop_command state = %q, want killed", state)
			}
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				running := 0
				for _, st := range sess.bgManager.List() {
					if st.State == "running" {
						running++
					}
				}
				if running == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d job(s) still running 5s after stop_command", running)
				}
			}
		})
	})

	config, root = golemWorkspace(t)
	withGolem(t, config, root, policy, []string{"-allow-exec", "-scratch"}, func(sess *replSession) {
		t.Run("scratch", func(t *testing.T) { cancelScratchRun(t, sess) })
	})
}

// cancelScratchRun cancels a scratched sleep once its snapshot exists and
// requires both snapshot directories to be gone afterwards.
func cancelScratchRun(t *testing.T, sess *replSession) {
	t.Helper()
	// The marker names this run's snapshot among any other scratch
	// sessions sharing the platform base.
	marker := "native-wiring-" + rand.Text()
	if err := os.WriteFile(filepath.Join(sess.root, marker), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"darwin": "/private/tmp", "linux": "/tmp"}[runtime.GOOS]
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	found := make(chan []string, 1)
	go func() {
		defer cancel()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			work, _ := filepath.Glob(filepath.Join(base, "go-llm-scratch-exec-*", "workspace", marker))
			ref, _ := filepath.Glob(filepath.Join(base, "go-llm-scratch-ref-*", "tree", marker))
			if len(work) == 1 && len(ref) == 1 {
				time.Sleep(200 * time.Millisecond) // let the command start
				found <- []string{filepath.Dir(filepath.Dir(work[0])), filepath.Dir(filepath.Dir(ref[0]))}
				return
			}
		}
		found <- nil
	}()
	res := invokeTool(ctx, t, sess.tools, "run_command", map[string]any{"argv": []string{"sleep", "30"}})
	t.Logf("canceled scratched run_command:\n%s", res.Content)
	dirs := <-found
	if dirs == nil {
		t.Fatal("the scratch snapshot never appeared; the cleanup check would be vacuous")
	}
	if !strings.Contains(res.Content, "command canceled") {
		t.Fatalf("result = %q, want command canceled", res.Content)
	}
	for _, dir := range dirs {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			_, err := os.Lstat(dir)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("snapshot directory %s still present 5s after cancellation (err %v)", dir, err)
			}
		}
	}
}

// TestNativeWiringToolchainMatrix runs each toolchain probe found on PATH as
// the native session's verifier and logs what happened, for the toolchain
// record: the host version, the native outcome and any dyld text. A tool
// failing under the sandbox is a finding to record, never a test failure; the
// test fails only when no tool's command launched (status passed or failed;
// a status: error spawn refusal launched nothing).
func TestNativeWiringToolchainMatrix(t *testing.T) {
	policy := nativePolicyForTest(t, canonicalDir(t, t.TempDir()))
	sess := newNativeSession(t, policy)
	launched := 0
	for _, tc := range []struct{ argv, version []string }{
		{[]string{"go", "version"}, []string{"version"}},
		{[]string{"python3", "-c", "1"}, []string{"--version"}},
		{[]string{"node", "-e", "1"}, []string{"--version"}},
		{[]string{"npm", "--version"}, []string{"--version"}},
		{[]string{"pnpm", "--version"}, []string{"--version"}},
		{[]string{"yarn", "--version"}, []string{"--version"}},
		{[]string{"git", "--version"}, []string{"--version"}},
	} {
		path, err := exec.LookPath(tc.argv[0])
		if err != nil {
			t.Logf("%s: not on PATH; not probed", tc.argv[0])
			continue
		}
		// Bounded: a version-manager shim can hang on first use.
		vctx, vcancel := context.WithTimeout(context.Background(), 30*time.Second)
		version, _ := exec.CommandContext(vctx, path, tc.version...).CombinedOutput()
		vcancel()
		writeVerifyArgv(t, sess.root, tc.argv)
		cmd, notice, err := buildVerifier(sess.root, policy)
		if err != nil {
			t.Fatalf("%s: buildVerifier: %v", tc.argv[0], err)
		}
		if cmd == nil {
			t.Logf("%s: path=%s host=%q refused: %s", tc.argv[0], path, strings.TrimSpace(string(version)), notice)
			continue
		}
		if err := sess.verifier.Set(cmd); err != nil {
			t.Fatalf("%s: slot refused the verifier: %v", tc.argv[0], err)
		}
		obs, err := sess.verifier.Verify(context.Background(), allowAllApprover{})
		if err != nil {
			t.Fatalf("%s: Verify: %v", tc.argv[0], err)
		}
		status := "unknown"
		var dyld []string
		for _, line := range strings.Split(obs, "\n") {
			if v, ok := strings.CutPrefix(line, "status: "); ok {
				status = v
			}
			if strings.Contains(line, "dyld") || strings.Contains(line, "xcrun") || strings.Contains(line, "xcode-select") {
				dyld = append(dyld, line)
			}
		}
		if status == "passed" || status == "failed" {
			launched++
		}
		t.Logf("%s: path=%s host=%q %s=%s dyld=%q\n%s", strings.Join(tc.argv, " "), path,
			strings.TrimSpace(string(version)), policy.Runtime, status, dyld, obs)
	}
	if launched == 0 {
		t.Fatal("no toolchain tool's command launched under the native backend")
	}
}
