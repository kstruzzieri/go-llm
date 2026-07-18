package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/agentflow"
	"golang.org/x/sync/errgroup"
)

type parallelWorkerFunc func(context.Context, parallelWorker) error

type parallelAggregateFunc func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error)

var errParallelIndexFlags = errors.New("parallel root has unexpected index flags")

type parallelWorker struct {
	step         agentflow.Step
	paths        []string
	root         string
	ownerID      string
	sourceID     string
	changedPaths []string
}

type parallelCoordinator struct {
	root       string
	plan       *agentflow.Plan
	maxWorkers int
	worker     parallelWorkerFunc
	aggregate  parallelAggregateFunc
	interrupts <-chan struct{}

	topLevel      string
	head          string
	tempParent    string
	lockFile      *os.File
	prepared      bool
	workers       []parallelWorker
	baseSnapshots map[string]parallelFileSnapshot
}

type synchronizedWriter struct {
	mu  sync.Mutex
	out io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

type parallelFileSnapshot struct {
	path   string
	exists bool
	data   []byte
	mode   fs.FileMode
	info   fs.FileInfo
}

type parallelPromotionChange struct {
	path   string
	data   []byte
	mode   fs.FileMode
	delete bool
}

type parallelPromotion struct {
	root        *os.Root
	applied     []parallelAppliedChange
	createdDirs []string
}

type parallelAppliedChange struct {
	before parallelFileSnapshot
	after  parallelFileSnapshot
}

func newParallelCoordinator(root string, plan *agentflow.Plan, maxWorkers int, worker parallelWorkerFunc) *parallelCoordinator {
	return &parallelCoordinator{root: root, plan: plan, maxWorkers: maxWorkers, worker: worker}
}

func newParallelAggregate(runnerForRoot func(string) agentflow.Runner) parallelAggregateFunc {
	return func(ctx context.Context, inputs []agentflow.AggregationInput, output, base string, dryRun bool) (agentflow.AggregationResult, error) {
		if runnerForRoot == nil {
			return agentflow.AggregationResult{}, errors.New("parallel Agentflow runner factory is nil")
		}
		runner := runnerForRoot(output)
		if runner == nil {
			return agentflow.AggregationResult{}, errors.New("parallel Agentflow runner factory returned nil")
		}
		return agentflow.NewClient(runner, output).AggregateLedgers(ctx, inputs, base, dryRun)
	}
}

func newAssignedParallelWorker(plan *agentflow.Plan, sess *replSession, approveEdits bool, out io.Writer, runnerForRoot func(string) agentflow.Runner) parallelWorkerFunc {
	return func(ctx context.Context, worker parallelWorker) error {
		if sess == nil || sess.newOrchestrator == nil {
			return errors.New("parallel orchestrator factory is nil")
		}
		orch := sess.newOrchestrator()
		if orch == nil {
			return errors.New("parallel orchestrator factory returned nil")
		}
		if runnerForRoot == nil {
			return errors.New("parallel Agentflow runner factory is nil")
		}
		runner := runnerForRoot(worker.root)
		if runner == nil {
			return errors.New("parallel Agentflow runner factory returned nil")
		}
		client := agentflow.NewOwnedClient(runner, worker.root, worker.ownerID)
		state, err := client.NextAction(ctx)
		if err != nil {
			return fmt.Errorf("validate worker resumability: %w", err)
		}
		if err := validateFreshWorkerProjection(state, worker.ownerID); err != nil {
			return err
		}

		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		runStep, err := newTaskStepRunner(worker.root, plan, client, orch, sess, approveEdits, out, nil, cancel)
		if err != nil {
			return err
		}
		d := &driver{af: client, plan: plan, runStep: runStep}
		return d.runOneStep(runCtx, worker.step.ID)
	}
}

func (c *parallelCoordinator) runCohort(ctx context.Context) (ran bool, err error) {
	waveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var interruptWatcher sync.WaitGroup
	defer func() {
		close(done)
		interruptWatcher.Wait()
	}()
	if c.interrupts != nil {
		select {
		case <-c.interrupts:
		default:
		}
		interruptWatcher.Add(1)
		go func() {
			defer interruptWatcher.Done()
			select {
			case <-c.interrupts:
				cancel()
			case <-done:
			}
		}()
	}

	selected, err := c.selectWorkers(waveCtx)
	if err != nil || !selected {
		return false, err
	}
	defer func() {
		if releaseErr := c.releaseWorkspaceLock(); releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	if err := c.prepareWorkers(waveCtx); err != nil {
		return true, fmt.Errorf("prepare parallel cohort: %w", err)
	}
	if err := c.runWorkers(waveCtx); err != nil {
		return true, fmt.Errorf("run parallel cohort: %w", err)
	}
	if err := c.recheckRoot(waveCtx); err != nil {
		return true, fmt.Errorf("recheck canonical root before promotion: %w", err)
	}
	promotion, err := c.promoteWorkers()
	if err != nil {
		return true, err
	}
	defer func() {
		if closeErr := promotion.close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close parallel promotion root: %w", closeErr))
		}
	}()
	if c.aggregate == nil {
		return true, parallelRollbackError(errors.New("parallel aggregate function is nil"), promotion.rollback)
	}
	inputs := make([]agentflow.AggregationInput, len(c.workers))
	for i, worker := range c.workers {
		inputs[i] = agentflow.AggregationInput{Root: worker.root, SourceID: worker.sourceID}
	}
	if _, err := c.aggregate(waveCtx, inputs, c.root, c.head, true); err != nil {
		return true, parallelRollbackError(fmt.Errorf("dry-run aggregate ledgers: %w", err), promotion.rollback)
	}
	if _, err := c.aggregate(waveCtx, inputs, c.root, c.head, false); err != nil {
		var collision *agentflow.AggregationCollisionError
		if errors.As(err, &collision) {
			return true, parallelRollbackError(fmt.Errorf("aggregate ledgers collision: %w", err), promotion.rollback)
		}
		return true, fmt.Errorf("aggregate ledgers real-write outcome is ambiguous; promoted source preserved: %w", err)
	}
	return true, nil
}

func (c *parallelCoordinator) promoteWorkers() (*parallelPromotion, error) {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return nil, fmt.Errorf("open canonical promotion root: %w", err)
	}
	promotion := &parallelPromotion{root: root}
	changes, snapshots, err := c.parallelPromotionChanges()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("snapshot parallel promotion: %w", err), promotion.close())
	}
	for i, change := range changes {
		if err := promotion.apply(change, snapshots[i]); err != nil {
			return nil, errors.Join(
				parallelRollbackError(fmt.Errorf("promote %s: %w", change.path, err), promotion.rollback),
				promotion.close(),
			)
		}
	}
	return promotion, nil
}

