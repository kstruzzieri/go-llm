//go:build linux

package tools

// Behavioral acceptance for the bwrap backend (#441): real processes under
// real bubblewrap namespaces. requireBwrapCapability skips with the probe's
// reason on incapable hosts (Docker's default seccomp, restricted userns);
// GO_LLM_REQUIRE_BWRAP=1 turns the skip into a hard failure so the CI gate
// cannot silently pass without exercising confinement.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func requireBwrapCapability(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
	defer cancel()
	err := checkSandboxBinary(bwrapExecPath, "bubblewrap")
	if err == nil {
		err = probeBwrap(ctx, probeBwrapArgv(bwrapExecPath, prlimitExecPath,
			SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0))
	}
	if err != nil {
		if os.Getenv("GO_LLM_REQUIRE_BWRAP") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_BWRAP=1 but bwrap is unavailable: %v", err)
		}
		t.Skipf("bwrap unavailable on this host: %v", err)
	}
}

// TestBwrapHelperProcess is the argv-selected helper entry point that runs
// INSIDE the sandbox. It is not a test: without the "bwrap-helper" argv
// marker after "--" it skips. The marker is argv-based (not an env sentinel)
// because the bwrap payload environment is restricted to the strict exec
// allowlist. Results carry the HELPER- prefix so assertions never confuse
// them with go-test chatter.
func TestBwrapHelperProcess(t *testing.T) {
	args := os.Args
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 >= len(args) || args[sep+1] != "bwrap-helper" {
		t.Skip("helper process entry point")
	}
	args = args[sep+2:]
	if len(args) == 0 {
		fmt.Println("HELPER-ERR: no mode")
		os.Exit(2)
	}
	fail := func(err error) {
		fmt.Printf("HELPER-ERR: %v\n", err)
		os.Exit(1)
	}
	switch mode := args[0]; mode {
	case "pid":
		fmt.Printf("HELPER-PID: %d\n", os.Getpid())
	case "listdir":
		entries, err := os.ReadDir(args[1])
		if err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-DIR: %d entries\n", len(entries))
	case "read":
		data, err := os.ReadFile(args[1])
		if err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-OK: %s\n", data)
	case "trywrite":
		// Report the errno, not just failure: a write that fails with ENOENT
		// because the parent never existed in the namespace proves nothing
		// about policy, and a test asserting only "non-zero exit" cannot tell
		// the two apart.
		if err := os.WriteFile(args[1], []byte("escaped"), 0o600); err != nil {
			var errno syscall.Errno
			if errors.As(err, &errno) {
				fmt.Printf("HELPER-ERRNO: %s\n", errno.Error())
			}
			fail(err)
		}
		fmt.Println("HELPER-OK: wrote")
	case "tmprw":
		p := filepath.Join(os.TempDir(), "canary.txt")
		if err := os.WriteFile(p, []byte("temp-canary"), 0o600); err != nil {
			fail(err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-OK: %s\n", data)
		fmt.Printf("HELPER-TMP: %s\n", os.TempDir())
	case "tcp", "unixdial":
		network := "tcp"
		if mode == "unixdial" {
			network = "unix"
		}
		conn, err := net.DialTimeout(network, args[1], 3*time.Second)
		if err != nil {
			fail(err)
		}
		_, _ = conn.Write([]byte("ping"))
		_ = conn.Close()
		fmt.Println("HELPER-OK: connected")
	case "abstract":
		conn, err := net.DialTimeout("unix", "@"+args[1], 3*time.Second)
		if err != nil {
			fail(err)
		}
		_ = conn.Close()
		fmt.Println("HELPER-OK: connected")
	case "udp":
		conn, err := net.Dial("udp", args[1])
		if err != nil {
			fail(err)
		}
		if _, err := conn.Write([]byte(args[2])); err != nil {
			fail(err)
		}
		_ = conn.Close()
		fmt.Println("HELPER-OK: sent")
	case "spawnchild":
		exe, err := os.Executable()
		if err != nil {
			fail(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestBwrapHelperProcess$", "--", "bwrap-helper", "hang", args[1])
		// Detach into its own session: a same-process-group child would die
		// with the host group kill anyway, so only a session-escaped child
		// proves the PID-namespace teardown (a plain group kill cannot reach
		// it — exactly the mutation the timeout test exists to catch).
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-CHILD: %d\n", cmd.Process.Pid)
		// A timer sleep, not select{}: an empty select trips Go's deadlock
		// detector and exits the helper before the timeout can kill it.
		time.Sleep(300 * time.Second)
	case "hang":
		// The unique token in argv makes the whole tree pgrep-addressable;
		// the heartbeat proves liveness stops at teardown, not just renaming.
		hb := "hb-" + args[1]
		for i := 0; i < 3000; i++ {
			f, err := os.OpenFile(hb, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				_, _ = f.WriteString("beat\n")
				_ = f.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
	case "exit":
		code, err := strconv.Atoi(args[1])
		if err != nil {
			fail(err)
		}
		os.Exit(code)
	case "envreport":
		env := append([]string(nil), os.Environ()...)
		fmt.Printf("HELPER-ENV: %s\n", strings.Join(env, "|"))
	case "procstatus":
		data, err := os.ReadFile("/proc/self/status")
		if err != nil {
			fail(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "NoNewPrivs:") {
				fmt.Printf("HELPER-STATUS: %s\n", strings.Join(strings.Fields(line), " "))
			}
		}
	case "nsreport":
		for _, ns := range []string{"user", "pid", "ipc", "uts", "net"} {
			target, err := os.Readlink("/proc/self/ns/" + ns)
			if err != nil {
				fail(err)
			}
			fmt.Printf("HELPER-NS: %s %s\n", ns, target)
		}
	default:
		fmt.Printf("HELPER-ERR: unknown mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// bwrapHelperBinary copies the test binary into the workspace: /tmp inside
// the namespace is a fresh tmpfs, so the go-test build location is invisible
// there and the workspace bind is the reliable carrier.
func bwrapHelperBinary(t *testing.T, ws string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(ws, "bwrap-helper-bin")
	if err := os.WriteFile(helper, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return helper
}

func bwrapHelperArgv(helper string, mode string, extra ...string) []string {
	argv := []string{helper, "-test.run=^TestBwrapHelperProcess$", "--", "bwrap-helper", mode}
	return append(argv, extra...)
}

const bwrapTestHome = "/home/bwrap-test"

// bwrapToken returns a token unique across the whole package run.
// filepath.Base(t.TempDir()) is Go's per-test counter and is "001" for nearly
// every test, so pgrep-style matchers built from it are not the unique
// matchers they read as.
func bwrapToken(t *testing.T) string {
	t.Helper()
	return strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, t.Name()) + "-" + strconv.Itoa(os.Getpid())
}

// realBwrapRun executes argv under the real bwrap backend rooted at ws. An
// infra error fails the test; a non-zero payload exit is a normal result.
func realBwrapRun(t *testing.T, cfg SandboxConfig, ws string, argv []string) execResult {
	t.Helper()
	backend, err := newExecBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := execSpec{
		Path:          argv[0],
		Argv:          argv,
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin:/bin", "HOME=" + bwrapTestHome},
		WorkspaceRoot: ws,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := backend.Run(ctx, spec)
	if err != nil {
		t.Fatalf("infra failure running %q: %v (stderr: %s)", argv, err, res.Stderr)
	}
	return res
}

func helperOutput(t *testing.T, res execResult) string {
	t.Helper()
	return string(res.Stdout) + "\n" + string(res.Stderr)
}

func TestBwrapDeniesOutboundTCPUDPAndAbstractUnix(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}
	token := bwrapToken(t)

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcpLn.Close() }()
	tcpAccepted := make(chan struct{}, 1)
	go func() {
		if conn, err := tcpLn.Accept(); err == nil {
			tcpAccepted <- struct{}{}
			_ = conn.Close()
		}
	}()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpConn.Close() }()
	udpGot := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			udpGot <- string(buf[:n])
		}
	}()

	abstractName := "go-llm-bwrap-" + token
	abstractLn, err := net.Listen("unix", "@"+abstractName)
	if err != nil {
		t.Skipf("abstract unix sockets unavailable: %v", err)
	}
	defer func() { _ = abstractLn.Close() }()

	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "tcp", tcpLn.Addr().String())); res.ExitCode == 0 {
		t.Fatalf("TCP dial succeeded inside --unshare-net: %s", helperOutput(t, res))
	}
	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "abstract", abstractName)); res.ExitCode == 0 {
		t.Fatalf("abstract unix dial succeeded inside --unshare-net: %s", helperOutput(t, res))
	}
	// UDP has no handshake: sender-side success or failure are both
	// acceptable — host non-receipt is the denial assertion.
	_ = realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "udp", udpConn.LocalAddr().String(), token))
	select {
	case <-tcpAccepted:
		t.Fatal("host TCP listener observed a connection from the sandbox")
	case got := <-udpGot:
		t.Fatalf("host UDP listener received %q from the sandbox", got)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestBwrapAllowNetworkControl(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap, AllowNetwork: true}
	token := bwrapToken(t)

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcpLn.Close() }()
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpConn.Close() }()
	udpGot := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := udpConn.ReadFrom(buf)
			if err != nil {
				return
			}
			udpGot <- string(buf[:n])
		}
	}()

	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "tcp", tcpLn.Addr().String())); res.ExitCode != 0 {
		t.Fatalf("AllowNetwork TCP control failed: %s", helperOutput(t, res))
	}
	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "udp", udpConn.LocalAddr().String(), token)); res.ExitCode != 0 {
		t.Fatalf("AllowNetwork UDP control failed: %s", helperOutput(t, res))
	}
	select {
	case got := <-udpGot:
		if got != token {
			t.Fatalf("UDP token = %q, want %q", got, token)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host UDP listener never received the allowed token")
	}
}

