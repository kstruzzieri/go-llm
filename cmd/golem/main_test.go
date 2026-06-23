package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

func TestParseFlagsAllowWrite(t *testing.T) {
	f, err := parseFlags([]string{"-allow-write"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.allowWrite {
		t.Fatal("-allow-write should set allowWrite")
	}
	f2, _ := parseFlags(nil)
	if f2.allowWrite {
		t.Fatal("allowWrite must default to false")
	}
}

func TestStartupNotices(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:       "/abs/root",
		useRecommend:    true,
		bootstrapWarns:  []error{errors.New("provider lab: refresh models failed")},
		preflightWarns:  []string{`agent fallback "ollama/m1" is not tool-capable (chat|stream|tool_call)`},
		retrieveOmitted: true,
	})
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"workspace: /abs/root",
		"no defaults.agent configured; using model recommendation",
		"warning: provider lab: refresh models failed",
		"ollama/m1",
		"retrieve unavailable: no RAG index configured; using file/search tools",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notices missing %q in:\n%s", want, joined)
		}
	}
}

func TestStartupNotices_RequestedRetrieveSuppressesGenericLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:         "/r",
		retrieveOmitted:   true,
		retrieveRequested: true,
		preflightWarns:    []string{`retrieve disabled: rag-db "/x.db": no such file or directory`},
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "retrieve disabled:") {
		t.Errorf("expected the specific retrieve-disabled reason, got:\n%s", joined)
	}
	if strings.Contains(joined, "no RAG index configured") {
		t.Errorf("generic no-index line must be suppressed when -rag-db was requested, got:\n%s", joined)
	}
}

func TestParseFlags_Defaults(t *testing.T) {
	f, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.root != "." {
		t.Errorf("root = %q, want \".\"", f.root)
	}
}

func TestParseFlags_Overrides(t *testing.T) {
	f, err := parseFlags([]string{"-root", "/x", "-config", "/c/models.json", "-no-color", "-max-steps", "8"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.root != "/x" || f.configPath != "/c/models.json" || !f.noColor || f.maxSteps != 8 {
		t.Errorf("flags = %+v, unexpected", f)
	}
}

func TestInterruptSignalsExcludeSIGTERM(t *testing.T) {
	for _, sig := range interruptSignals() {
		if sig == syscall.SIGTERM {
			t.Fatal("SIGTERM must keep Go's default process-termination behavior")
		}
	}
}

func TestParseFlags_SessionDefaults(t *testing.T) {
	f, err := parseFlags([]string{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.noSession || f.fresh || f.sessionID != "" {
		t.Errorf("session flag defaults wrong: %+v", f)
	}
}

func TestParseFlags_SessionOverrides(t *testing.T) {
	f, err := parseFlags([]string{"-no-session", "-fresh", "-session", "mychat"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noSession || !f.fresh || f.sessionID != "mychat" {
		t.Errorf("session overrides wrong: %+v", f)
	}
}

func TestValidateFlags(t *testing.T) {
	if err := validateFlags(flags{fresh: true, sessionID: "x"}); err == nil {
		t.Error("-fresh with -session must error (mutually exclusive)")
	}
	if err := validateFlags(flags{fresh: true}); err != nil {
		t.Errorf("-fresh alone must be allowed, got %v", err)
	}
	if err := validateFlags(flags{sessionID: "x"}); err != nil {
		t.Errorf("-session alone must be allowed, got %v", err)
	}
}

func TestStartupNotices_SessionLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:   "/r",
		sessionLine: "session: workspace:abcd (new)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "session: workspace:abcd (new)") {
		t.Errorf("session line missing in:\n%s", joined)
	}
}

func TestValidateFlags_NoRagWithRagDBExclusive(t *testing.T) {
	err := validateFlags(flags{noRag: true, ragDB: "/x.db"})
	if err == nil {
		t.Fatal("want error when -no-rag and -rag-db are both set")
	}
}

func TestParseFlags_NoRag(t *testing.T) {
	f, err := parseFlags([]string{"-no-rag"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.noRag {
		t.Error("noRag = false, want true")
	}
}

func TestParseFlagsProjectContextOptOut(t *testing.T) {
	f, err := parseFlags([]string{"-no-project-context"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noProjectContext {
		t.Fatal("-no-project-context should set noProjectContext")
	}
	f2, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags defaults: %v", err)
	}
	if f2.noProjectContext {
		t.Fatal("noProjectContext must default to false")
	}
}

func TestParseFlagsAllowExec(t *testing.T) {
	f, err := parseFlags([]string{"-allow-exec"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.allowExec {
		t.Error("allowExec should be true")
	}
}

func TestStartupNoticesProjectContextLineIsInformational(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace:          "/abs/root",
		projectContextLine: "project context: loaded 2 file(s)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "project context: loaded 2 file(s)") {
		t.Fatalf("startup notices missing project context line:\n%s", joined)
	}
	if strings.Contains(joined, "warning: project context") {
		t.Fatalf("project context loaded line must not be rendered as a warning:\n%s", joined)
	}
}
