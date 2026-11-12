package tools

import (
	"context"
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
// p, and why it is "" when #784 decides it. The key is "" (ungrantable,
// always prompt), with no reason, when the requested or the resolved spelling,
// below the repository prefix, is an agent instruction or harness file, or
// when the spelling cannot be verified (#725). Otherwise it is "" when a
// harness file loads, runs or links to either spelling, or when what they
// reference could not be checked, with the reason; the reference set is
// built afresh on every call (#784, spec D1). Anything else plans
// WriteClassApprovalKey.
func (w *Workspace) fileToolApprovalKey(ctx context.Context, p string) (key, reason string) {
	if w.harnessPrefixUnverified || interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, p)) {
		return "", ""
	}
	resolve := w.resolvedWriteRel
	if w.resolveForKey != nil {
		resolve = w.resolveForKey
	}
	rel, verified, err := resolve(p)
	if err != nil || !verified || interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, rel)) {
		return "", "" // unknown or unverified spelling: prompt (#725)
	}
	refs := w.harnessReferences(ctx)
	if !refs.complete {
		return "", "could not check what harness files reference: " + refs.cause
	}
	for _, s := range []string{p, rel} {
		if ok, why := refs.covers(s); ok {
			return "", why
		}
	}
	return WriteClassApprovalKey, ""
}