// TestBwrapWorkspacePathSocketIsReachable pins the disclosed limitation: a
// pathname Unix socket inside the shared workspace is a deliberate host
// channel, not covered by the IP-egress guarantee.
func TestBwrapWorkspacePathSocketIsReachable(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	sock := filepath.Join(ws, "chan.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws,
		bwrapHelperArgv(helper, "unixdial", sock))
	if res.ExitCode != 0 {
		t.Fatalf("workspace pathname socket unreachable (limitation contract changed): %s", helperOutput(t, res))
	}
}

func TestBwrapWorkspaceWriteAllowedOutsideDenied(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}
	token := bwrapToken(t)

	inWs := filepath.Join(ws, "out.txt")
	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "trywrite", inWs)); res.ExitCode != 0 {
		t.Fatalf("workspace write denied: %s", helperOutput(t, res))
	}
	data, err := os.ReadFile(inWs)
	if err != nil || string(data) != "escaped" {
		t.Fatalf("workspace write not host-visible: %q err=%v", data, err)
	}

	// Each denial must be a POLICY denial. Asserting only a non-zero exit
	// lets an ENOENT (parent absent in the namespace regardless of policy)
	// masquerade as enforcement -- mutation-proven: a full read-write /home
	// bind left the old assertion green.
	for _, target := range []string{
		"/bwrap-root-canary-" + token,
		"/usr/bin/bwrap-canary-" + token,
	} {
		res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "trywrite", target))
		out := helperOutput(t, res)
		if res.ExitCode == 0 {
			t.Fatalf("outside write to %q succeeded: %s", target, out)
		}
		if !strings.Contains(out, "HELPER-ERRNO: read-only file system") &&
			!strings.Contains(out, "HELPER-ERRNO: permission denied") {
			t.Fatalf("write to %q was not denied by policy (want EROFS/EACCES): %s", target, out)
		}
		if _, err := os.Lstat(target); err == nil {
			t.Fatalf("outside write to %q became host-visible", target)
		}
	}
	// The workspace-resident executable is read-only over the read-write
	// workspace bind: the approved binary cannot rewrite itself.
	res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "trywrite", helper))
	if res.ExitCode == 0 || !strings.Contains(helperOutput(t, res), "HELPER-ERRNO: read-only file system") {
		t.Fatalf("approved executable is self-modifiable: %s", helperOutput(t, res))
	}
}

