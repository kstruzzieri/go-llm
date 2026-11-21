//go:build darwin

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeXcodeSelect points the xcode-select seam at a shell script whose body
// runs only when it is called with exactly "-p", and restores the real path
// when the test ends.
func fakeXcodeSelect(t *testing.T, body string) {
	t.Helper()
	script := filepath.Join(t.TempDir(), "xcode-select")
	src := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = -p ] || { echo bad argv >&2; exit 9; }\n" + body + "\n"
	if err := os.WriteFile(script, []byte(src), 0o700); err != nil {
		t.Fatal(err)
	}
	old := xcodeSelectPath
	xcodeSelectPath = script
	t.Cleanup(func() { xcodeSelectPath = old })
}

// TestSeatbeltActiveDeveloperDirectory pins the host helper (#722 D3)
// through a stand-in xcode-select, so no row depends on this Mac's
// selection: DEVELOPER_DIR never reaches the child, an app bundle's
// Contents/Developer maps to the bundle, the result is canonical, and output
// that is not one clean absolute path, a non-zero exit and a hung command
// are refused. Valid controls run first.
func TestSeatbeltActiveDeveloperDirectory(t *testing.T) {
	canon := canonTempDirT(t)
	mkdirT(t, filepath.Join(canon, "CommandLineTools"))
	mkdirT(t, filepath.Join(canon, "Xcode.app", "Contents", "Developer"))
	mkdirT(t, filepath.Join(canon, "Env.app", "Contents", "Developer"))
	if err := os.Symlink(filepath.Join(canon, "Xcode.app"), filepath.Join(canon, "current")); err != nil {
		t.Fatal(err)
	}
	// Real xcode-select prints DEVELOPER_DIR when it is set; the stand-in
	// does too, so a leaked variable shows as the wrong directory.
	t.Setenv("DEVELOPER_DIR", filepath.Join(canon, "Env.app", "Contents", "Developer"))
	clt := filepath.Join(canon, "CommandLineTools")
	say := func(out string) string { return `printf '%s\n' "${DEVELOPER_DIR:-` + out + `}"` }

	valid := []struct{ name, body, want string }{
		{"command line tools", say(clt), clt},
		{"app bundle", say(filepath.Join(canon, "Xcode.app", "Contents", "Developer")), filepath.Join(canon, "Xcode.app")},
		{"canonical spelling", say(filepath.Join(canon, "current", "Contents", "Developer")), filepath.Join(canon, "Xcode.app")},
		{"no trailing newline", `printf '%s' "${DEVELOPER_DIR:-` + clt + `}"`, clt},
	}
	for _, r := range valid {
		t.Run(r.name, func(t *testing.T) {
			fakeXcodeSelect(t, r.body)
			got, err := ActiveDeveloperDirectory(context.Background())
			if err != nil || got != r.want {
				t.Fatalf("ActiveDeveloperDirectory = %q, %v; want %q", got, err, r.want)
			}
			if c, err := CanonicalToolchainRoot(got); err != nil || c != got {
				t.Fatalf("CanonicalToolchainRoot(%q) = %q, %v; want it unchanged", got, c, err)
			}
		})
	}

	refused := []struct{ name, body, reason string }{
		{"relative", say("Library/Developer/CommandLineTools"), "not one clean absolute path"},
		{"unclean", say(canon + "/Xcode.app/../CommandLineTools"), "not one clean absolute path"},
		{"trailing slash", say(clt + "/"), "not one clean absolute path"},
		{"empty", "exit 0", "not one clean absolute path"},
		{"two lines", say(clt) + "\n" + say(clt), "not one clean absolute path"},
		{"control character", `printf '%s\t\n' "${DEVELOPER_DIR:-` + clt + `}"`, "not one clean absolute path"},
		{"missing directory", say(filepath.Join(canon, "missing")), "no such file"},
		{"non-zero exit", "echo 'xcode-select: error: no developer tools' >&2; exit 2", "exit status 2"},
	}
	for _, r := range refused {
		t.Run(r.name, func(t *testing.T) {
			fakeXcodeSelect(t, r.body)
			got, err := ActiveDeveloperDirectory(context.Background())
			if !errors.Is(err, ErrSandboxPolicy) || !strings.Contains(err.Error(), r.reason) || got != "" {
				t.Fatalf("ActiveDeveloperDirectory = %q, %v; want ErrSandboxPolicy with reason %q", got, err, r.reason)
			}
		})
	}

	// A hung command returns at the deadline: the helper's own bound with a
	// caller context that never ends, and the caller's deadline under the
	// helper's default bound.
	deadlines := map[string]func(t *testing.T) context.Context{
		"helper bound": func(t *testing.T) context.Context {
			old := activeDeveloperDirectoryTimeout
			activeDeveloperDirectoryTimeout = 200 * time.Millisecond
			t.Cleanup(func() { activeDeveloperDirectoryTimeout = old })
			return context.Background()
		},
		"caller deadline": func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		},
	}
	for name, ctxFor := range deadlines {
		t.Run("hung command, "+name, func(t *testing.T) {
			fakeXcodeSelect(t, "exec sleep 30")
			ctx := ctxFor(t)
			start := time.Now()
			got, err := ActiveDeveloperDirectory(ctx)
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("ActiveDeveloperDirectory returned after %v; want the 200ms deadline", elapsed)
			}
			if !errors.Is(err, ErrSandboxPolicy) || !strings.Contains(err.Error(), "deadline exceeded") || got != "" {
				t.Fatalf("ActiveDeveloperDirectory = %q, %v; want ErrSandboxPolicy with a deadline", got, err)
			}
		})
	}
}

