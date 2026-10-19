package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// stubTargetLine is the stub's TargetLine, written out as a literal so the
// expected observations below never depend on the code under test.
const stubTargetLine = "target:  /ws (canonical workspace; unpromoted scratch artifacts are not checked)"

// stubVerifyExec scripts the bounded command so the slot's flow can be tested
// without spawning anything.
type stubVerifyExec struct {
	res     VerifyResult
	err     error
	runs    int
	command string
	target  string
}

func (s *stubVerifyExec) Command() string {
	if s.command != "" {
		return s.command
	}
	return "go test ./..."
}
func (s *stubVerifyExec) Preview() string {
	return "post-write verification command:\n  argv: go test ./...\n"
}
func (s *stubVerifyExec) ApprovalKey() string { return "verify:v2:deadbeef" }
func (s *stubVerifyExec) TargetLine() string {
	if s.target != "" {
		return s.target
	}
	return stubTargetLine
}

func (s *stubVerifyExec) Run(context.Context) (VerifyResult, error) {
	s.runs++
	return s.res, s.err
}

var _ verifyExecutor = (*stubVerifyExec)(nil)

// recordingApprover captures what the slot asked and returns a scripted
// decision, so the synthetic identity handed to the approver can be asserted.
type recordingApprover struct {
	decision agent.ApprovalDecision
	err      error
	calls    int
	name     string
	key      string
	preview  string
}

func (a *recordingApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	a.calls++
	return false, errors.New("plain Approve must not be used: the key would be dropped")
}

func (a *recordingApprover) ApproveKeyed(_ context.Context, call provider.ToolCall,
	preview, key string) (agent.ApprovalDecision, error) {
	a.calls++
	a.name, a.key, a.preview = call.Function.Name, key, preview
	return a.decision, a.err
}

func approved() *recordingApprover {
	return &recordingApprover{decision: agent.ApprovalDecision{Approved: true}}
}

func newTestSlot(t *testing.T, root string, policy SandboxConfig) *VerifierSlot {
	t.Helper()
	s, err := NewVerifierSlot(root, policy)
	if err != nil {
		t.Fatalf("NewVerifierSlot: %v", err)
	}
	return s
}

// armedSlot is a host slot over a fresh root whose executor is exec: the state
// Set reaches with a real command, without spawning anything.
func armedSlot(t *testing.T, exec verifyExecutor) *VerifierSlot {
	t.Helper()
	s := newTestSlot(t, t.TempDir(), SandboxConfig{})
	s.exec = exec
	return s
}

// canonicalTempDir returns a fresh directory and its symlink-resolved
// spelling, computed independently of the code under test.
func canonicalTempDir(t *testing.T) (dir, canon string) {
	t.Helper()
	dir = t.TempDir()
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, canon
}

