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

// The exact startup notices (#575). They stay literal and are never derived
// from the code under test, so reworded production text fails the tests that
// pin them.
const (
	guardsNoticeLine = "guards: invariants, egress, child_scope_denials (always on; -interceptors adds detectors, secrets, canary)"
	fullNoticeLine   = "interceptors: enabled (zero_width, encoding, typoglycemia, invariants, egress, secrets, child_scope_denials, canary)"
)

// TestParseFlags_InterceptorsHelpNamesAlwaysOnGuards (#575): the help states
// that -interceptors adds content inspection on top of guards that always run,
// and that risk display does not depend on the flag.
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
	// Scope every check to the -interceptors entry: it starts at the line
	// "  -interceptors" and ends before the next flag line.
	lines := strings.Split(string(help), "\n")
	start := slices.Index(lines, "  -interceptors")
	if start < 0 {
		t.Fatalf("help has no -interceptors entry:\n%s", help)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "  -") {
			end = i
			break
		}
	}
	entry := strings.Join(lines[start:end], "\n")
	for _, want := range []string{
		"on top of the always-on guards",
		"argument invariants for named tools",
		"exec-class egress labels",
		"scoped-child refusal reporting",
		"required by /consult",
		"with or without this flag",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("-interceptors help lacks %q:\n%s", want, entry)
		}
	}
}

// TestVersionPrintsNoChainNotice pins what -version emits: stdout is exactly
// the version line and stderr is empty. Neither channel carries a chain
// notice, whatever its wording.
func TestVersionPrintsNoChainNotice(t *testing.T) {
	stdin, stdout, stderr := runTestFiles(t)
	if err := run([]string{"-version"}, stdin, stdout, stderr); err != nil {
		t.Fatalf("run -version: %v", err)
	}
	if got, want := readRunTestFile(t, stdout), versionString()+"\n"; got != want {
		t.Fatalf("stdout = %q, want only the version %q", got, want)
	}
	if got := readRunTestFile(t, stderr); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

// TestStartupNoticeInEveryExecutionMode (#575): every agent-execution mode
// that reaches startup prints the guards line on stderr and never on stdout.
// The AgentFlow modes reach invokeAgentflow only after afterSessionReady, so
// stopping there pins their notice without the runtime.
func TestStartupNoticeInEveryExecutionMode(t *testing.T) {
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
			// Startup reads the user's consultants.json through
			// os.UserConfigDir; keep the developer's real file out of the run.
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
			if lines := strings.Split(strings.TrimSpace(readRunTestFile(t, stderr)), "\n"); !slices.Contains(lines, guardsNoticeLine) {
				t.Fatalf("stderr lines = %q, want %q", lines, guardsNoticeLine)
			}
			if strings.Contains(readRunTestFile(t, stdout), "guards:") {
				t.Fatalf("stdout carries the notice: %q", readRunTestFile(t, stdout))
			}
		})
	}
}
