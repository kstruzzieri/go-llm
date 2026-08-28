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
	"syscall"
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
	args = append(args, "--tmpfs", "/tmp", "--remount-ro", "/dev", "--remount-ro", "/", "/bin/true")
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
				if errors.Is(cerr, fs.ErrNotExist) {
					// Dangling compatibility link: grants nothing, so it is
					// omitted exactly like an absent entry. Debian and Ubuntu
					// ship /lib32 -> usr/lib32 from base-files while the target
					// belongs to separate 32-bit packages, so purging those
					// leaves this shape on an otherwise healthy host. Failing
					// here would break every sandboxed command permanently.
					continue
				}
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
	roots, err = collectSystemRoots(bwrapDefaultSystemRoots, workspaceRoot, canonicalHome(), bwrapBroadRoot)
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

// canonicalHome resolves the invoking user's home directory so coverage checks
// compare like paths with EvalSymlinks-resolved policy paths: a symlinked $HOME
// would otherwise miss an alias into its real target. Best-effort — an
// unresolvable home yields "" and only skips the home guard; the workspace
// guard and broad-root rejection still apply.
func canonicalHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if canon, cerr := filepath.EvalSymlinks(home); cerr == nil {
		return canon
	}
	return home
}

// safeEtcPolicyPaths filters the reviewed /etc literals for one invocation.
// An absent or dangling literal is omitted (strictly narrower); any other
// resolution failure fails closed so permission or I/O problems never become
// silent policy changes.
//
// Every present literal is canonicalized and rejected when it resolves to a
// broad root, or to the workspace or home or a parent of either: --ro-bind
// mounts the RESOLVED object at the literal's path, so a symlinked literal
// would otherwise mount an arbitrary subtree — up to all of / — read-only at
// an approved /etc location. This mirrors the discipline collectSystemRoots
// and collectBwrapLayout already apply; an aliased policy path is an attack
// indicator, never silently accepted.
//
// A canonical target outside /etc is legitimate and must stay allowed:
// systemd hosts ship /etc/resolv.conf -> /run/systemd/resolve/stub-resolv.conf,
// and Debian's /etc/alternatives entries point into /usr. Only the broad-root
// and coverage predicates apply, never a "must stay under /etc" rule. The
// returned paths keep their source spelling so each mount lands where the
// approved policy says it does.
func safeEtcPolicyPaths(paths []string, workspaceRoot, home string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		canon, err := filepath.EvalSymlinks(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("tools: bwrap inspect policy path %q: %w", p, err)
		}
		if !posixCleanAbs(canon) || bwrapBroadRoot(canon) {
			return nil, fmt.Errorf("tools: bwrap policy path %q resolves to broad root %q", p, canon)
		}
		if pathCovers(canon, workspaceRoot) || pathCovers(canon, home) {
			return nil, fmt.Errorf("tools: bwrap policy path %q (%q) covers the workspace or home", p, canon)
		}
		fi, err := os.Stat(canon)
		if err != nil {
			return nil, fmt.Errorf("tools: bwrap inspect policy path %q: %w", p, err)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil, fmt.Errorf("tools: bwrap policy path %q is neither a regular file nor a directory", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// rejectInheritableFDs enumerates /proc/self/fd immediately before spawn and
// fails closed on any host descriptor above stdio without FD_CLOEXEC: bwrap
// passes inherited descriptors through to the payload. An EBADF entry that
// vanished during the scan is ignored; the audit never mutates descriptors
// it does not own. The scan and os/exec are not one kernel-atomic operation
// — that residual interval is the documented D10 limitation.
func rejectInheritableFDs() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("tools: bwrap audit inherited fds: %w", err)
	}
	for _, e := range entries {
		fd, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			return fmt.Errorf("tools: bwrap audit inherited fds: unexpected entry %q", e.Name())
		}
		if fd <= 2 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == syscall.EBADF {
			continue // closed between the directory read and inspection
		}
		if errno != 0 {
			return fmt.Errorf("tools: bwrap audit fd %d: %w", fd, errno)
		}
		if flags&syscall.FD_CLOEXEC == 0 {
			return fmt.Errorf("tools: bwrap refuses to spawn with inheritable host fd %d (missing FD_CLOEXEC); "+
				"it was inherited from the process that launched this agent, which must set FD_CLOEXEC "+
				"on its own descriptors before spawning", fd)
		}
	}
	return nil
}

