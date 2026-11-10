package interceptor

import (
	"runtime"
	"slices"
	"testing"
)

// TestIsHarnessPath uses literal rows on purpose: a deleted harness entry
// must fail its own row, so the table is never built from HarnessPaths().
func TestIsHarnessPath(t *testing.T) {
	for _, tc := range harnessPathCases() {
		t.Run(tc.in, func(t *testing.T) {
			// containment: PATH-HARNESS
			if got := IsHarnessPath(tc.in); got != tc.want {
				t.Errorf("IsHarnessPath(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

type harnessPathCase struct {
	in   string
	want bool
}

func harnessPathCases() []harnessPathCase {
	cases := []harnessPathCase{
		{"AGENTS.md", true},
		{"AGENTS.override.md", true},
		{"CLAUDE.md", true},
		{"CLAUDE.local.md", true},
		{"GEMINI.md", true},
		{".cursorrules", true},
		{".windsurfrules", true},
		{".clinerules", true},
		{".mcp.json", true},
		{"opencode.json", true},
		{"opencode.jsonc", true},
		{".golem.json", true},
		{"models.json", true},
		{".claude/settings.json", true},
		{".cursor/rules/x.mdc", true},
		{".windsurf/rules/x.md", true},
		{".gemini/settings.json", true},
		{".codex/config.toml", true},
		{".opencode/plugins/x.js", true},
		{".agent/plan-lock.json", true},
		{".github/copilot-instructions.md", true},
		{".github/instructions/x.md", true},
		{".github/prompts/x.md", true},
		{".github/agents/x.md", true},
		{".github/hooks/x.json", true},
		{".github/copilot/settings.json", true},
		{".vscode/mcp.json", true},
		{".vscode/settings.json", true},
		{".claude", true},
		{".cursor", true},
		{".windsurf", true},
		{".gemini", true},
		{".codex", true},
		{".opencode", true},
		{".agent", true},
		{".clinerules/rules.md", true},
		{".github/hooks", true},
		{".github/copilot", true},
		{".github/instructions", true},
		{".github/prompts", true},
		{".github/agents", true},
		{"sub/AGENTS.md", true},
		{"sub/AGENTS.override.md", true},
		{"sub/CLAUDE.md", true},
		{"sub/CLAUDE.local.md", true},
		{"sub/GEMINI.md", true},
		{"sub/.cursorrules", true},
		{"sub/.windsurfrules", true},
		{"sub/.clinerules", true},
		{"sub/.mcp.json", true},
		{"sub/opencode.json", true},
		{"sub/opencode.jsonc", true},
		{"sub/.golem.json", true},
		{"sub/models.json", true},
		{"sub/.claude/settings.json", true},
		{"sub/.cursor/rules/x.mdc", true},
		{"sub/.windsurf/rules/x.md", true},
		{"sub/.gemini/settings.json", true},
		{"sub/.codex/config.toml", true},
		{"sub/.opencode/plugins/x.js", true},
		{"sub/.agent/plan-lock.json", true},
		{"sub/.github/copilot-instructions.md", true},
		{"sub/.github/instructions/x.md", true},
		{"sub/.github/prompts/x.md", true},
		{"sub/.github/agents/x.md", true},
		{"sub/.github/hooks/x.json", true},
		{"sub/.github/copilot/settings.json", true},
		{"sub/.vscode/mcp.json", true},
		{"sub/.vscode/settings.json", true},
		{"a/b/.claude/settings.json", true},
		{"x/.github/copilot/settings.json", true},
		{"./CLAUDE.md", true},
		{"docs/../CLAUDE.md", true},
		{"CLAUDE.MD", true},
		{".Claude/Settings.json", true},
		{"AGENT\u017F.md", true},
		{"CLAU\u200DDE.md", true},
		{"CLAUDE~1/settings.json", true},
		{".github/COPILO~1.MD", true},
		{"CLAUDE.md.", true},
		{"CLAUDE.md ", true},
		{"CLAUDE.md:x", true},
		{".claude./x", true},
		{"AGENTS.md.bak", false},
		{"docs/claude.md.txt", false},
		{"xAGENTS.md", false},
		{".claude-old/x", false},
		{".github/hooks-old/x", false},
		{".github/workflows/ci.yml", false},
		{".cursorrules.bak", false},
		{"x.~1~", false},
		{"notes~draft.md", false},
		{"README.md", false},
		{"src/main.go", false},
		{".github/agentsx/y", false},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, harnessPathCase{`foo\CLAUDE.md`, false})
	}
	return cases
}

func TestHarnessKey(t *testing.T) {
	equal := [][2]string{
		{"caf\u00e9/policy.md", "cafe\u0301/policy.md"}, // NFC vs NFD
		{"Docs/Policy.md", "docs/policy.md"},
		{"CLAUDE.md.", "claude.md"},
		{"a/./b/../c.md", "a/c.md"},
		{"Key.md", "key.md"},
	}
	for _, p := range equal {
		if a, b := HarnessKey(p[0]), HarnessKey(p[1]); a != b {
			t.Errorf("HarnessKey(%q) = %q, HarnessKey(%q) = %q, want equal", p[0], a, p[1], b)
		}
	}
	if HarnessKey("docs/a.md") == HarnessKey("docs/b.md") {
		t.Error("HarnessKey(docs/a.md) == HarnessKey(docs/b.md), want distinct")
	}
}

func TestHarnessKeyIsHarnessPathAgree(t *testing.T) {
	for _, tc := range harnessPathCases() {
		if tc.want && !IsHarnessPath(HarnessKey(tc.in)) {
			t.Errorf("IsHarnessPath(HarnessKey(%q)) = false, want true", tc.in)
		}
	}
}

func TestHasShortNameComponent(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"claude~1", true},
		{"copilo~1.md", true},
		{"a/claude~12/b", true},
		{"x.~1~", false},
		{"notes~draft.md", false},
		{".ab~1", false},
		{"foo~", false},
		{"foo~1.long", false},
	} {
		// containment: PATH-HARNESS
		if got := hasShortNameComponent(tc.in); got != tc.want {
			t.Errorf("hasShortNameComponent(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestHarnessPathsIsACopy(t *testing.T) {
	first := HarnessPaths()
	if len(first) != 28 {
		t.Fatalf("len(HarnessPaths()) = %d, want 28", len(first))
	}
	want := slices.Clone(first)
	first[0] = "mutated"
	if got := HarnessPaths(); !slices.Equal(got, want) {
		t.Errorf("HarnessPaths() changed after mutating a returned slice: %v", got)
	}
}
