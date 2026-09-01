package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errScratchRootReplaced = errors.New("tools: scratch root identity replaced")

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

	cleanupDone chan struct{}
	cleanupOnce sync.Once
}

// beginScratchSession admits, snapshots, and rewrites one approved spec.
// Any failure gives cleanup a fresh bounded phase, releases the admission
// slot, and returns an error: the command must not run — scratch never
// silently degrades to direct host execution.
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
	s := &scratchSession{id: id, rt: rt, cleanupDone: make(chan struct{})}
	fail := func(err error) (*scratchSession, execSpec, error) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), rt.cfg.CaptureTimeout)
		defer cancel()
		cleanupErrs := s.startCleanup(cleanupCtx)
		rt.store.dropPending(id)
		cleanupErr := errors.Join(cleanupErrs...)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("scratch cleanup deferred; admission remains owned until reaped: %w", cleanupErr))
		}
		return nil, execSpec{}, err
	}
	rt.store.beginPending(id)

	if s.refParent, err = os.MkdirTemp(rt.tempBase, "go-llm-scratch-ref-*"); err != nil {
		return fail(fmt.Errorf("tools: create scratch reference root: %w", err))
	}
	if err = os.Chmod(s.refParent, 0o700); err != nil {
		return fail(fmt.Errorf("tools: secure scratch reference root: %w", err))
	}
	if s.refInfo, err = os.Lstat(s.refParent); err != nil {
		return fail(err)
	}
	if s.execParent, err = os.MkdirTemp(rt.tempBase, "go-llm-scratch-exec-*"); err != nil {
		return fail(fmt.Errorf("tools: create scratch execution root: %w", err))
	}
	if err = os.Chmod(s.execParent, 0o700); err != nil {
		return fail(fmt.Errorf("tools: secure scratch execution root: %w", err))
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
	if err = os.Chmod(filepath.Join(s.execParent, "tmp"), 0o700); err != nil {
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
// The admission slot is released exactly once only after both roots are gone;
// a failed cleanup quarantines the slot so live-process orphan accumulation is
// bounded by MaxConcurrentSessions.
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
		out = scratchOutcome{truncated: true, captureErr: sanitizeScratchText(err.Error())}
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), s.rt.cfg.CaptureTimeout)
	defer cancelCleanup()
	cleanupErrors := s.startCleanup(cleanupCtx)
	cleanupErrs := make([]string, 0, len(cleanupErrors))
	for _, cleanupErr := range cleanupErrors {
		cleanupErrs = append(cleanupErrs, cleanupErr.Error())
	}
	if len(cleanupErrs) > 0 {
		out.cleanupErr = sanitizeScratchText(strings.Join(cleanupErrs, "; "))
	}
	s.rt.store.completePending(s.id, out)
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

	cleanupCtx, cancel := context.WithTimeout(context.Background(), s.rt.cfg.CaptureTimeout)
	defer cancel()
	s.startCleanup(cleanupCtx)
	s.rt.store.delete(s.id)
}

// startCleanup spends the approved bounded cleanup phase. If an owned root
// remains, a deferred reaper retains the admission slot and gets one fixed
// grace window. Background Wait joins that bounded window; a persistent
// filesystem failure quarantines the slot instead of hanging Shutdown.
func (s *scratchSession) startCleanup(ctx context.Context) []error {
	errs, retry := s.cleanupAttempt(ctx)
	if retry {
		go s.reapCleanup()
	} else {
		s.completeCleanup(true)
	}
	return errs
}

func (s *scratchSession) cleanupAttempt(ctx context.Context) ([]error, bool) {
	var errs []error
	retry := false
	for _, target := range []struct {
		path string
		info os.FileInfo
	}{{s.refParent, s.refInfo}, {s.execParent, s.execInfo}} {
		if err := guardedRemoveAll(ctx, target.path, target.info); err != nil {
			errs = append(errs, err)
			// A replaced pathname no longer names the root we own. It must be
			// abandoned, not reaped; every other failure retains ownership.
			if !errors.Is(err, errScratchRootReplaced) {
				retry = true
			}
		}
	}
	return errs, retry
}