func (c *parallelCoordinator) parallelPromotionChanges() ([]parallelPromotionChange, []parallelFileSnapshot, error) {
	var changes []parallelPromotionChange
	var snapshots []parallelFileSnapshot
	for _, worker := range c.workers {
		workerRoot, err := os.OpenRoot(worker.root)
		if err != nil {
			return nil, nil, fmt.Errorf("open worker %s promotion root: %w", worker.sourceID, err)
		}
		assigned := make(map[string]struct{}, len(worker.paths))
		for _, file := range worker.paths {
			assigned[file] = struct{}{}
		}
		for _, file := range worker.changedPaths {
			if _, ok := assigned[file]; !ok {
				_ = workerRoot.Close()
				return nil, nil, fmt.Errorf("worker %s changed unvalidated path %q", worker.sourceID, file)
			}
			source, err := parallelSnapshot(workerRoot, file)
			if err != nil {
				_ = workerRoot.Close()
				return nil, nil, fmt.Errorf("inspect worker %s path %q: %w", worker.sourceID, file, err)
			}
			change := parallelPromotionChange{path: file, data: source.data, mode: source.mode, delete: !source.exists}
			snapshot, ok := c.baseSnapshots[file]
			if !ok {
				_ = workerRoot.Close()
				return nil, nil, fmt.Errorf("canonical path %q has no pre-worker snapshot", file)
			}
			if !source.exists && !snapshot.exists {
				_ = workerRoot.Close()
				return nil, nil, fmt.Errorf("worker %s deletion target %q does not exist", worker.sourceID, file)
			}
			changes = append(changes, change)
			snapshots = append(snapshots, snapshot)
		}
		if err := workerRoot.Close(); err != nil {
			return nil, nil, fmt.Errorf("close worker %s promotion root: %w", worker.sourceID, err)
		}
	}
	return changes, snapshots, nil
}