func TestVerifierSlotBinding(t *testing.T) {
	_, canonA := canonicalTempDir(t)
	_, canonB := canonicalTempDir(t)
	wsA, err := NewWorkspace(canonA)
	if err != nil {
		t.Fatal(err)
	}
	wsB, err := NewWorkspace(canonB)
	if err != nil {
		t.Fatal(err)
	}
	argv := helperArgv(t, "echo", "ok")
	for _, tc := range []struct {
		name string
		cmd  *VerifyCommand
	}{
		{"other root", newTestVerifyCommand(t, wsB, &stubRunner{}, argv, "", time.Second)},
		{"other policy", newTestVerifyCommandWith(t, wsA, &stubRunner{},
			SandboxConfig{Runtime: SandboxRuntimeSeatbelt}, argv, "", time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slot := newTestSlot(t, canonA, SandboxConfig{})
			if err := slot.Accepts(tc.cmd); !errors.Is(err, ErrVerifierBinding) {
				t.Fatalf("Accepts = %v, want ErrVerifierBinding", err)
			}
			if err := slot.Set(tc.cmd); !errors.Is(err, ErrVerifierBinding) {
				t.Fatalf("Set = %v, want ErrVerifierBinding", err)
			}
			if state, target := slot.State(); state != "absent" || target != canonA {
				t.Fatalf("State = %q, %q after a refused command; want absent, %q", state, target, canonA)
			}
		})
	}
	t.Run("same root and policy", func(t *testing.T) {
		slot := newTestSlot(t, canonA, SandboxConfig{})
		if err := slot.Set(nil); err != nil {
			t.Fatalf("Set(nil) = %v", err)
		}
		if err := slot.Accepts(nil); err != nil {
			t.Fatalf("Accepts(nil) = %v", err)
		}
		if state, _ := slot.State(); state != "absent" {
			t.Fatalf("Set(nil) changed the state to %q", state)
		}
		cmd := newTestVerifyCommand(t, wsA, &stubRunner{}, argv, "", time.Second)
		if err := slot.Accepts(cmd); err != nil {
			t.Fatalf("Accepts = %v", err)
		}
		if state, _ := slot.State(); state != "absent" {
			t.Fatalf("Accepts armed the slot: %q", state)
		}
		if err := slot.Set(cmd); err != nil {
			t.Fatalf("Set = %v", err)
		}
		if state, target := slot.State(); state != "armed" || target != canonA {
			t.Fatalf("State = %q, %q; want armed, %q", state, target, canonA)
		}
		if err := slot.Set(nil); err != nil {
			t.Fatalf("Set(nil) = %v", err)
		}
		if state, _ := slot.State(); state != "armed" {
			t.Fatalf("Set(nil) disarmed the slot: %q", state)
		}
	})
	t.Run("BoundTo", func(t *testing.T) {
		slot := newTestSlot(t, canonA, SandboxConfig{})
		for _, tc := range []struct {
			name   string
			root   string
			policy SandboxConfig
			want   bool
		}{
			{"same root, zero host policy", canonA, SandboxConfig{}, true},
			{"same root, explicit host policy", canonA, SandboxConfig{Runtime: SandboxRuntimeHost}, true},
			{"other root", canonB, SandboxConfig{}, false},
			{"other policy", canonA, SandboxConfig{Runtime: SandboxRuntimeSeatbelt}, false},
			{"invalid policy", canonA, SandboxConfig{MemoryCapMB: -1}, false},
		} {
			if got := slot.BoundTo(tc.root, tc.policy); got != tc.want {
				t.Errorf("%s: BoundTo = %t, want %t", tc.name, got, tc.want)
			}
		}
	})
}

func TestVerifierSlotStates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlink creation")
	}
	dir, canon := canonicalTempDir(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	slot := newTestSlot(t, link, SandboxConfig{})
	want := func(step, wantState string) {
		t.Helper()
		if state, target := slot.State(); state != wantState || target != canon {
			t.Fatalf("%s: State = %q, %q; want %q, %q", step, state, target, wantState, canon)
		}
	}
	want("new", "absent")
	slot.Disable("verification disabled: x")
	want("disabled", "disabled")

	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	runner := &stubRunner{}
	if err := slot.Set(newTestVerifyCommand(t, ws, runner, helperArgv(t, "echo", "ok"), "", time.Second)); err != nil {
		t.Fatalf("Set over the symlinked spelling of the same root = %v", err)
	}
	want("set", "armed")

	slot.Disable("verification disabled: y")
	want("disabled after armed", "disabled")
	ap := approved()
	if out, err := slot.Verify(context.Background(), ap); out != "" || err != nil {
		t.Fatalf("disabled Verify = %q, %v; want \"\", nil", out, err)
	}
	if ap.calls != 0 || runner.n != 0 {
		t.Fatalf("a disabled slot asked %d approvals and ran %d commands", ap.calls, runner.n)
	}
}