// prepare validates the re-checked spec, collects the per-invocation policy,
// and rewrites the spec to the [prlimit] bwrap chain. The outer chain
// receives a non-nil empty environment — the approved payload environment
// crosses only as --setenv policy arguments, so loader-control variables can
// never reach the TCB binaries. The stamped WorkspaceRoot is consumed as-is;
// re-resolving it here could silently change the policy after approval.
func (b *bwrapBackend) prepare(spec execSpec) (execSpec, error) {
	if len(spec.Argv) == 0 || spec.Path == "" || spec.Dir == "" {
		return execSpec{}, errors.New("tools: bwrap requires a non-empty argv, executable path, and working directory")
	}
	if spec.WorkspaceRoot == "" {
		return execSpec{}, errors.New("tools: bwrap requires a workspace root in the exec spec")
	}
	if !posixCleanAbs(spec.WorkspaceRoot) || bwrapBroadRoot(spec.WorkspaceRoot) {
		return execSpec{}, fmt.Errorf("tools: bwrap workspace root %q must be a canonical non-root path", spec.WorkspaceRoot)
	}
	roots, layoutDirs, links, err := b.collect(spec.WorkspaceRoot)
	if err != nil {
		return execSpec{}, err
	}
	etc := append([]string(nil), bwrapEtcLiterals...)
	if b.cfg.AllowNetwork {
		etc = append(etc, bwrapNetworkEtcLiterals...)
	}
	etc, err = safeEtcPolicyPaths(etc, spec.WorkspaceRoot, canonicalHome())
	if err != nil {
		return execSpec{}, err
	}
	// Resolve the approved executable before it becomes a bind source.
	// --ro-bind resolves its source in the kernel, so binding a symlink
	// spelling would mount whatever the link points at when bwrap runs, not
	// what was approved — and a workspace symlink is writable by the payload
	// itself, which the disclosed same-UID race does not cover. Binding the
	// canonical target closes that and matches the Seatbelt backend, which
	// has always canonicalized here. The approved spelling is still what is
	// executed: it resolves through the workspace bind to this same object.
	canonExe, err := filepath.EvalSymlinks(spec.Path)
	if err != nil {
		return execSpec{}, fmt.Errorf("tools: bwrap resolve executable target: %w", err)
	}
	if fi, serr := os.Stat(canonExe); serr != nil {
		return execSpec{}, fmt.Errorf("tools: bwrap inspect executable target: %w", serr)
	} else if !fi.Mode().IsRegular() {
		return execSpec{}, fmt.Errorf("tools: bwrap executable %q resolves to a non-regular file", spec.Path)
	}
	args, err := buildBwrapArgs(bwrapPolicy{
		workspaceRoot:   spec.WorkspaceRoot,
		exePath:         canonExe,
		chdir:           spec.Dir,
		systemReadRoots: roots,
		topLevelDirs:    layoutDirs,
		topLevelLinks:   links,
		etcLiterals:     etc,
		payloadEnv:      sandboxChildEnv(spec.Env, "/tmp"),
		allowNetwork:    b.cfg.AllowNetwork,
		tmpfsSizeBytes:  b.capBytes,
	})
	if err != nil {
		return execSpec{}, err
	}
	if err := validateSandboxWorkspaceLinks(spec.WorkspaceRoot); err != nil {
		return execSpec{}, err
	}
	wrapped := spec
	wrapped.Env = []string{} // non-nil: the outer prlimit/bwrap chain inherits nothing
	wrapped.Path = b.bwrapPath
	wrapped.Argv = append(append(append([]string{"bwrap"}, args...), spec.Path), spec.Argv[1:]...)
	if b.capBytes > 0 {
		wrapped.Argv = append([]string{"prlimit", "--as=" + strconv.FormatInt(b.capBytes, 10), b.bwrapPath}, wrapped.Argv[1:]...)
		wrapped.Path = b.prlimitPath
	}
	if err := rejectInheritableFDs(); err != nil {
		return execSpec{}, err
	}
	return wrapped, nil
}

// Run executes one foreground command inside its per-invocation namespaces.
// The private tmpfs mounts vanish with the namespace, so no cleanup exists.
func (b *bwrapBackend) Run(ctx context.Context, spec execSpec) (execResult, error) {
	wrapped, err := b.prepare(spec)
	if err != nil {
		return execResult{}, err
	}
	return b.runner.Run(ctx, wrapped)
}

// Start launches one background command through the same preparation as Run;
// the manager's group-termination and reap semantics are untouched.
func (b *bwrapBackend) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	wrapped, err := b.prepare(spec)
	if err != nil {
		return nil, err
	}
	return b.starter.Start(wrapped, stdout, stderr)
}
