//go:build linux

package tools

// Linux Bubblewrap backend (#441): capability probe, per-invocation policy
// collection, and process wiring. The security-critical argv construction
// lives in the cross-platform bwrap.go so it is unit-tested on every CI
// platform.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// bwrapExecPath and prlimitExecPath are the fixed host TCB binaries
	// (bubblewrap and util-linux packaged locations). They are deliberately
	// never resolved through PATH: PATH is command input, not authority.
	bwrapExecPath   = "/usr/bin/bwrap"
	prlimitExecPath = "/usr/bin/prlimit"
	// bwrapProbeTimeout bounds the active capability probe.
	bwrapProbeTimeout = 5 * time.Second
	// bwrapProbeOutputCap bounds captured probe diagnostics.
	bwrapProbeOutputCap = 1024
)

// bwrapProbeFunc is the injectable probe seam for deterministic unit tests.
type bwrapProbeFunc func(ctx context.Context, argv []string) error

// probeBwrapArgv builds the production-prefix probe invocation for one
// config: the exact isolation/capability prefix production uses, an empty
// environment declaration, a broad read-only root acceptable only because
// the probe runs the trusted /bin/true (mirroring Seatbelt's allow-default
// probe), both private tmpfs mounts with their production quotas, and the
// prlimit chain exactly when the config caps memory.
func probeBwrapArgv(bwrapPath, prlimitPath string, cfg SandboxConfig, capBytes int64) []string {
	args := append([]string{bwrapPath}, bwrapIsolationArgs(cfg.AllowNetwork)...)
	args = append(args, "--clearenv",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev")
	size := strconv.FormatInt(capBytes, 10)
	if capBytes > 0 {
		args = append(args, "--size", size)
	}
	args = append(args, "--tmpfs", "/dev/shm")
	if capBytes > 0 {
		args = append(args, "--size", size)
	}
	args = append(args, "--tmpfs", "/tmp", "--remount-ro", "/", "/bin/true")
	if capBytes > 0 {
		args = append([]string{prlimitPath, "--as=" + size}, args...)
	}
	return args
}

// probeBwrap actively verifies the host can build the full namespace set — a
// present binary proves nothing (Docker's default seccomp blocks
// CLONE_NEWUSER; Ubuntu 24.04 AppArmor restricts unprivileged user
// namespaces). The chain runs with a non-nil empty environment: the TCB
// inherits nothing from the parent, matching production. Fixed-binary safety
// checks happen separately in the constructor.
func probeBwrap(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{}
	out := &cappedBuffer{cap: bwrapProbeOutputCap}
	cmd.Stdout = out
	cmd.Stderr = out
	// Bound Wait when a descendant of the probed binary outlives it while
	// holding the stdio pipes open, mirroring the runners.
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(string(out.buf)); detail != "" {
			return fmt.Errorf("tools: bwrap sandbox unavailable on this host: %w: %s", err, detail)
		}
		return fmt.Errorf("tools: bwrap sandbox unavailable on this host: %w", err)
	}
	return nil
}

// checkSandboxBinary attests one fixed-path TCB binary against ordinary host
// packaging mistakes (not a compromised TCB): it must exist as a regular,
// non-symlink file, be executable, carry no set-id bits, and not be group or
// other writable.
func checkSandboxBinary(path, install string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("tools: bwrap sandbox requires %s (install %s): %w", path, install, err)
	}
	mode := fi.Mode()
	if !mode.IsRegular() {
		return fmt.Errorf("tools: bwrap sandbox requires %s (install %s) to be a regular non-symlink file, got mode %v", path, install, mode)
	}
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("tools: bwrap sandbox requires %s (install %s): %w", path, install, errNotExecutable)
	}
	if mode&(os.ModeSetuid|os.ModeSetgid) != 0 || mode.Perm()&0o022 != 0 {
		return fmt.Errorf("tools: bwrap sandbox refuses %s: unsafe mode %v (set-id or group/other-writable)", path, mode)
	}
	return nil
}

