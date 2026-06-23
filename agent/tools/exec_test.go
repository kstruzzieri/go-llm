package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestRunCommandSpec(t *testing.T) {
	rc := NewRunCommand(nil, nil)
	s := rc.Spec()
	if s.Name != "run_command" {
		t.Fatalf("name = %q, want run_command", s.Name)
	}
	for _, want := range []string{"argv", "dir", "timeout_seconds"} {
		if !strings.Contains(string(s.Parameters), want) {
			t.Errorf("schema missing %q: %s", want, s.Parameters)
		}
	}
	if strings.Contains(string(s.Parameters), "oneOf") {
		t.Error("schema must not use oneOf")
	}
}

func TestRunCommandEffect(t *testing.T) {
	e := NewRunCommand(nil, nil).Effect()
	for _, c := range []agent.EffectClass{agent.Read, agent.Write, agent.Exec, agent.Network} {
		if !e.Class.Has(c) {
			t.Errorf("effect class missing bit %v", c)
		}
	}
	if e.Approval != agent.ApprovalAlways {
		t.Errorf("approval = %v, want ApprovalAlways", e.Approval)
	}
	if e.OutputCap != execRuntimeCap {
		t.Errorf("OutputCap = %d, want %d", e.OutputCap, execRuntimeCap)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResolveExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec-bit semantics")
	}
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// Use ws.root (canonical, post-EvalSymlinks) so path arithmetic is consistent.
	dir := ws.root
	// bin/ holds a workspace-relative script
	if err := os.Mkdir(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(dir, "bin", "tool.sh"), "#!/bin/sh\necho hi\n")

	// a PATH dir outside the workspace with a bare command
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	pathVal := pathDir + string(os.PathListSeparator) + "/usr/bin:/bin"

	t.Run("bare via PATH", func(t *testing.T) {
		got, fi, err := resolveExecutable(ws, dir, "mycmd", pathVal)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(pathDir, "mycmd") || fi == nil {
			t.Errorf("resolved %q", got)
		}
	})
	t.Run("bare not found", func(t *testing.T) {
		if _, _, err := resolveExecutable(ws, dir, "no_such_cmd_xyz", pathVal); err == nil {
			t.Error("want error")
		}
	})
	t.Run("absolute", func(t *testing.T) {
		abs := filepath.Join(pathDir, "mycmd")
		got, _, err := resolveExecutable(ws, dir, abs, pathVal)
		if err != nil || got != abs {
			t.Errorf("got %q err %v", got, err)
		}
	})
	t.Run("separator under workspace", func(t *testing.T) {
		got, _, err := resolveExecutable(ws, dir, "bin/tool.sh", pathVal)
		if err != nil {
			t.Fatal(err)
		}
		if got != filepath.Join(dir, "bin", "tool.sh") {
			t.Errorf("resolved %q", got)
		}
	})
	t.Run("separator escaping workspace rejected", func(t *testing.T) {
		if _, _, err := resolveExecutable(ws, dir, "../escape.sh", pathVal); err == nil {
			t.Error("want escape error")
		}
	})
	t.Run("separator symlink rejected", func(t *testing.T) {
		target := filepath.Join(pathDir, "mycmd")
		link := filepath.Join(dir, "bin", "linked.sh")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveExecutable(ws, dir, "bin/linked.sh", pathVal); err == nil {
			t.Error("want symlink rejection for in-workspace separator path")
		}
	})
	t.Run("non-executable rejected", func(t *testing.T) {
		p := filepath.Join(dir, "bin", "data.txt")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveExecutable(ws, dir, "bin/data.txt", pathVal); err == nil {
			t.Error("want non-executable rejection")
		}
	})
	_ = exec.ErrNotFound
}

