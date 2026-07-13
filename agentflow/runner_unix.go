//go:build unix

package agentflow

import (
	"os/exec"
	"syscall"
)

// configureProcessCancellation runs the child in its own process group and
// kills the group on cancellation so Agentflow gate-command grandchildren are
// not left behind.
func configureProcessCancellation(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