func TestBwrapHomeDataInvisible(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}

	// A canary under t.TempDir() lives in /tmp, which the namespace replaces
	// with a fresh tmpfs -- it proves tmpfs shadowing, not that $HOME is
	// unmounted. Mutation-proven: adding a read-write /home bind left the old
	// assertion green. Plant the canary in the REAL home as well.
	outside := filepath.Join(t.TempDir(), "host-secret")
	if err := os.WriteFile(outside, []byte("host-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "read", outside)); res.ExitCode == 0 {
		t.Fatalf("host file outside the workspace readable: %s", helperOutput(t, res))
	}
	if home := canonicalHome(); home != "" && !pathCovers(home, ws) {
		homeCanary := filepath.Join(home, ".go-llm-bwrap-canary-"+bwrapToken(t))
		if err := os.WriteFile(homeCanary, []byte("home-only"), 0o600); err != nil {
			t.Logf("home canary unwritable, skipping that leg: %v", err)
		} else {
			t.Cleanup(func() { _ = os.Remove(homeCanary) })
			if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "read", homeCanary)); res.ExitCode == 0 {
				t.Fatalf("real $HOME file readable inside the namespace: %s", helperOutput(t, res))
			}
			if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "listdir", home)); res.ExitCode == 0 {
				t.Fatalf("real $HOME is listable inside the namespace: %s", helperOutput(t, res))
			}
		}
	}
	inside := filepath.Join(ws, "copied-secret")
	if err := os.WriteFile(inside, []byte("host-only"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "read", inside))
	if res.ExitCode != 0 || !strings.Contains(helperOutput(t, res), "HELPER-OK: host-only") {
		t.Fatalf("workspace copy unreadable: %s", helperOutput(t, res))
	}
}

