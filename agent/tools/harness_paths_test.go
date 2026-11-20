package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessReadMissingPathSuffix(t *testing.T) {
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(prefix, "missing", "leaf")
	for _, r := range []struct{ name, path string }{
		{"native separators", want},
		{"forward slashes", filepath.ToSlash(prefix) + "/missing/leaf"},
	} {
		t.Run(r.name, func(t *testing.T) {
			if got := resolveExisting(r.path); got != want {
				t.Errorf("resolveExisting(%q) = %q, want %q", r.path, got, want)
			}
			if got := harnessPathCandidates(prefix, r.path); len(got) != 1 || got[0] != want {
				t.Errorf("harnessPathCandidates(%q) = %q, want [%q]", r.path, got, want)
			}
		})
	}
}

func TestHarnessRefsSymlinkParentPaths(t *testing.T) {
	rows := []struct{ name, file, body, target string }{
		{"command argument", ".mcp.json", `{"mcpServers":{"s":{"command":"sh","args":["./link/../script.sh"]}}}`, "other/script.sh"},
		{"absolute command argument", ".mcp.json", `{"command":"sh","args":["{root}/link/../script.sh"]}`, "other/script.sh"},
		{"project variable", ".mcp.json", `{"command":"sh","args":["${workspaceFolder}/link/../script.sh"]}`, "other/script.sh"},
		{"command cwd", ".mcp.json", `{"command":"sh","args":["script.sh"],"cwd":"./link/.."}`, "other/script.sh"},
		{"shell cd", ".claude/settings.json", `{"command":"cd -P link; cd -P ..; sh script.sh"}`, "other/script.sh"},
		{"TOML command", ".codex/config.toml", "command = 'sh'\nargs = ['./link/../script.sh']\n", "other/script.sh"},
		{"OpenCode instruction", "opencode.json", `{"instructions":["./link/../script.sh"]}`, "other/script.sh"},
		{"Markdown import", "CLAUDE.md", "@./link/../script.sh\n", "other/script.sh"},
		{"transitive import", "CLAUDE.md", "@./link/../import.md\n", "other/nested.txt"},
		{"missing command target", ".mcp.json", `{"command":"sh","args":["./link/../new.sh"]}`, "other/new.sh"},
		{"missing import target", "CLAUDE.md", "@./link/../new.sh\n", "other/new.sh"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			_, root, ws := harnessRefFixture(t, harnessRefRow{unix: true, files: map[string]string{
				r.file:                r.body,
				"other/deep/keep.txt": "keep\n",
				"other/script.sh":     "old\n",
				"other/import.md":     "@nested.txt\n",
				"other/nested.txt":    "old\n",
			}, links: map[string]string{"link": "other/deep"}})
			actual, err := os.ReadFile(root + "/link/../script.sh")
			if err != nil || string(actual) != "old\n" {
				t.Fatalf("actual resolution: %q, %v", actual, err)
			}
			for _, tool := range harnessPlanTools(t, ws, r.target) {
				if r.target == "other/new.sh" && tool.name == "edit_file" {
					continue // edit_file requires an existing target.
				}
				plan, err := tool.plan(context.Background(), tool.raw)
				if err != nil {
					t.Fatal(err)
				}
				// containment: PATH-HARNESS-REF
				if plan.ApprovalKey != "" {
					t.Errorf("%s Plan(%q).ApprovalKey = %q, want empty", tool.name, r.target, plan.ApprovalKey)
				}
			}
			if err := ws.CheckHarnessPromotion(context.Background(), []string{r.target}); err == nil {
				t.Errorf("CheckHarnessPromotion(%q) = nil, want reference refusal", r.target)
			}
			if key, why := ws.fileToolApprovalKey(context.Background(), "notes.txt"); key != WriteClassApprovalKey {
				t.Errorf("ordinary file key = %q (%s), want write class", key, why)
			}
		})
	}
}
