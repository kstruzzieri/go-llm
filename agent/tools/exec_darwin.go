//go:build darwin

package tools

// macOS Seatbelt (sandbox-exec) backend (#442): Darwin-only capability probe
// and process wiring. The security-critical SBPL rendering lives in the
// cross-platform seatbelt.go so it is unit-tested on every CI platform.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// seatbeltExecPath is the system sandbox-exec binary. Deprecated by Apple
	// but functional; its absence or inability to apply a profile fails
	// construction — never a silent host fallback.
	seatbeltExecPath = "/usr/bin/sandbox-exec"
	// seatbeltProbeTimeout bounds the active capability probe.
	seatbeltProbeTimeout = 5 * time.Second
	// seatbeltProbeOutputCap bounds captured probe diagnostics.
	seatbeltProbeOutputCap = 1024
)

// probeSeatbelt actively verifies the host can apply a Seatbelt profile:
// file checks first, then a harmless allow-default run of /usr/bin/true.
// Existence does not imply usability — a process that is already sandboxed
// has an executable sandbox-exec that fails with "sandbox_apply: Operation
// not permitted", so a stat-only check would be a false positive.
func probeSeatbelt(ctx context.Context, execPath string) error {
	fi, err := os.Stat(execPath)
	if err != nil {
		return fmt.Errorf("tools: seatbelt requires %s: %w", execPath, err)
	}
	if !isExecutableFile(fi) {
		return fmt.Errorf("tools: seatbelt requires %s: %w", execPath, errNotExecutable)
	}
	cmd := exec.CommandContext(ctx, execPath, "-p", "(version 1)(allow default)", "/usr/bin/true")
	out := &cappedBuffer{cap: seatbeltProbeOutputCap}
	cmd.Stdout = out
	cmd.Stderr = out
	// Bound Wait when a descendant of the probed binary outlives it while
	// holding the stdio pipes open, mirroring the runners.
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(string(out.buf)); detail != "" {
			return fmt.Errorf("tools: seatbelt unavailable on this host: %w: %s", err, detail)
		}
		return fmt.Errorf("tools: seatbelt unavailable on this host: %w", err)
	}
	return nil
}

// seatbeltProbeFunc is the injectable probe seam for deterministic unit tests.
type seatbeltProbeFunc func(context.Context, string) error

// seatbeltTempBase is the fixed parent for private per-invocation temp
// directories. Deliberately NOT the ambient TMPDIR: inherited temp locations
// are command input, never policy (D5).
const seatbeltTempBase = "/private/tmp"

// seatbeltDefaultSystemRoots is the reviewed, minimal read-only system
// execution surface (D1). Broad mutable roots (/System, /usr, /Library,
// /private/etc, /dev, Homebrew prefixes) are deliberately absent — on current
// macOS /System/Volumes/Data aliases user data, and the rest are not runtime
// prerequisites. Additions require a demonstrated denial, the narrowest
// filtered rule, and a cross-platform profile test.
var seatbeltDefaultSystemRoots = []string{
	"/System/Library",
	"/System/Cryptexes/OS",
	"/usr/bin",
	"/usr/lib",
	"/usr/libexec",
	"/usr/share",
	"/bin",
	"/sbin",
}

// seatbeltBackend runs both command lifetimes under per-invocation Seatbelt
// profiles by rewriting the re-checked spec to sandbox-exec and delegating to
// the host unix implementations.
type seatbeltBackend struct {
	execPath     string
	allowNetwork bool
	runner       commandRunner
	starter      backgroundStarter
	tempBase     string
	systemRoots  func(workspaceRoot string) ([]string, error)
}

// newSeatbeltExecBackend constructs the real Seatbelt backend, failing closed
// when the config requests ceilings Seatbelt cannot enforce or when the
// active capability probe fails.
func newSeatbeltExecBackend(cfg SandboxConfig) (execBackend, error) {
	return newSeatbeltExecBackendAt(seatbeltExecPath, probeSeatbelt, cfg)
}

