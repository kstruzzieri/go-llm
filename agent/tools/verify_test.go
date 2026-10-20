package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// verifyWorkspace returns a canonical workspace over a fresh temp dir.
func verifyWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// helperArgv runs this test binary's hermetic exec helper as the verify
// command, so no external tool is needed.
func helperArgv(t *testing.T, args ...string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{self, "__golem_exec_helper__"}, args...)
}

// stubRunner returns a scripted execResult so the mapping from execResult to
// VerifyResult can be pinned without a real process.
type stubRunner struct {
	res execResult
	err error
	got execSpec
	n   int
}

func (s *stubRunner) Run(_ context.Context, spec execSpec) (execResult, error) {
	s.n++
	s.got = spec
	return s.res, s.err
}

// verifyBackend binds a scripted runner to the normalized policy cfg: the
// same frozen pairing newExecBackend produces, without spawning anything.
func verifyBackend(t *testing.T, r commandRunner, cfg SandboxConfig) resolvedExecBackend {
	t.Helper()
	return bindExecBackend(hostBackend{commandRunner: r, backgroundStarter: starterOf()}, mustNormalizeSandbox(t, cfg))
}

func newTestVerifyCommand(t *testing.T, ws *Workspace, r commandRunner, argv []string, dir string, d time.Duration) *VerifyCommand {
	t.Helper()
	return newTestVerifyCommandWith(t, ws, r, SandboxConfig{}, argv, dir, d)
}

func newTestVerifyCommandWith(t *testing.T, ws *Workspace, r commandRunner, cfg SandboxConfig, argv []string, dir string, d time.Duration) *VerifyCommand {
	t.Helper()
	v, err := newVerifyCommandWithBackend(ws, verifyBackend(t, r, cfg), argv, dir, d)
	if err != nil {
		t.Fatalf("newVerifyCommandWithBackend: %v", err)
	}
	return v
}

func TestNewVerifyCommandRejectsBadInput(t *testing.T) {
	ws := verifyWorkspace(t)
	ok := helperArgv(t, "echo", "hi")
	cases := []struct {
		name    string
		argv    []string
		dir     string
		timeout time.Duration
		want    string
	}{
		{name: "empty argv", argv: nil, timeout: time.Second, want: "argv"},
		{name: "blank argv0", argv: []string{"  "}, timeout: time.Second, want: "argv[0]"},
		{name: "NUL in argv", argv: []string{ok[0], "a\x00b"}, timeout: time.Second, want: "NUL"},
		{name: "missing executable", argv: []string{"definitely-not-on-path-347"}, timeout: time.Second, want: "resolve"},
		{name: "dir escapes workspace", argv: ok, dir: "../..", timeout: time.Second, want: "dir"},
		{name: "zero timeout", argv: ok, timeout: 0, want: "timeout"},
		{name: "negative timeout", argv: ok, timeout: -time.Second, want: "timeout"},
		{name: "timeout above the ceiling", argv: ok, timeout: execMaxTimeout + time.Second, want: "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newVerifyCommandWithBackend(ws, verifyBackend(t, &stubRunner{}, SandboxConfig{}), tc.argv, tc.dir, tc.timeout)
			if err == nil {
				t.Fatalf("expected a construction error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
		})
	}
}

func TestVerifyCommandApprovalKeyIdentity(t *testing.T) {
	ws := verifyWorkspace(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := helperArgv(t, "echo", "hi")
	base := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 30*time.Second)

	if !strings.HasPrefix(base.ApprovalKey(), verifyApprovalKeyPrefix) {
		t.Fatalf("key %q must carry the verify namespace %q", base.ApprovalKey(), verifyApprovalKeyPrefix)
	}
	if strings.HasPrefix(base.ApprovalKey(), execApprovalKeyPrefix) {
		t.Fatalf("verify key must not share the exec namespace: %q", base.ApprovalKey())
	}
	same := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 30*time.Second)
	if base.ApprovalKey() != same.ApprovalKey() {
		t.Fatalf("identical inputs must yield one key: %q vs %q", base.ApprovalKey(), same.ApprovalKey())
	}

	otherArgs := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "bye"), "", 30*time.Second)
	otherDir := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "sub", 30*time.Second)
	otherTimeout := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 31*time.Second)
	for name, v := range map[string]*VerifyCommand{
		"argv":    otherArgs,
		"dir":     otherDir,
		"timeout": otherTimeout,
	} {
		if v.ApprovalKey() == base.ApprovalKey() {
			t.Fatalf("a changed %s must change the approval key", name)
		}
	}
}

