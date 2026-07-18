package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/agentflow"
	"golang.org/x/sync/errgroup"
)

type parallelWorkerFunc func(context.Context, parallelWorker) error

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

	topLevel   string
	head       string
	tempParent string
	prepared   bool
	workers    []parallelWorker
}

func newParallelCoordinator(root string, plan *agentflow.Plan, maxWorkers int, worker parallelWorkerFunc) *parallelCoordinator {
	return &parallelCoordinator{root: root, plan: plan, maxWorkers: maxWorkers, worker: worker}
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
	if c.maxWorkers < 2 || !parallelGraphHasEdge(c.plan) {
		c.workers = nil
		return false, nil
	}
	if err := c.recordBase(ctx); err != nil {
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
	return len(c.workers) >= 2, nil
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
		return fmt.Errorf("Git toplevel changed from %s to %s", c.topLevel, got)
	}
	head, err := runParallelGit(ctx, c.root, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("recheck Git HEAD: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != c.head {
		return fmt.Errorf("Git HEAD changed from %s to %s", c.head, got)
	}
	return c.requireCleanRoot(ctx)
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
	return nil
}

func (c *parallelCoordinator) gitEligiblePath(ctx context.Context, file string) (bool, error) {
	exists, err := parallelSafePath(c.root, file)
	if err != nil {
		return false, nil
	}
	if exists {
		out, err := runParallelGit(ctx, c.root, "ls-tree", "-z", "--full-tree", "--name-only", c.head, "--", ":(literal)"+file)
		if err != nil {
			return false, fmt.Errorf("check whether %s is tracked at %s: %w", file, c.head, err)
		}
		return string(out) == file+"\x00", nil
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
	for _, file := range worker.paths {
		if _, err := parallelSafePath(worker.root, file); err != nil {
			return nil, fmt.Errorf("worker %s produced unsafe assigned path %q: %w", worker.sourceID, file, err)
		}
	}
	out, err := runParallelGit(ctx, worker.root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return nil, fmt.Errorf("inspect worker %s changes: %w", worker.sourceID, err)
	}
	assigned := make(map[string]struct{}, len(worker.paths))
	for _, file := range worker.paths {
		assigned[file] = struct{}{}
	}
	var changed []string
	seen := make(map[string]struct{}, len(worker.paths))
	for _, file := range parallelStatusPaths(out) {
		if isParallelAgentPath(file) {
			continue
		}
		if _, ok := assigned[file]; !ok {
			return nil, fmt.Errorf("worker %s produced unexpected changed path %q", worker.sourceID, file)
		}
		if _, ok := seen[file]; ok {
			continue
		}
		if _, err := parallelSafePath(worker.root, file); err != nil {
			return nil, fmt.Errorf("worker %s changed unsafe path %q: %w", worker.sourceID, file, err)
		}
		seen[file] = struct{}{}
		changed = append(changed, file)
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
