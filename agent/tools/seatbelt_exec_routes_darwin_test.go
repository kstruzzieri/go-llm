//go:build darwin

package tools

// Behavioral coverage for the exec-scoping fix (#708 follow-through to #576).
// `756fb566` scoped (allow process-exec*) to the read allowances; these tests
// pin the routes a bare exec rule used to open — a workspace symlink, an
// outside shebang interpreter, an approved script with an outside interpreter,
// and posix_spawn (including POSIX_SPAWN_SETEXEC) — and assert the
// route-specific refusal each one produces. Each has a positive control, and a
// matching mutation (revert to the bare rule, drop the workspace exec subpath)
// is recorded in docs/superpowers/evidence/708/mutations.md.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seatbeltMarker is the string a run leaks to stdout only if the outside image
// actually executed; its absence plus the route's refusal oracle is the denial.
const seatbeltMarker = "SEATBELT-EXEC-ROUTE-RAN"

// seatbeltHasCC reports the C compiler path, or skips — or, under
// GO_LLM_REQUIRE_SEATBELT=1, fails — when none is on this host. The posix_spawn
// route needs a compiled launcher, which the macos-latest runner has.
func seatbeltHasCC(t *testing.T) string {
	t.Helper()
	for _, cc := range []string{"/usr/bin/cc", "/usr/bin/clang"} {
		if fi, err := os.Stat(cc); err == nil && !fi.IsDir() {
			return cc
		}
	}
	if os.Getenv("GO_LLM_REQUIRE_SEATBELT") == "1" {
		t.Fatal("GO_LLM_REQUIRE_SEATBELT=1 but no C compiler to build the posix_spawn launcher")
	}
	t.Skip("no C compiler on this host")
	return ""
}

// buildSeatbeltExe compiles src to dir/name and returns the path. The build
// runs on the host, outside any sandbox.
func buildSeatbeltExe(t *testing.T, cc, dir, name, src string) string {
	t.Helper()
	return buildSeatbeltExeArgs(t, cc, dir, name, src, nil)
}

// buildSeatbeltExeArgs is buildSeatbeltExe with extra compiler flags (e.g.
// -DSETEXEC).
func buildSeatbeltExeArgs(t *testing.T, cc, dir, name, src string, extraArgs []string) string {
	t.Helper()
	cfile := filepath.Join(t.TempDir(), name+".c")
	if err := os.WriteFile(cfile, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{cfile, "-o", out}, extraArgs...)
	build := exec.CommandContext(ctx, cc, args...)
	build.Env = []string{"PATH=/usr/bin:/bin", "TMPDIR=" + t.TempDir()}
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, b)
	}
	return out
}

// markerExeSource prints seatbeltMarker and exits 0 — a self-contained native
// executable with no external dependencies, so it loads under any read roots.
func markerExeSource() string {
	return "extern int puts(const char *);\nint main(void){puts(\"" + seatbeltMarker + "\");return 0;}\n"
}

// assertSeatbeltRefused checks a refusal's exit code and the one diagnostic
// line that identifies the attempt, never all of stderr: a /bin/sh leg also
// prints the /private/var/select/sh selector line, which is unrelated noise.
func assertSeatbeltRefused(t *testing.T, res execResult, wantExit int, wantDiag string) {
	t.Helper()
	if strings.Contains(string(res.Stdout), seatbeltMarker) {
		t.Fatalf("outside image ran (marker in stdout): %q", res.Stdout)
	}
	if res.ExitCode != wantExit {
		t.Fatalf("exit = %d, want %d (stderr: %q)", res.ExitCode, wantExit, res.Stderr)
	}
	if !strings.Contains(string(res.Stderr), wantDiag) {
		t.Fatalf("stderr %q does not contain the refusal diagnostic %q", res.Stderr, wantDiag)
	}
}

