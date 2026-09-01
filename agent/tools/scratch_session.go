package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// scratchSession is one per-invocation scratch lifecycle: two guarded temp
// roots (a pristine reference and an execution root holding workspace/ and
// tmp/), the canonical manifest, and exactly one terminal transition —
// finish (capture then remove) or discard (remove without publishing).
type scratchSession struct {
	id       string
	rt       *scratchRuntime
	manifest snapshotManifest

	reference  string // <refParent>/tree — pristine diff base
	work       string // <execParent>/workspace — the command's root
	refParent  string
	execParent string
	refInfo    os.FileInfo
	execInfo   os.FileInfo

	mu    sync.Mutex
	state int // 0 open, 1 finished, 2 discarded
}

// beginScratchSession admits, snapshots, and rewrites one approved spec.
// Any failure removes every partial root, releases the admission slot, and
// returns an error: the command must not run — scratch never silently
// degrades to direct host execution.
func beginScratchSession(ctx context.Context, rt *scratchRuntime, spec execSpec) (*scratchSession, execSpec, error) {
	if spec.WorkspaceRoot != rt.root {
		return nil, execSpec{}, fmt.Errorf("tools: scratch session for root %q does not match runtime root %q", spec.WorkspaceRoot, rt.root)
	}
	// Admission before any filesystem work: a rejected session creates
	// nothing and a queued wait would silently stack disk commitments.
	select {
	case rt.slots <- struct{}{}:
	default:
		return nil, execSpec{}, fmt.Errorf("tools: all %d scratch sessions are in use; retry after one finishes", rt.cfg.MaxConcurrentSessions)
	}
	release := func() { <-rt.slots }

	id, err := rt.store.newID()
	if err != nil {
		release()
		return nil, execSpec{}, err
	}
	s := &scratchSession{id: id, rt: rt}
	fail := func(err error) (*scratchSession, execSpec, error) {
		if s.refParent != "" {
			_ = os.RemoveAll(s.refParent)
		}
		if s.execParent != "" {
			_ = os.RemoveAll(s.execParent)
		}
		rt.store.dropPending(id)
		release()
		return nil, execSpec{}, err
	}
	rt.store.beginPending(id)

	if s.refParent, err = os.MkdirTemp(rt.tempBase, "go-llm-scratch-ref-*"); err != nil {
		return fail(fmt.Errorf("tools: create scratch reference root: %w", err))
	}
	if s.refInfo, err = os.Lstat(s.refParent); err != nil {
		return fail(err)
	}
	if s.execParent, err = os.MkdirTemp(rt.tempBase, "go-llm-scratch-exec-*"); err != nil {
		return fail(fmt.Errorf("tools: create scratch execution root: %w", err))
	}
	if s.execInfo, err = os.Lstat(s.execParent); err != nil {
		return fail(err)
	}

	s.reference = filepath.Join(s.refParent, "tree")
	if s.manifest, err = snapshotCanonical(ctx, rt.root, s.reference, rt.cfg, rt.clone); err != nil {
		return fail(fmt.Errorf("tools: snapshot canonical workspace: %w", err))
	}
	s.work = filepath.Join(s.execParent, "workspace")
	if _, err = snapshotCanonical(ctx, s.reference, s.work, rt.cfg, rt.clone); err != nil {
		return fail(fmt.Errorf("tools: clone scratch workspace: %w", err))
	}
	if err = os.Mkdir(filepath.Join(s.execParent, "tmp"), 0o700); err != nil {
		return fail(err)
	}

	rewritten, err := rewriteScratchSpec(spec, rt.root, s.work, filepath.Join(s.execParent, "tmp"))
	if err != nil {
		return fail(err)
	}
	return s, rewritten, nil
}

// rewriteScratchSpec remaps the approved spec onto the scratch execution
// root: workspace root, cwd (same workspace-relative path), a
// workspace-local executable, and TMPDIR. Argv and every other environment
// value stay exactly as approved.
func rewriteScratchSpec(spec execSpec, canonicalRoot, work, tmp string) (execSpec, error) {
	out := spec
	out.WorkspaceRoot = work
	rel, err := filepath.Rel(canonicalRoot, spec.Dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return execSpec{}, fmt.Errorf("tools: scratch cwd %q escapes workspace root %q", spec.Dir, canonicalRoot)
	}
	out.Dir = filepath.Join(work, rel)
	if prel, err := filepath.Rel(canonicalRoot, spec.Path); err == nil &&
		prel != ".." && !strings.HasPrefix(prel, ".."+string(filepath.Separator)) && !filepath.IsAbs(prel) {
		out.Path = filepath.Join(work, prel)
	}
	env := make([]string, 0, len(spec.Env)+1)
	replaced := false
	for _, kv := range spec.Env {
		if strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, "TMPDIR="+tmp)
			replaced = true
			continue
		}
		env = append(env, kv)
	}
	if !replaced {
		env = append(env, "TMPDIR="+tmp)
	}
	out.Env = env
	return out, nil
}

// finish captures the outcome and removes both roots. Idempotent; a capture
// failure publishes a truncated outcome and never blocks cleanup; a
// replaced root is abandoned, never deleted, and the failure is queryable.
// The admission slot is released exactly once, after both cleanup attempts.
func (s *scratchSession) finish(ctx context.Context) {
	s.mu.Lock()
	if s.state != 0 {
		s.mu.Unlock()
		return
	}
	s.state = 1
	s.mu.Unlock()

	out, err := diffTrees(ctx, s.reference, s.work, s.manifest, s.rt.cfg)
	if err != nil {
		out = scratchOutcome{truncated: true, captureErr: err.Error()}
	}
	var cleanupErrs []string
	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{s.refParent, s.refInfo}, {s.execParent, s.execInfo}} {
		if err := guardedRemoveAll(target.path, target.info); err != nil {
			cleanupErrs = append(cleanupErrs, err.Error())
		}
	}
	if len(cleanupErrs) > 0 {
		out.cleanupErr = strings.Join(cleanupErrs, "; ")
	}
	s.rt.store.completePending(s.id, out)
	<-s.rt.slots
}

// discard tears the session down without publishing an outcome. Called
// after finish (an abandoned background start whose wrapper already
// captured), it deletes the now-unreachable outcome instead. Idempotent.
func (s *scratchSession) discard() {
	s.mu.Lock()
	switch s.state {
	case 1:
		s.state = 2
		s.mu.Unlock()
		s.rt.store.delete(s.id)
		return
	case 2:
		s.mu.Unlock()
		return
	}
	s.state = 2
	s.mu.Unlock()

	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{s.refParent, s.refInfo}, {s.execParent, s.execInfo}} {
		_ = guardedRemoveAll(target.path, target.info)
	}
	s.rt.store.delete(s.id)
	<-s.rt.slots
}

// guardedRemoveAll removes path only while it is still the directory this
// session created (seatbeltTempCleanup twin): a vanished path is fine, a
// replaced path is abandoned rather than deleted.
func guardedRemoveAll(path string, created os.FileInfo) error {
	if path == "" || created == nil {
		return nil
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tools: inspect scratch root %q: %w", path, err)
	}
	if !os.SameFile(created, fi) {
		return fmt.Errorf("tools: scratch root %q was replaced; abandoning cleanup", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("tools: remove scratch root %q: %w", path, err)
	}
	return nil
}
