package main

import (
	"os"
	"path/filepath"
	"runtime"
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
	goRoot := canonicalDir(t, runtime.GOROOT())
	wantRoots := []string{goRoot, real}
	slices.Sort(wantRoots)
	wantEnv := []string{"GOCACHE=<private temp>/gocache", "GOFLAGS=-buildvcs=false", "GOMODCACHE=" + real, "GOPROXY=off", "GOTOOLCHAIN=local"}
	flags := []string{"-allow-exec", "-sandbox-read-root", goRoot, "-sandbox-read-root", link,
		"-sandbox-env", "GOMODCACHE=" + link, "-sandbox-env", "GOPROXY=off", "-sandbox-env", "GOTOOLCHAIN=local",
		"-sandbox-env", "GOFLAGS=-buildvcs=false", "-sandbox-temp-env", "GOCACHE"}
	lines := withGolem(t, config, root, policy, flags, func(sess *replSession) {
		got := sess.runtime.EffectivePermissions().Command
		if !slices.Equal(got.ReadOnlyRoots, wantRoots) || !slices.Equal(got.Env, wantEnv) {
			t.Errorf("Command = %+v, want roots %q and Env %q", got, wantRoots, wantEnv)
		}
	})
	const before = "limits=deny-default profile with reviewed system reads, "
	const after = "allowed workspace and private temp; network operations denied; no memory/CPU quota"
	quoted := func(values []string) string {
		var out []string
		for _, v := range values {
			out = append(out, strconv.Quote(v))
		}
		return "[" + strings.Join(out, ", ") + "]"
	}
	want := before + "declared read-only roots, " + after + " read-only-roots=" + quoted(wantRoots) + " env=" + quoted(wantEnv)
	if !strings.Contains(lines[0], want) || strings.Contains(lines[0], link) {
		t.Fatalf("startup notice %q, want %q and not the link %s", lines[0], want, link)
	}
	lines = withGolem(t, config, root, policy, []string{"-allow-exec"}, func(*replSession) {})
	if !strings.Contains(lines[0], before+after+" scratch=") {
		t.Fatalf("default Seatbelt permissions changed: %s", lines[0])
	}

	home := canonicalDir(t, os.Getenv("HOME")) // golemWorkspace set a fresh test home.
	ssh := filepath.Join(home, ".ssh")
	if err := os.Mkdir(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"home", []string{"-sandbox", "seatbelt", "-sandbox-read-root", home}, "is or covers the home directory"},
		{"workspace", []string{"-sandbox", "seatbelt", "-sandbox-read-root", root}, "overlaps the workspace root"},
		{"empty", []string{"-sandbox", "seatbelt", "-sandbox-read-root", ""}, "-sandbox-read-root must not be empty"},
		{"proxy", []string{"-sandbox", "seatbelt", "-sandbox-env", "GOPROXY=https://proxy.golang.org"}, `GOPROXY accepts only "off"`},
		{"host", []string{"-sandbox-read-root", real}, "-sandbox-read-root requires -sandbox seatbelt"},
		{"excluded", []string{"-sandbox", "seatbelt", "-sandbox-read-root", ssh}, "excluded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, out, diag := runTestFiles(t)
			args := append([]string{"-config", config, "-root", root, "-allow-exec"}, tc.flags...)
			err := run(args, in, out, diag)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error()+readRunTestFile(t, diag), "https://proxy.golang.org") {
				t.Fatalf("startup refusal = %v, want %q with no proxy URL", err, tc.want)
			}
		})
	}
}
