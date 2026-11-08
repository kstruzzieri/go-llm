package tools

import (
	"os"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// repositoryPrefix returns canonRoot's path from the nearest directory, the
// root included, that holds a .git entry of any type (a checkout's directory,
// a worktree's file, or a link), and that directory as boundary. The prefix
// starts with the boundary's own name, because a repository can itself be a
// harness folder (a git-tracked ~/.claude, or a .claude submodule); a worktree
// root adds only its own name. With no .git up to the volume root the prefix
// is canonRoot itself, so the whole root is classified (#725).
func repositoryPrefix(canonRoot string) (prefix, boundary string) {
	for d := canonRoot; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			rel, err := filepath.Rel(filepath.Dir(d), canonRoot)
			if err != nil {
				return canonRoot, d // unreachable: d is an ancestor; classify everything
			}
			return rel, d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return canonRoot, d
		}
		d = parent
	}
}

// fileToolApprovalKey is the ApprovalKey write_file and edit_file plan for
// p: "" (ungrantable, always prompt) when the requested or the resolved
// spelling, below the repository prefix, is an agent instruction or harness
// file, or when the spelling cannot be verified; WriteClassApprovalKey
// otherwise (#725).
func (w *Workspace) fileToolApprovalKey(p string) string {
	if w.harnessPrefixUnverified || interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, p)) {
		return ""
	}
	resolve := w.resolvedWriteRel
	if w.resolveForKey != nil {
		resolve = w.resolveForKey
	}
	rel, verified, err := resolve(p)
	if err != nil || !verified || interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, rel)) {
		return "" // unknown or unverified spelling: prompt (#725)
	}
	return WriteClassApprovalKey
}
