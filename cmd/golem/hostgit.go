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
// caller owns Stdout, Stderr and WaitDelay. cmd.Path is the resolved path that
// was checked, so a symlink retargeted after the check cannot change what runs.
func hostGitCommand(ctx context.Context, gitPath, workspace, dir string, env []string, args ...string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, gitPath, append(slices.Clone(hostGitPrefix), args...)...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("git: %w", cmd.Err)
	}
	resolved, err := requireHostGit(ctx, cmd.Path, workspace, env)
	if err != nil {
		return nil, err
	}
	cmd.Path = resolved
	cmd.Dir = dir
	cmd.Env = env
	return cmd, nil
}

// requireHostGit refuses a git executable Golem must not run: one inside the
// workspace, or older than 2.38. It returns the resolved absolute path to run.
func requireHostGit(ctx context.Context, path, workspace string, env []string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("git: refusing relative executable path %s", gitContextText(path))
	}
	if !filepath.IsAbs(workspace) {
		return "", fmt.Errorf("git: workspace %s is not an absolute path", gitContextText(workspace))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("git: resolve %s: %w", gitContextText(path), err)
	}
	ws, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("git: resolve workspace %s: %w", gitContextText(workspace), err)
	}
	inside, err := pathWithin(resolved, ws)
	if err != nil {
		return "", fmt.Errorf("git: cannot place %s relative to the workspace: %w", gitContextText(resolved), err)
	}
	if inside {
		return "", fmt.Errorf("git: refusing %s: the executable is inside the workspace", gitContextText(resolved))
	}

	before, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("git: stat %s: %w", gitContextText(resolved), err)
	}
	hostGitMu.Lock()
	cached, ok := hostGitVerified[resolved]
	hostGitMu.Unlock()
	if ok && os.SameFile(cached, before) && cached.Size() == before.Size() && cached.ModTime().Equal(before.ModTime()) {
		return resolved, nil
	}

	probe := exec.CommandContext(ctx, resolved, "--version")
	probe.Args[0] = path // multi-call shims dispatch on argv[0]; match the real command
	probe.Env = env
	stdout := &capWriter{max: gitContextStderrCap}
	probe.Stdout = stdout
	probe.Stderr = &capWriter{max: gitContextStderrCap}
	probe.WaitDelay = gitContextWaitDelay
	runErr := probe.Run()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if runErr != nil {
		return "", fmt.Errorf("git: %s --version: %w", gitContextText(resolved), runErr)
	}
	major, minor, perr := parseGitVersion(stdout.String())
	if perr != nil || !gitVersionSupported(major, minor) {
		return "", fmt.Errorf("git: %s reports %s; Golem needs git 2.38 or later", gitContextText(resolved), gitContextText(strings.TrimSpace(stdout.String())))
	}
	hostGitMu.Lock()
	hostGitVerified[resolved] = before
	hostGitMu.Unlock()
	return resolved, nil
}

// pathWithin reports whether path is root or lies under it, by file identity
// rather than spelling: a case-insensitive filesystem, Unicode normalization,
// firmlinks and bind mounts all give one directory several spellings. It walks
// up from path's directory comparing each ancestor with root through
// os.SameFile, and fails closed on any stat error.
func pathWithin(path, root string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	for dir := filepath.Dir(path); ; {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		if os.SameFile(info, rootInfo) {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
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
