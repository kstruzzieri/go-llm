//go:build darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Launchd acceptance for the Seatbelt backend (#712): a sandboxed command
// starts with no launchd connection, so it can neither read nor change the
// user's launchd domain. Each refusal follows a positive control through the
// same backend configuration, and an unsandboxed control that proves the
// domain answers this user.

// The only launchd labels these tests ever load, kickstart, enable or disable.
const (
	launchdKickLabel   = "com.kstruzzieri.golem-test-712.kick"
	launchdToggleLabel = "com.kstruzzieri.golem-test-712.toggle"
)

// launchdDomain is the user's launchd domain: gui/<uid> when an unsandboxed
// launchctl print of it succeeds, else user/<uid> on the same check. With
// neither it fails when GO_LLM_REQUIRE_SEATBELT=1 or GO_LLM_LAUNCHD_TESTS=1
// asks for these tests, and skips otherwise.
func launchdDomain(t *testing.T) string {
	t.Helper()
	uid := strconv.Itoa(os.Getuid())
	for _, d := range []string{"gui/" + uid, "user/" + uid} {
		if exec.Command("/bin/launchctl", "print", d).Run() == nil {
			return d
		}
	}
	const msg = "neither gui/%[1]s nor user/%[1]s prints from an unsandboxed launchctl"
	if os.Getenv("GO_LLM_REQUIRE_SEATBELT") == "1" || os.Getenv("GO_LLM_LAUNCHD_TESTS") == "1" {
		t.Fatalf(msg, uid)
	}
	t.Skipf(msg, uid)
	return ""
}

// requireLaunchdTests gates the tests that change this user's real launchd
// state.
func requireLaunchdTests(t *testing.T) {
	t.Helper()
	if os.Getenv("GO_LLM_LAUNCHD_TESTS") != "1" {
		t.Skip("set GO_LLM_LAUNCHD_TESTS=1 to run: this test loads, kickstarts, enables or disables this user's " +
			"launchd jobs com.kstruzzieri.golem-test-712.*, and the toggle test leaves a persistent enabled " +
			"override entry for its label")
	}
}

// lifetimeReport is one command's result under one backend lifetime.
type lifetimeReport struct {
	Lifetime string
	Exit     int
	Output   string // stdout then stderr for run; both interleaved for start
	Err      string // an infra failure, never a nonzero exit
}

// seatbeltLifetimeRun runs argv under b in workspace ws through lifetime: "run"
// (Run) or "start" (Start, then Wait). It takes no *testing.T so the driver
// helpers can call it.
func seatbeltLifetimeRun(b execBackend, lifetime, ws string, argv ...string) lifetimeReport {
	r := lifetimeReport{Lifetime: lifetime, Exit: -1}
	fi, err := os.Stat(argv[0])
	if err != nil {
		r.Err = err.Error()
		return r
	}
	spec := execSpec{
		Path:          argv[0],
		Argv:          argv,
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME")},
		WorkspaceRoot: ws,
		ExeIdentity:   fi,
	}
	if lifetime == "run" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := b.Run(ctx, spec)
		r.Exit, r.Output = res.ExitCode, string(res.Stdout)+string(res.Stderr)
		if err != nil {
			r.Err = err.Error()
		}
		return r
	}
	out := &lockedBuffer{}
	proc, err := b.Start(spec, out, out)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	stop := time.AfterFunc(30*time.Second, func() { _ = proc.Kill() })
	defer stop.Stop()
	code, _, err := proc.Wait()
	r.Exit, r.Output = code, out.String()
	if err != nil {
		r.Err = err.Error()
	}
	return r
}

// runSeatbeltDriver runs a driver helper mode in its own process and returns
// its LIFETIME reports by lifetime. The driver, not this test binary, holds
// the seeded Mach state.
func runSeatbeltDriver(t *testing.T, mode, ws string) map[string]lifetimeReport {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, self, "__golem_exec_helper__", mode, ws).CombinedOutput()
	t.Logf("%s output:\n%s", mode, out)
	if err != nil {
		t.Fatalf("driver %s: %v", mode, err)
	}
	reports := map[string]lifetimeReport{}
	for line := range strings.Lines(string(out)) {
		if js, ok := strings.CutPrefix(line, "LIFETIME "); ok {
			var r lifetimeReport
			if err := json.Unmarshal([]byte(js), &r); err != nil {
				t.Fatalf("driver %s: bad LIFETIME line %q: %v", mode, js, err)
			}
			reports[r.Lifetime] = r
		}
	}
	return reports
}

// seatbeltPositiveControl requires an ordinary command to run under the real
// backend, so a refusal after it is the profile's, not a broken backend's.
func seatbeltPositiveControl(t *testing.T, ws string) {
	t.Helper()
	if res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/true"}); res.ExitCode != 0 {
		t.Fatalf("positive control: /usr/bin/true exited %d under Seatbelt (stderr %q)", res.ExitCode, res.Stderr)
	}
}

