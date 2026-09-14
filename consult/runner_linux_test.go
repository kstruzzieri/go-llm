package consult

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A child deliberately left unreaped makes this independent of PID 1's reaper.
func TestGroupExitedAcceptsZombiesButRejectsLiveProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, script(t, "exec sleep 30"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = killGroup(pid); _ = cmd.Wait() })
	if groupExited(pid) {
		t.Fatal("live process group reported as cleaned up")
	}
	if err := killGroup(pid); err != nil {
		t.Fatal(err)
	}
	var status unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &status, unix.WEXITED|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		break
	}
	if err := syscall.Kill(-pid, 0); err != nil {
		t.Fatalf("expected an unreaped process group: %v", err)
	}
	if !groupExited(pid) {
		t.Fatal("zombie-only process group reported as live")
	}
}

func TestZombieGroupFailsClosed(t *testing.T) {
	getpgid := func(int) (int, error) { return 42, nil }
	// comm contains both spaces and a ')' so a whitespace split of the whole
	// record would read the wrong process group and thread count.
	const zombie = "123 (worker ) child) Z 1 42 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1"
	for _, tc := range []struct {
		name, stat string
		want       bool
	}{
		{"zombie", zombie, true},
		{"live", strings.Replace(zombie, " Z ", " S ", 1), false},
		{"other_group", strings.Replace(zombie, " 42 ", " 43 ", 1), false},
		{"live_threads", strings.TrimSuffix(zombie, "1") + "2", false},
		{"malformed_group", strings.Replace(zombie, " 42 ", " bad ", 1), false},
		{"malformed_threads", strings.TrimSuffix(zombie, "1") + "bad", false},
		{"truncated", "123 (worker) Z 1 42", false},
		{"malformed", "123 worker Z 1 42", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			pidDir := filepath.Join(root, "123")
			if err := os.Mkdir(pidDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(pidDir, "stat"), []byte(tc.stat), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := zombieGroup(root, 42, getpgid); got != tc.want {
				t.Fatalf("zombieGroup = %v, want %v", got, tc.want)
			}
			if tc.want {
				// A live member or an unreadable stat must invalidate otherwise
				// sufficient evidence from another member of the same group.
				other := filepath.Join(root, "456")
				if err := os.Mkdir(other, 0o700); err != nil {
					t.Fatal(err)
				}
				statPath := filepath.Join(other, "stat")
				if err := os.WriteFile(statPath, []byte(strings.Replace(zombie, " Z ", " S ", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				if zombieGroup(root, 42, getpgid) {
					t.Fatal("live member ignored")
				}
				if err := os.Remove(statPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(statPath, 0o700); err != nil {
					t.Fatal(err)
				}
				if zombieGroup(root, 42, getpgid) {
					t.Fatal("unreadable member ignored")
				}
			}
		})
	}
	if zombieGroup(filepath.Join(t.TempDir(), "absent"), 42, getpgid) {
		t.Fatal("missing proc filesystem accepted")
	}
}

func TestZombieGroupSkipsOnlyKnownUnrelatedOrReapedProcesses(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "123"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "123", "stat"),
		[]byte("123 (worker) Z 1 42 0 0 0 0 0 0 0 0 0 0 0 0 0 0 1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory makes ReadFile fail even as root, so this also exercises the
	// permission-failure boundary in CI without depending on chmod enforcement.
	if err := os.MkdirAll(filepath.Join(root, "456", "stat"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		group int
		err   error
		want  bool
	}{
		{"unrelated", 43, nil, true},
		{"reaped", 0, syscall.ESRCH, true},
		{"member", 42, nil, false},
		{"unknown", 0, syscall.EPERM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getpgid := func(pid int) (int, error) {
				if pid == 123 {
					return 42, nil
				}
				return tc.group, tc.err
			}
			if got := zombieGroup(root, 42, getpgid); got != tc.want {
				t.Fatalf("cleanup = %v, want %v", got, tc.want)
			}
		})
	}
}
