//go:build darwin

package tools

// The developer directory on macOS (#722 D3): the host helper that reads the
// active selection, the lstat adapter behind checkDeveloperLinks, and the
// metadata literals the Seatbelt profile grants with a developer directory.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// seatbeltDeveloperLiterals are the exact file-read-metadata literals a
// developer directory needs (#722 A0 F4): /var, the spelling the xcode-select
// shim reads, and the two selection links. The link entries stay only if the
// linked-Xcode CI leg shows a denial without them (D3).
var seatbeltDeveloperLiterals = []string{"/var", "/private/var/select/developer_dir", "/private/var/db/xcode_select_link"}

// seatbeltLinkLstat is the lstat construction reads the selection links
// through (checkDeveloperLinks); tests replace it.
var seatbeltLinkLstat = lstatLinkInfo

// lstatLinkInfo is os.Lstat as a linkInfo: a path that does not exist is not
// an error, and the owner is Stat_t's.
func lstatLinkInfo(path string) (linkInfo, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return linkInfo{}, nil
	}
	if err != nil {
		return linkInfo{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return linkInfo{}, fmt.Errorf("tools: no owner recorded for %q", path)
	}
	return linkInfo{exists: true, symlink: fi.Mode()&fs.ModeSymlink != 0, uid: st.Uid}, nil
}

// xcodeSelectPath is the system xcode-select; tests point it at a stand-in.
var xcodeSelectPath = "/usr/bin/xcode-select"

// activeDeveloperDirectoryTimeout bounds one xcode-select run on top of the
// caller's context.
var activeDeveloperDirectoryTimeout = 5 * time.Second

// xcodeSelectReasonMax bounds the stderr line ActiveDeveloperDirectory quotes
// in its error when xcode-select fails.
const xcodeSelectReasonMax = 200

// ActiveDeveloperDirectory returns the canonical spelling of the host's
// active developer directory, for a host to declare as
// SandboxConfig.DeveloperDirectory. It runs xcode-select -p with
// DEVELOPER_DIR removed from its environment, so the system selection
// decides, under a timeout on top of ctx; refuses output that is not one
// clean absolute path; and requires the standalone Apple Command Line Tools.
// Full Xcode shims require state outside the declared bundle. An unsupported
// selection wraps errors.ErrUnsupported. Every failure wraps ErrSandboxPolicy.
func ActiveDeveloperDirectory(ctx context.Context) (string, error) {
	return selectedCommandLineTools(ctx, "/Library/Developer/CommandLineTools")
}

func selectedCommandLineTools(ctx context.Context, supported string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, activeDeveloperDirectoryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, xcodeSelectPath, "-p")
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "DEVELOPER_DIR=") })
	cmd.WaitDelay = execWaitDelay
	out, err := cmd.Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("tools: active developer directory: %w: xcode-select -p: %w", ErrSandboxPolicy, ctxErr)
		}
		// The tool's own reason (for example, no developer tools installed)
		// is what the user can act on: its first stderr line, quoted and
		// bounded, follows the exit status.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if reason, _, _ := strings.Cut(strings.TrimSpace(string(exitErr.Stderr)), "\n"); reason != "" {
				if len(reason) > xcodeSelectReasonMax {
					reason = reason[:xcodeSelectReasonMax]
				}
				return "", fmt.Errorf("tools: active developer directory: %w: xcode-select -p: %w: %q", ErrSandboxPolicy, err, reason)
			}
		}
		return "", fmt.Errorf("tools: active developer directory: %w: xcode-select -p: %w", ErrSandboxPolicy, err)
	}
	dir := strings.TrimSuffix(string(out), "\n")
	if !toolchainPath(dir) {
		return "", fmt.Errorf("tools: active developer directory: %w: xcode-select -p printed not one clean absolute path", ErrSandboxPolicy)
	}
	canon, err := CanonicalToolchainRoot(dir)
	if err != nil {
		return "", fmt.Errorf("tools: active developer directory: %w: %w", ErrSandboxPolicy, err)
	}
	if canon != supported {
		return "", fmt.Errorf("tools: active developer directory: %w: %w: select the standalone Command Line Tools at %s", ErrSandboxPolicy, errors.ErrUnsupported, supported)
	}
	return canon, nil
}
