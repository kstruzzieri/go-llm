package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// CheckHarnessPromotion is the promotion classifier for Agentflow parallel
// workers (#784, spec D10, Open 2(b)). A worker runs in a worktree of HEAD and
// cannot see the canonical root's gitignored harness files, and promotion
// copies its files with plain writes that never call Plan, so the coordinator
// calls this on every workspace-relative path it is about to write or delete,
// immediately before applying any of them. It returns an error naming the
// first path, and why, that is an agent instruction or harness file (#725),
// whose spelling cannot be resolved or verified, or that a harness file loads,
// runs or links to; while what they reference could not be checked, every
// path is refused. Unlike the file tools' approval key, a path whose parent
// folders are missing is classified, not refused. The batch shares one fresh
// reference scan.
func (w *Workspace) CheckHarnessPromotion(ctx context.Context, paths []string) error {
	refs := w.harnessReferences(ctx)
	for _, p := range paths {
		if why := w.promotionRefusal(refs, p); why != "" {
			return fmt.Errorf("%s: %s", harnessQuote(p), why)
		}
	}
	return nil
}

// promotionRefusal is why promoting p is refused, or "": #725's checks in
// fileToolApprovalKey's order, then the reference set.
func (w *Workspace) promotionRefusal(refs *harnessRefs, p string) string {
	if w.harnessPrefixUnverified {
		return "the workspace root's spelling cannot be verified"
	}
	if interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, p)) {
		return "agent instruction or harness file"
	}
	rel, verified, err := w.futureWriteRel(p)
	switch {
	case err != nil:
		return "cannot resolve: " + err.Error()
	case !verified:
		return "its stored spelling cannot be verified"
	case interceptor.IsHarnessPath(filepath.Join(w.harnessPrefix, rel)):
		return "resolves to agent instruction or harness file " + harnessQuote(rel)
	case !refs.complete:
		return "could not check what harness files reference: " + refs.cause
	}
	for _, s := range []string{p, rel} {
		if ok, why := refs.covers(s); ok {
			return why
		}
	}
	return ""
}

// futureWriteRel returns the workspace-relative spelling the filesystem
// stores for p, appending the missing components to the deepest existing
// ancestor's stored spelling, and whether every existing component's spelling
// was verified. It is also the portable backend's resolvedWriteRel.
func (w *Workspace) futureWriteRel(p string) (string, bool, error) {
	abs, err := w.cleanRel(p)
	if err != nil {
		return "", false, err
	}
	canonical, verified, err := canonicalFuturePathVerified(w.root, abs)
	if err != nil {
		return "", false, err
	}
	if !w.underRoot(canonical) {
		return "", false, errEscape
	}
	rel, err := filepath.Rel(w.root, canonical)
	if err != nil {
		return "", false, err
	}
	return rel, verified, nil
}
