//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	screenEnvValue  = "value-0001-abcdef"
	screenEnvFile   = "NAME=" + screenEnvValue + "\n"
	screenSentinel  = "sentinel-ok"
	screenMatchNote = "note: credential screen: 1 match(es) withheld from output (credential_path)\n"
)

// specSpy records the spec it is handed, then runs it through inner.
type specSpy struct {
	inner commandRunner
	spec  execSpec
}

func (s *specSpy) Run(ctx context.Context, spec execSpec) (execResult, error) {
	s.spec = spec
	return s.inner.Run(ctx, spec)
}

// planInvoke plans and invokes one run_command call on rc under ctx.
func planInvoke(t *testing.T, ctx context.Context, rc *RunCommand, label string, args map[string]any) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatalf("%s: plan failed: %v", label, err)
	}
	res, err := rc.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("%s: invoke failed: %v", label, err)
	}
	return res
}

// stdoutSection is the text between the stdout and stderr headers.
func stdoutSection(t *testing.T, label, content string) string {
	t.Helper()
	_, rest, ok := strings.Cut(content, "--- stdout ---\n")
	if !ok {
		t.Fatalf("%s: no stdout header", label)
	}
	out, _, ok := strings.Cut(rest, "\n--- stderr ---\n")
	if !ok {
		t.Fatalf("%s: no stderr header", label)
	}
	return out
}

// assertScreensOutput runs the first case of TestRunCommandScreensOutput
// through rc, whose workspace root is root.
func assertScreensOutput(t *testing.T, rc *RunCommand, root string) {
	t.Helper()
	writeTree(t, root, map[string]string{".env": screenEnvFile, "marker.txt": screenSentinel + "\n"})
	res := planInvoke(t, context.Background(), rc, "glob", map[string]any{"argv": []string{"sh", "-c", "cat .e* marker.txt"}})
	c := res.Content
	head, _, _ := strings.Cut(c, "--- stdout ---\n")
	switch {
	case res.IsError:
		t.Fatalf("glob: tool error (len %d)", len(c))
	case strings.Contains(c, screenEnvValue):
		t.Fatal("glob: the value reached the result")
	case !strings.HasPrefix(c, "exit code: 0\n"):
		t.Fatalf("glob: no exit code 0 (len %d)", len(c))
	case !strings.Contains(c, screenSentinel), !strings.Contains(c, tM):
		t.Fatalf("glob: sentinel %t, marker %t", strings.Contains(c, screenSentinel), strings.Contains(c, tM))
	case !strings.Contains(head, screenMatchNote):
		t.Fatalf("glob: no match note before stdout (head len %d)", len(head))
	}
}

func TestRunCommandScreensOutput(t *testing.T) {
	ws, root, _ := tolerantFixture(t, nil, nil)
	spy := &specSpy{inner: newPlatformRunner()}
	rc := NewRunCommand(ws, spy)
	assertScreensOutput(t, rc, root)
	if spy.spec.Screen == nil {
		t.Fatal("glob: runner got no screen")
	}
	if keys, _, released := spy.spec.Screen.snapshot(); keys != nil || !released {
		t.Fatalf("glob: screen not released (keys %d)", len(keys))
	}

	res := planInvoke(t, context.Background(), rc, "partial", map[string]any{"argv": []string{"sh", "-c", "printf partial"}})
	if res.IsError || stdoutSection(t, "partial", res.Content) != "partial" {
		t.Fatalf("partial: unterminated line lost (len %d)", len(res.Content))
	}
	if keys, _, released := spy.spec.Screen.snapshot(); keys != nil || !released {
		t.Fatalf("partial: screen not released (keys %d)", len(keys))
	}

	// The value alone on an unterminated last line: it is screened at finish,
	// and the note counts it.
	res = planInvoke(t, context.Background(), rc, "last", map[string]any{"argv": []string{"sh", "-c", "cut -d= -f2 .e* | tr -d '\\n'"}})
	c := res.Content
	head, _, _ := strings.Cut(c, "--- stdout ---\n")
	switch {
	case res.IsError:
		t.Fatalf("last: tool error (len %d)", len(c))
	case strings.Contains(c, screenEnvValue):
		t.Fatal("last: the value reached the result")
	case stdoutSection(t, "last", c) != tM:
		t.Fatalf("last: stdout is not the marker alone (len %d)", len(stdoutSection(t, "last", c)))
	case !strings.Contains(head, screenMatchNote):
		t.Fatalf("last: no match note before stdout (head len %d)", len(head))
	}
}