// TestSeatbeltActiveDeveloperDirectoryHost runs the real xcode-select once,
// with DEVELOPER_DIR set to a path that does not exist: a leak would fail
// canonicalization. Hosts with no developer directory selected skip, judged
// by running xcode-select directly.
func TestSeatbeltActiveDeveloperDirectoryHost(t *testing.T) {
	if err := exec.Command(xcodeSelectPath, "-p").Run(); err != nil {
		t.Skipf("no active developer directory on this host: %v", err)
	}
	t.Setenv("DEVELOPER_DIR", "/nonexistent-go-llm-developer-dir")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := ActiveDeveloperDirectory(ctx)
	if err != nil {
		t.Fatalf("ActiveDeveloperDirectory: %v", err)
	}
	if c, err := CanonicalToolchainRoot(got); err != nil || c != got || strings.HasSuffix(got, "/Contents/Developer") {
		t.Fatalf("ActiveDeveloperDirectory = %q (canonical %q, %v); want a canonical root, an app bundle rather than its Contents/Developer", got, c, err)
	}
}

// TestSeatbeltLinkLstat pins the production adapter behind
// seatbeltLinkLstat: a missing path is absent, not an error; a symlink is
// reported as one, with its owner; / is a root-owned directory; and an lstat
// error other than "does not exist" (a path under a regular file) is
// returned, so the link check refuses it.
func TestSeatbeltLinkLstat(t *testing.T) {
	base := canonTempDirT(t)
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "missing"), link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		path string
		want linkInfo
	}{
		{filepath.Join(base, "missing"), linkInfo{}},
		{link, linkInfo{exists: true, symlink: true, uid: uint32(os.Getuid())}},
		{"/", linkInfo{exists: true}},
	} {
		if got, err := lstatLinkInfo(r.path); err != nil || got != r.want {
			t.Fatalf("lstatLinkInfo(%q) = %+v, %v; want %+v", r.path, got, err, r.want)
		}
	}
	if got, err := lstatLinkInfo(filepath.Join(file, "child")); err == nil {
		t.Fatalf("lstatLinkInfo under a regular file = %+v, nil; want an error", got)
	}
}
