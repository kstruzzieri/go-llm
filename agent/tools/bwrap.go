package tools

// Bubblewrap policy construction (#441). This file is deliberately
// cross-platform so the security-critical argv construction is unit-tested on
// every CI platform, mirroring seatbelt.go's discipline for SBPL rendering.
// The Linux-only probe and process wiring live in exec_linux.go.

import (
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
