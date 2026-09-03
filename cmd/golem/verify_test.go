package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// stubVerifyExec scripts the bounded command so the policy layer can be tested
// without spawning anything.
type stubVerifyExec struct {
	res     agenttools.VerifyResult
	err     error
	runs    int
	command string
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
func (s *stubVerifyExec) ApprovalKey() string { return "verify:v1:deadbeef" }

func (s *stubVerifyExec) Run(context.Context) (agenttools.VerifyResult, error) {
	s.runs++
	return s.res, s.err
}

// recordingApprover captures what the policy asked and returns a scripted
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

func TestVerifyRunnerApprovalIdentity(t *testing.T) {
	exec := &stubVerifyExec{}
	ap := approved()
	if _, err := newVerifyRunner(exec).Verify(context.Background(), ap); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ap.calls != 1 {
		t.Fatalf("expected one approval request, got %d", ap.calls)
	}
	if ap.name != verifyToolName {
		t.Fatalf("approval tool name = %q, want %q", ap.name, verifyToolName)
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

func TestVerifyRunnerDenialIsAnObservationNotAnAbort(t *testing.T) {
	exec := &stubVerifyExec{}
	out, err := newVerifyRunner(exec).Verify(context.Background(),
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

func TestVerifyRunnerApproverErrorsAbort(t *testing.T) {
	cases := map[string]error{
		"interrupted prompt normalized to Canceled": context.Canceled,
		"prompt io failure":                         errors.New("read answer: broken pipe"),
	}
	for name, sentinel := range cases {
		t.Run(name, func(t *testing.T) {
			exec := &stubVerifyExec{}
			out, err := newVerifyRunner(exec).Verify(context.Background(),
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

func TestVerifyRunnerCancellationPropagates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exec := &stubVerifyExec{err: context.Canceled}
	out, err := newVerifyRunner(exec).Verify(ctx, approved())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation must abort, got %v", err)
	}
	if out != "" {
		t.Fatalf("a cancelled verification must produce no observation: %q", out)
	}
}

func TestVerifyRunnerInfraFailureIsAnObservation(t *testing.T) {
	exec := &stubVerifyExec{err: errors.New("executable changed since approval; retry")}
	out, err := newVerifyRunner(exec).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("a spawn failure must not fail the run: %v", err)
	}
	if !strings.Contains(out, "status: error") ||
		!strings.Contains(out, "executable changed since approval") {
		t.Fatalf("infra failure observation = %q", out)
	}
}

func TestVerifyRunnerObservationBytes(t *testing.T) {
	const header = "\n--- post-batch verification (ran after all calls in this batch) ---\n"
	cases := []struct {
		name string
		res  agenttools.VerifyResult
		want string
	}{
		{
			name: "passed carries no output body",
			res:  agenttools.VerifyResult{ExitCode: 0, Stdout: []byte("ok\n")},
			want: header + "command: go test ./...\nstatus: passed\nexit_code: 0\n",
		},
		{
			name: "failed leads with stderr",
			res: agenttools.VerifyResult{
				ExitCode: 2,
				Stdout:   []byte("building\n"),
				Stderr:   []byte("./foo.go:12:2: undefined: bar\n"),
			},
			want: header + "command: go test ./...\nstatus: failed\nexit_code: 2\n" +
				"timed_out: false\noutput_truncated: false\n" +
				"--- stderr ---\n./foo.go:12:2: undefined: bar\n--- stdout ---\nbuilding\n",
		},
		{
			name: "timeout omits the exit code the process never chose",
			res:  agenttools.VerifyResult{ExitCode: -1, TimedOut: true, Stderr: []byte("partial\n")},
			want: header + "command: go test ./...\nstatus: failed\n" +
				"timed_out: true\noutput_truncated: false\n" +
				"--- stderr ---\npartial\n",
		},
		{
			name: "runner truncation is reported even when the cap was not hit",
			res:  agenttools.VerifyResult{ExitCode: 1, Truncated: true, Stderr: []byte("x\n")},
			want: header + "command: go test ./...\nstatus: failed\nexit_code: 1\n" +
				"timed_out: false\noutput_truncated: true\n" +
				"--- stderr ---\nx\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := newVerifyRunner(&stubVerifyExec{res: tc.res}).Verify(context.Background(), approved())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if out != tc.want {
				t.Fatalf("observation mismatch\n got: %q\nwant: %q", out, tc.want)
			}
		})
	}
}

func TestVerifyRunnerObservationAlwaysStartsWithANewline(t *testing.T) {
	// The observation is appended to an existing tool result; without the
	// leading newline it would glue onto the write's last line.
	out, err := newVerifyRunner(&stubVerifyExec{}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !strings.HasPrefix(out, "\n") {
		t.Fatalf("observation must start with a newline: %q", out)
	}
}

func TestVerifyRunnerCapsModelVisibleOutput(t *testing.T) {
	flood := strings.Repeat("e", verifyObservationCap*3)
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

func TestVerifyRunnerCapsTheEntireModelVisibleFailureObservation(t *testing.T) {
	const failure = "./foo.go:12:2: undefined: bar\n"
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

func TestVerifyRunnerCapsLongCommandObservation(t *testing.T) {
	const failure = "./foo.go:12:2: undefined: bar"
	out, err := newVerifyRunner(&stubVerifyExec{
		command: strings.Repeat("x", verifyObservationCap),
		res: agenttools.VerifyResult{
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

func TestVerifyRunnerNormalizesInvalidUTF8Observation(t *testing.T) {
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

// TestVerifyRunnerNeverLosesStderrToAChattyStdout is the regression pin for
// the failure this ordering exists to prevent: a verifier that prints a lot on
// stdout and reports its actual error on stderr must not have the error cut
// off by the cap.
func TestVerifyRunnerNeverLosesStderrToAChattyStdout(t *testing.T) {
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

// TestVerifyRunnerBoundsTheTotalAcrossBothStreams is the case that actually
// distinguishes a shared budget from two independent per-stream caps: with a
// short stderr the split changes nothing observable, so both streams must be
// over the limit for the total to be the assertion.
func TestVerifyRunnerBoundsTheTotalAcrossBothStreams(t *testing.T) {
	big := strings.Repeat("x", verifyObservationCap*2)
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

func TestVerifyRunnerSingleStreamDoesNotSplitItsAvailableBudget(t *testing.T) {
	// With only one stream populated there is nothing to split the available
	// body budget with, so the two-stream halving must not apply.
	body := strings.Repeat("e", verifyObservationCap/2+100)
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
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

func TestVerifyRunnerCapNeverSplitsARune(t *testing.T) {
	// A multi-byte rune straddling the cap boundary must not be cut in half.
	out, err := newVerifyRunner(&stubVerifyExec{res: agenttools.VerifyResult{
		ExitCode: 1, Stderr: []byte(strings.Repeat("é", verifyObservationCap)),
	}}).Verify(context.Background(), approved())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !utf8.ValidString(out) {
		t.Fatal("capped observation is not valid UTF-8")
	}
}

// TestVerifyRunnerWithoutAnApproverFailsClosedVisibly covers the seam being
// used by a consumer whose writes are auto-approved. Skipping must never look
// like a clean check.
func TestVerifyRunnerWithoutAnApproverFailsClosedVisibly(t *testing.T) {
	exec := &stubVerifyExec{}
	out, err := newVerifyRunner(exec).Verify(context.Background(), nil)
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

// Unset, the slot performs no verification: agent.verifyBatch treats "" as
// "append nothing". A nil receiver and a nil runner are both no-ops, so a
// session outside the REPL (nil slot) and a mount without .golem.json are
// safe.
func TestLateVerifierUnsetReturnsNoObservation(t *testing.T) {
	var slot lateVerifier
	if out, err := slot.Verify(context.Background(), nil); out != "" || err != nil {
		t.Fatalf("unset Verify = %q, %v; want \"\", nil", out, err)
	}
	var nilSlot *lateVerifier
	nilSlot.set(newVerifyRunner(&stubVerifyExec{})) // must not panic
	slot.set(nil)
	if out, err := slot.Verify(context.Background(), nil); out != "" || err != nil {
		t.Fatalf("set(nil) changed the slot: %q, %v", out, err)
	}
}

func TestLateVerifierDelegatesOnceSet(t *testing.T) {
	exec := &stubVerifyExec{res: agenttools.VerifyResult{ExitCode: 0}}
	var slot lateVerifier
	slot.set(newVerifyRunner(exec))
	ap := approved()
	out, err := slot.Verify(context.Background(), ap)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if exec.runs != 1 || ap.calls != 1 || !strings.Contains(out, verifyHeader) {
		t.Fatalf("delegation broken: runs=%d approvals=%d out=%q", exec.runs, ap.calls, out)
	}
}