func TestVerifierSlotEmptyIsSilent(t *testing.T) {
	ws := verifyWorkspace(t)
	cmd := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "ok"), "", time.Second)
	ap := approved()

	var nilSlot *VerifierSlot
	if out, err := nilSlot.Verify(context.Background(), ap); out != "" || err != nil {
		t.Fatalf("nil-slot Verify = %q, %v", out, err)
	}
	if err := nilSlot.Accepts(cmd); err != nil {
		t.Fatalf("nil-slot Accepts = %v", err)
	}
	if err := nilSlot.Set(cmd); err != nil {
		t.Fatalf("nil-slot Set = %v", err)
	}
	nilSlot.Disable("verification disabled: x") // must not panic
	if state, target := nilSlot.State(); state != "absent" || target != "" {
		t.Fatalf("nil-slot State = %q, %q", state, target)
	}
	if nilSlot.BoundTo(ws.root, SandboxConfig{}) {
		t.Fatal("a nil slot is bound to nothing")
	}

	if out, err := newTestSlot(t, ws.root, SandboxConfig{}).Verify(context.Background(), ap); out != "" || err != nil {
		t.Fatalf("absent Verify = %q, %v", out, err)
	}
	exec := &stubVerifyExec{}
	disabled := armedSlot(t, exec)
	disabled.Disable("verification disabled: x")
	if out, err := disabled.Verify(context.Background(), ap); out != "" || err != nil {
		t.Fatalf("disabled Verify = %q, %v", out, err)
	}
	if ap.calls != 0 || exec.runs != 0 {
		t.Fatalf("an unarmed slot asked %d approvals and ran %d commands", ap.calls, exec.runs)
	}
}

// TestVerifierSlotFlowMoved pins the observations the slot renders, moved
// byte for byte from cmd/golem's runner; the only addition is the target line.
func TestVerifierSlotFlowMoved(t *testing.T) {
	const head = "\n--- post-batch verification (ran after all calls in this batch) ---\n" +
		"command: go test ./...\n" + stubTargetLine + "\n"
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name     string
		ctx      context.Context
		exec     *stubVerifyExec
		approver agent.Approver
		want     string
		wantErr  error
		wantRuns int
	}{
		{
			name:     "passed carries no output body",
			exec:     &stubVerifyExec{res: VerifyResult{ExitCode: 0, Stdout: []byte("ok\n")}},
			approver: approved(),
			want:     head + "status: passed\nexit_code: 0\n",
			wantRuns: 1,
		},
		{
			name: "failed leads with stderr",
			exec: &stubVerifyExec{res: VerifyResult{
				ExitCode: 2,
				Stdout:   []byte("building\n"),
				Stderr:   []byte("./foo.go:12:2: undefined: bar\n"),
			}},
			approver: approved(),
			want: head + "status: failed\nexit_code: 2\n" +
				"timed_out: false\noutput_truncated: false\n" +
				"--- stderr ---\n./foo.go:12:2: undefined: bar\n--- stdout ---\nbuilding\n",
			wantRuns: 1,
		},
		{
			name:     "timeout omits the exit code the process never chose",
			exec:     &stubVerifyExec{res: VerifyResult{ExitCode: -1, TimedOut: true, Stderr: []byte("partial\n")}},
			approver: approved(),
			want: head + "status: failed\n" +
				"timed_out: true\noutput_truncated: false\n" +
				"--- stderr ---\npartial\n",
			wantRuns: 1,
		},
		{
			name:     "runner truncation is reported even when the cap was not hit",
			exec:     &stubVerifyExec{res: VerifyResult{ExitCode: 1, Truncated: true, Stderr: []byte("x\n")}},
			approver: approved(),
			want: head + "status: failed\nexit_code: 1\n" +
				"timed_out: false\noutput_truncated: true\n" +
				"--- stderr ---\nx\n",
			wantRuns: 1,
		},
		{
			name:     "canceled aborts with no observation",
			ctx:      canceled,
			exec:     &stubVerifyExec{err: context.Canceled},
			approver: approved(),
			wantErr:  context.Canceled,
			wantRuns: 1,
		},
		{
			name:     "denied is skipped and never runs",
			exec:     &stubVerifyExec{},
			approver: &recordingApprover{},
			want:     head + "status: skipped\nreason: not approved\n",
		},
		{
			name: "no approver is skipped and never runs",
			exec: &stubVerifyExec{},
			want: head + "status: skipped\nreason: no approver available\n",
		},
		{
			name:     "spawn error is an observation",
			exec:     &stubVerifyExec{err: errors.New("executable changed since approval; retry")},
			approver: approved(),
			want:     head + "status: error\nreason: executable changed since approval; retry\n",
			wantRuns: 1,
		},
		{
			name:     "spawn error reason stays on one line",
			exec:     &stubVerifyExec{err: errors.New(" a\nstatus: passed\r\nb ")},
			approver: approved(),
			want:     head + "status: error\nreason: a status: passed  b\n",
			wantRuns: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			out, err := armedSlot(t, tc.exec).Verify(ctx, tc.approver)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, tc.wantErr)
			}
			if out != tc.want {
				t.Fatalf("observation mismatch\n got: %q\nwant: %q", out, tc.want)
			}
			if tc.exec.runs != tc.wantRuns {
				t.Fatalf("runs = %d, want %d", tc.exec.runs, tc.wantRuns)
			}
		})
	}
}

