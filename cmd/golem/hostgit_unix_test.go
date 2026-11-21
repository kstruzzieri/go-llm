//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func fakeGitScript(version, marker string) string {
	return `if [ "$1" = "--version" ]; then echo "git version ` + version + `"; exit 0; fi
` + `echo ran > ` + shellQuote(marker) + "\n"
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestHostGitCommandPrefixesEveryCall(t *testing.T) {
	ctx := context.Background()
	bin, ws, dir := resolvedTemp(t), resolvedTemp(t), resolvedTemp(t)
	fake := filepath.Join(bin, "git")
	writeExecutable(t, fake, `if [ "$1" = "--version" ]; then echo "git version 2.56.0"; exit 0; fi
for a in "$@"; do echo "$a"; done
`)
	env := []string{"PATH=/usr/bin:/bin", "FOO=bar"}
	cmd, err := hostGitCommand(ctx, fake, ws, dir, env, "status")
	if err != nil {
		t.Fatal(err)
	}
	want := append(slices.Clone(hostGitPrefix), "status")
	if !slices.Equal(cmd.Args[1:], want) {
		t.Fatalf("args = %q; want %q", cmd.Args[1:], want)
	}
	if cmd.Dir != dir || !slices.Equal(cmd.Env, env) {
		t.Fatalf("Dir=%q Env=%q", cmd.Dir, cmd.Env)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(out)); !slices.Equal(got, want) {
		t.Fatalf("ran with %q; want %q", got, want)
	}
}

func TestHostGitCommandRefusesOldGit(t *testing.T) {
	ctx := context.Background()
	bin, ws := resolvedTemp(t), resolvedTemp(t)
	marker := filepath.Join(bin, "marker")
	fake := filepath.Join(bin, "git")
	writeExecutable(t, fake, fakeGitScript("2.37.9", marker))
	_, err := hostGitCommand(ctx, fake, ws, ws, nil, "status")
	if err == nil || !strings.Contains(err.Error(), "2.37.9") || !strings.Contains(err.Error(), "git 2.38 or later") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("old git ran a non-version command")
	}
	bin2 := resolvedTemp(t)
	fake2 := filepath.Join(bin2, "git")
	writeExecutable(t, fake2, fakeGitScript("2.38.0", marker))
	if _, err := hostGitCommand(ctx, fake2, ws, ws, nil, "status"); err != nil {
		t.Fatalf("2.38.0 refused: %v", err)
	}
}

func TestHostGitCommandRefusesWorkspaceExecutable(t *testing.T) {
	ctx := context.Background()
	ws, out := resolvedTemp(t), resolvedTemp(t)
	marker := filepath.Join(out, "marker")
	if err := os.MkdirAll(filepath.Join(ws, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(ws, "bin", "git")
	writeExecutable(t, fake, "echo ran > "+shellQuote(marker)+"\necho 'git version 2.56.0'\n")
	link := filepath.Join(out, "gitlink")
	if err := os.Symlink(fake, link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fake, link} {
		_, err := hostGitCommand(ctx, p, ws, ws, nil, "status")
		if err == nil || !strings.Contains(err.Error(), "inside the workspace") || !strings.Contains(err.Error(), fake) {
			t.Fatalf("%s: err = %v", p, err)
		}
		if _, serr := os.Stat(marker); serr == nil {
			t.Fatalf("%s: workspace git was executed", p)
		}
	}
}

func TestHostGitCommandReprobesReplacedBinary(t *testing.T) {
	ctx := context.Background()
	bin, ws := resolvedTemp(t), resolvedTemp(t)
	p := filepath.Join(bin, "git")
	try := func(script string) error {
		writeExecutable(t, p, script)
		_, err := hostGitCommand(ctx, p, ws, ws, nil, "status")
		return err
	}
	if err := try("echo 'git version 2.56.0'\n"); err != nil {
		t.Fatal(err)
	}
	if err := try("echo 'git version 2.37.0' # padding to change size\n"); err == nil {
		t.Fatal("replaced old git accepted from cache")
	}
	if err := try("echo broken >&2; exit 1 # more padding here\n"); err == nil {
		t.Fatal("failing --version accepted")
	}
	if err := try("echo 'git version 2.56.0'\n"); err != nil {
		t.Fatalf("error was cached: %v", err)
	}
}

func TestHostGitCommandSurfacesLookupError(t *testing.T) {
	_, err := hostGitCommand(context.Background(), "definitely-not-a-git-binary-803", t.TempDir(), "", nil, "status")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
