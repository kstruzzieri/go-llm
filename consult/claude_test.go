package consult

import (
	"slices"
	"strings"
	"testing"
)

// TestClaudeArgsAreFixedAndVersionPinned locks the Claude adapter's argv to
// the E2 fixed launch contract and guards the evidence-approved version set.
func TestClaudeArgsAreFixedAndVersionPinned(t *testing.T) {
	want := []string{"-p", "--safe-mode", "--tools", "", "--allowedTools", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence", "--disable-slash-commands", "--no-chrome", "--model", "opus", "--system-prompt", "Give advisory text only. Do not use tools or access files or networks.", "--setting-sources=", "--output-format", "stream-json", "--verbose"}
	if got := claudeArgs("opus"); !slices.Equal(got, want) {
		t.Fatalf("argv drift:\n%q\n%q", got, want)
	}
	for _, a := range claudeArgs("opus") {
		if strings.Contains(a, "fallback") || strings.Contains(a, "bare") || strings.Contains(a, "budget") {
			t.Fatalf("forbidden flag %q", a)
		}
	}
	if !claudeSupportedVersions["2.1.240"] || claudeSupportedVersions["2.1.241"] || len(claudeSupportedVersions) != 1 {
		t.Fatalf("version pin drift: %v", claudeSupportedVersions)
	}
}

// TestClaudeArgsFreshSlice ensures callers cannot corrupt future invocations
// by mutating a returned slice, and that the system prompt is a sane
// single-line constant.
func TestClaudeArgsFreshSlice(t *testing.T) {
	first := claudeArgs("opus")
	first[0] = "MUTATED"
	second := claudeArgs("opus")
	if second[0] != "-p" {
		t.Fatalf("claudeArgs shared underlying slice across calls: %q", second)
	}
	if claudeSystemPrompt == "" {
		t.Fatal("claudeSystemPrompt must not be empty")
	}
	if strings.Contains(claudeSystemPrompt, "\n") {
		t.Fatalf("claudeSystemPrompt must not contain a newline: %q", claudeSystemPrompt)
	}
}