func TestVerifierSlotApprovalIdentity(t *testing.T) {
	exec := &stubVerifyExec{}
	ap := approved()
	if _, err := armedSlot(t, exec).Verify(context.Background(), ap); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ap.calls != 1 {
		t.Fatalf("expected one approval request, got %d", ap.calls)
	}
	if ap.name != "verify_command" {
		t.Fatalf("approval tool name = %q, want %q", ap.name, "verify_command")
	}
	if ap.key != exec.ApprovalKey() {
		t.Fatalf("approval key = %q, want the command's own key %q", ap.key, exec.ApprovalKey())
	}
	if !strings.Contains(ap.preview, "go test ./...") {
		t.Fatalf("approval preview must show the command: %q", ap.preview)
	}
	if exec.runs != 1 {
		t.Fatalf("an approved verification must run once, got %d", exec.runs)
	}
}

func TestVerifierSlotDenialIsAnObservationNotAnAbort(t *testing.T) {
	exec := &stubVerifyExec{}
	out, err := armedSlot(t, exec).Verify(context.Background(),
		&recordingApprover{decision: agent.ApprovalDecision{}})
	if err != nil {
		t.Fatalf("a denial must not fail the run: %v", err)
	}
	if !strings.Contains(out, "status: skipped") || !strings.Contains(out, "not approved") {
		t.Fatalf("denial observation = %q", out)
	}
	if exec.runs != 0 {
		t.Fatalf("a denied verification must not run, got %d runs", exec.runs)
	}
}

func TestVerifierSlotApproverErrorsAbort(t *testing.T) {
	cases := map[string]error{
		"interrupted prompt normalized to Canceled": context.Canceled,
		"prompt io failure":                         errors.New("read answer: broken pipe"),
	}
	for name, sentinel := range cases {
		t.Run(name, func(t *testing.T) {
			exec := &stubVerifyExec{}
			out, err := armedSlot(t, exec).Verify(context.Background(),
				&recordingApprover{err: sentinel})
			if !errors.Is(err, sentinel) {
				t.Fatalf("approver error must abort, got %v", err)
			}
			if out != "" {
				t.Fatalf("an aborting approval must produce no observation: %q", out)
			}
			if exec.runs != 0 {
				t.Fatalf("must not run after an approval failure, got %d", exec.runs)
			}
		})
	}
}

func TestVerifierSlotCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec := &stubVerifyExec{err: context.Canceled}
	out, err := armedSlot(t, exec).Verify(ctx, approved())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must abort, got %v", err)
	}
	if out != "" {
		t.Fatalf("a cancelled verification must produce no observation: %q", out)
	}
}