func TestBwrapPrivateTempAndShm(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}
	token := bwrapToken(t)

	canaries := []string{filepath.Join("/tmp", "bwrap-host-"+token)}
	if fi, err := os.Stat("/dev/shm"); err == nil && fi.IsDir() {
		canaries = append(canaries, filepath.Join("/dev/shm", "bwrap-host-"+token))
	}
	for _, c := range canaries {
		if err := os.WriteFile(c, []byte("host-canary"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(c) })
	}

	res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "tmprw"))
	out := helperOutput(t, res)
	if res.ExitCode != 0 || !strings.Contains(out, "HELPER-OK: temp-canary") ||
		!strings.Contains(out, "HELPER-TMP: /tmp") {
		t.Fatalf("private temp round-trip failed: %s", out)
	}
	for _, c := range canaries {
		if res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "read", c)); res.ExitCode == 0 {
			t.Fatalf("host canary %q visible inside the namespace: %s", c, helperOutput(t, res))
		}
		data, err := os.ReadFile(c)
		if err != nil || string(data) != "host-canary" {
			t.Fatalf("host canary %q altered: %q err=%v", c, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join("/tmp", "canary.txt")); err == nil {
		t.Fatal("sandbox temp write leaked to the host /tmp")
	}
}

// bwrapQuotaFillPython streams a reusable 1 MiB buffer so the writer stays
// far below the RLIMIT_AS ceiling while exceeding the tmpfs quota.
// bwrapQuotaReportPython reports the exact tmpfs size, so a quota set far
// BELOW the approved cap is caught too. The fill probe alone is one-sided:
// mutation-proven, dividing --size by 512 left it green.
const bwrapQuotaReportPython = `
import os, sys
st = os.statvfs(sys.argv[1])
print("HELPER-FSSIZE:", st.f_blocks * st.f_frsize)
`

