//go:build !unix

package agentflow

import "os/exec"

// Non-Unix retains exec.CommandContext's direct-child cancellation; gate-command
// grandchildren are not reaped on cancel because there is no process-group kill.
// See runner_unix.go.
func configureProcessCancellation(_ *exec.Cmd) {}
