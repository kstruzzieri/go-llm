package interceptor

import (
	"regexp"
	"slices"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// harnessPaths lists the files and folders whose contents change how an agent
// behaves or what it runs, so a write to one needs a human (#725). Entries are
// lower-case in normalized form and matched as whole path components at any
// depth; an entry holding a slash is a multi-component path. .clinerules may
// be a file or a folder; both spellings match.
//
//   - Instruction files that other agents load on start (AGENTS.md, CLAUDE.md,
//     GEMINI.md and their local and override variants, the legacy rules files)
//     plus the MCP and OpenCode configs that name commands to launch.
//   - Folders holding other agents' settings, hooks, rules, commands and
//     skills; a hook or plugin there runs code.
//   - Golem's own files: .golem.json carries the post-write verify argv, and
//     models.json is discovered from the working directory and its base_url
//     decides where every prompt is sent. .agent holds proof state.
//   - GitHub Copilot's instruction, prompt, agent, hook and settings paths,
//     and the VS Code files that configure MCP servers and tool auto-approval.
var harnessPaths = []string{
	"agents.md", "agents.override.md", "claude.md", "claude.local.md", "gemini.md",
	".cursorrules", ".windsurfrules", ".clinerules", ".mcp.json",
	"opencode.json", "opencode.jsonc", ".golem.json", "models.json",
	".claude", ".cursor", ".windsurf", ".gemini", ".codex", ".opencode", ".agent",
	".github/copilot-instructions.md", ".github/instructions", ".github/prompts",
	".github/agents", ".github/hooks", ".github/copilot",
	".vscode/mcp.json", ".vscode/settings.json",
}

var (
	harnessPattern = componentPattern(harnessPaths)
	// shortNamePattern matches the 8.3 generated shape of one component:
	// a dotless name ending in ~digits, then optionally a 1-3 character
	// extension. It skips Emacs backups (x.~1~) and notes~draft.md.
	shortNamePattern = regexp.MustCompile(`^[^.]*~[0-9]+(\.[^.]{1,3})?$`)
)

// HarnessPaths returns a fresh copy of the normalized harness entries.
func HarnessPaths() []string { return slices.Clone(harnessPaths) }

// IsHarnessPath reports whether p names an agent instruction or harness
// configuration file (#725). p is a workspace- or repository-relative path; an
// absolute path also matches through harness-named or 8.3-shaped ancestors.
// It matches like IsProtectedPath, plus the Windows component fold on every
// platform: FAT and exFAT strip trailing dots on Linux and macOS too, and
// spaces are folded as well because, for a prompt-only matcher, a wider fold
// can only add prompts. The fold lives here rather than in normalizePath because
// that also feeds IsCredentialPath's template exception, where folding would
// turn a deny into an allow. A component shaped like an 8.3 short name also
// matches, since NTFS can reach .claude as CLAUDE~1.
func IsHarnessPath(p string) bool {
	n := HarnessKey(p)
	return harnessPattern.MatchString(n) || hasShortNameComponent(n)
}

// HarnessKey is the one fold every harness path and reference is compared
// through (#784): Unicode NFC, because APFS treats NFC and NFD spellings as one
// name, then normalizePath, then windowsComponent on every component, rejoined
// with "/". IsHarnessPath matches against it, so a write target and a
// reference that name the same file on some filesystem share one key.
func HarnessKey(p string) string {
	parts := strings.Split(normalizePath(norm.NFC.String(p)), "/")
	for i, c := range parts {
		parts[i] = windowsComponent(c)
	}
	return strings.Join(parts, "/")
}

// hasShortNameComponent reports whether any component of the normalized path
// n has the 8.3 generated short-name shape.
func hasShortNameComponent(n string) bool {
	return slices.ContainsFunc(strings.Split(n, "/"), shortNamePattern.MatchString)
}