// collectBwrapLayout inspects the fixed top-level layout entries under root
// ("/" in production; a fake root in tests) and types each present entry as
// a directory bind or a relative symlink to recreate. A layout entry that is
// neither, that links non-relatively, or whose canonical target is not
// covered by the collected read-only roots is an attack indicator and fails
// closed rather than being silently dropped.
func collectBwrapLayout(root string, coveredRoots []string) (dirs []string, links map[string]string, err error) {
	coveredBy := func(target string) bool {
		for _, r := range coveredRoots {
			if target == r || strings.HasPrefix(target, r+"/") {
				return true
			}
		}
		return false
	}
	links = make(map[string]string)
	for _, dest := range bwrapLayoutDirs {
		hostPath := filepath.Join(root, dest)
		fi, lerr := os.Lstat(hostPath)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				continue // absent layout entry: strictly narrower
			}
			return nil, nil, fmt.Errorf("tools: bwrap inspect layout entry %q: %w", dest, lerr)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, rerr := os.Readlink(hostPath)
			if rerr != nil {
				return nil, nil, fmt.Errorf("tools: bwrap inspect layout entry %q: %w", dest, rerr)
			}
			if target == "" || strings.HasPrefix(target, "/") {
				return nil, nil, fmt.Errorf("tools: bwrap layout link %q has non-relative target %q", dest, target)
			}
			canon, cerr := filepath.EvalSymlinks(hostPath)
			if cerr != nil {
				return nil, nil, fmt.Errorf("tools: bwrap resolve layout link %q: %w", dest, cerr)
			}
			if !coveredBy(canon) {
				return nil, nil, fmt.Errorf("tools: bwrap layout link %q resolves to %q outside the reviewed read-only roots", dest, canon)
			}
			links[dest] = target
		case fi.IsDir():
			dirs = append(dirs, dest)
		default:
			return nil, nil, fmt.Errorf("tools: bwrap layout entry %q is neither a directory nor a symlink", dest)
		}
	}
	return dirs, links, nil
}

// defaultBwrapCollect is the production per-invocation policy collector:
// canonicalized reviewed system roots (fail-closed on aliasing over the
// workspace or home) plus the typed top-level layout of the real root.
func defaultBwrapCollect(workspaceRoot string) (roots, layoutDirs []string, links map[string]string, err error) {
	// Canonicalize home so the coverage check compares like paths with the
	// EvalSymlinks-resolved system roots: a symlinked $HOME would otherwise
	// miss a system root aliased into its real target. Best-effort — a
	// missing or unresolvable home only skips the home guard; the workspace
	// guard and broad-root rejection still apply.
	home, _ := os.UserHomeDir()
	if canon, cerr := filepath.EvalSymlinks(home); cerr == nil {
		home = canon
	}
	roots, err = collectSystemRoots(bwrapDefaultSystemRoots, workspaceRoot, home, bwrapBroadRoot)
	if err != nil {
		return nil, nil, nil, err
	}
	layoutDirs, links, err = collectBwrapLayout("/", roots)
	if err != nil {
		return nil, nil, nil, err
	}
	return roots, layoutDirs, links, nil
}

// bwrapBackend runs both command lifetimes inside per-invocation Bubblewrap
// namespaces by rewriting the re-checked spec to the [prlimit] bwrap chain
// and delegating to the host unix implementations.
type bwrapBackend struct {
	bwrapPath   string
	prlimitPath string
	cfg         SandboxConfig
	capBytes    int64 // checked once by bwrapMemoryCapBytes
	runner      commandRunner
	starter     backgroundStarter
	collect     func(workspaceRoot string) (roots, layoutDirs []string, links map[string]string, err error)
}

// newBwrapExecBackend constructs the real bwrap backend, failing closed when
// the config requests ceilings bwrap cannot enforce or when the active
// capability probe fails.
func newBwrapExecBackend(cfg SandboxConfig) (execBackend, error) {
	return newBwrapExecBackendAt(bwrapExecPath, prlimitExecPath, probeBwrap, cfg)
}

func newBwrapExecBackendAt(bwrapPath, prlimitPath string, probe bwrapProbeFunc, cfg SandboxConfig) (execBackend, error) {
	if err := bwrapConfigSupport(cfg); err != nil {
		return nil, err
	}
	capBytes, err := bwrapMemoryCapBytes(cfg.MemoryCapMB)
	if err != nil {
		return nil, err
	}
	if err := checkSandboxBinary(bwrapPath, "bubblewrap"); err != nil {
		return nil, err
	}
	if capBytes > 0 {
		if err := checkSandboxBinary(prlimitPath, "util-linux"); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
	defer cancel()
	if err := probe(ctx, probeBwrapArgv(bwrapPath, prlimitPath, cfg, capBytes)); err != nil {
		return nil, err
	}
	return &bwrapBackend{
		bwrapPath:   bwrapPath,
		prlimitPath: prlimitPath,
		cfg:         cfg,
		capBytes:    capBytes,
		runner:      unixRunner{},
		starter:     unixStarter{},
		collect:     defaultBwrapCollect,
	}, nil
}

// Run and Start are wired to prepare in the following commit; until then the
// constructed backend fails closed at invocation rather than executing on
// the host.
func (b *bwrapBackend) Run(context.Context, execSpec) (execResult, error) {
	return execResult{}, errors.New("tools: bwrap execution wiring incomplete")
}

func (b *bwrapBackend) Start(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
	return nil, errors.New("tools: bwrap execution wiring incomplete")
}