func TestFormatExecResult(t *testing.T) {
	out := formatExecResult(execResult{ExitCode: 1, Stdout: []byte("hello\n"), Stderr: []byte("oops\n")})
	for _, want := range []string{"exit code: 1", "--- stdout ---", "hello", "--- stderr ---", "oops"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestFormatExecResultTruncationMarkers(t *testing.T) {
	out := formatExecResult(execResult{ExitCode: 0, Stdout: []byte("a"), StdoutTruncated: true, Stderr: []byte("b"), StderrTruncated: true})
	if c := strings.Count(out, "[truncated]"); c != 2 {
		t.Errorf("want 2 truncation markers, got %d:\n%s", c, out)
	}
}

func TestRenderExecPreview(t *testing.T) {
	p := execPending{
		path:        "/usr/local/go/bin/go",
		argv:        []string{"go", "test", "./..."},
		dirLabel:    "(workspace root)",
		envNames:    []string{"HOME", "PATH"},
		timeout:     60 * time.Second,
		fingerprint: "abc123def456",
	}
	out := renderExecPreview(p, "go")
	for _, want := range []string{
		"go test ./...",
		"go -> /usr/local/go/bin/go",
		"(workspace root)",
		"60s",
		"HOME(parent)", "PATH(parent)",
		"abc123def456",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "clamped") {
		t.Error("unclamped preview should not mention clamping")
	}
}

func TestRenderExecPreviewClamped(t *testing.T) {
	p := execPending{
		path: "/bin/sleep", argv: []string{"sleep", "999"},
		dirLabel: "sub", envNames: []string{"PATH"},
		timeout: 600 * time.Second, requestedTO: 900, clamped: true, fingerprint: "f1",
	}
	out := renderExecPreview(p, "sleep")
	if !strings.Contains(out, "600s (requested 900s, clamped)") {
		t.Errorf("missing clamp note:\n%s", out)
	}
}

func TestCommandFingerprint(t *testing.T) {
	base := commandFingerprint([]string{"go", "test"}, "/w", []string{"PATH"}, 60*time.Second)
	if len(base) != fingerprintLen {
		t.Fatalf("len = %d, want %d", len(base), fingerprintLen)
	}
	if base != commandFingerprint([]string{"go", "test"}, "/w", []string{"PATH"}, 60*time.Second) {
		t.Error("must be stable for identical inputs")
	}
	diff := []struct {
		name string
		f    string
	}{
		{"argv", commandFingerprint([]string{"go", "vet"}, "/w", []string{"PATH"}, 60*time.Second)},
		{"cwd", commandFingerprint([]string{"go", "test"}, "/x", []string{"PATH"}, 60*time.Second)},
		{"env", commandFingerprint([]string{"go", "test"}, "/w", []string{"PATH", "HOME"}, 60*time.Second)},
		{"timeout", commandFingerprint([]string{"go", "test"}, "/w", []string{"PATH"}, 30*time.Second)},
	}
	for _, d := range diff {
		if d.f == base {
			t.Errorf("%s change did not alter fingerprint", d.name)
		}
	}
}

func TestBuildExecEnv(t *testing.T) {
	parent := map[string]string{
		"PATH":           "/usr/bin:/bin",
		"HOME":           "/home/x",
		"LANG":           "en_US.UTF-8",
		"USER":           "x",
		"SECRET_TOKEN":   "shhh",
		"OPENAI_API_KEY": "sk-xyz",
		// TMPDIR intentionally absent -> skipped, not errored
	}
	lookup := func(k string) (string, bool) { v, ok := parent[k]; return v, ok }

	env, names := buildExecEnv(lookup)

	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "SECRET_TOKEN") || strings.Contains(joined, "OPENAI_API_KEY") {
		t.Fatalf("secret leaked into child env: %q", env)
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/x", "LANG=en_US.UTF-8", "USER=x"} {
		found := false
		for _, e := range env {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("env missing %q (got %v)", want, env)
		}
	}
	if got := strings.Join(names, ","); got != "HOME,LANG,PATH,USER" {
		t.Errorf("names = %q, want sorted present names HOME,LANG,PATH,USER", got)
	}
	if pathFromEnv(env) != "/usr/bin:/bin" {
		t.Errorf("pathFromEnv = %q", pathFromEnv(env))
	}
}

func TestResolveExecTimeout(t *testing.T) {
	p := func(n int) *int { return &n }
	cases := []struct {
		name      string
		in        *int
		wantEff   time.Duration
		wantReq   int
		wantClamp bool
		wantErr   bool
	}{
		{"default", nil, 60 * time.Second, 0, false, false},
		{"explicit", p(120), 120 * time.Second, 120, false, false},
		{"zero", p(0), 0, 0, false, true},
		{"negative", p(-5), 0, 0, false, true},
		{"max", p(600), 600 * time.Second, 600, false, false},
		{"clamp", p(900), 600 * time.Second, 900, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff, req, clamp, err := resolveExecTimeout(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return
			}
			if eff != c.wantEff || req != c.wantReq || clamp != c.wantClamp {
				t.Errorf("got (%v,%d,%v) want (%v,%d,%v)", eff, req, clamp, c.wantEff, c.wantReq, c.wantClamp)
			}
		})
	}
}