func TestVerifierSlotInfraFailureIsAnObservation(t *testing.T) {
	exec := &stubVerifyExec{err: errors.New("executable changed since approval; retry")}
	out, err := armedSlot(t, exec).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("a spawn failure must not fail the run: %v", err)
	}
	if !strings.Contains(out, "status: error") ||
		!strings.Contains(out, "executable changed since approval") {
		t.Fatalf("infra failure observation = %q", out)
	}
}

func TestVerifierSlotObservationAlwaysStartsWithANewline(t *testing.T) {
	// The observation is appended to an existing tool result; without the
	// leading newline it would glue onto the write's last line.
	out, err := armedSlot(t, &stubVerifyExec{}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.HasPrefix(out, "\n") {
		t.Fatalf("observation must start with a newline: %q", out)
	}
}

func TestVerifierSlotCapsModelVisibleOutput(t *testing.T) {
	flood := strings.Repeat("e", verifyObservationCap*3)
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 1, Stderr: []byte(flood),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(out) > verifyObservationCap+1024 {
		t.Fatalf("observation not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "output_truncated: true") {
		t.Fatalf("a capped observation must say so:\n%s", out[:200])
	}
	// Head-first: a compiler's first error is the actionable one.
	if !strings.Contains(out, "--- stderr ---\n"+strings.Repeat("e", 64)) {
		t.Fatalf("cap must keep the HEAD of the output")
	}
}

func TestVerifierSlotCapsTheEntireModelVisibleFailureObservation(t *testing.T) {
	const failure = "./foo.go:12:2: undefined: bar\n"
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 2,
		Stderr:   []byte(failure + strings.Repeat("stderr noise\n", verifyObservationCap)),
		Stdout:   []byte(strings.Repeat("stdout noise\n", verifyObservationCap)),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(out) > verifyObservationCap {
		t.Fatalf("entire model-visible observation exceeds cap: %d bytes", len(out))
	}
	for _, want := range []string{
		"command: go test ./...", "status: failed", "exit_code: 2",
		"timed_out: false", "output_truncated: true", "--- stderr ---", failure,
		"--- stdout ---",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in observation:\n%s", want, out[:300])
		}
	}
	if strings.Index(out, "--- stderr ---") > strings.Index(out, "--- stdout ---") {
		t.Fatal("stderr must be rendered before stdout")
	}
	if !utf8.ValidString(out) {
		t.Fatal("capped observation is not valid UTF-8")
	}
}

func TestVerifierSlotCapsLongCommandObservation(t *testing.T) {
	const failure = "./foo.go:12:2: undefined: bar"
	out, err := armedSlot(t, &stubVerifyExec{
		command: strings.Repeat("x", verifyObservationCap),
		res: VerifyResult{
			ExitCode: 1,
			Stderr:   []byte(failure),
		},
	}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(out) > verifyObservationCap {
		t.Fatalf("long command made observation exceed cap: %d bytes", len(out))
	}
	for _, want := range []string{"status: failed", "exit_code: 1", failure} {
		if !strings.Contains(out, want) {
			t.Fatalf("long command hid %q:\n%s", want, out)
		}
	}
}

// A root near the OS path limit must not crowd the status and the failure
// out of the observation, exactly as a long command must not.
func TestVerifierSlotCapsLongTargetObservation(t *testing.T) {
	const failure = "./foo.go:12:2: undefined: bar"
	out, err := armedSlot(t, &stubVerifyExec{
		target: "target:  " + strings.Repeat("y", verifyObservationCap),
		res:    VerifyResult{ExitCode: 1, Stderr: []byte(failure)},
	}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(out) > verifyObservationCap {
		t.Fatalf("long target made observation exceed cap: %d bytes", len(out))
	}
	for _, want := range []string{"target:  yyyy", "status: failed", "exit_code: 1", failure} {
		if !strings.Contains(out, want) {
			t.Fatalf("long target hid %q:\n%s", want, out)
		}
	}
}

func TestVerifierSlotNormalizesInvalidUTF8Observation(t *testing.T) {
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 1,
		Stderr:   []byte{'b', 'a', 'd', ':', ' ', 0xff, 0xfe},
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Fatalf("observation is not valid UTF-8: %q", out)
	}
}

// TestVerifierSlotNeverLosesStderrToAChattyStdout is the regression pin for
// the failure this ordering exists to prevent: a verifier that prints a lot on
// stdout and reports its actual error on stderr must not have the error cut
// off by the cap.
func TestVerifierSlotNeverLosesStderrToAChattyStdout(t *testing.T) {
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 2,
		Stdout:   []byte(strings.Repeat("noise\n", verifyObservationCap)),
		Stderr:   []byte("./foo.go:12:2: undefined: bar\n"),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.Contains(out, "./foo.go:12:2: undefined: bar") {
		t.Fatalf("the actionable stderr was lost to stdout:\n%s", out[:400])
	}
	if !strings.Contains(out, "--- stdout ---") {
		t.Fatalf("stdout must still be represented:\n%s", out[:400])
	}
	// The whole point of splitting the budget is that verifyObservationCap
	// bounds the TOTAL, not each stream: a cap*2 allowance here would be
	// satisfied by capping the streams independently at full size.
	if len(out) > verifyObservationCap+1024 {
		t.Fatalf("two capped streams must share the budget, got %d bytes", len(out))
	}
	if strings.Index(out, "--- stderr ---") > strings.Index(out, "--- stdout ---") {
		t.Fatal("stderr must be rendered before stdout")
	}
}

// TestVerifierSlotBoundsTheTotalAcrossBothStreams is the case that actually
// distinguishes a shared budget from two independent per-stream caps: with a
// short stderr the split changes nothing observable, so both streams must be
// over the limit for the total to be the assertion.
func TestVerifierSlotBoundsTheTotalAcrossBothStreams(t *testing.T) {
	big := strings.Repeat("x", verifyObservationCap*2)
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 1, Stdout: []byte(big), Stderr: []byte(big),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(out) > verifyObservationCap+1024 {
		t.Fatalf("two over-cap streams must SHARE the budget, got %d bytes", len(out))
	}
	for _, want := range []string{"--- stderr ---", "--- stdout ---", "output_truncated: true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out[:300])
		}
	}
}

func TestVerifierSlotSingleStreamDoesNotSplitItsAvailableBudget(t *testing.T) {
	// With only one stream populated there is nothing to split the available
	// body budget with, so the two-stream halving must not apply.
	body := strings.Repeat("e", verifyObservationCap/2+100)
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 1, Stderr: []byte(body),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("a single stream below the cap must not be trimmed (len %d)", len(out))
	}
	if strings.Contains(out, "output_truncated: true") {
		t.Fatalf("nothing was over the cap:\n%s", out[:200])
	}
}

