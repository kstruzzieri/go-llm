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

func TestStartupNotices_MCPLine(t *testing.T) {
	got := startupNotices(startupInfo{
		workspace: "/r",
		mcpLine:   "mcp: attached 3 tool(s) from 2 configured server(s)",
	})
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "mcp: attached 3 tool(s) from 2 configured server(s)") {
		t.Errorf("mcp line missing in:\n%s", joined)
	}
}

func TestParseFlags_MCPServersRepeatable(t *testing.T) {
	f, err := parseFlags([]string{"-mcp-stdio", "npx a", "-mcp-stdio", "npx b", "-mcp-http", "https://h/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.mcpStdio) != 2 {
		t.Errorf("mcpStdio = %v, want 2 entries", f.mcpStdio)
	}
	if len(f.mcpHTTP) != 1 {
		t.Errorf("mcpHTTP = %v, want 1 entry", f.mcpHTTP)
	}
}

func TestValidateFlags_NoRagWithRagDBExclusive(t *testing.T) {
	err := validateFlags(flags{noRag: true, ragDB: "/x.db"})
	if err == nil {
		t.Fatal("want error when -no-rag and -rag-db are both set")
	}
}

func TestValidateFlags_FeedbackDBRequiresFeedback(t *testing.T) {
	if err := validateFlags(flags{feedbackDB: "/x.db"}); err == nil {
		t.Fatal("want error when -feedback-db is set without -feedback")
	}
	if err := validateFlags(flags{feedback: true, feedbackDB: "/x.db"}); err != nil {
		t.Fatalf("want no error when -feedback and -feedback-db both set: %v", err)
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

func TestParseFlagsNoMemory(t *testing.T) {
	f, err := parseFlags([]string{"-no-memory"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.noMemory {
		t.Error("-no-memory not parsed")
	}
	def, _ := parseFlags(nil)
	if def.noMemory {
		t.Error("noMemory should default false")
	}
}

func TestParsePressureWarnFlag(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.pressureWarn != 75 {
		t.Fatalf("default pressureWarn = %d, want 75", f.pressureWarn)
	}
	f, err = parseFlags([]string{"-pressure-warn", "0"})
	if err != nil || f.pressureWarn != 0 {
		t.Fatalf("explicit 0: f=%d err=%v", f.pressureWarn, err)
	}
}

func TestValidatePressureWarnRange(t *testing.T) {
	if err := validateFlags(flags{pressureWarn: 75}); err != nil {
		t.Fatalf("75 should be valid: %v", err)
	}
	if err := validateFlags(flags{pressureWarn: -1}); err == nil {
		t.Fatal("negative pressureWarn should be rejected")
	}
	if err := validateFlags(flags{pressureWarn: 101}); err == nil {
		t.Fatal(">100 pressureWarn should be rejected")
	}
}

func TestAgentMemoryFlag(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.agentMemory {
		t.Error("agent memory must default to disabled (opt-in)")
	}
	f, err = parseFlags([]string{"-agent-memory"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !f.agentMemory {
		t.Error("-agent-memory not parsed")
	}
}

func TestStartupNoticesAgentMemoryLine(t *testing.T) {
	lines := startupNotices(startupInfo{workspace: "/w", agentMemoryLine: "agent memory: enabled"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "agent memory: enabled") {
		t.Errorf("notices missing agent memory line: %v", lines)
	}
}

func TestParseFlags_NoAutoIndex(t *testing.T) {
	f, err := parseFlags([]string{"-no-auto-index"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.noAutoIndex {
		t.Error("-no-auto-index not parsed")
	}
	f, err = parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.noAutoIndex {
		t.Error("noAutoIndex must default to false")
	}
}

func TestParseFlags_NoAutoIndexNoConflicts(t *testing.T) {
	for _, args := range [][]string{
		{"-no-auto-index", "-rag-db", "x.db"},
		{"-no-auto-index", "-no-rag"},
	} {
		f, err := parseFlags(args)
		if err != nil {
			t.Fatalf("parseFlags(%v): %v", args, err)
		}
		if err := validateFlags(f); err != nil {
			t.Errorf("validateFlags(%v): %v", args, err)
		}
	}
}

func TestShouldStartAutoIndex(t *testing.T) {
	tests := []struct {
		name string
		f    flags
		want bool
	}{
		{name: "default", f: flags{}, want: true},
		{name: "one-shot -p", f: flags{prompt: "hi", promptSet: true}, want: false},
		{name: "-no-auto-index", f: flags{noAutoIndex: true}, want: false},
		{name: "-no-rag", f: flags{noRag: true}, want: false},
		{name: "-rag-db", f: flags{ragDB: "x.db"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldStartAutoIndex(tt.f); got != tt.want {
				t.Errorf("shouldStartAutoIndex = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseFlags_PromptSkipsAutoIndex(t *testing.T) {
	f, err := parseFlags([]string{"-p", "hello"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if err := validateFlags(f); err != nil {
		t.Fatalf("validateFlags: %v", err)
	}
	if shouldStartAutoIndex(f) {
		t.Error("one-shot -p must not start the background auto-index")
	}
}
