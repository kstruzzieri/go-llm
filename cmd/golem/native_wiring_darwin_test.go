package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestNativeWiringToolchainNotice: a -sandbox-read-root given as a symlink,
// and a GOMODCACHE typed through that link, reach the Seatbelt session through
// run() in the root's canonical spelling, and the startup permissions notice
// shows that spelling and every variable a command gets (#722). macOS only:
// the toolchain flags are Seatbelt's, so the Linux run has no skip.
func TestNativeWiringToolchainNotice(t *testing.T) {
	config, root := golemWorkspace(t)
	policy := nativePolicyForTest(t, root)
	real := canonicalDir(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "toolchain")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{"GOCACHE=<private temp>/gocache", "GOMODCACHE=" + filepath.Join(real, "mod"), "GOPROXY=off"}
	lines := withGolem(t, config, root, policy, []string{"-sandbox-read-root", link,
		"-sandbox-env", "GOMODCACHE=" + filepath.Join(link, "mod"), "-sandbox-env", "GOPROXY=off", "-sandbox-temp-env", "GOCACHE"},
		func(sess *replSession) {
			got := sess.runtime.EffectivePermissions().Command
			if !slices.Equal(got.ReadOnlyRoots, []string{real}) || !slices.Equal(got.Env, wantEnv) {
				t.Errorf("Command = %+v, want roots [%s] and Env %q", got, real, wantEnv)
			}
		})
	want := " read-only-roots=[" + strconv.Quote(real) + "] env=[" + strconv.Quote(wantEnv[0]) + ", " +
		strconv.Quote(wantEnv[1]) + ", " + strconv.Quote(wantEnv[2]) + "] "
	if !strings.Contains(lines[0], want) || strings.Contains(lines[0], link) {
		t.Fatalf("startup notice %q, want %q and not the link %s", lines[0], want, link)
	}
}
