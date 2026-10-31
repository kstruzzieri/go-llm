//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The -cache-probe flags are refused outside their mode, the mode excludes
// every other, and its required flags fail before any request is sent.
func TestCacheProbeFlagsRefusedByMain(t *testing.T) {
	out := filepath.Join(t.TempDir(), "probe.md")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-cache-probe-out", "x.md", "-fc-render"}, "-cache-probe-out require -cache-probe"},
		{[]string{"-cache-probe-scenario", "pressure", "-fc-render"}, "-cache-probe-scenario require -cache-probe"},
		{[]string{"-cache-probe", "-fc-render"}, "are mutually exclusive"},
		{[]string{"-cache-probe"}, "-cache-probe requires -candidate-base-url"},
		{[]string{"-cache-probe", "-candidate-base-url", "http://127.0.0.1:1"}, "-cache-probe requires -models"},
		{[]string{"-cache-probe", "-candidate-base-url", "http://127.0.0.1:1", "-models", "openai-compat/m"}, "-cache-probe requires -cache-probe-out"},
		{[]string{"-cache-probe", "-candidate-base-url", "http://127.0.0.1:1", "-models", "openai-compat/m", "-cache-probe-out", "x.md", "-cache-probe-scenario", "bogus"}, "must be"},
		// Every other flag is valid, so only the session id can be what is refused:
		// the probe mints its own id and would otherwise ignore this one silently.
		{[]string{"-cache-probe", "-candidate-base-url", "http://127.0.0.1:1", "-models", "openai-compat/m", "-cache-probe-out", out, "-candidate-session-id", "mine"},
			"-candidate-session-id cannot be combined with -cache-probe"},
	} {
		code, log := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(log, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, log, tc.want)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused invocation created the output file")
	}
}

// The flags reach runCacheProbeMode: a real invocation against a fake server
// writes the table, with the embedded fixtures, from the package directory.
func TestCacheProbeByMain(t *testing.T) {
	ps := newProbeServer(t)
	out := filepath.Join(t.TempDir(), "probe.md")
	code, log := runLLMBenchMain(t, nil, "-cache-probe", "-candidate-base-url", ps.URL, "-models", "openai-compat/probe-model",
		"-cache-probe-out", out, "-cache-probe-scenario", "pressure")
	if code != 0 {
		t.Fatalf("exit %d output %q", code, log)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "scenario pressure") {
		t.Errorf("table does not name the requested scenario:\n%s", b)
	}
	if ps.count() != 12 {
		t.Errorf("server saw %d requests, want 12", ps.count())
	}
}