// firstLine is s up to its first newline. A domain or job listing carries
// the user's launchd environment, credentials included, so failures quote
// one line of it, never the whole listing.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// hostLaunchctl runs launchctl unsandboxed, as this test's user.
func hostLaunchctl(args ...string) (string, error) {
	out, err := exec.Command("/bin/launchctl", args...).CombinedOutput()
	return string(out), err
}

// TestSeatbeltLaunchdPrintRefused: a sandboxed launchctl cannot list the
// user's launchd domain, through either lifetime, though the same print
// succeeds unsandboxed (launchdDomain).
func TestSeatbeltLaunchdPrintRefused(t *testing.T) {
	requireSeatbeltCapability(t)
	domain := launchdDomain(t)
	ws := canonTempDirT(t)
	seatbeltPositiveControl(t, ws)
	backend, err := newSeatbeltExecBackend(seatbeltTestCfg())
	if err != nil {
		t.Fatal(err)
	}
	for _, lifetime := range []string{"run", "start"} {
		t.Run(lifetime, func(t *testing.T) {
			r := seatbeltLifetimeRun(backend, lifetime, ws, "/bin/launchctl", "print", domain)
			if r.Err != "" {
				t.Fatalf("infra failure: %s", r.Err)
			}
			t.Logf("sandboxed launchctl print %s: exit %d, first line %q", domain, r.Exit, firstLine(r.Output))
			listed := false
			for line := range strings.Lines(r.Output) {
				listed = listed || strings.HasPrefix(line, domain+" = {")
			}
			// containment: LAUNCHD-SEATBELT
			if r.Exit == 0 || listed {
				t.Errorf("sandboxed launchctl print %s: exit %d, listed %v, first line %q; want a refusal", domain, r.Exit, listed, firstLine(r.Output))
			}
		})
	}
}

// TestSeatbeltLaunchdChildMachState: a sandboxed command started through Run
// or Start has a null bootstrap port, empty registered slots and null
// exception ports for every mask, though its parent held a port in every
// registered slot and exception handlers outside EXC_MASK_ALL. An inspection
// the profile refuses fails the test.
func TestSeatbeltLaunchdChildMachState(t *testing.T) {
	requireSeatbeltCapability(t)
	if raceInstrumented {
		t.Skip("race-instrumented helper cannot initialize under the sandbox; helper legs run in the non-race pass")
	}
	ws := canonTempDirT(t)
	seatbeltPositiveControl(t, ws)
	reports := runSeatbeltDriver(t, "seatbeltmachdriver", ws)
	for _, lifetime := range []string{"run", "start"} {
		r, ok := reports[lifetime]
		if !ok {
			t.Errorf("%s: the driver printed no report", lifetime)
			continue
		}
		if r.Err != "" || r.Exit != 0 {
			t.Errorf("%s: machstate exit %d, err %q (a refused inspection lands here):\n%s", lifetime, r.Exit, r.Err, r.Output)
			continue
		}
		st, err := machStateFromOutput(r.Output)
		if err != nil {
			t.Errorf("%s: %v", lifetime, err)
			continue
		}
		// containment: LAUNCHD-SEATBELT
		if err := noLaunchdConnection(st); err != nil {
			t.Errorf("%s: sandboxed child keeps a launchd connection: %v", lifetime, err)
		}
	}
}

// TestSeatbeltLaunchdReattachRefused: a sandboxed command finds no registered
// port to reinstall as its bootstrap port, so it cannot reach launchd again,
// though its parent held the bootstrap port in slot 0 as a normal process
// does.
func TestSeatbeltLaunchdReattachRefused(t *testing.T) {
	requireSeatbeltCapability(t)
	if raceInstrumented {
		t.Skip("race-instrumented helper cannot initialize under the sandbox; helper legs run in the non-race pass")
	}
	cmd := exec.Command("/bin/launchctl", "managername")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME")}
	out, err := cmd.Output()
	manager := strings.TrimSpace(string(out))
	if err != nil || manager == "" {
		t.Fatalf("control: unsandboxed launchctl managername = %q, %v; want a manager name", manager, err)
	}
	ws := canonTempDirT(t)
	seatbeltPositiveControl(t, ws)
	r, ok := runSeatbeltDriver(t, "seatbeltreattachdriver", ws)["run"]
	if !ok {
		t.Fatal("the driver printed no report")
	}
	if r.Err != "" {
		t.Fatalf("infra failure: %s", r.Err)
	}
	// containment: LAUNCHD-SEATBELT
	if r.Exit != 3 || !strings.Contains(r.Output, "HELPER-REATTACH: no-slot") || strings.Contains(r.Output, manager) {
		t.Errorf("sandboxed reattach: exit %d, want 3 with HELPER-REATTACH: no-slot and without manager name %q. Output:\n%s", r.Exit, manager, r.Output)
	}
}