func TestRunCommandLongLineCutsOnlyThatStream(t *testing.T) {
	ws, _, _ := tolerantFixture(t, nil, nil)
	rc := NewRunCommand(ws, newPlatformRunner())
	script := "head -c 40000 /dev/zero | tr '\\0' x; printf '\\nafter\\n'; echo err-ok >&2"
	res := planInvoke(t, context.Background(), rc, "long", map[string]any{"argv": []string{"sh", "-c", script}})
	c := res.Content
	if res.IsError || !strings.HasPrefix(c, "exit code: 0\n") {
		t.Fatalf("long: not a clean exit (len %d)", len(c))
	}
	if got := stdoutSection(t, "long", c); got != tL+"\n" {
		t.Fatalf("long: stdout is not the cut marker alone (len %d)", len(got))
	}
	if _, stderr, _ := strings.Cut(c, "\n--- stderr ---\n"); stderr != "err-ok\n" {
		t.Fatalf("long: stderr differs (len %d)", len(stderr))
	}
}

func TestRunCommandNoScreenFails(t *testing.T) {
	dir := t.TempDir()
	spec := execSpec{Path: "/bin/sh", Argv: []string{"sh", "-c", "touch ran.txt"}, Dir: dir, Env: []string{"PATH=/usr/bin:/bin"}}
	_, err := unixRunner{}.Run(context.Background(), spec)
	if !errors.Is(err, errNoOutputScreen) {
		t.Fatalf("err = %v, want errNoOutputScreen", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ran.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the command ran (stat: %v)", err)
	}
}

func TestRunCommandWithheldStreams(t *testing.T) {
	tree := map[string]string{
		".env":      "NAME=value-0001-abcdef\nvalue-0002-abcdef\n",
		"data/.env": "NAME=value-0003-abcdef\n",
	}
	rows := []struct {
		label  string
		limits screenLimits
		slow   bool // beforeReadDir sleeps 50 ms
		seams  *screenSeams
		cause  string
	}{
		{"fileBytes", screenLimits{fileBytes: 4}, false, nil, `file over 4 bytes: ".env"`},
		{"keyBytes", screenLimits{keyBytes: 8}, false, nil, `line over 8 bytes: ".env"`},
		{"keys", screenLimits{keys: 1}, false, nil, "more than 1 keys"},
		{"totalBytes", screenLimits{totalBytes: 10}, false, nil, "keys over 10 bytes"},
		{"budget", screenLimits{budget: 10 * time.Millisecond}, true, nil, "time budget of 10ms exhausted"},
		{"file denied", screenLimits{}, false, &screenSeams{openErr: answer(".env", fs.ErrPermission)}, `unreadable: ".env"`},
		{"dir denied", screenLimits{}, false, &screenSeams{openErr: answer("data", fs.ErrPermission)}, `unreadable: "data"`},
		{"stat eio", screenLimits{}, false, &screenSeams{statErr: answer(".env", syscall.EIO)}, `unreadable: ".env"`},
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			ws, root, _ := tolerantFixture(t, tree, nil)
			ws.screenLimits = r.limits
			ws.screenSeams = r.seams
			if r.slow {
				ws.beforeReadDir = func(string) { time.Sleep(50 * time.Millisecond) }
			}
			rc := NewRunCommand(ws, newPlatformRunner())
			res := planInvoke(t, context.Background(), rc, r.label,
				map[string]any{"argv": []string{"sh", "-c", "echo out; echo err >&2; touch ran.txt"}})
			want := "exit code: 0\nnote: " + withheldNote + r.cause + ")\n--- stdout ---\n\n--- stderr ---\n"
			if res.IsError || res.Content != want {
				t.Fatalf("%s: result differs (error %t, len %d, want len %d)", r.label, res.IsError, len(res.Content), len(want))
			}
			if _, err := os.Stat(filepath.Join(root, "ran.txt")); err != nil {
				t.Fatalf("%s: the command did not run (stat: %v)", r.label, err)
			}
		})
	}
}

