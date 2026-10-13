//go:build !windows

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

func TestMainReplayAttemptExistingDirExitsOne(t *testing.T) {
	srv := newCompatServer(t)
	dir := t.TempDir() // exists
	if code, out := runLLMBenchMain(t, nil, replayAttemptArgs(srv.URL, dir)...); code != 1 {
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
	got := strayAttemptFlags(map[string]bool{"attempt-dir": true, "models": true, "attempt-trace": true})
	if want := []string{"-attempt-trace", "-attempt-dir"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("strayAttemptFlags = %v, want %v", got, want)
	}
	if got := strayAttemptFlags(map[string]bool{"models": true}); len(got) != 0 {
		t.Fatalf("strayAttemptFlags = %v, want none", got)
	}
}
