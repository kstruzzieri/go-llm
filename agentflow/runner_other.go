//go:build !unix

package agentflow

import "os/exec"

// Non-Unix platforms retain exec.CommandContext's direct-child cancellation.
func configureProcessCancellation(_ *exec.Cmd) {}