func (p *parallelPromotion) apply(change parallelPromotionChange, snapshot parallelFileSnapshot) error {
	if err := parallelRequireSnapshot(p.root, snapshot); err != nil {
		return err
	}
	if change.delete {
		if err := p.root.Remove(filepath.FromSlash(change.path)); err != nil {
			return err
		}
		p.applied = append(p.applied, parallelAppliedChange{
			before: snapshot,
			after:  parallelFileSnapshot{path: change.path},
		})
		return nil
	}
	created, err := parallelEnsureParents(p.root, change.path)
	p.createdDirs = append(p.createdDirs, created...)
	if err != nil {
		return err
	}
	if err := parallelRequireSnapshot(p.root, snapshot); err != nil {
		return err
	}
	if err := parallelAtomicWrite(p.root, change.path, change.data, change.mode); err != nil {
		return err
	}
	p.applied = append(p.applied, parallelAppliedChange{
		before: snapshot,
		after:  parallelFileSnapshot{path: change.path, exists: true, data: change.data, mode: change.mode},
	})
	after, err := parallelSnapshot(p.root, change.path)
	if err != nil {
		return fmt.Errorf("verify promoted path: %w", err)
	}
	if after.mode != change.mode || !bytes.Equal(after.data, change.data) {
		return errors.New("promoted path does not match worker source")
	}
	p.applied[len(p.applied)-1].after = after
	return nil
}

func parallelAtomicWrite(root *os.Root, target string, data []byte, mode fs.FileMode) (err error) {
	tempName := path.Join(path.Dir(target), ".golem-promote-"+rand.Text())
	temp, err := root.OpenFile(filepath.FromSlash(tempName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if err != nil {
			err = parallelAtomicCleanup(err, root, temp, tempName, closed)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Chmod(mode); err != nil {
		return err
	}
	if closeErr := temp.Close(); closeErr != nil {
		closed = true
		return closeErr
	}
	closed = true
	return root.Rename(filepath.FromSlash(tempName), filepath.FromSlash(target))
}

func parallelAtomicCleanup(primary error, root *os.Root, temp *os.File, name string, closed bool) error {
	errs := []error{primary}
	if !closed {
		if err := temp.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close promotion temp %s: %w", name, err))
		}
	}
	if err := root.Remove(filepath.FromSlash(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove promotion temp %s: %w", name, err))
	}
	return errors.Join(errs...)
}

func (p *parallelPromotion) rollback() error {
	var errs []error
	for i := len(p.applied) - 1; i >= 0; i-- {
		applied := p.applied[i]
		if err := parallelRequireSnapshot(p.root, applied.after); err != nil {
			errs = append(errs, fmt.Errorf("refuse to restore changed path %s: %w", applied.after.path, err))
			continue
		}
		if !applied.before.exists {
			if err := p.root.Remove(filepath.FromSlash(applied.after.path)); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", applied.after.path, err))
			}
			continue
		}
		if err := parallelRequireParents(p.root, applied.before.path); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", applied.before.path, err))
			continue
		}
		if err := parallelAtomicWrite(p.root, applied.before.path, applied.before.data, applied.before.mode); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", applied.before.path, err))
		}
	}
	for i := len(p.createdDirs) - 1; i >= 0; i-- {
		if err := p.root.Remove(filepath.FromSlash(p.createdDirs[i])); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove created parent %s: %w", p.createdDirs[i], err))
		}
	}
	return errors.Join(errs...)
}

func (p *parallelPromotion) close() error {
	if p == nil || p.root == nil {
		return nil
	}
	err := p.root.Close()
	p.root = nil
	return err
}

func parallelSnapshot(root *os.Root, file string) (parallelFileSnapshot, error) {
	if _, err := parallelPromotionPath(root.Name(), file); err != nil {
		return parallelFileSnapshot{}, err
	}
	snapshot := parallelFileSnapshot{path: file}
	rootPath := filepath.FromSlash(file)
	info, err := root.Lstat(rootPath)
	if errors.Is(err, fs.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !info.Mode().IsRegular() {
		return snapshot, errors.New("path is not a regular file")
	}
	fileHandle, err := root.Open(rootPath)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = fileHandle.Close() }()
	openedInfo, err := fileHandle.Stat()
	if err != nil {
		return snapshot, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return snapshot, errors.New("path changed while opening snapshot")
	}
	snapshot.data, err = io.ReadAll(fileHandle)
	if err != nil {
		return snapshot, err
	}
	snapshot.exists = true
	snapshot.mode = parallelCopiedMode(openedInfo.Mode())
	snapshot.info = openedInfo
	return snapshot, nil
}