func TestVerifierSlotCapNeverSplitsARune(t *testing.T) {
	// A multi-byte rune straddling the cap boundary must not be cut in half.
	out, err := armedSlot(t, &stubVerifyExec{res: VerifyResult{
		ExitCode: 1, Stderr: []byte(strings.Repeat("é", verifyObservationCap)),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Fatal("capped observation is not valid UTF-8")
	}
}

// TestVerifierSlotWithoutAnApproverFailsClosedVisibly covers the seam being
// used by a consumer whose writes are auto-approved. Skipping must never look
// like a clean check.
func TestVerifierSlotWithoutAnApproverFailsClosedVisibly(t *testing.T) {
	exec := &stubVerifyExec{}
	out, err := armedSlot(t, exec).Verify(context.Background(), nil)
	if err != nil {
		t.Fatalf("a missing approver must not fail the run: %v", err)
	}
	if exec.runs != 0 {
		t.Fatalf("must not run unapproved, got %d runs", exec.runs)
	}
	if !strings.Contains(out, "status: skipped") || !strings.Contains(out, "no approver") {
		t.Fatalf("the skip must be visible to the model, got %q", out)
	}
	if strings.Contains(out, "status: passed") {
		t.Fatalf("a skip must never read as a pass: %q", out)
	}
}

// plainTap is an Approver with no key support; it records what it received.
type plainTap struct{ preview, args string }

func (p *plainTap) Approve(_ context.Context, call provider.ToolCall, preview string) (bool, error) {
	p.preview, p.args = preview, string(call.Function.Arguments)
	return true, nil
}

// keyedTap is a KeyedApprover that records what it received.
type keyedTap struct{ preview, key, args string }

func (k *keyedTap) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return false, errors.New("plain Approve must not be used: the key would be dropped")
}

func (k *keyedTap) ApproveKeyed(_ context.Context, call provider.ToolCall, preview, key string) (agent.ApprovalDecision, error) {
	k.preview, k.key, k.args = preview, key, string(call.Function.Arguments)
	return agent.ApprovalDecision{Approved: true}, nil
}

func TestVerifyApprovalPreviewSanitized(t *testing.T) {
	const (
		dirty = "post-write verification command:\n  argv: go test \x1b[2J\x1b]52;c;cHduZWQ=\a \u009b\r\b\x7f\n"
		want  = "post-write verification command:\n  argv: go test \\x1b[2J\\x1b]52;c;cHduZWQ=\\a \\u009b\\r\\b\\x7f\n"
		// The key and the call's arguments carry runes the sanitizer WOULD
		// change (ESC, tab, raw U+009B, raw U+202E); a clean literal could not
		// show that approveVerify leaves them alone.
		key  = "verify:v2:k\x1b[2J\t\u009b"
		args = "{\"cmd\":\t\"go test\u009b\u202e\"}"
	)
	if !strings.ContainsAny(key, "\x1b\t\u009b") || !strings.ContainsAny(args, "\t\u009b\u202e") {
		t.Fatalf("fixtures must carry controls: key %q args %q", key, args)
	}
	call := provider.ToolCall{
		ID: "v1", Type: "function",
		Function: provider.ToolCallFunction{Name: VerifyToolName, Arguments: json.RawMessage(args)},
	}

	t.Run("plain Approver", func(t *testing.T) {
		ap := &plainTap{}
		d, err := approveVerify(context.Background(), ap, call, dirty, key)
		if err != nil || !d.Approved {
			t.Fatalf("approveVerify: d=%+v err=%v", d, err)
		}
		// containment: PREVIEW-PROFILE
		if ap.preview != want {
			t.Fatalf("preview = %q, want %q", ap.preview, want)
		}
		if ap.args != args {
			t.Fatalf("call arguments = %q, want %q untouched", ap.args, args)
		}
	})
	t.Run("KeyedApprover", func(t *testing.T) {
		ap := &keyedTap{}
		d, err := approveVerify(context.Background(), ap, call, dirty, key)
		if err != nil || !d.Approved {
			t.Fatalf("approveVerify: d=%+v err=%v", d, err)
		}
		if ap.preview != want {
			t.Fatalf("preview = %q, want %q", ap.preview, want)
		}
		if ap.key != key {
			t.Fatalf("key = %q, want %q untouched", ap.key, key)
		}
		if ap.args != args {
			t.Fatalf("call arguments = %q, want %q untouched", ap.args, args)
		}
	})
}

// A slot is bound to a policy before any command exists, so an invalid policy
// is a policy error, while a root that does not resolve is not.
func TestNewVerifierSlotRejectsBadInput(t *testing.T) {
	if _, err := NewVerifierSlot(t.TempDir(), SandboxConfig{MemoryCapMB: -1}); !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("invalid policy: err = %v, want ErrSandboxPolicy", err)
	}
	_, err := NewVerifierSlot(filepath.Join(t.TempDir(), "missing"), SandboxConfig{})
	if err == nil || errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("missing root: err = %v, want a non-policy error", err)
	}
}