func newSeatbeltExecBackendAt(execPath string, probe seatbeltProbeFunc, cfg SandboxConfig) (execBackend, error) {
	if err := seatbeltConfigSupport(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	if err := probe(ctx, execPath); err != nil {
		return nil, err
	}
	return &seatbeltBackend{
		execPath:     execPath,
		allowNetwork: cfg.AllowNetwork,
		runner:       unixRunner{},
		starter:      unixStarter{},
		tempBase:     seatbeltTempBase,
		systemRoots: func(workspaceRoot string) ([]string, error) {
			// Canonicalize home so the coverage check compares like inodes
			// with the EvalSymlinks-resolved system roots: a symlinked $HOME
			// would otherwise miss a system root aliased into its real target.
			// Best-effort — a missing or unresolvable home only skips the
			// home-parent guard; the workspace guard and broad-root rejection
			// still apply.
			home, _ := os.UserHomeDir()
			if canon, err := filepath.EvalSymlinks(home); err == nil {
				home = canon
			}
			return seatbeltCollectSystemRoots(seatbeltDefaultSystemRoots, workspaceRoot, home)
		},
	}, nil
}

// seatbeltTempCleanup returns the single-shot guarded remover for one private
// temp directory. Before recursive removal the path must still name the
// directory that was created (os.SameFile on the recorded Lstat identity); a
// vanished or replaced path abandons cleanup rather than deleting a
// replacement or claiming the original was reaped. The directory is 0700, so
// an abandoned one leaks nothing and macOS temp cleanup reaps it eventually.
func seatbeltTempCleanup(dir string, created os.FileInfo) func() error {
	var once sync.Once
	var err error
	return func() error {
		once.Do(func() {
			fi, statErr := os.Lstat(dir)
			if statErr != nil {
				err = fmt.Errorf("tools: seatbelt temp %s vanished before cleanup; not reaped: %w", dir, statErr)
				return
			}
			if !os.SameFile(fi, created) {
				err = fmt.Errorf("tools: seatbelt temp %s was replaced; cleanup abandoned", dir)
				return
			}
			err = os.RemoveAll(dir)
		})
		return err
	}
}

// prepare builds the per-invocation private temp directory, SBPL profile, and
// wrapper spec. On success the returned cleanup is the guarded temp remover;
// on failure prepare has already attempted guarded cleanup itself and nothing
// may spawn. The stamped WorkspaceRoot is consumed as-is — re-resolving it
// here could silently change the policy after approval.
func (b *seatbeltBackend) prepare(spec execSpec) (execSpec, func() error, error) {
	if len(spec.Argv) == 0 || spec.Path == "" {
		return execSpec{}, nil, errors.New("tools: seatbelt requires a non-empty argv and executable path")
	}
	if spec.WorkspaceRoot == "" {
		return execSpec{}, nil, errors.New("tools: seatbelt requires a workspace root in the exec spec")
	}
	if !seatbeltCleanAbs(spec.WorkspaceRoot) || seatbeltBroadRoot(spec.WorkspaceRoot) {
		return execSpec{}, nil, fmt.Errorf("tools: seatbelt workspace root %q must be a canonical non-root path", spec.WorkspaceRoot)
	}
	canonBase, err := filepath.EvalSymlinks(b.tempBase)
	if err != nil {
		return execSpec{}, nil, fmt.Errorf("tools: seatbelt temp base: %w", err)
	}
	tempDir, err := os.MkdirTemp(b.tempBase, "go-llm-seatbelt-*")
	if err != nil {
		return execSpec{}, nil, fmt.Errorf("tools: seatbelt private temp: %w", err)
	}
	created, err := os.Lstat(tempDir)
	if err != nil {
		_ = os.Remove(tempDir)
		return execSpec{}, nil, fmt.Errorf("tools: seatbelt private temp identity: %w", err)
	}
	cleanup := seatbeltTempCleanup(tempDir, created)
	fail := func(err error) (execSpec, func() error, error) {
		_ = cleanup()
		return execSpec{}, nil, err
	}
	canonTemp, err := filepath.EvalSymlinks(tempDir)
	if err != nil {
		return fail(fmt.Errorf("tools: seatbelt private temp: %w", err))
	}
	if !seatbeltCleanAbs(canonTemp) || seatbeltBroadRoot(canonTemp) || filepath.Dir(canonTemp) != canonBase {
		return fail(fmt.Errorf("tools: seatbelt private temp %q has an unexpected parent", canonTemp))
	}
	if canonTemp != tempDir {
		// The guard identity was recorded on the created spelling; cleanup
		// targets that spelling, so the profile must use the same directory.
		fi, err := os.Lstat(canonTemp)
		if err != nil || !os.SameFile(fi, created) {
			return fail(fmt.Errorf("tools: seatbelt private temp %q lost its identity", canonTemp))
		}
	}
	canonExe, err := filepath.EvalSymlinks(spec.Path)
	if err != nil {
		return fail(fmt.Errorf("tools: seatbelt resolve executable target: %w", err))
	}
	sysRoots, err := b.systemRoots(spec.WorkspaceRoot)
	if err != nil {
		return fail(err)
	}
	ancestorSources := append(append([]string(nil), sysRoots...),
		spec.WorkspaceRoot, canonTemp, canonExe,
		"/dev/null", "/dev/random", "/dev/urandom")
	if canonExe != spec.Path && seatbeltCleanAbs(spec.Path) {
		// A symlinked approved path needs metadata on its own spine to be
		// resolvable; the content allowance stays on the canonical target.
		ancestorSources = append(ancestorSources, spec.Path)
	}
	ancestors, err := seatbeltMetadataAncestors(ancestorSources)
	if err != nil {
		return fail(err)
	}
	profile, err := buildSeatbeltProfile(seatbeltPolicy{
		workspaceRoot:     spec.WorkspaceRoot,
		tempRoot:          canonTemp,
		exePath:           canonExe,
		systemReadRoots:   sysRoots,
		metadataAncestors: ancestors,
		allowNetwork:      b.allowNetwork,
	})
	if err != nil {
		return fail(err)
	}
	wrapped := spec
	wrapped.Env = seatbeltChildEnv(spec.Env, canonTemp)
	wrapped.Path = b.execPath
	wrapped.Argv = append([]string{"sandbox-exec", "-p", profile, spec.Path}, spec.Argv[1:]...)
	return wrapped, cleanup, nil
}

// Run executes one foreground command under its per-invocation profile. A
// cleanup failure never overwrites the command's observed result: the private
// directory is 0700 and host temp reaping is the documented fallback.
func (b *seatbeltBackend) Run(ctx context.Context, spec execSpec) (execResult, error) {
	wrapped, cleanup, err := b.prepare(spec)
	if err != nil {
		return execResult{}, err
	}
	res, runErr := b.runner.Run(ctx, wrapped)
	_ = cleanup()
	return res, runErr
}

// seatbeltProcess decorates the delegate background process with the guarded
// private-temp cleanup, run exactly once after the underlying Wait. PID and
// Kill delegate unchanged via embedding; the manager's group-termination and
// reap semantics are untouched. A cleanup failure never rewrites the
// process's exit code, managerKilled flag, or error taxonomy.
type seatbeltProcess struct {
	backgroundProcess
	cleanup func() error
}

func (p *seatbeltProcess) Wait() (int, bool, error) {
	code, managerKilled, err := p.backgroundProcess.Wait()
	_ = p.cleanup()
	return code, managerKilled, err
}

// Start launches one background command under its per-invocation profile
// through the same preparation as Run. Spawn failure cleans the private temp;
// after publication the returned process owns cleanup at Wait/reap time.
func (b *seatbeltBackend) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	wrapped, cleanup, err := b.prepare(spec)
	if err != nil {
		return nil, err
	}
	proc, err := b.starter.Start(wrapped, stdout, stderr)
	if err != nil {
		_ = cleanup()
		return nil, err
	}
	return &seatbeltProcess{backgroundProcess: proc, cleanup: cleanup}, nil
}