func parallelRequireSnapshot(root *os.Root, want parallelFileSnapshot) error {
	got, err := parallelSnapshot(root, want.path)
	if err != nil {
		return err
	}
	if got.exists != want.exists || got.mode != want.mode || !bytes.Equal(got.data, want.data) ||
		(want.exists && want.info != nil && !os.SameFile(want.info, got.info)) {
		return errors.New("canonical path changed after snapshot")
	}
	return nil
}

func parallelRollbackError(primary error, rollback func() error) error {
	if rollback == nil {
		return primary
	}
	if err := rollback(); err != nil {
		return errors.Join(primary, fmt.Errorf("rollback promoted source: %w", err))
	}
	return primary
}

func parallelPromotionPath(root, file string) (string, error) {
	if file == "" || file != strings.TrimSpace(file) || strings.ContainsAny(file, "\\*?[\x00") ||
		strings.HasSuffix(file, "/") || path.IsAbs(file) || path.Clean(file) != file || file == "." {
		return "", fmt.Errorf("unsafe promotion path %q", file)
	}
	first := strings.ToLower(strings.SplitN(file, "/", 2)[0])
	if first == ".agent" || first == ".git" {
		return "", fmt.Errorf("unsafe promotion path %q", file)
	}
	target := filepath.Join(root, filepath.FromSlash(file))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("promotion path %q escapes root", file)
	}
	return target, nil
}

func parallelEnsureParents(root *os.Root, file string) ([]string, error) {
	return parallelParents(root, file, true)
}

func parallelRequireParents(root *os.Root, file string) error {
	_, err := parallelParents(root, file, false)
	return err
}

func parallelParents(root *os.Root, file string, create bool) ([]string, error) {
	dir := path.Dir(file)
	if dir == "." {
		return nil, nil
	}
	var created []string
	current := ""
	for _, part := range strings.Split(dir, "/") {
		current = path.Join(current, part)
		info, err := root.Lstat(filepath.FromSlash(current))
		if errors.Is(err, fs.ErrNotExist) {
			if !create {
				return created, errors.New("promotion parent disappeared")
			}
			if err := root.Mkdir(filepath.FromSlash(current), 0o755); err != nil {
				return created, err
			}
			created = append(created, current)
			continue
		}
		if err != nil {
			return created, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return created, errors.New("promotion parent is not a real directory")
		}
	}
	return created, nil
}

func selectParallelSteps(plan *agentflow.Plan, maxWorkers int, eligible func(agentflow.Step, []string) bool) []agentflow.Step {
	if plan == nil || maxWorkers < 2 || !parallelGraphHasEdge(plan) {
		return nil
	}

	var selected []agentflow.Step
	var selectedPaths []string
	for _, step := range plan.Steps {
		if len(step.DependsOn) != 0 {
			continue
		}
		paths, ok := parallelLiteralPaths(plan, step)
		if !ok || parallelPathsOverlap(selectedPaths, paths) || !eligible(step, paths) {
			continue
		}
		selected = append(selected, step)
		selectedPaths = append(selectedPaths, paths...)
		if len(selected) == maxWorkers {
			break
		}
	}
	if len(selected) < 2 {
		return nil
	}
	return selected
}

func parallelGraphHasEdge(plan *agentflow.Plan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		if len(step.DependsOn) > 0 {
			return true
		}
	}
	return false
}

func parallelLiteralPaths(plan *agentflow.Plan, step agentflow.Step) ([]string, bool) {
	if len(step.Files) == 0 {
		return nil, false
	}
	allowed, blocked := agentflow.EffectiveScope(plan, step.ID)
	if len(allowed) != len(step.Files) {
		return nil, false
	}
	paths := make([]string, 0, len(step.Files))
	for _, file := range step.Files {
		if file == "" || file != strings.TrimSpace(file) || strings.ContainsAny(file, "\\*?[\x00") ||
			strings.HasSuffix(file, "/") || path.IsAbs(file) || path.Clean(file) != file || file == "." {
			return nil, false
		}
		first := strings.ToLower(strings.SplitN(file, "/", 2)[0])
		if first == ".agent" || first == ".git" || agentflow.MatchesPath(file, blocked) {
			return nil, false
		}
		if parallelPathsOverlap(paths, []string{file}) {
			return nil, false
		}
		paths = append(paths, file)
	}
	return paths, true
}