func TestVerifyCommandPreview(t *testing.T) {
	ws := verifyWorkspace(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "hi"), "sub", 45*time.Second)
	preview := v.Preview()
	for _, want := range []string{
		"__golem_exec_helper__", "sub", "45s", "may change the code this command runs",
		"unsandboxed", "host user's filesystem", "network authority", "outside /undo",
	} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestVerifyCommandMapsRunnerResult(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	r := &stubRunner{res: execResult{
		ExitCode: 7,
		Stdout:   []byte("out"), Stderr: []byte("err"),
		StdoutTruncated: true,
	}}
	v := newTestVerifyCommand(t, ws, r, argv, "", time.Minute)

	got, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.ExitCode != 7 || string(got.Stdout) != "out" || string(got.Stderr) != "err" {
		t.Fatalf("result not mapped through: %+v", got)
	}
	if !got.Truncated {
		t.Fatalf("truncation must pass through: %+v", got)
	}
	if got.TimedOut {
		t.Fatalf("TimedOut must be false for a completed run: %+v", got)
	}
	if r.got.Dir != ws.root {
		t.Fatalf("spec Dir = %q, want workspace root %q", r.got.Dir, ws.root)
	}
}

func TestVerifyCommandTruncationOnEitherStream(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	for name, res := range map[string]execResult{
		"stdout only": {StdoutTruncated: true},
		"stderr only": {StderrTruncated: true},
		"both":        {StdoutTruncated: true, StderrTruncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifyCommand(t, ws, &stubRunner{res: res}, argv, "", time.Minute)
			got, err := v.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !got.Truncated {
				t.Fatalf("%s must report Truncated: %+v", name, got)
			}
		})
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", time.Minute)
	got, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Truncated {
		t.Fatalf("an uncapped run must not report Truncated: %+v", got)
	}
}

func TestVerifyCommandInfraFailureIsAnError(t *testing.T) {
	ws := verifyWorkspace(t)
	boom := errors.New("spawn refused")
	v := newTestVerifyCommand(t, ws, &stubRunner{err: boom}, helperArgv(t, "echo", "hi"), "", time.Minute)
	if _, err := v.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("infra failure must surface, got %v", err)
	}
}

func TestVerifyCommandRunsRealCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"success", []string{"echo", "ok"}, 0},
		{"non-zero exit", []string{"fail"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewVerifyCommand(ws, helperArgv(t, tc.args...), "", 30*time.Second)
			if err != nil {
				t.Fatalf("NewVerifyCommand: %v", err)
			}
			got, err := v.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.ExitCode != tc.want {
				t.Fatalf("ExitCode = %d, want %d", got.ExitCode, tc.want)
			}
			if got.TimedOut {
				t.Fatalf("TimedOut must be false: %+v", got)
			}
		})
	}
}

// TestVerifyCommandAppliesItsOwnTimeout is the pin that matters most for a
// non-tool runner: nothing wraps Run the way the Orchestrator's invokeCall
// wraps a dispatched tool, so the deadline must come from VerifyCommand.
func TestVerifyCommandAppliesItsOwnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	v, err := NewVerifyCommand(ws, helperArgv(t, "sleep"), "", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	start := time.Now()
	got, runErr := v.Run(context.Background())
	if runErr != nil {
		t.Fatalf("a timeout is an outcome, not an error: %v", runErr)
	}
	if !got.TimedOut {
		t.Fatalf("want TimedOut, got %+v", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the helper sleeps 30s; the deadline was not applied (took %s)", elapsed)
	}
}

func TestVerifyCommandParentCancellationPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	v, err := NewVerifyCommand(ws, helperArgv(t, "sleep"), "", 30*time.Second)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	got, runErr := v.Run(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("parent cancellation must surface as an error, got %v (%+v)", runErr, got)
	}
	if got.TimedOut {
		t.Fatalf("a cancelled run is not a timeout: %+v", got)
	}
}

func TestVerifyCommandRechecksExecutableIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec-bit semantics")
	}
	ws := verifyWorkspace(t)
	script := filepath.Join(ws.root, "check.sh")
	writeExecutable(t, script, "#!/bin/sh\nexit 0\n")

	v := newTestVerifyCommand(t, ws, &stubRunner{}, []string{"./check.sh"}, "", 30*time.Second)

	// Swap the binary by write-sibling + rename, NOT remove + recreate: inode
	// reuse would mask the identity change on APFS.
	sibling := filepath.Join(ws.root, "check.sh.new")
	writeExecutable(t, sibling, "#!/bin/sh\nexit 1\n")
	if err := os.Rename(sibling, script); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "executable changed") {
		t.Fatalf("a swapped executable must fail closed, got %v", err)
	}
}

func TestVerifyCommandRechecksWorkingDirectoryIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix path semantics")
	}
	ws := verifyWorkspace(t)
	dir := filepath.Join(ws.root, "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "hi"), "work", 30*time.Second)

	// Create the replacement FIRST, so its inode is allocated before the
	// original is unlinked and cannot be the reused one. (Directory rename
	// onto an existing directory is not portable, so the unlink is required;
	// creating first keeps the identity change guaranteed, which a plain
	// remove-then-recreate would not.)
	replacement := filepath.Join(ws.root, "work.new")
	if err := os.Mkdir(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "working directory changed") {
		t.Fatalf("a swapped cwd must fail closed, got %v", err)
	}
}

func TestVerifyCommandIsNotAModelTool(t *testing.T) {
	// The model must be unable to call the workspace's verifier: VerifyCommand
	// must not satisfy agent.Tool, and no tool set may carry it.
	if _, ok := any(&VerifyCommand{}).(agent.Tool); ok {
		t.Fatal("VerifyCommand must not satisfy agent.Tool")
	}
	for _, tool := range NewMutatingTools(nil, nil) {
		if strings.Contains(tool.Spec().Name, "verify") {
			t.Fatalf("verify must not be a model-visible tool, found %q", tool.Spec().Name)
		}
	}
}

func TestVerifyKeyDigestVector(t *testing.T) {
	// python3: sha256(b"sandbox\0" + b"a"*64 + b"\0command\0" + b"b"*64 + b"\0")
	got := verifyKeyDigest(strings.Repeat("a", 64), strings.Repeat("b", 64))
	if want := "d528f7af198e8525678ddde5954b84fb11b69e59ac2eca436bdbcd256fe19cde"; got != want {
		t.Fatalf("verifyKeyDigest = %s, want %s", got, want)
	}
}

