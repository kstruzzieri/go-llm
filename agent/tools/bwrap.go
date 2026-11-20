package tools

// Bubblewrap policy construction (#441). This file is deliberately
// cross-platform so the security-critical argv construction is unit-tested on
// every CI platform, mirroring seatbelt.go's discipline for SBPL rendering.
// The Linux-only probe and process wiring live in exec_linux.go.

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
)

// bwrapBroadRoot rejects allowance/workspace positions that would reach the
// volume root or a reviewed top-level system/mount root. Subdirectories (e.g.
// /home/user/project) remain valid.
func bwrapBroadRoot(p string) bool {
	if p == "/" {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/")
	if !ok || strings.Contains(rest, "/") {
		return false // only "/" and top-level entries are candidates
	}
	switch "/" + rest {
	case "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32", "/lib64",
		"/lost+found", "/media", "/mnt", "/nix", "/opt", "/proc", "/root",
		"/run", "/sbin", "/snap", "/srv", "/sys", "/tmp", "/usr", "/var":
		return true
	}
	return false
}

// bwrapPolicy is the per-invocation input to bwrap argv construction. All
// paths must already be clean POSIX absolutes; collectors canonicalize the
// roots they own and retain reviewed alias destinations separately, while
// exePath is the resolved executable target. The builder validates lexical
// shape, treats every value as data (exec argv, no quoting layer), and
// performs no filesystem I/O so it is unit-testable on every platform.
type bwrapPolicy struct {
	workspaceRoot     string
	exePath           string
	chdir             string
	systemReadRoots   []string
	systemRootAliases map[string]string // original destination -> canonical source
	topLevelDirs      []string          // exact fixed-layout directory binds
	topLevelLinks     map[string]string // dest -> relative link target
	etcLiterals       []string
	payloadEnv        []string // ordered approved NAME=VALUE entries
	allowNetwork      bool
	tmpfsSizeBytes    int64
	gitWritable       []string // dot-git dirs and git dirs: rw self-bind (un-renamable mount point)
	gitReadOnly       []string // containers, intermediates, present metadata, modules-tree hooks/info dirs that are not git dirs, gitfiles: ro-bind over self
}

// bwrapLayoutDirs is the fixed set of top-level compatibility entries a
// usr-merged or split host may carry. Only these may appear in topLevelDirs
// or as topLevelLinks destinations.
var bwrapLayoutDirs = []string{"/bin", "/sbin", "/lib", "/lib32", "/lib64"}

// bwrapDefaultSystemRoots is the reviewed, minimal read-only execution
// surface, mirroring seatbeltDefaultSystemRoots' discipline: additions
// require a demonstrated denial, the narrowest rule, and a builder test.
// Every bwrapLayoutDirs entry's usr-merge target must be present here, or a
// benign standard layout fails the collector's covered-target check —
// observed on ubuntu-24.04, where the stock /lib32 -> usr/lib32 link failed
// prepare with: layout link "/lib32" resolves to "/usr/lib32" outside the
// reviewed read-only roots. Absent roots are omitted (strictly narrower).
var bwrapDefaultSystemRoots = []string{
	"/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib32", "/usr/lib64",
	"/usr/libexec", "/usr/share",
}

// bwrapEtcLiterals is the exec-critical /etc surface (dynamic linker cache
// and Debian's alternatives farm). bwrapNetworkEtcLiterals is appended only
// when the approved config allows network.
var bwrapEtcLiterals = []string{
	"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
	"/etc/alternatives",
}

var bwrapNetworkEtcLiterals = []string{
	"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf",
	"/etc/ssl/certs", "/etc/ssl/cert.pem", "/etc/pki/tls/certs",
	"/etc/pki/ca-trust/extracted",
}

// bwrapIsolationArgs is the single owner of the namespace/capability prefix
// shared by production invocations and the capability probe: fresh user
// namespace with nested creation disabled, PID/IPC/UTS isolation, network
// unshared unless the approved config allows it, monitor-tied lifetime, a new
// session (severs the controlling terminal), and all capabilities dropped.
func bwrapIsolationArgs(allowNetwork bool) []string {
	args := []string{
		"--unshare-user", "--disable-userns", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts",
	}
	if !allowNetwork {
		args = append(args, "--unshare-net")
	}
	return append(args, "--die-with-parent", "--new-session", "--cap-drop", "ALL")
}

// bwrapMemoryCapBytes converts the approved MemoryCapMB to the single byte
// value that feeds prlimit --as and both private tmpfs quotas. The checked
// conversion fails closed on values whose byte count is unrepresentable.
func bwrapMemoryCapBytes(memoryCapMB int) (int64, error) {
	if memoryCapMB == 0 {
		return 0, nil
	}
	// Bound against MaxInt64, not MaxInt: the result is int64, so a MaxInt
	// bound would reject a 4 GiB cap on 32-bit Linux while accepting it on
	// amd64 -- the same config answering differently per architecture.
	if memoryCapMB < 0 || int64(memoryCapMB) > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("tools: bwrap MemoryCapMB %d is outside the representable byte range", memoryCapMB)
	}
	return int64(memoryCapMB) * 1024 * 1024, nil
}