func (s *scratchSession) reapCleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), scratchEffectGrace)
	defer cancel()
	if s.rt.beforeReap != nil {
		s.rt.beforeReap()
	}
	for {
		_, retry := s.cleanupAttempt(ctx)
		if !retry {
			s.completeCleanup(true)
			return
		}
		select {
		case <-ctx.Done():
			// Persistent filesystem failures are quarantined: keep the slot,
			// but never hang command completion or manager Shutdown forever.
			s.completeCleanup(false)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *scratchSession) completeCleanup(releaseSlot bool) {
	s.cleanupOnce.Do(func() {
		if releaseSlot {
			<-s.rt.slots
		}
		close(s.cleanupDone)
	})
}

// guardedRemoveAll removes path only while it is still the directory this
// session created (seatbeltTempCleanup twin): a vanished path is fine, a
// replaced path is abandoned rather than deleted.
func guardedRemoveAll(ctx context.Context, path string, created os.FileInfo) error {
	if path == "" {
		return nil
	}
	if created == nil {
		var err error
		created, err = os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tools: inspect scratch root %q: %w", path, err)
		}
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tools: inspect scratch root %q: %w", path, err)
	}
	if !os.SameFile(created, fi) {
		return fmt.Errorf("%w: %q; abandoning cleanup", errScratchRootReplaced, path)
	}
	if err := forcedRemoveAllContext(ctx, path, created); err != nil {
		return fmt.Errorf("tools: remove scratch root %q: %w", path, err)
	}
	return nil
}

// forcedRemoveAllContext removes a session-private tree without following
// symlinks outside it, checking ctx between bounded directory batches. A
// deadline hands the still-owned partial root to the deferred reaper.
func forcedRemoveAllContext(ctx context.Context, path string, created os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(created, opened) {
		_ = root.Close()
		if err != nil {
			return err
		}
		return errScratchRootReplaced
	}
	removeErr := removeScratchContents(ctx, root)
	closeErr := root.Close()
	if removeErr != nil {
		return removeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(opened, current) {
		return errScratchRootReplaced
	}
	return os.Remove(path)
}

func removeScratchContents(ctx context.Context, root *os.Root) error {
	_ = root.Chmod(".", 0o700)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		names, readErr := dir.Readdirnames(128)
		closeErr := dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(names) == 0 {
			return nil
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := root.Remove(name); err == nil || errors.Is(err, fs.ErrNotExist) {
				continue
			}
			child, err := root.OpenRoot(name)
			if err != nil {
				_ = root.Chmod(name, 0o700)
				child, err = root.OpenRoot(name)
			}
			if err != nil {
				return err
			}
			childErr := removeScratchContents(ctx, child)
			closeErr := child.Close()
			if childErr != nil {
				return childErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
}

// forcedRemoveAll removes a session-private tree even when the snapshot
// faithfully cloned read-only directories (os.RemoveAll cannot unlink their
// children). The tree belongs to this session alone, so forcing directories
// writable is safe; without it every run against a workspace containing a
// 0555 directory would leak both scratch roots — with a workspace copy
// inside — to the temp base.
func forcedRemoveAll(path string) error {
	err := os.RemoveAll(path)
	if err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil // keep walking what we can
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// scratchProcess decorates a background process so Wait owns the session's
// capture and cleanup (the seatbeltProcess pattern): it runs before reap
// publishes exit state and before job.done closes. A bounded cleanup failure
// is reported in scratch_changes; Wait joins the bounded deferred reaper. A
// persistent failure leaves its slot quarantined rather than hanging manager
// Shutdown or enabling repeated accumulation. Capture uses a fresh
// Background-derived context — the tool call's context is long dead by the
// time a job exits — and never rewrites the exit code, managerKilled, or
// wait-error classification.
type scratchProcess struct {
	backgroundProcess
	session *scratchSession
}

func (p *scratchProcess) Wait() (int, bool, error) {
	code, managerKilled, err := p.backgroundProcess.Wait()
	captureCtx, cancel := context.WithTimeout(context.Background(), p.session.rt.cfg.CaptureTimeout)
	p.session.finish(captureCtx)
	cancel()
	<-p.session.cleanupDone
	return code, managerKilled, err
}