// TestSeatbeltBehavioralExecViaWorkspaceSymlink proves the exec filter matches
// the canonical target, not the spelled path: a workspace (or private-temp)
// symlink to an outside executable is refused, whether it is created before
// the run or inside the sandbox. The control is a symlink to a *workspace*
// executable, launched through /usr/bin/env so the approved-executable literal
// is env, not the symlink target — the distinguishing input for M-A4 (dropping
// the workspace exec subpath), which the outside-target literal would mask.
func TestSeatbeltBehavioralExecViaWorkspaceSymlink(t *testing.T) {
	requireSeatbeltCapability(t)
	cc := seatbeltHasCC(t)
	ws := canonTempDirT(t)
	outsideDir, _ := outsideCanaryDir(t, ws)
	outsideExe := buildSeatbeltExe(t, cc, outsideDir, "outside", markerExeSource())
	workspaceExe := buildSeatbeltExe(t, cc, ws, "inside", markerExeSource())

	preLink := filepath.Join(ws, "pre-link")
	if err := os.Symlink(outsideExe, preLink); err != nil {
		t.Fatal(err)
	}

	t.Run("precreated_workspace_symlink", func(t *testing.T) {
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", preLink})
		assertSeatbeltRefused(t, res, 126, preLink+": Operation not permitted")
	})

	t.Run("symlink_created_in_sandbox", func(t *testing.T) {
		link := filepath.Join(ws, "run-link")
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws,
			[]string{"/bin/sh", "-c", "ln -s " + outsideExe + " " + link + " && exec " + link})
		assertSeatbeltRefused(t, res, 126, link+": Operation not permitted")
	})

	t.Run("private_temp_symlink", func(t *testing.T) {
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws,
			[]string{"/bin/sh", "-c", `ln -s ` + outsideExe + ` "$TMPDIR/l" && exec "$TMPDIR/l"`})
		assertSeatbeltRefused(t, res, 126, `/l: Operation not permitted`)
	})

	// Control: a symlink to a workspace executable runs. M-A4 (drop the
	// workspace exec subpath) turns this red; the outside-target refusals do
	// not depend on the workspace subpath, so this control carries M-A4.
	t.Run("control_workspace_symlink_runs", func(t *testing.T) {
		link := filepath.Join(ws, "ok-link")
		if err := os.Symlink(workspaceExe, link); err != nil {
			t.Fatal(err)
		}
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", link})
		if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), seatbeltMarker) {
			t.Fatalf("workspace symlink control: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
		}
	})
}

// A refused PATH lookup must not be confused with an actually missing tool.
func TestSeatbeltBehavioralExecViaPATH(t *testing.T) {
	requireSeatbeltCapability(t)
	cc := seatbeltHasCC(t)
	ws := canonTempDirT(t)
	outsideDir, _ := outsideCanaryDir(t, ws)
	const name = "seatbelt-outside-path-tool"
	buildSeatbeltExe(t, cc, outsideDir, name, markerExeSource())
	argv := []string{"/usr/bin/env", "PATH=" + outsideDir, name}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host := exec.CommandContext(ctx, argv[0], argv[1:]...)
	host.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := host.CombinedOutput(); err != nil || string(out) != seatbeltMarker+"\n" {
		t.Fatalf("host PATH control must find and run the outside tool: output=%q err=%v", out, err)
	}

	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, argv)
	assertSeatbeltRefused(t, res, 127, "env: "+name+": No such file or directory")
}