// bwrapConfigSupport rejects ceiling fields bwrap cannot faithfully enforce.
// CPULimit is a cores fraction with no rlimit equivalent (RLIMIT_CPU counts
// seconds), so it fails construction; MemoryCapMB must survive the checked
// byte conversion. DropCaps is accepted: the backend always drops ALL, which
// satisfies any requested subset under the #440 ceiling contract.
func bwrapConfigSupport(cfg SandboxConfig) error {
	if cfg.CPULimit != 0 {
		return fmt.Errorf("tools: bwrap cannot enforce CPULimit")
	}
	_, err := bwrapMemoryCapBytes(cfg.MemoryCapMB)
	return err
}

// bwrapPayloadEnvArgs renders the approved payload environment as bwrap
// --setenv policy arguments, preserving caller order. Only the strict exec
// allowlist names are accepted: the payload environment crosses the TCB as
// bwrap argv, so a malformed, duplicated, or unexpected name fails closed
// instead of widening what one approval can express.
func bwrapPayloadEnvArgs(env []string) ([]string, error) {
	allowed := make(map[string]bool, len(defaultExecEnvAllowlist))
	for _, name := range defaultExecEnvAllowlist {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(env))
	args := make([]string, 0, len(env)*3)
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" || !allowed[name] {
			return nil, fmt.Errorf("tools: bwrap payload env entry %q is not an approved NAME=VALUE allowlist entry", kv)
		}
		if seen[name] {
			return nil, fmt.Errorf("tools: bwrap payload env name %q is duplicated", name)
		}
		seen[name] = true
		args = append(args, "--setenv", name, value)
	}
	return args, nil
}

// bwrapExeCoveredByReadOnly reports whether the resolved executable path is
// lexically inside one of the read-only mounted regions (a reviewed system
// root, a fixed-layout directory bind, a fixed-layout symlink whose canonical
// target the collector already verified is covered, or a git metadata
// read-only mount). Among the git mounts containing the executable the deepest
// decides, because a later, deeper mount covers a shallower one: an executable
// in a writable git dir nested under a read-only container is not covered and
// keeps its read-only literal.
func bwrapExeCoveredByReadOnly(exe string, sysRoots, layoutDirs, linkDests, gitReadOnly, gitWritable []string) bool {
	within := func(root string) bool {
		return exe == root || strings.HasPrefix(exe, root+"/")
	}
	for _, set := range [][]string{sysRoots, layoutDirs, linkDests} {
		for _, root := range set {
			if within(root) {
				return true
			}
		}
	}
	deepest, readOnly := "", false
	for _, root := range gitWritable {
		if within(root) && len(root) > len(deepest) {
			deepest, readOnly = root, false
		}
	}
	for _, root := range gitReadOnly {
		if within(root) && len(root) > len(deepest) {
			deepest, readOnly = root, true
		}
	}
	return readOnly
}

// bwrapGitMetadataArgs renders the git metadata self-binds (#721). Binds apply
// in argv order and a later mount covers an earlier one at or below it, so the
// merged list is sorted by path: a parent is a strict prefix of its children
// and sorts first, letting a read-only container precede the writable re-bind
// of a child beneath it. Every path must be a canonical absolute strictly
// inside the workspace and may appear in only one list.
func bwrapGitMetadataArgs(workspaceRoot string, writable, readOnly []string) ([]string, error) {
	writable = slices.Compact(slices.Sorted(slices.Values(writable)))
	readOnly = slices.Compact(slices.Sorted(slices.Values(readOnly)))
	type mount struct{ path, flag string }
	var mounts []mount
	for _, group := range []struct {
		flag string
		set  []string
	}{{"--bind", writable}, {"--ro-bind", readOnly}} {
		flag := group.flag
		for _, p := range group.set {
			if !posixCleanAbs(p) || !strings.HasPrefix(p, workspaceRoot+"/") {
				return nil, fmt.Errorf("tools: bwrap git path %q must be canonical and inside workspace %q", p, workspaceRoot)
			}
			mounts = append(mounts, mount{p, flag})
		}
	}
	for _, p := range writable {
		if slices.Contains(readOnly, p) {
			return nil, fmt.Errorf("tools: bwrap git path %q is both writable and read-only", p)
		}
	}
	slices.SortFunc(mounts, func(a, b mount) int { return strings.Compare(a.path, b.path) })
	args := make([]string, 0, len(mounts)*3)
	for _, m := range mounts {
		args = append(args, m.flag, m.path, m.path)
	}
	return args, nil
}