// TestVerifyKeyV2Matrix pins which inputs share a verify:v2 key: identical
// normalized policy plus identical prepared command, and nothing else.
func TestVerifyKeyV2Matrix(t *testing.T) {
	ws := verifyWorkspace(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := helperArgv(t, "echo", "hi")
	const d = 30 * time.Second
	var keys []string
	key := func(v *VerifyCommand) string {
		k := v.ApprovalKey()
		keys = append(keys, k)
		return k
	}
	build := func(t *testing.T, cfg SandboxConfig) *VerifyCommand {
		t.Helper()
		return newTestVerifyCommandWith(t, ws, &stubRunner{}, cfg, argv, "", d)
	}
	seatbelt := SandboxConfig{Runtime: SandboxRuntimeSeatbelt}
	bwrap := SandboxConfig{Runtime: SandboxRuntimeBwrap}

	t.Run("equal policies share a key", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			a, b SandboxConfig
		}{
			{"zero vs explicit host", SandboxConfig{}, SandboxConfig{Runtime: SandboxRuntimeHost}},
			{"capability set semantics",
				SandboxConfig{Runtime: SandboxRuntimeSeatbelt, DropCaps: []string{"b", "a", "a"}},
				SandboxConfig{Runtime: SandboxRuntimeSeatbelt, DropCaps: []string{"a", "b"}}},
			{"signed zero CPU",
				SandboxConfig{Runtime: SandboxRuntimeSeatbelt, CPULimit: math.Copysign(0, -1)},
				SandboxConfig{Runtime: SandboxRuntimeSeatbelt, CPULimit: 0}},
		} {
			if ka, kb := key(build(t, tc.a)), key(build(t, tc.b)); ka != kb {
				t.Errorf("%s: keys differ: %q vs %q", tc.name, ka, kb)
			}
		}
		if got := build(t, SandboxConfig{}).Sandbox().Runtime; got != "host" {
			t.Errorf("zero policy stored as runtime %q, want normalized host", got)
		}
	})

	t.Run("policy changes the key", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			a, b SandboxConfig
		}{
			{"host vs seatbelt", SandboxConfig{}, seatbelt},
			{"host vs bwrap", SandboxConfig{}, bwrap},
			{"seatbelt vs bwrap", seatbelt, bwrap},
			{"network denied vs allowed", seatbelt, SandboxConfig{Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true}},
			{"memory cap 0 vs 64", bwrap, SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 64}},
			{"cpu limit 0 vs 1", bwrap, SandboxConfig{Runtime: SandboxRuntimeBwrap, CPULimit: 1}},
			{"drop caps [a] vs [a,b]",
				SandboxConfig{Runtime: SandboxRuntimeBwrap, DropCaps: []string{"a"}},
				SandboxConfig{Runtime: SandboxRuntimeBwrap, DropCaps: []string{"a", "b"}}},
		} {
			if ka, kb := key(build(t, tc.a)), key(build(t, tc.b)); ka == kb {
				t.Errorf("%s: one key %q for two policies", tc.name, ka)
			}
		}
	})

	t.Run("command changes the key", func(t *testing.T) {
		base := key(build(t, SandboxConfig{}))
		other := map[string]*VerifyCommand{
			"root":    newTestVerifyCommandWith(t, verifyWorkspace(t), &stubRunner{}, SandboxConfig{}, argv, "", d),
			"argv":    newTestVerifyCommandWith(t, ws, &stubRunner{}, SandboxConfig{}, helperArgv(t, "echo", "bye"), "", d),
			"cwd":     newTestVerifyCommandWith(t, ws, &stubRunner{}, SandboxConfig{}, argv, "sub", d),
			"timeout": newTestVerifyCommandWith(t, ws, &stubRunner{}, SandboxConfig{}, argv, "", d+time.Second),
		}
		for name, v := range other {
			if key(v) == base {
				t.Errorf("a changed %s must change the key", name)
			}
		}
		t.Setenv("LANG", "C")
		langC := key(build(t, SandboxConfig{}))
		t.Setenv("LANG", "C.UTF-8")
		if key(build(t, SandboxConfig{})) == langC {
			t.Error("a changed environment value must change the key")
		}
	})

	t.Run("resolved executable changes the key", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix exec-bit semantics")
		}
		// One PATH value throughout, so only the resolved executable differs.
		first, second := t.TempDir(), t.TempDir()
		t.Setenv("PATH", first+string(os.PathListSeparator)+second)
		writeExecutable(t, filepath.Join(second, "bash"), "#!/bin/sh\nexit 0\n")
		a := newTestVerifyCommandWith(t, ws, &stubRunner{}, SandboxConfig{}, []string{"bash"}, "", d)
		writeExecutable(t, filepath.Join(first, "bash"), "#!/bin/sh\nexit 0\n")
		b := newTestVerifyCommandWith(t, ws, &stubRunner{}, SandboxConfig{}, []string{"bash"}, "", d)
		if a.pending.path == b.pending.path {
			t.Fatalf("fixture must resolve two different executables, both %q", a.pending.path)
		}
		if key(a) == key(b) {
			t.Errorf("%s and %s share key %q", a.pending.path, b.pending.path, key(a))
		}
	})

	t.Run("copy independence", func(t *testing.T) {
		caps := []string{"b", "a"}
		v := build(t, SandboxConfig{Runtime: SandboxRuntimeSeatbelt, DropCaps: caps})
		k := key(v)
		caps[0], caps[1] = "x", "y"
		if v.ApprovalKey() != k {
			t.Fatal("mutating the caller's DropCaps changed the key")
		}
		got := v.Sandbox()
		if got.Runtime != "seatbelt" || !slices.Equal(got.DropCaps, []string{"a", "b"}) {
			t.Fatalf("Sandbox() = %+v, want normalized seatbelt [a b]", got)
		}
		got.DropCaps[0] = "x"
		if v.ApprovalKey() != k {
			t.Fatal("mutating the slice Sandbox() returned changed the key")
		}
		if again := v.Sandbox().DropCaps; !slices.Equal(again, []string{"a", "b"}) {
			t.Fatalf("mutating the slice Sandbox() returned changed the stored policy: %q", again)
		}
	})

	t.Run("host key recipe", func(t *testing.T) {
		// The host sandbox digest is TestSandboxFingerprintHostVector's
		// literal; the composition is recomputed here, not by the code under
		// test.
		const hostDigest = "a396771a1742ed98ad837e2ba6ff902e5d725bf811708e3c3f9646e9fa4ed454"
		v := build(t, SandboxConfig{})
		sum := sha256.Sum256([]byte("sandbox\x00" + hostDigest + "\x00command\x00" + v.pending.fingerprint + "\x00"))
		if got, want := key(v), "verify:v2:"+hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("host key = %q, want %q", got, want)
		}
	})

	for _, k := range keys {
		if !strings.HasPrefix(k, "verify:v2:") {
			t.Errorf("key %q lacks the verify:v2: namespace", k)
		}
	}
}