const bwrapQuotaFillPython = `
import sys
buf = b"x" * 1048576
written = 0
try:
    with open(sys.argv[1], "wb") as f:
        for _ in range(int(sys.argv[2])):
            f.write(buf)
            written += 1
except OSError:
    print("HELPER-ENOSPC:", written)
    raise SystemExit(0)
print("HELPER-OVER:", written)
raise SystemExit(1)
`

func requireBwrapPython(t *testing.T) {
	t.Helper()
	// os.Stat (symlink-following): python3 is a payload vehicle, not a TCB
	// binary, and is conventionally a versioned symlink.
	fi, err := os.Stat("/usr/bin/python3")
	if err == nil && !isExecutableFile(fi) {
		err = errNotExecutable
	}
	if err != nil {
		if os.Getenv("GO_LLM_REQUIRE_BWRAP") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_BWRAP=1 but python3 is unavailable: %v", err)
		}
		t.Skipf("python3 unavailable: %v", err)
	}
}

func TestBwrapTmpfsQuotasRejectExcess(t *testing.T) {
	requireBwrapCapability(t)
	requireBwrapPython(t)
	ws := wsTempDir(t)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 512}
	for _, dest := range []string{"/tmp", "/dev/shm"} {
		res := realBwrapRun(t, cfg, ws, []string{"/usr/bin/python3", "-c", bwrapQuotaReportPython, dest})
		out := helperOutput(t, res)
		if res.ExitCode != 0 || !strings.Contains(out, "HELPER-FSSIZE: 536870912") {
			t.Fatalf("%s quota is not exactly the approved 512MiB: exit=%d %s", dest, res.ExitCode, out)
		}
	}
	for _, dest := range []string{"/tmp/fill.bin", "/dev/shm/fill.bin"} {
		res := realBwrapRun(t, cfg, ws, []string{
			"/usr/bin/python3", "-c", bwrapQuotaFillPython, dest, "513",
		})
		out := helperOutput(t, res)
		if res.ExitCode != 0 || !strings.Contains(out, "HELPER-ENOSPC:") {
			t.Fatalf("quota on %s not enforced: exit=%d %s", dest, res.ExitCode, out)
		}
	}
	// /dev is its own tmpfs and --size attaches to the following --tmpfs, so
	// without the read-only remount it is an unbounded host-RAM sink that
	// neither RLIMIT_AS nor either quota bounds.
	helper := bwrapHelperBinary(t, ws)
	res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "trywrite", "/dev/bwrap-fill"))
	if res.ExitCode == 0 {
		t.Fatalf("/dev is writable: unbounded memory sink under an approved cap: %s", helperOutput(t, res))
	}
}

const bwrapLimitReportPython = `
import resource
soft, hard = resource.getrlimit(resource.RLIMIT_AS)
print("HELPER-RLIMIT:", soft, hard)
`

const bwrapAllocPython = `
import sys
n = int(sys.argv[1])
b = bytearray(n)
for i in range(0, n, 4096):
    b[i] = 1
print("HELPER-ALLOC-OK:", n)
`

func TestBwrapMemoryCeilingDeniesAllocation(t *testing.T) {
	requireBwrapCapability(t)
	requireBwrapPython(t)
	ws := wsTempDir(t)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 512}

	res := realBwrapRun(t, cfg, ws, []string{"/usr/bin/python3", "-c", bwrapLimitReportPython})
	if res.ExitCode != 0 || !strings.Contains(helperOutput(t, res), "HELPER-RLIMIT: 536870912 536870912") {
		t.Fatalf("RLIMIT_AS not applied exactly: exit=%d %s", res.ExitCode, helperOutput(t, res))
	}

	small := realBwrapRun(t, cfg, ws, []string{"/usr/bin/python3", "-c", bwrapAllocPython, "33554432"})
	if small.ExitCode != 0 {
		t.Fatalf("32 MiB control allocation failed under the cap: %s", helperOutput(t, small))
	}
	big := realBwrapRun(t, cfg, ws, []string{"/usr/bin/python3", "-c", bwrapAllocPython, "1073741824"})
	if big.ExitCode == 0 {
		t.Fatalf("1 GiB allocation succeeded under a 512 MiB address-space ceiling: %s", helperOutput(t, big))
	}
}

