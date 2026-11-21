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
	wsLink := filepath.Join(out, "wslink")
	if err := os.Symlink(ws, wsLink); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{fake, ws}, {link, ws}, {fake, wsLink}, {link, wsLink}} {
		p := c[0]
		_, err := hostGitCommand(ctx, p, c[1], ws, nil, "status")
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
	if err := try("echo 'git version 2.56.0'\n"); err != nil {
		t.Fatalf("fixed binary refused: %v", err)
	}
}

// TestHostGitCommandDoesNotCacheFailures calls hostGitCommand repeatedly on
// UNCHANGED binaries: a refusal or probe failure must be re-probed every time.
func TestHostGitCommandDoesNotCacheFailures(t *testing.T) {
	ctx := context.Background()
	ws := resolvedTemp(t)
	for name, body := range map[string]string{
		"old":     `echo "git version 2.37.9"`,
		"failing": `echo broken >&2; exit 1`,
	} {
		t.Run(name, func(t *testing.T) {
			bin := resolvedTemp(t)
			counter := filepath.Join(bin, "count")
			fake := filepath.Join(bin, "git")
			writeExecutable(t, fake, "echo x >> "+shellQuote(counter)+"\n"+body+"\n")
			for i := 0; i < 3; i++ {
				if _, err := hostGitCommand(ctx, fake, ws, ws, nil, "status"); err == nil {
					t.Fatalf("call %d accepted an unchanged bad git", i)
				}
			}
			data, err := os.ReadFile(counter)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(data), "\n"); n != 3 {
				t.Fatalf("probe ran %d times; want 3", n)
			}
		})
	}
}

func TestHostGitCommandRunsResolvedPath(t *testing.T) {
	ctx := context.Background()
	bin, ws, out := resolvedTemp(t), resolvedTemp(t), resolvedTemp(t)
	fake := filepath.Join(bin, "git")
	writeExecutable(t, fake, "echo 'git version 2.56.0'\n")
	link := filepath.Join(out, "gitlink")
	if err := os.Symlink(fake, link); err != nil {
		t.Fatal(err)
	}
	cmd, err := hostGitCommand(ctx, link, ws, ws, nil, "status")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != fake {
		t.Fatalf("Path = %q; want resolved %q", cmd.Path, fake)
	}
	if cmd.Args[0] != link {
		t.Fatalf("Args[0] = %q; want %q", cmd.Args[0], link)
	}
}

func TestHostGitCommandRefusesRelativePaths(t *testing.T) {
	ctx := context.Background()
	bin, ws := resolvedTemp(t), resolvedTemp(t)
	fake := filepath.Join(bin, "git")
	writeExecutable(t, fake, "echo 'git version 2.56.0'\n")
	// The attack: cwd holds an outside bin/git that passes every check, while
	// the command would run <dir>/bin/git, which is inside the workspace.
	cwd := resolvedTemp(t)
	if err := os.MkdirAll(filepath.Join(cwd, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(cwd, "bin", "git"), "echo 'git version 2.56.0'\n")
	marker := filepath.Join(cwd, "marker")
	if err := os.MkdirAll(filepath.Join(ws, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(ws, "bin", "git"), "echo ran > "+shellQuote(marker)+"\necho 'git version 2.56.0'\n")
	t.Chdir(cwd)
	_, err := hostGitCommand(ctx, "bin/git", ws, ws, nil, "status")
	if err == nil || !strings.Contains(err.Error(), "bin/git") {
		t.Errorf("relative git path: err = %v", err)
	}
	if _, serr := os.Stat(marker); serr == nil {
		t.Error("workspace git was executed")
	}
	for _, w := range []string{"", "ws"} {
		if _, err := hostGitCommand(ctx, fake, w, ws, nil, "status"); err == nil {
			t.Errorf("workspace %q accepted", w)
		}
	}
}

func TestHostGitCommandErrorsAreSanitized(t *testing.T) {
	ctx := context.Background()
	ws := resolvedTemp(t)
	evil := filepath.Join(ws, "a\x1b[31mb", "bin")
	if err := os.MkdirAll(evil, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(evil, "git")
	writeExecutable(t, fake, "echo 'git version 2.56.0'\n")
	_, err := hostGitCommand(ctx, fake, ws, ws, nil, "status")
	if err == nil {
		t.Fatal("workspace git accepted")
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("raw ESC in error: %q", err)
	}
	if !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("err = %q, want the inside-the-workspace refusal", err)
	}
}

func TestHostGitCommandRefusesCaseVariantWorkspacePath(t *testing.T) {
	base := resolvedTemp(t)
	ws := filepath.Join(base, "proj")
	if err := os.MkdirAll(filepath.Join(ws, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "PROJ")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	marker := filepath.Join(base, "marker")
	writeExecutable(t, filepath.Join(ws, "bin", "git"), "echo ran > "+shellQuote(marker)+"\necho 'git version 2.56.0'\n")
	_, err := hostGitCommand(context.Background(), filepath.Join(base, "PROJ", "bin", "git"), ws, ws, nil, "status")
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("err = %v, want the inside-the-workspace refusal", err)
	}
	requireNoMarker(t, marker)
}

func TestHostGitCommandDotDotPrefixedNames(t *testing.T) {
	base := resolvedTemp(t)
	ws := filepath.Join(base, "ws")
	inside := filepath.Join(ws, "..foo")
	outside := filepath.Join(base, "..foo-sibling")
	for _, d := range []string{inside, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(d, "git"), "echo 'git version 2.56.0'\n")
	}
	if _, err := hostGitCommand(context.Background(), filepath.Join(inside, "git"), ws, ws, nil, "status"); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("git in ..foo child: err = %v, want refusal", err)
	}
	if _, err := hostGitCommand(context.Background(), filepath.Join(outside, "git"), ws, ws, nil, "status"); err != nil {
		t.Fatalf("git in ..foo-sibling outside the workspace: %v", err)
	}
}

// A multi-call binary dispatches on argv[0]; the version probe must present the
// same name the real command does.
func TestHostGitCommandProbeUsesLookedUpArgv0(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH")
	}
	src := t.TempDir()
	writeTestFile(t, filepath.Join(src, "main.go"), "package main\nimport (\"fmt\"; \"os\"; \"path/filepath\")\nfunc main() {\n\tif filepath.Base(os.Args[0]) == \"git\" { fmt.Println(\"git version 2.56.0\"); return }\n\tfmt.Println(\"multitool 1.0\")\n}\n", 0o600)
	writeTestFile(t, filepath.Join(src, "go.mod"), "module multitool\n\ngo 1.21\n", 0o600)
	dir := resolvedTemp(t)
	tool := filepath.Join(dir, "multitool")
	build := exec.Command(goBin, "build", "-o", tool, ".")
	build.Dir = src
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	link := filepath.Join(dir, "git")
	if err := os.Symlink(tool, link); err != nil {
		t.Fatal(err)
	}
	if _, err := hostGitCommand(context.Background(), link, resolvedTemp(t), "", nil, "status"); err != nil {
		t.Fatalf("multi-call git shim refused: %v", err)
	}
}

func TestHostGitCommandSurfacesLookupError(t *testing.T) {
	_, err := hostGitCommand(context.Background(), "definitely-not-a-git-binary-803", t.TempDir(), "", nil, "status")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}