func TestVerifyKeyNeverEqualsExecKey(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	raw, err := json.Marshal(map[string]any{"argv": argv, "timeout_seconds": 60})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewRunCommand(ws, &markerRunner{}).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 60*time.Second)
	ek, vk := plan.ApprovalKey, v.ApprovalKey()
	if !strings.HasPrefix(ek, "exec:v3:") || !strings.HasPrefix(vk, "verify:v2:") {
		t.Fatalf("namespaces: exec %q, verify %q", ek, vk)
	}
	if ek == vk {
		t.Fatalf("one key for both tools: %q", ek)
	}
	// Same argv, cwd, root, env, timeout and executable: the exec key's
	// digest is the bare command fingerprint, and the verify key must not
	// reuse it.
	cmdFP := strings.TrimPrefix(ek, "exec:v3:")
	if cmdFP != v.pending.fingerprint {
		t.Fatalf("fixture must give both tools one command fingerprint: %s vs %s", cmdFP, v.pending.fingerprint)
	}
	if strings.TrimPrefix(vk, "verify:v2:") == cmdFP {
		t.Fatalf("verify key %q reuses the bare command fingerprint", vk)
	}
	for _, other := range []string{execApprovalKeyPrefix, bgExecApprovalKeyPrefix} {
		if strings.HasPrefix(verifyApprovalKeyPrefix, other) || strings.HasPrefix(other, verifyApprovalKeyPrefix) {
			t.Errorf("prefixes %q and %q nest", verifyApprovalKeyPrefix, other)
		}
	}
}

