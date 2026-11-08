package interceptor

import (
	"runtime"
	"slices"
	"testing"
)

// TestIsHarnessPath uses literal rows on purpose: a deleted harness entry
// must fail its own row, so the table is never built from HarnessPaths().
func TestIsHarnessPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
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
		{".clinerules", true},
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
		cases = append(cases, struct {
			in   string
			want bool
		}{`foo\CLAUDE.md`, false})
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := IsHarnessPath(tc.in); got != tc.want {
				t.Errorf("IsHarnessPath(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
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
