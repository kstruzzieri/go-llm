package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// hostGitPrefix is placed before every Golem host git command's own arguments.
var hostGitPrefix = []string{"-c", "safe.bareRepository=explicit", "-c", "core.fsmonitor=false"}

// hostGitVerified caches git executables that passed requireHostGit, keyed by
// resolved path. An entry is trusted only while the file's identity is unchanged.
var (
	hostGitMu       sync.Mutex
	hostGitVerified = map[string]os.FileInfo{}
)

// hostGitCommand builds the one exec.Cmd every Golem host git call uses. The
// caller owns Stdout, Stderr and WaitDelay.
func hostGitCommand(ctx context.Context, gitPath, workspace, dir string, env []string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, gitPath, append(slices.Clone(hostGitPrefix), args...)...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("git: %w", cmd.Err)
	}
	if err := requireHostGit(ctx, cmd.Path, workspace, env); err != nil {
		return nil, err
	}
	cmd.Dir = dir
	cmd.Env = env
	return cmd, nil
}

// requireHostGit refuses a git executable Golem must not run: one inside the
// workspace, or older than 2.38.
func requireHostGit(ctx context.Context, path, workspace string, env []string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("git: resolve %s: %w", path, err)
	}
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("git: resolve workspace %s: %w", workspace, err)
	}
	if rel, err := filepath.Rel(ws, resolved); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("git: refusing %s: the executable is inside the workspace", resolved)
	}

	before, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("git: stat %s: %w", resolved, err)
	}
	hostGitMu.Lock()
	cached, ok := hostGitVerified[resolved]
	hostGitMu.Unlock()
	if ok && os.SameFile(cached, before) && cached.Size() == before.Size() && cached.ModTime().Equal(before.ModTime()) {
		return nil
	}

	probe := exec.CommandContext(ctx, resolved, "--version")
	probe.Env = env
	stdout := &capWriter{max: gitContextStderrCap}
	probe.Stdout = stdout
	probe.Stderr = &capWriter{max: gitContextStderrCap}
	probe.WaitDelay = gitContextWaitDelay
	runErr := probe.Run()
	if err := ctx.Err(); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("git: %s --version: %w", resolved, runErr)
	}
	major, minor, perr := parseGitVersion(stdout.String())
	if perr != nil || !gitVersionSupported(major, minor) {
		return fmt.Errorf("git: %s reports %q; Golem needs git 2.38 or later", resolved, gitContextText(strings.TrimSpace(stdout.String())))
	}
	hostGitMu.Lock()
	hostGitVerified[resolved] = before
	hostGitMu.Unlock()
	return nil
}

// gitVersionSupported reports whether major.minor is at least 2.38.
func gitVersionSupported(major, minor int) bool {
	return major > 2 || (major == 2 && minor >= 38)
}

// parseGitVersion extracts major and minor from `git --version` output.
func parseGitVersion(out string) (major, minor int, err error) {
	rest, ok := strings.CutPrefix(out, "git version ")
	if !ok {
		return 0, 0, fmt.Errorf("unrecognized git version %q", out)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return 0, 0, fmt.Errorf("missing git version in %q", out)
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("unrecognized git version %q", out)
	}
	nums := [2]int{}
	for i := range nums {
		p := parts[i]
		if p == "" || strings.TrimLeft(p, "0123456789") != "" {
			return 0, 0, fmt.Errorf("unrecognized git version %q", out)
		}
		if nums[i], err = strconv.Atoi(p); err != nil {
			return 0, 0, fmt.Errorf("unrecognized git version %q: %w", out, err)
		}
	}
	return nums[0], nums[1], nil
}