func TestBwrapTimeoutKillsTree(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	token := "bwrap-tree-" + bwrapToken(t)

	backend, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	spec := execSpec{
		Path:          helper,
		Argv:          bwrapHelperArgv(helper, "spawnchild", token),
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin:/bin", "HOME=" + bwrapTestHome},
		WorkspaceRoot: ws,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	start := time.Now()
	res, runErr := backend.Run(ctx, spec)
	elapsed := time.Since(start)
	if runErr == nil || !res.TimedOut {
		t.Fatalf("runaway not killed by timeout: err=%v timedOut=%v", runErr, res.TimedOut)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("kill took %s", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", token).Output()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendants still alive after teardown: pids %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	hb := filepath.Join(ws, "hb-"+token)
	sizeAt := func() int64 {
		fi, err := os.Stat(hb)
		if err != nil {
			return 0
		}
		return fi.Size()
	}
	before := sizeAt()
	time.Sleep(1 * time.Second)
	if after := sizeAt(); after != before {
		t.Fatalf("heartbeat still advancing after teardown: %d -> %d", before, after)
	}
}

func TestBwrapBackgroundLifetime(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	token := "bwrap-bg-" + bwrapToken(t)

	mgr, err := NewSandboxedBackgroundManager(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + bwrapTestHome}

	var stdout, stderr strings.Builder
	proc, err := mgr.backend.Start(execSpec{
		Path: helper, Argv: bwrapHelperArgv(helper, "pid"),
		Dir: ws, Env: env, WorkspaceRoot: ws,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	code, managerKilled, err := proc.Wait()
	if err != nil || code != 0 || managerKilled {
		t.Fatalf("background pid helper: code=%d mk=%v err=%v stderr=%s", code, managerKilled, err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "HELPER-PID:") {
		t.Fatalf("background stdout not captured: %q", stdout.String())
	}

	hangProc, err := mgr.backend.Start(execSpec{
		Path: helper, Argv: bwrapHelperArgv(helper, "hang", token),
		Dir: ws, Env: env, WorkspaceRoot: ws,
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// Give the tree a moment to exist, then kill the group.
	time.Sleep(500 * time.Millisecond)
	if err := hangProc.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hangProc.Wait(); err != nil {
		t.Logf("wait after kill: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", token).Output()
		if strings.TrimSpace(string(out)) == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background tree residue after Kill: pids %s", out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestBwrapExitCodeFidelity(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)

	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws,
		bwrapHelperArgv(helper, "exit", "42"))
	if res.ExitCode != 42 {
		t.Fatalf("direct chain exit = %d, want 42: %s", res.ExitCode, helperOutput(t, res))
	}

	requireBwrapPython(t)
	res = realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 512}, ws,
		[]string{"/usr/bin/python3", "-c", "raise SystemExit(42)"})
	if res.ExitCode != 42 {
		t.Fatalf("prlimit chain exit = %d, want 42: %s", res.ExitCode, helperOutput(t, res))
	}
}

// TestBwrapDeniedEtcStaysDenied pins the reviewed-literal boundary: an
// accidental widening of the /etc surface turns this red.
func TestBwrapDeniedEtcStaysDenied(t *testing.T) {
	requireBwrapCapability(t)
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Skipf("host has no /etc/passwd: %v", err)
	}
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws,
		bwrapHelperArgv(helper, "read", "/etc/passwd"))
	if res.ExitCode == 0 {
		t.Fatalf("/etc/passwd readable inside the namespace: %s", helperOutput(t, res))
	}
}

func TestBwrapProcessInvariants(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}

	res := realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "procstatus"))
	out := helperOutput(t, res)
	if res.ExitCode != 0 ||
		!strings.Contains(out, "HELPER-STATUS: CapEff: 0000000000000000") ||
		!strings.Contains(out, "HELPER-STATUS: NoNewPrivs: 1") {
		t.Fatalf("capability/no-new-privs invariants not held: %s", out)
	}

	// The nested-userns probe must be a single-threaded binary: unshare(2)
	// with CLONE_NEWUSER always fails EINVAL from a multithreaded Go
	// process, which would pass this assertion with or without
	// --disable-userns (a blind assertion, proven by mutation).
	fi, err := os.Stat("/usr/bin/unshare")
	if err != nil || !isExecutableFile(fi) {
		if os.Getenv("GO_LLM_REQUIRE_BWRAP") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_BWRAP=1 but /usr/bin/unshare is unavailable: %v", err)
		}
		t.Skipf("/usr/bin/unshare unavailable: %v", err)
	}
	{
		res = realBwrapRun(t, cfg, ws, []string{"/usr/bin/unshare", "--user", "/bin/true"})
		if res.ExitCode == 0 {
			t.Fatalf("nested user namespace creation succeeded despite --disable-userns: %s", helperOutput(t, res))
		}
	}

	res = realBwrapRun(t, cfg, ws, bwrapHelperArgv(helper, "nsreport"))
	out = helperOutput(t, res)
	if res.ExitCode != 0 {
		t.Fatalf("nsreport failed: %s", out)
	}
	for _, ns := range []string{"user", "pid", "ipc", "uts", "net"} {
		hostNS, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			t.Fatal(err)
		}
		var childNS string
		for _, line := range strings.Split(out, "\n") {
			if rest, ok := strings.CutPrefix(line, "HELPER-NS: "+ns+" "); ok {
				childNS = rest
			}
		}
		if childNS == "" {
			t.Fatalf("nsreport missing %s namespace: %s", ns, out)
		}
		if childNS == hostNS {
			t.Fatalf("%s namespace shared with the host: %s", ns, childNS)
		}
	}
}