func parallelPathsOverlap(a, b []string) bool {
	for _, left := range a {
		for _, right := range b {
			if parallelPathPrefix(left, right) || parallelPathPrefix(right, left) {
				return true
			}
		}
	}
	return false
}

func parallelPathPrefix(pathname, prefix string) bool {
	pathParts := strings.Split(pathname, "/")
	prefixParts := strings.Split(prefix, "/")
	if len(prefixParts) > len(pathParts) {
		return false
	}
	for i := range prefixParts {
		if !strings.EqualFold(pathParts[i], prefixParts[i]) {
			return false
		}
	}
	return true
}

func (c *parallelCoordinator) selectWorkers(ctx context.Context) (bool, error) {
	c.prepared = false
	c.baseSnapshots = nil
	if c.maxWorkers < 2 || !parallelGraphHasEdge(c.plan) {
		c.workers = nil
		return false, nil
	}
	if err := c.recordBase(ctx); err != nil {
		if errors.Is(err, errParallelIndexFlags) {
			c.workers = nil
			return false, nil
		}
		return false, err
	}
	var eligibilityErr error
	selected := selectParallelSteps(c.plan, c.maxWorkers, func(step agentflow.Step, paths []string) bool {
		for _, file := range paths {
			eligible, err := c.gitEligiblePath(ctx, file)
			if err != nil {
				eligibilityErr = err
				return false
			}
			if !eligible {
				return false
			}
		}
		return true
	})
	if eligibilityErr != nil {
		return false, eligibilityErr
	}
	c.workers = nil
	for i, step := range selected {
		paths, _ := parallelLiteralPaths(c.plan, step)
		id := strconv.Itoa(i + 1)
		c.workers = append(c.workers, parallelWorker{
			step: step, paths: paths, ownerID: "golem-w" + id, sourceID: "w" + id,
		})
	}
	if len(c.workers) < 2 {
		return false, nil
	}
	if err := c.acquireWorkspaceLock(ctx); err != nil {
		return false, err
	}
	if err := c.recheckRoot(ctx); err != nil {
		return false, errors.Join(err, c.releaseWorkspaceLock())
	}
	if err := c.captureBaseSnapshots(); err != nil {
		return false, errors.Join(err, c.releaseWorkspaceLock())
	}
	if err := c.recheckRoot(ctx); err != nil {
		return false, errors.Join(err, c.releaseWorkspaceLock())
	}
	return true, nil
}