func TestRunCommandUnreadableAfterFirstCommand(t *testing.T) {
	ws, _, _ := tolerantFixture(t, map[string]string{".env": screenEnvFile}, nil)
	rc := NewRunCommand(ws, newPlatformRunner())
	args := map[string]any{"argv": []string{"sh", "-c", "cat .e*"}}
	first := planInvoke(t, context.Background(), rc, "first", args)
	if first.IsError || !strings.Contains(first.Content, tM) || strings.Contains(first.Content, screenEnvValue) {
		t.Fatalf("first: not screened (len %d)", len(first.Content))
	}
	ws.screenSeams = &screenSeams{openErr: answer(".env", fs.ErrPermission)}
	second := planInvoke(t, context.Background(), rc, "second", args)
	c := second.Content
	if second.IsError || strings.Contains(c, tM) || strings.Contains(c, screenEnvValue) || stdoutSection(t, "second", c) != "" {
		t.Fatalf("second: streams not withheld (len %d)", len(c))
	}
}

func TestRunCommandRootReplacedNotRun(t *testing.T) {
	ws, root, _ := tolerantFixture(t, map[string]string{".env": screenEnvFile}, nil)
	rc := NewRunCommand(ws, newPlatformRunner())
	raw := json.RawMessage(`{"argv":["sh","-c","touch ran.txt"]}`)
	if _, err := rc.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	ws.screenSeams = &screenSeams{listErr: answer("", ErrRootReplaced)}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.HasPrefix(res.Content, "credential screen failed; command not run: ") {
		t.Fatalf("result differs (error %t, len %d)", res.IsError, len(res.Content))
	}
	if _, err := os.Stat(filepath.Join(root, "ran.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the command ran (stat: %v)", err)
	}
}

func TestRunCommandCanceledDuringWalk(t *testing.T) {
	ws, root, _ := tolerantFixture(t, map[string]string{".env": screenEnvFile}, nil)
	rc := NewRunCommand(ws, newPlatformRunner())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws.beforeReadDir = func(string) { cancel() }
	res := planInvoke(t, ctx, rc, "cancel", map[string]any{"argv": []string{"sh", "-c", "touch ran.txt"}})
	if !res.IsError || res.Content != "command canceled" {
		t.Fatalf("result differs (error %t, len %d)", res.IsError, len(res.Content))
	}
	if _, err := os.Stat(filepath.Join(root, "ran.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the command ran (stat: %v)", err)
	}
}

// deadlineSpy records how long the context it runs under has left.
type deadlineSpy struct{ left time.Duration }

func (d *deadlineSpy) Run(ctx context.Context, _ execSpec) (execResult, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.left = time.Until(dl)
	}
	return execResult{}, nil
}

func TestRunCommandWalkTimeNotCharged(t *testing.T) {
	ws, _, _ := tolerantFixture(t, nil, nil)
	spy := &deadlineSpy{}
	rc := NewRunCommand(ws, spy)
	raw := json.RawMessage(`{"argv":["sh","-c","true"],"timeout_seconds":2}`)
	plan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2*time.Second + defaultScreenLimits.budget; plan.Effect.Timeout != want {
		t.Fatalf("effect timeout = %v, want %v", plan.Effect.Timeout, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), plan.Effect.Timeout)
	defer cancel()
	ws.beforeReadDir = func(string) { time.Sleep(300 * time.Millisecond) }
	if _, err := rc.Invoke(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if spy.left < 1950*time.Millisecond || spy.left > 2000*time.Millisecond {
		t.Fatalf("run deadline left = %v, want 1950ms to 2000ms", spy.left)
	}
}

func TestVerifyFailingObservationScreened(t *testing.T) {
	ws, _, _ := tolerantFixture(t, map[string]string{".env": screenEnvFile}, nil)
	v, err := NewVerifyCommand(ws, []string{"sh", "-c", "echo " + screenSentinel + "; cat .e*; exit 3"}, "", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	obs := verifyRenderResult(v, res)
	switch {
	case strings.Contains(obs, screenEnvValue):
		t.Fatal("the value reached the observation")
	case !strings.Contains(obs, screenSentinel), !strings.Contains(obs, tM):
		t.Fatalf("sentinel %t, marker %t", strings.Contains(obs, screenSentinel), strings.Contains(obs, tM))
	case !strings.Contains(obs, "status: failed\n"), !strings.Contains(obs, "exit_code: 3\n"):
		t.Fatalf("failing status missing (len %d)", len(obs))
	}
	// The command line quotes the sentinel too; the printed one is the last.
	note := strings.Index(obs, screenMatchNote)
	if note < 0 || note > strings.LastIndex(obs, screenSentinel) {
		t.Fatalf("match note missing or after the output (note at %d)", note)
	}
}