func TestBwrapPayloadEnvironment(t *testing.T) {
	requireBwrapCapability(t)
	t.Setenv("GO_LLM_BWRAP_PARENT_SENTINEL", "leaked")
	ws := wsTempDir(t)
	helper := bwrapHelperBinary(t, ws)
	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws,
		bwrapHelperArgv(helper, "envreport"))
	out := helperOutput(t, res)
	if res.ExitCode != 0 {
		t.Fatalf("envreport failed: %s", out)
	}
	var envLine string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "HELPER-ENV: "); ok {
			envLine = rest
		}
	}
	got := map[string]bool{}
	if envLine != "" {
		for _, kv := range strings.Split(envLine, "|") {
			got[kv] = true
		}
	}
	// PWD is set by bwrap itself as part of --chdir; everything else must be
	// exactly the approved payload set.
	want := []string{"PATH=/usr/bin:/bin", "HOME=" + bwrapTestHome, "TMPDIR=/tmp", "PWD=" + ws}
	for _, kv := range want {
		if !got[kv] {
			t.Fatalf("payload env missing %q: %q", kv, envLine)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("payload env carries extra entries: %q", envLine)
	}
}

// TestBwrapPublicPlanToInvoke proves the chain end to end through the public
// tool surface: executable resolution, approval planning (with the sandbox
// preview line), identity recheck, dispatch, prepare, and invocation.
func TestBwrapPublicPlanToInvoke(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	tools, err := NewSandboxedExecTools(ws, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	rc, ok := tools[0].(*RunCommand)
	if !ok {
		t.Fatalf("tool = %T, want *RunCommand", tools[0])
	}
	raw := json.RawMessage(`{"argv":["true"]}`)
	plan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Preview, `runtime="bwrap"`) ||
		!strings.Contains(plan.Preview, "temp=private") {
		t.Fatalf("preview lacks the sandbox line: %q", plan.Preview)
	}
	if !strings.Contains(plan.ApprovalKey, "sb:") {
		t.Fatalf("approval key lacks the sandbox namespace: %q", plan.ApprovalKey)
	}
	result, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "exit code: 0") {
		t.Fatalf("invoke result = %q", result.Content)
	}
}
