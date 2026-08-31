package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	t.Run("bare via relative PATH returns absolute path", func(t *testing.T) {
		processRoot := t.TempDir()
		relBin := filepath.Join(processRoot, "relbin")
		if err := os.Mkdir(relBin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(relBin, "relcmd"), "#!/bin/sh\n")
		t.Chdir(processRoot)

		got, fi, err := resolveExecutable(ws, dir, "relcmd", "relbin")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(relBin, "relcmd")
		if got != want || !filepath.IsAbs(got) || fi == nil {
			t.Errorf("resolved %q, want absolute %q", got, want)
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

func TestRenderExecPreviewKeepsDynamicFieldsOnOneLine(t *testing.T) {
	p := execPending{
		path:        "/tmp/tool\n  cwd: forged",
		argv:        []string{"tool\n  id: forged"},
		dirLabel:    "work\n  exe: forged",
		timeout:     60 * time.Second,
		fingerprint: "abc123",
	}
	out := renderExecPreview(p, p.argv[0])
	for _, label := range []string{"  exe:", "  cwd:", "  id:"} {
		got := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, label) {
				got++
			}
		}
		if got != 1 {
			t.Errorf("renderExecPreview injected %q label count = %d, want 1:\n%s", label, got, out)
		}
	}
	if !strings.Contains(out, `\n  cwd: forged`) || !strings.Contains(out, `\n  exe: forged`) {
		t.Errorf("renderExecPreview did not visibly escape embedded newlines:\n%s", out)
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
	base := commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/bin/go")
	if len(base) != 64 { // full sha256 hex: the approval key uses all of it
		t.Fatalf("len = %d, want 64", len(base))
	}
	if base != commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/bin/go") {
		t.Error("must be stable for identical inputs")
	}
	diff := []struct {
		name string
		f    string
	}{
		{"argv", commandFingerprint([]string{"go", "vet"}, "/w", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/bin/go")},
		{"cwd", commandFingerprint([]string{"go", "test"}, "/x", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/bin/go")},
		{"workspace root", commandFingerprint([]string{"go", "test"}, "/w", "/", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/bin/go")},
		{"env shape", commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin", "HOME=/home/x"}, 60*time.Second, 0, "/w/bin/go")},
		{"env value", commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/opt/bin"}, 60*time.Second, 0, "/w/bin/go")},
		{"timeout", commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin"}, 30*time.Second, 0, "/w/bin/go")},
		{"requested timeout", commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 60, "/w/bin/go")},
		{"exe", commandFingerprint([]string{"go", "test"}, "/w", "/w", []string{"PATH=/usr/bin"}, 60*time.Second, 0, "/w/other/go")},
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
	if strconv.IntSize == 64 {
		hugeValue := int64(10_000_000_000)
		huge := int(hugeValue)
		cases = append(cases, struct {
			name      string
			in        *int
			wantEff   time.Duration
			wantReq   int
			wantClamp bool
			wantErr   bool
		}{"clamp before duration overflow", p(huge), execMaxTimeout, huge, true, false})
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

func TestInvokeDirIdentityChangedFailsClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	subDir := filepath.Join(ws.root, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
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

	oldSub := filepath.Join(ws.root, "sub.old")
	newSub := filepath.Join(ws.root, "sub.new")
	if err := os.Rename(subDir, oldSub); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newSub, subDir); err != nil {
		t.Fatal(err)
	}

	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("dir identity changed: want IsError=true")
	}
	if fr.called {
		t.Error("runner must not be called when dir identity check fails")
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

// #346: shared preparation helpers usable by foreground and background tools.

// prepWS builds a Workspace plus a PATH dir holding "mycmd", with PATH/HOME pinned.
func prepWS(t *testing.T) (*Workspace, string) {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	writeExecutable(t, filepath.Join(pathDir, "mycmd"), "#!/bin/sh\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("HOME", "/home/x")
	return ws, pathDir
}

func TestExecPrepareRejectsArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, _ := prepWS(t)
	cases := []struct {
		name    string
		argv    []string
		wantMsg string
	}{
		{"nil argv", nil, "argv is required and must be non-empty"},
		{"empty argv", []string{}, "argv is required and must be non-empty"},
		{"blank argv0", []string{"   "}, "argv[0] must not be blank"},
		{"nul in argv0", []string{"echo\x00evil"}, "argv must not contain NUL bytes"},
		{"nul in later arg", []string{"mycmd", "a\x00b"}, "argv must not contain NUL bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := prepareExecPlan(ws, c.argv, "", execDefaultTimeout, 0, false)
			if err == nil {
				t.Fatalf("prepareExecPlan(%q) = nil err, want error", c.argv)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("err = %q, want substring %q", err, c.wantMsg)
			}
		})
	}
}

func TestExecPrepareResolvesDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, _ := prepWS(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(ws.root, "link")); err != nil {
		t.Fatal(err)
	}

	t.Run("empty dir is workspace root", func(t *testing.T) {
		p, err := prepareExecPlan(ws, []string{"mycmd"}, "", execDefaultTimeout, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if p.dir != ws.root || p.dirLabel != "" || p.dirIdentity == nil {
			t.Errorf("dir=%q label=%q identity=%v, want root/empty/non-nil", p.dir, p.dirLabel, p.dirIdentity)
		}
	})
	t.Run("subdir resolves through workspace", func(t *testing.T) {
		p, err := prepareExecPlan(ws, []string{"mycmd"}, "sub", execDefaultTimeout, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if p.dir != filepath.Join(ws.root, "sub") || p.dirLabel != "sub" || p.dirIdentity == nil {
			t.Errorf("dir=%q label=%q, want %q/sub", p.dir, p.dirLabel, filepath.Join(ws.root, "sub"))
		}
	})
	t.Run("escape rejected", func(t *testing.T) {
		if _, err := prepareExecPlan(ws, []string{"mycmd"}, "../escape", execDefaultTimeout, 0, false); err == nil {
			t.Error("want containment error")
		}
	})
	t.Run("symlink dir rejected", func(t *testing.T) {
		if _, err := prepareExecPlan(ws, []string{"mycmd"}, "link", execDefaultTimeout, 0, false); err == nil {
			t.Error("want symlink rejection")
		}
	})
}

func TestExecPrepareResolvesExecutableAndEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, pathDir := prepWS(t)
	t.Setenv("SECRET_TOKEN", "shhh")

	p, err := prepareExecPlan(ws, []string{"mycmd", "arg"}, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(pathDir, "mycmd"); p.path != want {
		t.Errorf("path = %q, want %q", p.path, want)
	}
	if p.identity == nil {
		t.Error("executable identity must be captured")
	}
	// Env must be exactly the sanitized allowlist env the foreground path builds.
	wantEnv, wantNames := buildExecEnv(os.LookupEnv)
	if strings.Join(p.env, "\n") != strings.Join(wantEnv, "\n") {
		t.Errorf("env = %v, want sanitized %v", p.env, wantEnv)
	}
	if strings.Join(p.envNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("envNames = %v, want %v", p.envNames, wantNames)
	}
	if strings.Contains(strings.Join(p.env, "\n"), "SECRET_TOKEN") {
		t.Errorf("secret leaked into plan env: %v", p.env)
	}
	t.Run("unresolvable executable errors", func(t *testing.T) {
		if _, err := prepareExecPlan(ws, []string{"no_such_cmd_xyz"}, "", execDefaultTimeout, 0, false); err == nil {
			t.Error("want resolve error")
		}
	})
}

func TestExecPrepareCopiesArgv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, _ := prepWS(t)
	argv := []string{"mycmd", "arg"}
	p, err := prepareExecPlan(ws, argv, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	argv[1] = "MUTATED"
	if p.argv[1] != "arg" {
		t.Errorf("stored argv aliases caller slice: %v", p.argv)
	}
}

// The helper must produce the CURRENT foreground fingerprint recipe: expected
// digest computed by calling commandFingerprint directly with independently
// known inputs, never by re-asking the helper.
func TestExecPrepareFingerprintMatchesForeground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, pathDir := prepWS(t)
	argv := []string{"mycmd", "arg"}
	env, _ := buildExecEnv(os.LookupEnv)
	wantPath := filepath.Join(pathDir, "mycmd")
	want := commandFingerprint(argv, ws.root, ws.root, env, 90*time.Second, 90, wantPath)

	p, err := prepareExecPlan(ws, argv, "", 90*time.Second, 90, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.fingerprint != want {
		t.Errorf("fingerprint = %q, want foreground recipe %q", p.fingerprint, want)
	}
	if p.timeout != 90*time.Second || p.requestedTO != 90 || p.clamped {
		t.Errorf("timeout fields = (%v,%d,%v), want (90s,90,false)", p.timeout, p.requestedTO, p.clamped)
	}
}

func TestExecRecheckSuccessReturnsOwnedSpec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, _ := prepWS(t)
	pp, err := prepareExecPlan(ws, []string{"mycmd", "arg"}, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := recheckExecPlan(ws, pp)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Path != pp.path || spec.Dir != pp.dir {
		t.Errorf("spec = %+v, want path %q dir %q", spec, pp.path, pp.dir)
	}
	if strings.Join(spec.Argv, " ") != "mycmd arg" {
		t.Errorf("spec.Argv = %v", spec.Argv)
	}
	// Returned snapshot must not alias the pending plan's slices.
	spec.Argv[0] = "evil"
	spec.Env = append(spec.Env[:0], "evil")
	if pp.argv[0] != "mycmd" {
		t.Errorf("spec.Argv aliases pending argv: %v", pp.argv)
	}
	if len(pp.env) > 0 && pp.env[0] == "evil" {
		t.Errorf("spec.Env aliases pending env: %v", pp.env)
	}
}

func TestExecRecheckPreservesEmptySanitizedEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	ws, pathDir := prepWS(t)
	pp, err := prepareExecPlan(ws, []string{filepath.Join(pathDir, "mycmd")}, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	pp.env = nil

	spec, err := recheckExecPlan(ws, pp)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env == nil {
		t.Fatal("empty sanitized environment became nil, which makes os/exec inherit the parent environment")
	}
}

func TestExecRecheckDirChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix symlink semantics")
	}
	ws, _ := prepWS(t)
	subDir := filepath.Join(ws.root, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pp, err := prepareExecPlan(ws, []string{"mycmd"}, "sub", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(subDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), subDir); err != nil {
		t.Fatal(err)
	}
	if _, err := recheckExecPlan(ws, pp); err == nil || err.Error() != "working directory changed since approval; retry" {
		t.Errorf("err = %v, want exact dir-changed message", err)
	}
}

func TestExecRecheckDirIdentityChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	ws, _ := prepWS(t)
	subDir := filepath.Join(ws.root, "sub")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pp, err := prepareExecPlan(ws, []string{"mycmd"}, "sub", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Same path, new inode: rename away, create fresh, rename over.
	oldSub := filepath.Join(ws.root, "sub.old")
	newSub := filepath.Join(ws.root, "sub.new")
	if err := os.Rename(subDir, oldSub); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(newSub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newSub, subDir); err != nil {
		t.Fatal(err)
	}
	if _, err := recheckExecPlan(ws, pp); err == nil || err.Error() != "working directory changed since approval; retry" {
		t.Errorf("err = %v, want exact dir-changed message", err)
	}
}

func TestExecRecheckExecutableChanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	ws, pathDir := prepWS(t)
	pp, err := prepareExecPlan(ws, []string{"mycmd"}, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	// Write-sibling+rename guarantees a fresh inode (see TestInvokeExecutableChangedFailsClosed).
	swapped := filepath.Join(pathDir, "mycmd.new")
	writeExecutable(t, swapped, "#!/bin/sh\n# inode B\n")
	if err := os.Rename(swapped, filepath.Join(pathDir, "mycmd")); err != nil {
		t.Fatal(err)
	}
	if _, err := recheckExecPlan(ws, pp); err == nil || err.Error() != "executable changed since approval; retry" {
		t.Errorf("err = %v, want exact executable-changed message", err)
	}
}

// TestRecheckExecPlanStampsWorkspaceRoot pins the #442 contract: the spec a
// backend receives carries the canonical root of the Workspace the plan was
// approved against, so sandbox backends can scope allowances without a second
// resolution that could drift from the approval.
func TestRecheckExecPlanStampsWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pp, err := prepareExecPlan(ws, []string{"go", "version"}, "", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := recheckExecPlan(ws, pp)
	if err != nil {
		t.Fatal(err)
	}
	// Independent expectation: canonicalize the root we created ourselves,
	// not whatever the Workspace happens to hold.
	want, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if spec.WorkspaceRoot != want {
		t.Fatalf("spec.WorkspaceRoot = %q, want %q", spec.WorkspaceRoot, want)
	}
}

// TestRecheckExecPlanRejectsReplacedWorkspaceRoot swaps the workspace root for
// a fresh directory while PRESERVING the approved cwd's inode (rename the root
// away, recreate it, move the original subdirectory back). The cwd identity
// check alone cannot see this substitution; only a root identity check can.
func TestRecheckExecPlanRejectsReplacedWorkspaceRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix inode/SameFile semantics")
	}
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pp, err := prepareExecPlan(ws, []string{"go", "version"}, "sub", execDefaultTimeout, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(moved, "sub"), sub); err != nil {
		t.Fatal(err)
	}
	if _, err := recheckExecPlan(ws, pp); err == nil || err.Error() != "workspace root changed since approval; retry" {
		t.Errorf("err = %v, want exact root-changed message", err)
	}
}