// TestSeatbeltLaunchdKickstartRefused: a sandboxed launchctl cannot start a
// loaded user job, which would run outside the sandbox. The host kickstart at
// the end proves the job would have left its marker. It and the toggle test
// carry no containment marker: they need the GO_LLM_LAUNCHD_TESTS opt-in, so
// the inventory cannot require them.
func TestSeatbeltLaunchdKickstartRefused(t *testing.T) {
	requireLaunchdTests(t)
	requireSeatbeltCapability(t)
	domain := launchdDomain(t)
	ws := canonTempDirT(t)
	seatbeltPositiveControl(t, ws)
	dir, _ := outsideCanaryDir(t, ws)
	marker := filepath.Join(dir, "kicked")
	plist := filepath.Join(t.TempDir(), launchdKickLabel+".plist")
	body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key><array><string>/usr/bin/touch</string><string>%s</string></array>
	<key>RunAtLoad</key><false/>
</dict>
</plist>
`, launchdKickLabel, marker)
	if err := os.WriteFile(plist, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	target := domain + "/" + launchdKickLabel
	_, _ = hostLaunchctl("bootout", target) // a job left by an interrupted run
	if out, err := hostLaunchctl("bootstrap", domain, plist); err != nil {
		t.Fatalf("host launchctl bootstrap %s %s: %v\n%s", domain, plist, err, out)
	}
	t.Cleanup(func() {
		if out, err := hostLaunchctl("bootout", target); err != nil {
			t.Errorf("host launchctl bootout %s: %v\n%s", target, err, out)
		}
	})
	idle := func(when string) {
		t.Helper()
		out, err := hostLaunchctl("print", target)
		if err != nil {
			t.Fatalf("%s: host launchctl print %s: %v (%s)", when, target, err, firstLine(out))
		}
		state := "no state line"
		for line := range strings.Lines(out) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "state = "); ok {
				state = v
				break
			}
		}
		_, statErr := os.Stat(marker)
		if state == "running" || !os.IsNotExist(statErr) {
			t.Fatalf("%s: job ran: state %q, marker stat %v", when, state, statErr)
		}
	}
	idle("after bootstrap")

	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/launchctl", "kickstart", target})
	t.Logf("sandboxed launchctl kickstart %s: exit %d, stdout %q, stderr %q", target, res.ExitCode, res.Stdout, res.Stderr)
	if res.ExitCode == 0 {
		t.Errorf("sandboxed launchctl kickstart %s exited 0 (stdout %q, stderr %q)", target, res.Stdout, res.Stderr)
	}
	time.Sleep(2 * time.Second)
	idle("after the sandboxed kickstart")

	if out, err := hostLaunchctl("kickstart", target); err != nil {
		t.Fatalf("host launchctl kickstart %s: %v\n%s", target, err, out)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("host kickstart of %s left no marker within 5s: the job cannot detect a kickstart", target)
		}
	}
}

// TestSeatbeltLaunchdEnableDisableRefused: a sandboxed launchctl cannot
// enable or disable a user job; launchd keeps both changes across logins.
// The host enable and disable around each attempt prove the override entry
// reads each state. It uses one fixed label, so at most one entry ever
// accumulates, and it leaves that persistent override entry behind, enabled.
func TestSeatbeltLaunchdEnableDisableRefused(t *testing.T) {
	requireLaunchdTests(t)
	requireSeatbeltCapability(t)
	domain := launchdDomain(t)
	ws := canonTempDirT(t)
	seatbeltPositiveControl(t, ws)
	target := domain + "/" + launchdToggleLabel
	t.Cleanup(func() {
		if out, err := hostLaunchctl("enable", target); err != nil {
			t.Errorf("host launchctl enable %s: %v\n%s", target, err, out)
		}
	})
	// entry is the label's line in print-disabled: enabled or disabled.
	entry := func() string {
		t.Helper()
		out, err := hostLaunchctl("print-disabled", domain)
		if err != nil {
			t.Fatalf("host launchctl print-disabled %s: %v\n%s", domain, err, out)
		}
		for line := range strings.Lines(out) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), `"`+launchdToggleLabel+`" => `); ok {
				return v
			}
		}
		return ""
	}
	for _, step := range []struct{ host, sandboxed string }{{"enable", "disable"}, {"disable", "enable"}} {
		if out, err := hostLaunchctl(step.host, target); err != nil {
			t.Fatalf("host launchctl %s %s: %v\n%s", step.host, target, err, out)
		}
		before := entry()
		if before != step.host+"d" {
			t.Fatalf("after host %s the override entry reads %q, want %q", step.host, before, step.host+"d")
		}
		res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/launchctl", step.sandboxed, target})
		after := entry()
		t.Logf("sandboxed launchctl %s %s: exit %d, stderr %q; override entry %q -> %q", step.sandboxed, target, res.ExitCode, res.Stderr, before, after)
		if res.ExitCode == 0 || after != before {
			t.Fatalf("sandboxed launchctl %s %s: exit %d, override entry %q -> %q; want a refusal and no change (stderr %q)",
				step.sandboxed, target, res.ExitCode, before, after, res.Stderr)
		}
	}
}
