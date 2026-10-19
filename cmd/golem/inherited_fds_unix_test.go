//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// inheritableAtInit is what init left inheritable: none, unless the
// sanitizer is broken.
var inheritableAtInit []int

// init makes this test process a clean native-sandbox parent with the same
// sanitizer run() applies to a native session. CI runners (observed: GitHub
// Actions) leak an inheritable descriptor into test processes, and both
// backends' fail-closed descriptor audit would then refuse every native
// spawn of a test that builds its session without run(). Marking an
// inherited descriptor close-on-exec only changes what FUTURE children of
// this process inherit; the owner's use of its own copy is untouched.
func init() {
	clearInheritedDescriptors()
	inheritableAtInit = inheritableFDs()
}

// inheritableFDs lists the descriptors above stdio without FD_CLOEXEC, read
// with F_GETFD independently of the sanitizer.
func inheritableFDs() []int {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	var fds []int
	for _, name := range names {
		fd, convErr := strconv.Atoi(name)
		if convErr != nil || fd <= 2 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			fds = append(fds, fd)
		}
	}
	return fds
}

// The init above ran before any test, so no descriptor the launcher leaked
// can reach a native child and trip the backend's audit. It reads the set
// init recorded: a test that later resolves a hostname may hold libSystem's
// own netsrc socket, which the audit exempts.
func TestInheritedDescriptorsAreCloseOnExec(t *testing.T) {
	if len(inheritableAtInit) != 0 {
		t.Fatalf("descriptors %v are inheritable; the native fd audit would refuse every spawn", inheritableAtInit)
	}
}

// A launcher-leaked writable descriptor (a CI runner's, a shell redirection)
// must not carry a native command's write past the sandbox. A host session
// keeps it, and the write lands: the positive control. A native run()
// marks it close-on-exec before the startup probe, so startup succeeds and
// the same write has no descriptor to go through.
func TestNativeWiringClearsInheritedDescriptors(t *testing.T) {
	hostConfig, hostRoot := golemWorkspace(t)
	nativeConfig, nativeRoot := golemWorkspace(t)
	policy := nativePolicyForTest(t, nativeRoot)
	outside := filepath.Join(canonicalDir(t, t.TempDir()), "leaked.txt")
	fd, err := syscall.Open(outside, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	for _, leg := range []struct {
		label        string
		config, root string
		policy       agenttools.SandboxConfig
		want         string
	}{
		{"host", hostConfig, hostRoot, agenttools.SandboxConfig{}, "leaked"},
		{string(policy.Runtime), nativeConfig, nativeRoot, policy, ""},
	} {
		if err := os.Truncate(outside, 0); err != nil {
			t.Fatal(err)
		}
		if flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0); errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
			t.Fatalf("%s: precondition: fd %d flags=%d errno=%v, want it inheritable", leg.label, fd, flags, errno)
		}
		withGolem(t, leg.config, leg.root, leg.policy, []string{"-allow-exec"}, func(sess *replSession) {
			script := fmt.Sprintf("printf ok > in.txt; printf leaked >&%d", fd)
			res := invokeTool(context.Background(), t, sess.tools, "run_command", map[string]any{"argv": []string{"bash", "-c", script}})
			t.Logf("%s run_command:\n%s", leg.label, res.Content)
		})
		if got, err := os.ReadFile(filepath.Join(leg.root, "in.txt")); err != nil || string(got) != "ok" {
			t.Fatalf("%s: control in.txt = %q (err %v), want \"ok\"", leg.label, got, err)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != leg.want {
			t.Fatalf("%s: outside file = %q (err %v), want %q", leg.label, got, err, leg.want)
		}
	}
}

// TestNativeWiringLocalhostProvider reproduces the libSystem netsrc case
// through golem's own path: a provider at a hostname that resolves makes the
// first model request look it up, which on macOS leaves a non-CLOEXEC
// com.apple.netsrc control socket in golem's process after the startup
// sanitizer ran. The native command after that turn must still run. The
// provider is "localhost." (the FQDN spelling): golem's destination guard
// dials plain "localhost" as 127.0.0.1 and ::1 without resolving it (#665),
// while "localhost." is a remote destination resolved through the system,
// as a hosted or LAN provider name is. The proof runs in a fresh child
// process: this test binary may already hold that socket, marked
// close-on-exec by an earlier native run().
func TestNativeWiringLocalhostProvider(t *testing.T) {
	if os.Getenv("GO_LLM_NATIVE_LOCALHOST_CHILD") == "1" {
		localhostProviderChild(t)
		return
	}
	nativePolicyForTest(t, canonicalDir(t, t.TempDir()))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestNativeWiringLocalhostProvider$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "GO_LLM_NATIVE_LOCALHOST_CHILD=1")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil || !strings.Contains(string(out), "\n--- PASS: TestNativeWiringLocalhostProvider ") {
		t.Fatalf("child = %v, want its TestNativeWiringLocalhostProvider to pass", err)
	}
}

func localhostProviderChild(t *testing.T) {
	configPath, root, _ := fenceWireHarness(t, func(wireRequest) []string { return sseAnswer("done") })
	root = canonicalDir(t, root)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://127.0.0.1:") {
		t.Fatalf("fixture config has no 127.0.0.1 provider URL:\n%s", raw)
	}
	resolved := strings.Replace(string(raw), "http://127.0.0.1:", "http://localhost.:", 1)
	if err := os.WriteFile(configPath, []byte(resolved), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Providers map[string]struct {
			BaseURL string `json:"base_url"`
		} `json:"providers"`
	}
	if err := json.Unmarshal([]byte(resolved), &cfg); err != nil {
		t.Fatal(err)
	}
	policy := nativePolicyForTest(t, root)
	admit := []string{"-allow-exec", "-allow-destination", "test/" + cfg.Providers["test"].BaseURL}
	withGolem(t, configPath, root, policy, admit, func(sess *replSession) {
		ctx := context.Background()
		var out strings.Builder
		if _, err := runOnce(ctx, &out, nil, sess, "hello", nil); err != nil {
			t.Fatalf("turn: %v\n%s", err, out.String())
		}
		if fds := inheritableFDs(); runtime.GOOS == "darwin" && len(fds) == 0 {
			t.Fatal("precondition: the localhost. turn left no inheritable libSystem descriptor, so this would test nothing")
		} else {
			t.Logf("inheritable after the localhost. turn: %v", fds)
		}
		res := invokeTool(ctx, t, sess.tools, "run_command", map[string]any{"argv": []string{"bash", "-c", "printf ok > in.txt"}})
		t.Logf("run_command:\n%s", res.Content)
	})
	if got, err := os.ReadFile(filepath.Join(root, "in.txt")); err != nil || string(got) != "ok" {
		t.Fatalf("in.txt = %q (err %v), want \"ok\" from the native command after the localhost. turn", got, err)
	}
}
