package main

import (
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestParseFlags_InterceptorsHelpNamesAlwaysOnGuards (#575): the help states
// that -interceptors adds content inspection on top of guards that always run.
func TestParseFlags_InterceptorsHelpNamesAlwaysOnGuards(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	t.Cleanup(func() { os.Stderr = original })
	os.Stderr = w
	_, parseErr := parseFlags([]string{"-help"})
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	help, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(parseErr, flag.ErrHelp) {
		t.Fatalf("parse error = %v", parseErr)
	}
	for _, want := range []string{
		"on top of the always-on guards",
		"argument invariants for named tools",
		"exec-class egress labels",
		"scoped-child refusal reporting",
		"required by /consult",
	} {
		if !strings.Contains(string(help), want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}
}

// TestVersionPrintsNoChainNotice pins that -version exits before chain
// construction.
func TestVersionPrintsNoChainNotice(t *testing.T) {
	stdin, stdout, stderr := runTestFiles(t)
	if err := run([]string{"-version"}, stdin, stdout, stderr); err != nil {
		t.Fatalf("run -version: %v", err)
	}
	if got, want := readRunTestFile(t, stdout), versionString()+"\n"; got != want {
		t.Fatalf("stdout = %q, want only the version %q", got, want)
	}
	for name, s := range map[string]string{"stdout": readRunTestFile(t, stdout), "stderr": readRunTestFile(t, stderr)} {
		if strings.Contains(s, "guards:") || strings.Contains(s, "interceptors:") {
			t.Fatalf("%s = %q, want no chain notice", name, s)
		}
	}
}

// TestStartupNoticeInEveryExecutionMode (§4.3): every agent-execution mode
// that reaches startup prints the guards line on stderr and never on stdout.
// The AgentFlow modes reach invokeAgentflow only after afterSessionReady, so
// stopping there pins their notice without the runtime.
func TestStartupNoticeInEveryExecutionMode(t *testing.T) {
	const guards = "guards: invariants, egress, child_scope_denials (always on; -interceptors adds detectors, secrets, canary)"
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"repl", nil},
		{"one-shot", []string{"-p", "hi"}},
		{"goal", []string{"-goal", "plan it"}},
		{"plan", []string{"-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
		{"resume", []string{"-agentflow-resume", "-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			args := append([]string{"-config", configPath, "-root", root, "-base-url", server.URL,
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag",
				"-no-project-context", "-no-auto-index"}, tc.args...)
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(*replSession) error { return errStop }})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v, want the session-ready stop\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			if lines := strings.Split(strings.TrimSpace(readRunTestFile(t, stderr)), "\n"); !slices.Contains(lines, guards) {
				t.Fatalf("stderr lines = %q, want %q", lines, guards)
			}
			if strings.Contains(readRunTestFile(t, stdout), "guards:") {
				t.Fatalf("stdout carries the notice: %q", readRunTestFile(t, stdout))
			}
		})
	}
}