func TestSandboxedVerifyPolicyErrorsAreTyped(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	unsupported := SandboxRuntimeBwrap
	if runtime.GOOS == "linux" {
		unsupported = SandboxRuntimeSeatbelt
	}
	t.Run("unsupported runtime", func(t *testing.T) {
		_, err := NewSandboxedVerifyCommand(ws, SandboxConfig{Runtime: unsupported}, argv, "", time.Second)
		if !errors.Is(err, ErrSandboxPolicy) {
			t.Fatalf("runtime %q on %s: err = %v, want ErrSandboxPolicy", unsupported, runtime.GOOS, err)
		}
	})
	t.Run("invalid policy", func(t *testing.T) {
		_, err := NewSandboxedVerifyCommand(ws, SandboxConfig{MemoryCapMB: 1}, argv, "", time.Second)
		if !errors.Is(err, ErrSandboxPolicy) {
			t.Fatalf("host with a memory cap: err = %v, want ErrSandboxPolicy", err)
		}
	})
	t.Run("bwrap volume root", func(t *testing.T) {
		root, err := NewWorkspace("/")
		if err != nil {
			t.Fatal(err)
		}
		_, err = newVerifyCommandWithBackend(root, verifyBackend(t, &stubRunner{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}),
			argv, "", time.Second)
		if !errors.Is(err, ErrSandboxPolicy) {
			t.Fatalf("bwrap over /: err = %v, want ErrSandboxPolicy", err)
		}
	})
	for _, tc := range []struct {
		name, dir string
		argv      []string
	}{
		{"missing executable", "", []string{"definitely-not-on-path-576"}},
		{"invalid cwd", "../..", argv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSandboxedVerifyCommand(ws, SandboxConfig{}, tc.argv, tc.dir, time.Second)
			if err == nil || errors.Is(err, ErrSandboxPolicy) {
				t.Fatalf("err = %v, want a command error that is not ErrSandboxPolicy", err)
			}
		})
	}
}

func TestVerifyPreviewNamesRuntimeAndTarget(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	const (
		hostWarning = "  warning: unsandboxed; runs with the host user's filesystem and network authority\n" +
			"           verifier writes are outside /undo\n"
		note = "  note:    declared by this workspace; later edits to files here\n" +
			"           may change the code this command runs\n"
	)

	t.Run("host", func(t *testing.T) {
		v := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", time.Minute)
		p := v.Preview()
		for _, want := range []string{hostWarning, note, "  " + v.TargetLine() + "\n"} {
			if !strings.Contains(p, want) {
				t.Errorf("host preview missing %q:\n%s", want, p)
			}
		}
		if strings.Contains(p, "sandbox: ") {
			t.Errorf("host preview claims a sandbox:\n%s", p)
		}
	})

	for _, tc := range []struct {
		cfg  SandboxConfig
		line string
	}{
		{SandboxConfig{Runtime: SandboxRuntimeSeatbelt},
			`  sandbox: runtime="seatbelt" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private` + "\n"},
		{SandboxConfig{Runtime: SandboxRuntimeBwrap},
			`  sandbox: runtime="bwrap" network=ip-denied memory_cap=none cpu_limit=none drop_caps=[] temp=private` + "\n"},
	} {
		t.Run(string(tc.cfg.Runtime), func(t *testing.T) {
			v := newTestVerifyCommandWith(t, ws, &stubRunner{}, tc.cfg, argv, "", time.Minute)
			p := v.Preview()
			if strings.Contains(p, "unsandboxed") {
				t.Errorf("native preview carries the host warning:\n%s", p)
			}
			for _, want := range []string{
				tc.line,
				"writes inside the workspace and private temp are outside /undo",
				note,
				"  " + v.TargetLine() + "\n",
			} {
				if !strings.Contains(p, want) {
					t.Errorf("native preview missing %q:\n%s", want, p)
				}
			}
		})
	}

	t.Run("target quotes the canonical root", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip(`'"' is not a legal Windows file name character`)
		}
		dir := filepath.Join(t.TempDir(), `quo"te dir`)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		qws, err := NewWorkspace(dir)
		if err != nil {
			t.Fatal(err)
		}
		v := newTestVerifyCommand(t, qws, &stubRunner{}, argv, "", time.Minute)
		if v.Root() != qws.root {
			t.Fatalf("Root() = %q, want canonical %q", v.Root(), qws.root)
		}
		want := "target:  " + strconv.QuoteToGraphic(qws.root) +
			" (canonical workspace; unpromoted scratch artifacts are not checked)"
		if got := v.TargetLine(); got != want {
			t.Fatalf("TargetLine() = %q, want %q", got, want)
		}
	})
}