// TestSeatbeltBehavioralExecOutsideInterpreter covers a workspace script whose
// shebang names an interpreter outside the roots: process-exec* covers the
// interpreter (process-exec-interpreter), so the launch is refused. The
// control is a #!/bin/sh script, whose interpreter is under /bin.
func TestSeatbeltBehavioralExecOutsideInterpreter(t *testing.T) {
	requireSeatbeltCapability(t)
	cc := seatbeltHasCC(t)
	ws := canonTempDirT(t)
	outsideDir, _ := outsideCanaryDir(t, ws)
	// The interpreter must itself run the marker so a false pass is visible;
	// an interpreter that merely exists is enough for the refusal leg.
	interp := buildSeatbeltExe(t, cc, outsideDir, "interp", markerExeSource())
	script := filepath.Join(ws, "outside-interp")
	if err := os.WriteFile(script, []byte("#!"+interp+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("outside_interpreter_refused", func(t *testing.T) {
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", script})
		assertSeatbeltRefused(t, res, 126, script+": Operation not permitted")
	})

	t.Run("control_system_interpreter_runs", func(t *testing.T) {
		ok := filepath.Join(ws, "ok-interp")
		if err := os.WriteFile(ok, []byte("#!/bin/sh\necho "+seatbeltMarker+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", ok})
		if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), seatbeltMarker) {
			t.Fatalf("system interpreter control: exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
		}
	})
}

// TestSeatbeltBehavioralApprovedScriptOutsideInterpreter pins the approved-exe
// residual: when the approved executable is a script whose shebang interpreter
// is outside the roots, the exe literal covers the script but not the
// interpreter, so sandbox-exec's own execvp of the script fails. That surfaces
// as exit 71 with a sandbox-exec execvp diagnostic, distinct from the 126 of a
// child launched through env.
func TestSeatbeltBehavioralApprovedScriptOutsideInterpreter(t *testing.T) {
	requireSeatbeltCapability(t)
	cc := seatbeltHasCC(t)
	ws := canonTempDirT(t)
	outsideDir, _ := outsideCanaryDir(t, ws)
	interp := buildSeatbeltExe(t, cc, outsideDir, "interp", markerExeSource())
	script := filepath.Join(ws, "approved")
	if err := os.WriteFile(script, []byte("#!"+interp+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{script})
	assertSeatbeltRefused(t, res, 71, "execvp() of '"+script+"' failed: Operation not permitted")
}

// seatbeltSpawnLauncherSrc posix_spawns argv[1] with argv[2:]. With the
// -DSETEXEC flag it sets POSIX_SPAWN_SETEXEC, which replaces the current image
// rather than forking. Either way the kernel runs its exec MAC check on the
// target, so an outside target is refused. On refusal it prints the errno and
// exits 4 (its own choice); the test asserts the EPERM text, never that 4.
const seatbeltSpawnLauncherSrc = `#include <spawn.h>
#include <stdio.h>
#include <string.h>
extern char **environ;
int main(int argc, char **argv) {
  posix_spawnattr_t attr;
  posix_spawnattr_t *ap = NULL;
#ifdef SETEXEC
  posix_spawnattr_init(&attr);
  posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETEXEC);
  ap = &attr;
#endif
  pid_t pid;
  int rc = posix_spawn(&pid, argv[1], NULL, ap, &argv[1], environ);
  printf("SPAWN-ERRNO %s\n", strerror(rc));
  return 4;
}
`

// TestSeatbeltBehavioralExecPosixSpawn covers posix_spawn and its SETEXEC
// variant launching an outside executable from an in-workspace launcher. The
// kernel's exec check refuses the target; the launcher reports EPERM. The
// control spawns /usr/bin/true.
func TestSeatbeltBehavioralExecPosixSpawn(t *testing.T) {
	requireSeatbeltCapability(t)
	cc := seatbeltHasCC(t)
	ws := canonTempDirT(t)
	outsideDir, _ := outsideCanaryDir(t, ws)
	outsideExe := buildSeatbeltExe(t, cc, outsideDir, "outside", markerExeSource())

	for _, setexec := range []bool{false, true} {
		name := "posix_spawn"
		var ccArgs []string
		if setexec {
			name = "posix_spawn_setexec"
			ccArgs = []string{"-DSETEXEC"}
		}
		t.Run(name, func(t *testing.T) {
			launcher := buildSeatbeltExeArgs(t, cc, ws, name, seatbeltSpawnLauncherSrc, ccArgs)
			res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", launcher, outsideExe})
			if strings.Contains(string(res.Stdout), seatbeltMarker) {
				t.Fatalf("outside image ran via %s: %q", name, res.Stdout)
			}
			if !strings.Contains(string(res.Stdout), "SPAWN-ERRNO Operation not permitted") {
				t.Fatalf("%s: launcher did not report EPERM: stdout=%q stderr=%q", name, res.Stdout, res.Stderr)
			}
		})
	}

	t.Run("control_spawn_system_true", func(t *testing.T) {
		launcher := buildSeatbeltExeArgs(t, cc, ws, "control-launcher", seatbeltSpawnLauncherSrc, nil)
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/env", launcher, "/usr/bin/true"})
		if !strings.Contains(string(res.Stdout), "SPAWN-ERRNO Undefined error: 0") &&
			!strings.Contains(string(res.Stdout), "SPAWN-ERRNO Success") {
			t.Fatalf("control spawn of /usr/bin/true did not succeed: stdout=%q stderr=%q", res.Stdout, res.Stderr)
		}
	})
}
