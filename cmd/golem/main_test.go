package main

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

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