func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{cap: 4}
	n, err := b.Write([]byte("abcdef"))
	if err != nil || n != 6 {
		t.Fatalf("Write = %d,%v; want 6,nil (must consume all to avoid pipe deadlock)", n, err)
	}
	if string(b.buf) != "abcd" {
		t.Errorf("buf = %q, want abcd", b.buf)
	}
	if !b.truncated {
		t.Error("truncated should be true")
	}
	n, _ = b.Write([]byte("g"))
	if n != 1 || string(b.buf) != "abcd" {
		t.Errorf("post-cap write: n=%d buf=%q", n, b.buf)
	}
}

// Task 8: Plan tests

func planWS(t *testing.T) (*RunCommand, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunCommand(ws, nil), root
}

func TestPlanValid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	rc, root := planWS(t)
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")

	raw := json.RawMessage(`{"argv":["mycmd","arg"]}`)
	plan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effect.Timeout != execDefaultTimeout {
		t.Errorf("timeout = %v, want %v", plan.Effect.Timeout, execDefaultTimeout)
	}
	if plan.Effect.OutputCap != execRuntimeCap {
		t.Errorf("OutputCap = %d", plan.Effect.OutputCap)
	}
	if !strings.Contains(plan.Preview, "mycmd arg") {
		t.Errorf("preview missing argv:\n%s", plan.Preview)
	}
	// pending plan is stashed under the raw-args hash
	if _, ok := rc.consume(ContentHash(raw)); !ok {
		t.Error("expected stashed pending plan")
	}
	_ = root
}

func TestPlanRejects(t *testing.T) {
	rc, _ := planWS(t)
	cases := map[string]string{
		"empty argv":   `{"argv":[]}`,
		"blank argv0":  `{"argv":["   "]}`,
		"bad json":     `{"argv":`,
		"zero timeout": `{"argv":["echo"],"timeout_seconds":0}`,
		"dir escape":   `{"argv":["echo"],"dir":"../../etc"}`,
		"absolute dir": `{"argv":["echo"],"dir":"/etc"}`,
		"nul in argv":  "{\"argv\":[\"echo\x00evil\"]}",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := rc.Plan(context.Background(), json.RawMessage(raw)); err == nil {
				t.Errorf("Plan(%s) = nil err, want error", raw)
			}
		})
	}
}

func TestPlanDirDotIsRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	rc, _ := planWS(t)
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")

	raw := json.RawMessage(`{"argv":["mycmd"],"dir":"."}`)
	plan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Preview, "(workspace root)") {
		t.Errorf("dir=. should resolve to (workspace root) in preview:\n%s", plan.Preview)
	}
	// Must NOT show a bare "cwd: ." in the preview.
	if strings.Contains(plan.Preview, "cwd:     .") {
		t.Errorf("dir=. must not show as bare dot in cwd line:\n%s", plan.Preview)
	}
}

// Task 9: Invoke tests

type fakeRunner struct {
	result   execResult
	err      error
	blockCtx bool // when true, block until ctx is done then report TimedOut
	gotSpec  execSpec
	called   bool
}

func (f *fakeRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	f.called, f.gotSpec = true, spec
	if f.blockCtx {
		<-ctx.Done()
		return execResult{TimedOut: true}, context.DeadlineExceeded
	}
	return f.result, f.err
}

func invokeRC(t *testing.T, fr *fakeRunner) (*RunCommand, string) {
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
	return NewRunCommand(ws, fr), root
}

func TestInvokeNonZeroExitNotError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{result: execResult{ExitCode: 1, Stdout: []byte("FAIL\n")}}
	rc, _ := invokeRC(t, fr)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Error("non-zero exit must NOT be a tool error")
	}
	if !strings.Contains(res.Content, "exit code: 1") || !strings.Contains(res.Content, "FAIL") {
		t.Errorf("content = %q", res.Content)
	}
	if !fr.called || fr.gotSpec.Dir == "" {
		t.Error("runner not invoked with a resolved spec")
	}
}

func TestInvokePlanMismatchFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{}
	rc, _ := invokeRC(t, fr)
	if _, err := rc.Plan(context.Background(), json.RawMessage(`{"argv":["mycmd"]}`)); err != nil {
		t.Fatal(err)
	}
	// different args -> hash mismatch -> fail closed, runner never called
	res, err := rc.Invoke(context.Background(), json.RawMessage(`{"argv":["mycmd","x"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || fr.called {
		t.Errorf("want fail-closed without spawning; IsError=%v called=%v", res.IsError, fr.called)
	}
}

func TestInvokeInfraError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{err: fmt.Errorf("spawn boom")}
	rc, _ := invokeRC(t, fr)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("runner infra error must surface as IsError")
	}
}

func TestInvokeTimeoutIsError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{blockCtx: true}
	rc, _ := invokeRC(t, fr)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	res, err := rc.Invoke(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("timeout must surface as IsError")
	}
}

func TestInvokeDirChangedFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix symlink semantics")
	}
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	// Create root/sub as a real directory so Plan accepts it.
	subDir := filepath.Join(ws.root, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Set up a PATH dir with mycmd so Plan can resolve the executable.
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")

	fr := &fakeRunner{}
	rc := NewRunCommand(ws, fr)

	raw := json.RawMessage(`{"argv":["mycmd"],"dir":"sub"}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}

	// Between Plan and Invoke: replace root/sub with a symlink pointing outside the workspace.
	outside := t.TempDir()
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, subDir); err != nil {
		t.Fatal(err)
	}

	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("dir changed to symlink: want IsError=true")
	}
	if fr.called {
		t.Error("runner must not be called when dir check fails")
	}
}

func TestInvokeExecutableChangedFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n# inode A\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")

	fr := &fakeRunner{}
	rc := NewRunCommand(ws, fr)

	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}

	// Between Plan and Invoke: replace mycmd with a different file at a NEW inode.
	// Writing a sibling and renaming it over the path guarantees a distinct inode on
	// any filesystem: the sibling is allocated a fresh inode while the original still
	// exists, then rename relinks the path to it. A plain remove+recreate can reuse the
	// original inode on ext4/overlayfs, which would make os.SameFile pass and silently
	// defeat the identity check this test exists to verify.
	swapped := filepath.Join(pathDir, "mycmd.new")
	writeExecutable(t, swapped, "#!/bin/sh\n# inode B\n")
	if err := os.Rename(swapped, filepath.Join(pathDir, "mycmd")); err != nil {
		t.Fatal(err)
	}

	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("executable swapped: want IsError=true")
	}
	if fr.called {
		t.Error("runner must not be called when executable identity check fails")
	}
}

// Fix 1: runner returning DeadlineExceeded + TimedOut=true must produce "timed out" message.
func TestInvokeRunnerTimeoutMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{result: execResult{TimedOut: true}, err: context.DeadlineExceeded}
	rc, _ := invokeRC(t, fr)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("timeout runner error must surface as IsError")
	}
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("want 'timed out' in content, got: %q", res.Content)
	}
}

// Fix 1: runner returning context.Canceled (no TimedOut) must produce "canceled" message.
func TestInvokeRunnerCanceledMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	fr := &fakeRunner{err: context.Canceled}
	rc, _ := invokeRC(t, fr)
	raw := json.RawMessage(`{"argv":["mycmd"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("canceled runner error must surface as IsError")
	}
	if !strings.Contains(res.Content, "canceled") {
		t.Errorf("want 'canceled' in content, got: %q", res.Content)
	}
}

// Fix 3: renderExecPreview must quote args that contain spaces.
func TestRenderExecPreviewQuotesSpacedArgs(t *testing.T) {
	p := execPending{
		path:        "/usr/bin/git",
		argv:        []string{"git", "commit", "-m", "hello world"},
		dirLabel:    "",
		envNames:    []string{"PATH"},
		timeout:     60 * time.Second,
		fingerprint: "abc123",
	}
	out := renderExecPreview(p, "git")
	// The spaced arg must appear quoted.
	if !strings.Contains(out, `"hello world"`) {
		t.Errorf("spaced arg not quoted in preview:\n%s", out)
	}
	// Simple args must appear bare (no quotes).
	if strings.Contains(out, `"git"`) || strings.Contains(out, `"commit"`) || strings.Contains(out, `"-m"`) {
		t.Errorf("simple args should not be quoted in preview:\n%s", out)
	}
}

// Fix 3: renderArgvForPreview must leave simple argv like "go test ./..." unquoted.
func TestRenderArgvForPreviewNoExtraQuotes(t *testing.T) {
	argv := []string{"go", "test", "./..."}
	got := renderArgvForPreview(argv)
	want := "go test ./..."
	if got != want {
		t.Errorf("renderArgvForPreview = %q, want %q", got, want)
	}
}