func (c *parallelCoordinator) captureBaseSnapshots() (err error) {
	root, err := os.OpenRoot(c.root)
	if err != nil {
		return fmt.Errorf("open canonical snapshot root: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	snapshots := make(map[string]parallelFileSnapshot)
	for _, worker := range c.workers {
		for _, file := range worker.paths {
			snapshot, err := parallelSnapshot(root, file)
			if err != nil {
				return fmt.Errorf("snapshot canonical path %q: %w", file, err)
			}
			snapshots[file] = snapshot
		}
	}
	c.baseSnapshots = snapshots
	return nil
}

func (c *parallelCoordinator) prepareWorkers(ctx context.Context) error {
	c.prepared = false
	if len(c.workers) < 2 {
		return errors.New("parallel cohort has fewer than two selected workers")
	}
	if err := c.recheckRoot(ctx); err != nil {
		return err
	}
	parent, err := os.MkdirTemp("", "golem-agentflow-parallel-")
	if err != nil {
		return fmt.Errorf("create parallel worktree parent: %w", err)
	}
	c.tempParent = parent
	for i := range c.workers {
		workerRoot := filepath.Join(parent, c.workers[i].sourceID)
		c.workers[i].root = workerRoot
		if _, err := runParallelGit(ctx, c.root, "worktree", "add", "--detach", workerRoot, c.head); err != nil {
			return fmt.Errorf("create worktree %s: %w", c.workers[i].sourceID, err)
		}
		if err := copyParallelAgentTree(filepath.Join(c.root, ".agent"), filepath.Join(workerRoot, ".agent")); err != nil {
			return fmt.Errorf("copy .agent to %s: %w", c.workers[i].sourceID, err)
		}
	}
	c.prepared = true
	return nil
}

func (c *parallelCoordinator) runWorkers(ctx context.Context) error {
	if !c.prepared || len(c.workers) < 2 {
		return errors.New("parallel cohort does not have at least two successfully prepared workers")
	}
	for _, worker := range c.workers {
		if worker.root == "" {
			return fmt.Errorf("worker %s has no prepared worktree root", worker.sourceID)
		}
		info, err := os.Stat(worker.root)
		if err != nil {
			return fmt.Errorf("worker %s prepared worktree root is unavailable: %w", worker.sourceID, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("worker %s prepared worktree root is not a directory", worker.sourceID)
		}
	}
	if c.worker == nil {
		return errors.New("parallel worker function is nil")
	}
	g, waveCtx := errgroup.WithContext(ctx)
	for i := range c.workers {
		worker := c.workers[i]
		g.Go(func() error {
			if err := c.worker(waveCtx, worker); err != nil {
				return fmt.Errorf("worker %s: %w", worker.sourceID, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for i := range c.workers {
		if err := c.validateWorkerGitState(ctx, c.workers[i]); err != nil {
			return err
		}
		changed, err := c.validateWorkerDiff(ctx, c.workers[i])
		if err != nil {
			return err
		}
		c.workers[i].changedPaths = changed
	}
	return nil
}

func (c *parallelCoordinator) preservedRoots() []string {
	var roots []string
	for _, worker := range c.workers {
		if worker.root != "" {
			roots = append(roots, worker.root)
		}
	}
	return roots
}

func (c *parallelCoordinator) cleanup(ctx context.Context) error {
	c.prepared = false
	var errs []error
	for i := range c.workers {
		if c.workers[i].root == "" {
			continue
		}
		if _, err := runParallelGit(ctx, c.root, "worktree", "remove", "--force", c.workers[i].root); err != nil {
			errs = append(errs, fmt.Errorf("remove worktree %s: %w", c.workers[i].sourceID, err))
			continue
		}
		c.workers[i].root = ""
	}
	if len(errs) == 0 && c.tempParent != "" {
		if err := os.Remove(c.tempParent); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove worktree parent: %w", err))
		} else {
			c.tempParent = ""
		}
	}
	return errors.Join(errs...)
}

func (c *parallelCoordinator) validateWorkerGitState(ctx context.Context, worker parallelWorker) error {
	top, err := runParallelGit(ctx, worker.root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("recheck worker %s Git toplevel: %w", worker.sourceID, err)
	}
	gotTop, err := filepath.EvalSymlinks(filepath.Clean(strings.TrimSpace(string(top))))
	if err != nil {
		return fmt.Errorf("resolve worker %s Git toplevel: %w", worker.sourceID, err)
	}
	wantTop, err := filepath.EvalSymlinks(worker.root)
	if err != nil {
		return fmt.Errorf("resolve worker %s prepared root: %w", worker.sourceID, err)
	}
	if gotTop != wantTop {
		return fmt.Errorf("worker %s Git toplevel is %s, want %s", worker.sourceID, gotTop, wantTop)
	}
	head, err := runParallelGit(ctx, worker.root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("recheck worker %s HEAD: %w", worker.sourceID, err)
	}
	if got := strings.TrimSpace(string(head)); got != c.head {
		return fmt.Errorf("worker %s HEAD changed from %s to %s", worker.sourceID, c.head, got)
	}
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--quiet", "HEAD")
	cmd.Dir = worker.root
	if out, err := cmd.CombinedOutput(); err == nil {
		return fmt.Errorf("worker %s is attached to branch %s; want detached HEAD", worker.sourceID, strings.TrimSpace(string(out)))
	} else {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return fmt.Errorf("check worker %s detached HEAD: %w: %s", worker.sourceID, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (c *parallelCoordinator) acquireWorkspaceLock(ctx context.Context) error {
	if c.lockFile != nil {
		return errors.New("parallel workspace lock is already held")
	}
	out, err := runParallelGit(ctx, c.root, "rev-parse", "--path-format=absolute", "--git-path", "golem-parallel.lock")
	if err != nil {
		return fmt.Errorf("resolve parallel workspace lock: %w", err)
	}
	lockPath := filepath.Clean(strings.TrimSpace(string(out)))
	if !filepath.IsAbs(lockPath) {
		lockPath = filepath.Join(c.root, lockPath)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("parallel workspace is already locked: %s", lockPath)
		}
		return fmt.Errorf("acquire parallel workspace lock: %w", err)
	}
	c.lockFile = file
	return nil
}

func (c *parallelCoordinator) releaseWorkspaceLock() error {
	if c.lockFile == nil {
		return nil
	}
	file, lockPath := c.lockFile, c.lockFile.Name()
	c.lockFile = nil
	heldInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return errors.Join(closeErr, fmt.Errorf("inspect held parallel workspace lock: %w", statErr))
	}
	pathInfo, pathErr := os.Lstat(lockPath)
	if pathErr == nil && !os.SameFile(heldInfo, pathInfo) {
		return errors.Join(closeErr, errors.New("parallel workspace lock path changed while held"))
	}
	if pathErr != nil && !errors.Is(pathErr, fs.ErrNotExist) {
		return errors.Join(closeErr, fmt.Errorf("inspect parallel workspace lock during release: %w", pathErr))
	}
	var removeErr error
	if pathErr == nil {
		removeErr = os.Remove(lockPath)
	}
	return errors.Join(closeErr, removeErr)
}

func (c *parallelCoordinator) recordBase(ctx context.Context) error {
	abs, err := filepath.Abs(c.root)
	if err != nil {
		return fmt.Errorf("resolve parallel root: %w", err)
	}
	c.root, err = filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return fmt.Errorf("resolve parallel root symlinks: %w", err)
	}
	top, err := runParallelGit(ctx, c.root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("resolve Git toplevel: %w", err)
	}
	c.topLevel, err = filepath.EvalSymlinks(filepath.Clean(strings.TrimSpace(string(top))))
	if err != nil {
		return fmt.Errorf("resolve Git toplevel symlinks: %w", err)
	}
	if c.root != c.topLevel {
		return fmt.Errorf("parallel root %s must equal Git toplevel %s", c.root, c.topLevel)
	}
	head, err := runParallelGit(ctx, c.root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("record Git HEAD: %w", err)
	}
	c.head = strings.TrimSpace(string(head))
	return c.requireCleanRoot(ctx)
}

func (c *parallelCoordinator) recheckRoot(ctx context.Context) error {
	top, err := runParallelGit(ctx, c.root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("recheck Git toplevel: %w", err)
	}
	got, err := filepath.EvalSymlinks(filepath.Clean(strings.TrimSpace(string(top))))
	if err != nil {
		return fmt.Errorf("resolve rechecked Git toplevel symlinks: %w", err)
	}
	if got != c.topLevel || got != c.root {
		return fmt.Errorf("git toplevel changed from %s to %s", c.topLevel, got)
	}
	head, err := runParallelGit(ctx, c.root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("recheck Git HEAD: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != c.head {
		return fmt.Errorf("git HEAD changed from %s to %s", c.head, got)
	}
	if err := c.requireCleanRoot(ctx); err != nil {
		return err
	}
	for _, worker := range c.workers {
		for _, file := range worker.paths {
			eligible, err := c.gitEligiblePath(ctx, file)
			if err != nil {
				return fmt.Errorf("recheck parallel candidate %s: %w", file, err)
			}
			if !eligible {
				return fmt.Errorf("parallel candidate changed after selection: %s", file)
			}
		}
	}
	return nil
}

func (c *parallelCoordinator) requireCleanRoot(ctx context.Context) error {
	out, err := runParallelGit(ctx, c.root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("inspect parallel root: %w", err)
	}
	for _, file := range parallelStatusPaths(out) {
		if !isParallelAgentPath(file) {
			return fmt.Errorf("parallel root must be clean outside .agent: %s", file)
		}
	}
	unexpected, err := parallelUnexpectedIndexPath(ctx, c.root)
	if err != nil {
		return fmt.Errorf("inspect parallel root index flags: %w", err)
	}
	if unexpected != "" {
		return fmt.Errorf("%w: tracked path %q", errParallelIndexFlags, unexpected)
	}
	return nil
}

func (c *parallelCoordinator) gitEligiblePath(ctx context.Context, file string) (bool, error) {
	exists, err := parallelSafePath(c.root, file)
	if err != nil {
		return false, nil
	}
	index, err := runParallelGit(ctx, c.root, "ls-files", "-v", "-z", "--", ":(literal)"+file)
	if err != nil {
		return false, fmt.Errorf("inspect index flags for %s: %w", file, err)
	}
	if len(index) > 0 {
		if string(index) != "H "+file+"\x00" {
			return false, nil
		}
		if !exists {
			return false, nil
		}
		out, err := runParallelGit(ctx, c.root, "ls-tree", "-z", "--full-tree", "--name-only", c.head, "--", ":(literal)"+file)
		if err != nil {
			return false, fmt.Errorf("check whether %s is tracked at %s: %w", file, c.head, err)
		}
		return string(out) == file+"\x00", nil
	}
	if exists {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "check-ignore", "-q", "--", file)
	cmd.Dir = c.root
	err = cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("check whether %s is ignored: %w", file, err)
}

func (c *parallelCoordinator) validateWorkerDiff(ctx context.Context, worker parallelWorker) ([]string, error) {
	assigned := make(map[string]struct{}, len(worker.paths))
	for _, file := range worker.paths {
		assigned[file] = struct{}{}
	}
	unexpected, err := parallelUnexpectedIndexPath(ctx, worker.root)
	if err != nil {
		return nil, fmt.Errorf("inspect worker %s index flags: %w", worker.sourceID, err)
	}
	if unexpected != "" {
		if _, ok := assigned[unexpected]; ok {
			return nil, fmt.Errorf("worker %s assigned path %q has unexpected index flags", worker.sourceID, unexpected)
		}
		return nil, fmt.Errorf("worker %s tracked path %q has unexpected index flags", worker.sourceID, unexpected)
	}
	for _, file := range worker.paths {
		if _, err := parallelSafePath(worker.root, file); err != nil {
			return nil, fmt.Errorf("worker %s produced unsafe assigned path %q: %w", worker.sourceID, file, err)
		}
	}
	out, err := runParallelGit(ctx, worker.root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return nil, fmt.Errorf("inspect worker %s changes: %w", worker.sourceID, err)
	}
	changedSet := make(map[string]struct{}, len(worker.paths))
	for _, file := range parallelStatusPaths(out) {
		if isParallelAgentPath(file) {
			continue
		}
		if _, ok := assigned[file]; !ok {
			return nil, fmt.Errorf("worker %s produced unexpected changed path %q", worker.sourceID, file)
		}
		if _, ok := changedSet[file]; ok {
			continue
		}
		if _, err := parallelSafePath(worker.root, file); err != nil {
			return nil, fmt.Errorf("worker %s changed unsafe path %q: %w", worker.sourceID, file, err)
		}
		changedSet[file] = struct{}{}
	}
	changed := make([]string, 0, len(changedSet))
	for _, file := range worker.paths {
		if _, ok := changedSet[file]; ok {
			changed = append(changed, file)
		}
	}
	return changed, nil
}

func runParallelGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func parallelUnexpectedIndexPath(ctx context.Context, root string) (string, error) {
	index, err := runParallelGit(ctx, root, "ls-files", "-v", "-z")
	if err != nil {
		return "", err
	}
	for _, entry := range bytes.Split(index, []byte{'\x00'}) {
		if len(entry) == 0 || (len(entry) >= 3 && entry[0] == 'H' && entry[1] == ' ') {
			continue
		}
		if len(entry) < 3 || entry[1] != ' ' {
			return "", fmt.Errorf("malformed index entry %q", entry)
		}
		return string(entry[2:]), nil
	}
	return "", nil
}

func parallelStatusPaths(status []byte) []string {
	entries := strings.Split(string(status), "\x00")
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		if len(entry) >= 4 && entry[2] == ' ' {
			paths = append(paths, entry[3:])
			continue
		}
		paths = append(paths, entry)
	}
	return paths
}

func isParallelAgentPath(file string) bool {
	first := strings.SplitN(strings.TrimSuffix(file, "/"), "/", 2)[0]
	return first == ".agent"
}

func parallelSafePath(root, file string) (bool, error) {
	current := root
	parts := strings.Split(file, "/")
	for i, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false, errors.New("symlink component")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return false, errors.New("non-directory parent")
		}
		if i == len(parts)-1 && info.IsDir() {
			return false, errors.New("path is a directory")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return false, errors.New("path is not a regular file")
		}
	}
	return true, nil
}

func copyParallelAgentTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("canonical .agent must be a directory, not a symlink")
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, err := os.Lstat(file)
		if err != nil {
			return err
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("reject symlink %s", file)
		}
		rel, err := filepath.Rel(src, file)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case entryInfo.IsDir():
			if err := os.MkdirAll(target, entryInfo.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, parallelCopiedMode(entryInfo.Mode()))
		case entryInfo.Mode().IsRegular():
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, entryInfo.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, parallelCopiedMode(entryInfo.Mode()))
		default:
			return fmt.Errorf("reject non-file .agent entry %s", file)
		}
	})
}

func parallelCopiedMode(mode fs.FileMode) fs.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}
