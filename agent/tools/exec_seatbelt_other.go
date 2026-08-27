//go:build !darwin

package tools

import "fmt"

// newSeatbeltExecBackend fails closed off macOS, mirroring exec_other.go:
// selecting an unavailable isolation runtime must never degrade to host
// execution.
func newSeatbeltExecBackend(SandboxConfig) (execBackend, error) {
	return nil, fmt.Errorf("tools: sandbox runtime %q requires macOS", SandboxRuntimeSeatbelt)
}