// buildBwrapArgs renders the bwrap policy argv for one invocation in the
// fixed D5 order: isolation prefix, payload environment, /proc and /dev,
// quota'd private /dev/shm, reviewed read-only system roots and aliases,
// fixed-layout directories and symlinks, /etc literals, quota'd private /tmp, the
// workspace as the only host-visible writable mount (git metadata then
// re-mounts paths inside it: read-only protections and writable self-binds that
// keep their directories un-renamable), the executable literal,
// the approved cwd, and finally the read-only root remount. List inputs are
// sorted and de-duplicated into owned copies; ordering is part of the policy.
func buildBwrapArgs(p bwrapPolicy) ([]string, error) {
	if !posixCleanAbs(p.workspaceRoot) || bwrapBroadRoot(p.workspaceRoot) {
		return nil, fmt.Errorf("tools: bwrap workspace root %q must be a canonical non-root path", p.workspaceRoot)
	}
	if !posixCleanAbs(p.exePath) || bwrapBroadRoot(p.exePath) {
		return nil, fmt.Errorf("tools: bwrap executable path %q must be canonical and outside broad roots", p.exePath)
	}
	if p.chdir == "" || !posixCleanAbs(p.chdir) ||
		(p.chdir != p.workspaceRoot && !strings.HasPrefix(p.chdir, p.workspaceRoot+"/")) {
		return nil, fmt.Errorf("tools: bwrap chdir %q must be a canonical path inside workspace %q", p.chdir, p.workspaceRoot)
	}
	if p.tmpfsSizeBytes < 0 {
		return nil, fmt.Errorf("tools: bwrap tmpfs size must not be negative")
	}

	sysRoots := slices.Compact(slices.Sorted(slices.Values(p.systemReadRoots)))
	sysRootSet := make(map[string]bool, len(sysRoots))
	for _, root := range sysRoots {
		// Depth >= 2 rather than the broad-root name list: every reviewed root
		// is /usr/<x>, and a canonicalized root that lands on an unlisted
		// top-level name (a /usr/share aliased to /data) would otherwise mount
		// that whole volume read-only. Layout entries are depth 1 by
		// definition and are validated against the fixed set instead.
		if !posixCleanAbs(root) || strings.Count(root, "/") < 2 {
			return nil, fmt.Errorf("tools: bwrap system read root %q must be canonical and at least two components deep", root)
		}
		// Containment, not equality: a root at or above the workspace would
		// mount user data read-only. collectSystemRoots enforces this on the
		// canonicalized roots, but b.collect is injectable, so the builder --
		// the part unit-tested on every platform -- must hold the invariant too.
		if pathCovers(root, p.workspaceRoot) {
			return nil, fmt.Errorf("tools: bwrap system read root %q covers the workspace %q", root, p.workspaceRoot)
		}
		sysRootSet[root] = true
	}
	aliasDests := slices.Sorted(maps.Keys(p.systemRootAliases))
	for _, dest := range aliasDests {
		source := p.systemRootAliases[dest]
		if !slices.Contains(bwrapDefaultSystemRoots, dest) {
			return nil, fmt.Errorf("tools: bwrap system root alias destination %q is outside the reviewed root set", dest)
		}
		if !sysRootSet[source] {
			return nil, fmt.Errorf("tools: bwrap system root alias %q references uncollected source %q", dest, source)
		}
		if dest == source || sysRootSet[dest] {
			return nil, fmt.Errorf("tools: bwrap system root alias destination %q is already canonical", dest)
		}
	}

	layoutSet := make(map[string]bool, len(bwrapLayoutDirs))
	for _, dir := range bwrapLayoutDirs {
		layoutSet[dir] = true
	}
	layoutDirs := slices.Compact(slices.Sorted(slices.Values(p.topLevelDirs)))
	for _, dir := range layoutDirs {
		if !layoutSet[dir] {
			return nil, fmt.Errorf("tools: bwrap layout entry %q is outside the fixed layout set", dir)
		}
	}
	linkDests := slices.Sorted(maps.Keys(p.topLevelLinks))
	for _, dest := range linkDests {
		target := p.topLevelLinks[dest]
		if !layoutSet[dest] {
			return nil, fmt.Errorf("tools: bwrap layout link %q is outside the fixed layout set", dest)
		}
		if target == "" || strings.HasPrefix(target, "/") || containsControl(target) {
			return nil, fmt.Errorf("tools: bwrap layout link %q needs a relative target, got %q", dest, target)
		}
		if slices.Contains(layoutDirs, dest) {
			return nil, fmt.Errorf("tools: bwrap layout entry %q appears as both directory and symlink", dest)
		}
	}

	etcLits := slices.Compact(slices.Sorted(slices.Values(p.etcLiterals)))
	for _, lit := range etcLits {
		if !posixCleanAbs(lit) || !strings.HasPrefix(lit, "/etc/") {
			return nil, fmt.Errorf("tools: bwrap literal %q must live under /etc/", lit)
		}
	}

	envArgs, err := bwrapPayloadEnvArgs(p.payloadEnv)
	if err != nil {
		return nil, err
	}

	size := strconv.FormatInt(p.tmpfsSizeBytes, 10)
	args := bwrapIsolationArgs(p.allowNetwork)
	args = append(args, "--clearenv")
	args = append(args, envArgs...)
	args = append(args, "--proc", "/proc", "--dev", "/dev")
	if p.tmpfsSizeBytes > 0 {
		args = append(args, "--size", size)
	}
	args = append(args, "--tmpfs", "/dev/shm")
	for _, root := range sysRoots {
		args = append(args, "--ro-bind", root, root)
	}
	for _, dest := range aliasDests {
		args = append(args, "--ro-bind", p.systemRootAliases[dest], dest)
	}
	for _, dir := range layoutDirs {
		args = append(args, "--ro-bind", dir, dir)
	}
	for _, dest := range linkDests {
		args = append(args, "--symlink", p.topLevelLinks[dest], dest)
	}
	for _, lit := range etcLits {
		args = append(args, "--ro-bind", lit, lit)
	}
	// The private /tmp mounts before the workspace bind so a workspace living
	// under the host's /tmp is bound over the tmpfs and stays visible.
	if p.tmpfsSizeBytes > 0 {
		args = append(args, "--size", size)
	}
	args = append(args, "--tmpfs", "/tmp")
	args = append(args, "--bind", p.workspaceRoot, p.workspaceRoot)
	// Git metadata mounts follow the workspace bind and precede the executable
	// literal, so an executable inside a writable git dir keeps its read-only
	// literal on top.
	gitArgs, err := bwrapGitMetadataArgs(p.workspaceRoot, p.gitWritable, p.gitReadOnly)
	if err != nil {
		return nil, err
	}
	args = append(args, gitArgs...)
	// The executable literal is emitted only when the resolved path is NOT
	// already inside a read-only mounted region: a redundant file bind into
	// an RO bind fails mountpoint creation ("Can't create file"), and the
	// executable is already readable there. An out-of-policy path keeps the
	// literal so the payload is reachable at all (bwrap creates the parent
	// spine in the still-writable base tmpfs), and a workspace-resident
	// executable keeps it read-only over the rw bind, which additionally
	// blocks self-modification of the approved binary -- except where the
	// workspace itself sits under a fixed-layout directory, where the layout
	// bind already covers it and that secondary property does not apply.
	if !bwrapExeCoveredByReadOnly(p.exePath, sysRoots, layoutDirs, linkDests, p.gitReadOnly, p.gitWritable) {
		args = append(args, "--ro-bind", p.exePath, p.exePath)
	}
	args = append(args, "--chdir", p.chdir)
	// --dev creates its own tmpfs, and --size is a next-operation modifier
	// that attaches to the following --tmpfs (/dev/shm), so /dev itself is
	// otherwise writable and unquota'd: a payload can fill host RAM through
	// /dev/<file> under an approved memory cap, bounded by neither RLIMIT_AS
	// (tmpfs pages are page cache) nor either --size. Remounting it read-only
	// closes that sink; device writes such as /dev/null are governed by the
	// device, not the mount, and /dev/shm is a separate mount that keeps its
	// own quota and write access.
	args = append(args, "--remount-ro", "/dev")
	args = append(args, "--remount-ro", "/")
	return args, nil
}
