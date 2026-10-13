//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runLLMBenchMain(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), "LLM_BENCH_TEST_MAIN=1"), env...)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, string(out)
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), string(out)
	default:
		t.Fatalf("run llm-bench: %v", err)
		return 0, ""
	}
}

func replayAttemptArgs(baseURL, dir string) []string {
	return []string{
		"-replay-attempt",
		"-attempt-trace", fixtureSmokeTrace,
		"-models", "openai-compat/cand",
		"-candidate-base-url", baseURL,
		"-attempt-id", "att-cli",
		"-attempt-arm", "arm-a",
		"-attempt-dir", dir,
	}
}

func readTerminal(t *testing.T, dir string) attemptRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, attemptTerminalName))
	if err != nil {
		t.Fatalf("read terminal: %v", err)
	}
	var rec attemptRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestMainReplayAttemptEndToEnd(t *testing.T) {
	const key = "cli-key-0123456789"
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", `{"prompt_tokens":5,"completion_tokens":1}`)})
	dir := filepath.Join(t.TempDir(), "attempt")
	code, out := runLLMBenchMain(t, []string{"LLM_BENCH_CANDIDATE_API_KEY=" + key}, replayAttemptArgs(srv.URL, dir)...)
	if code != 0 || strings.Contains(out, key) {
		t.Fatalf("exit %d output %q", code, out)
	}
	if rec := readTerminal(t, dir); rec.Status != attemptStatusCompleted || rec.AttemptID != "att-cli" {
		t.Fatalf("record %+v", rec)
	}
	if got := srv.Received(); len(got) != 1 || got[0].Authorization != "Bearer "+key || got[0].Correlation != "att-cli/1" {
		t.Fatalf("server saw %+v", got)
	}
}

func TestMainReplayAttemptFailedAttemptExitsZero(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Status: 500, Body: "boom"})
	dir := filepath.Join(t.TempDir(), "attempt")
	code, out := runLLMBenchMain(t, nil, replayAttemptArgs(srv.URL, dir)...)
	if code != 0 {
		t.Fatalf("exit %d output %q; a published failed attempt exits 0", code, out)
	}
	if rec := readTerminal(t, dir); rec.Status != attemptStatusFailed || rec.Error.Code != attemptCodeHTTPStatus {
		t.Fatalf("record %+v", rec)
	}
}

// main hands runReplayAttempt the SIGINT/SIGTERM context. Only a signal sent
// to the real binary shows it: with context.Background() in that call the
// signal is swallowed and the attempt runs on until the server answers.
func TestMainReplayAttemptSIGINT(t *testing.T) {
	const delay = 5 * time.Second
	srv := newCompatServer(t, compatExchange{Delay: delay})
	dir := filepath.Join(t.TempDir(), "attempt")
	cmd := exec.Command(os.Args[0], replayAttemptArgs(srv.URL, dir)...)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start llm-bench: %v", err)
	}
	exited := make(chan struct{})
	var waitErr error // read only after exited is closed
	go func() { waitErr = cmd.Wait(); close(exited) }()
	// stop kills the child and waits for it. Until it has exited, exec may
	// still be writing to out, so a failure path reads out only after stop.
	// Kill after a normal exit is a harmless error.
	stop := func() { _ = cmd.Process.Kill(); <-exited }
	t.Cleanup(stop)

	// Signal only once the request is in flight. By then main has installed
	// its handler; an earlier SIGINT would take the default action and kill
	// the process instead of cancelling the attempt.
	for deadline := time.Now().Add(10 * time.Second); len(srv.Received()) == 0; {
		select {
		case <-exited:
			t.Fatalf("llm-bench exited before sending a request: %v\n%s", waitErr, out.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("no request within 10s\n%s", out.String())
		}
	}
	signalled := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal llm-bench: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(delay + 10*time.Second):
		stop()
		t.Fatalf("llm-bench still running %v after SIGINT\n%s", delay+10*time.Second, out.String())
	}
	if waitErr != nil {
		t.Fatalf("llm-bench: %v; a signalled attempt publishes its terminal and exits 0\n%s", waitErr, out.String())
	}
	if took := time.Since(signalled); took > delay-2*time.Second {
		t.Fatalf("llm-bench took %v to exit after SIGINT; the server's delay is %v", took, delay)
	}
	rec := readTerminal(t, dir)
	if rec.Status != attemptStatusFailed || rec.Error == nil || rec.Error.Code != attemptCodeCanceled || rec.Error.CancelSource != attemptCancelSignal {
		t.Fatalf("status %q error %+v; want failed, canceled by signal", rec.Status, rec.Error)
	}
}

func TestMainReplayAttemptExistingDirExitsOne(t *testing.T) {
	srv := newCompatServer(t)
	dir := t.TempDir() // exists
	// The output pins the exit-1 path to the claim refusing an existing dir:
	// a missing parent, a missing -attempt-dir, or a bare -replay-attempt
	// also exit 1 but print a different cause.
	code, out := runLLMBenchMain(t, nil, replayAttemptArgs(srv.URL, dir)...)
	if code != 1 || !strings.Contains(out, "claim attempt dir") || !strings.Contains(out, "file exists") {
		t.Fatalf("exit %d output %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, attemptTerminalName)); !os.IsNotExist(err) {
		t.Fatalf("terminal written into a pre-existing dir: %v", err)
	}
}

func TestMainAttemptFlagsRequireMode(t *testing.T) {
	code, out := runLLMBenchMain(t, nil, "-attempt-id", "x", "-traces", fixtureSmokeTrace, "-models", "fake")
	if code != 1 || !strings.Contains(out, "-attempt-id") || !strings.Contains(out, "require -replay-attempt") {
		t.Fatalf("exit %d output %q", code, out)
	}
}

func TestMainReplayAttemptIsExclusive(t *testing.T) {
	code, out := runLLMBenchMain(t, nil, "-replay-attempt", "-calibrate-capture")
	if code != 1 || !strings.Contains(out, "-replay-attempt") || !strings.Contains(out, "mutually exclusive") {
		t.Fatalf("exit %d output %q", code, out)
	}
}

func TestStrayAttemptFlags(t *testing.T) {
	got := strayAttemptFlags(map[string]bool{
		"attempt-dir": true, "attempt-arm": true, "models": true, "attempt-id": true, "attempt-trace": true,
	})
	if want := []string{"-attempt-trace", "-attempt-id", "-attempt-arm", "-attempt-dir"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strayAttemptFlags = %v, want %v", got, want)
	}
	if got := strayAttemptFlags(map[string]bool{"models": true}); len(got) != 0 {
		t.Fatalf("strayAttemptFlags = %v, want none", got)
	}
}
