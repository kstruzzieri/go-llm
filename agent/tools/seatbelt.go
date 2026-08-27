package tools

// Seatbelt (macOS sandbox-exec) profile generation (#442). This file is
// deliberately cross-platform so the profile builder — the security-critical
// string construction — is unit-tested on every CI platform. The Darwin-only
// probe and process wiring live in exec_darwin.go.

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"
)

// seatbeltCleanAbs reports whether p is an absolute POSIX path in canonical
// spelling. SBPL paths are always POSIX (Seatbelt exists only on macOS), so
// the stdlib path package is used deliberately instead of filepath: the
// builder must behave identically on the Linux CI hosts that unit-test it.
// Canonical spelling matters for security, not style — the kernel normalizes
// paths before matching, so an un-normalized spelling (dot-dot, trailing
// slash) could evade the broad-root string checks while enforcing something
// else entirely.
func seatbeltCleanAbs(p string) bool {
	return strings.HasPrefix(p, "/") && path.Clean(p) == p
}

// sbplQuote renders path as an SBPL string literal. Paths are data: backslash
// and double quote are escaped, and any invalid UTF-8 or Unicode control
// character (which could smuggle profile syntax past the quoting) is rejected
// outright. Valid bytes are preserved exactly — never replaced with U+FFFD.
func sbplQuote(path string) (string, error) {
	if !utf8.ValidString(path) {
		return "", fmt.Errorf("tools: seatbelt path %q is not valid UTF-8", path)
	}
	if containsControl(path) {
		return "", fmt.Errorf("tools: seatbelt path %q contains control characters", path)
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range path {
		if r == '\\' || r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String(), nil
}

// seatbeltMetadataAncestors returns the sorted, de-duplicated strict parent
// directories (including "/") of every given absolute path. Path resolution
// stats each component, so a deny-default profile needs exact metadata
// literals for the traversal spine of every allowance — and nothing more.
func seatbeltMetadataAncestors(paths []string) ([]string, error) {
	seen := map[string]bool{}
	for _, p := range paths {
		if !seatbeltCleanAbs(p) {
			return nil, fmt.Errorf("tools: seatbelt ancestor computation requires clean absolute paths, got %q", p)
		}
		for dir := path.Dir(p); ; dir = path.Dir(dir) {
			seen[dir] = true
			if dir == "/" {
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for dir := range seen {
		out = append(out, dir)
	}
	slices.Sort(out)
	return out, nil
}

// seatbeltPolicy is the per-invocation input to profile generation. All paths
// must already be canonical (symlink-free); the builder validates shape and
// treats them as data, performing no filesystem I/O.
type seatbeltPolicy struct {
	workspaceRoot     string
	tempRoot          string
	exePath           string
	systemReadRoots   []string
	metadataAncestors []string
	allowNetwork      bool
}

// seatbeltBroadRoot rejects allowance positions that would reach beyond the
// reviewed policy: the volume root, bare /System, and the /System/Volumes/Data
// firmlink, which is an alternate mount path to user data on current macOS.
func seatbeltBroadRoot(p string) bool {
	return p == "/" || p == "/System" || p == "/System/Volumes/Data" ||
		strings.HasPrefix(p, "/System/Volumes/Data/")
}

func seatbeltAllowancePath(kind, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("tools: seatbelt profile requires a %s path", kind)
	}
	if !seatbeltCleanAbs(p) {
		return "", fmt.Errorf("tools: seatbelt %s path %q must be absolute and in canonical spelling", kind, p)
	}
	if seatbeltBroadRoot(p) {
		return "", fmt.Errorf("tools: seatbelt %s path %q is a broad or data-bearing root", kind, p)
	}
	return sbplQuote(p)
}

// buildSeatbeltProfile renders the SBPL profile for one invocation:
// deny-default, exec allowed, reads restricted to the reviewed system roots +
// workspace + private temp + the exact executable and device literals,
// metadata scoped to allowed subtrees plus exact traversal ancestors, writes
// restricted to workspace + private temp, and network denied unless the
// approved config allows it. Rendering is deterministic: list inputs are
// sorted and de-duplicated into owned copies.
func buildSeatbeltProfile(p seatbeltPolicy) (string, error) {
	wsQ, err := seatbeltAllowancePath("workspace root", p.workspaceRoot)
	if err != nil {
		return "", err
	}
	tempQ, err := seatbeltAllowancePath("private temp", p.tempRoot)
	if err != nil {
		return "", err
	}
	exeQ, err := seatbeltAllowancePath("executable", p.exePath)
	if err != nil {
		return "", err
	}
	sysRoots := slices.Compact(slices.Sorted(slices.Values(p.systemReadRoots)))
	sysQ := make([]string, 0, len(sysRoots))
	for _, root := range sysRoots {
		q, err := seatbeltAllowancePath("system read root", root)
		if err != nil {
			return "", err
		}
		sysQ = append(sysQ, q)
	}
	ancestors := slices.Compact(slices.Sorted(slices.Values(p.metadataAncestors)))
	ancQ := make([]string, 0, len(ancestors))
	for _, dir := range ancestors {
		if !seatbeltCleanAbs(dir) {
			return "", fmt.Errorf("tools: seatbelt metadata ancestor %q must be absolute and in canonical spelling", dir)
		}
		q, err := sbplQuote(dir)
		if err != nil {
			return "", err
		}
		ancQ = append(ancQ, q)
	}

	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	b.WriteString("(allow process-exec*)\n(allow process-fork)\n")

	b.WriteString("(allow file-read*")
	for _, q := range sysQ {
		fmt.Fprintf(&b, "\n  (subpath %s)", q)
	}
	fmt.Fprintf(&b, "\n  (subpath %s)", wsQ)
	fmt.Fprintf(&b, "\n  (subpath %s)", tempQ)
	fmt.Fprintf(&b, "\n  (literal %s)", exeQ)
	b.WriteString("\n  (literal \"/dev/null\")")
	b.WriteString("\n  (literal \"/dev/random\")")
	b.WriteString("\n  (literal \"/dev/urandom\"))\n")

	b.WriteString("(allow file-read-metadata")
	for _, q := range ancQ {
		fmt.Fprintf(&b, "\n  (literal %s)", q)
	}
	b.WriteString(")\n")

	fmt.Fprintf(&b, "(allow file-write*\n  (subpath %s)\n  (subpath %s))\n", wsQ, tempQ)
	b.WriteString("(allow file-write-data (literal \"/dev/null\"))\n")

	if p.allowNetwork {
		b.WriteString("(allow network*)\n")
	}
	return b.String(), nil
}

// seatbeltConfigSupport rejects ceiling fields Seatbelt cannot enforce. The
// #440 contract forbids silently providing less isolation than approved, so an
// unenforceable request fails construction instead of being ignored.
func seatbeltConfigSupport(cfg SandboxConfig) error {
	var unsupported []string
	if cfg.MemoryCapMB != 0 {
		unsupported = append(unsupported, "MemoryCapMB")
	}
	if cfg.CPULimit != 0 {
		unsupported = append(unsupported, "CPULimit")
	}
	if len(cfg.DropCaps) != 0 {
		unsupported = append(unsupported, "DropCaps")
	}
	if len(unsupported) != 0 {
		return fmt.Errorf("tools: seatbelt cannot enforce %s", strings.Join(unsupported, ", "))
	}
	return nil
}
