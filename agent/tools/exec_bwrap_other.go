//go:build !linux

package tools

import "fmt"

// newBwrapExecBackend fails closed off Linux, mirroring exec_seatbelt_other.go:
// selecting an unavailable isolation runtime must never degrade to host
// execution.
func newBwrapExecBackend(SandboxConfig) (execBackend, error) {
	return nil, fmt.Errorf("tools: sandbox runtime %q requires Linux", SandboxRuntimeBwrap)
}
