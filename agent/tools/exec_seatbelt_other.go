//go:build !darwin

package tools

import "fmt"

// newSeatbeltExecBackend fails closed off macOS, mirroring exec_other.go:
// selecting an unavailable isolation runtime must never degrade to host
// execution.
// errSeatbeltRequiresDarwin is the constant failure of this stub. A named
// variable rather than an inline fmt.Errorf keeps staticcheck from proving
// the caller's error check always true on this platform (SA4023).
var errSeatbeltRequiresDarwin = fmt.Errorf("tools: sandbox runtime %q requires macOS", SandboxRuntimeSeatbelt)

func newSeatbeltExecBackend(SandboxConfig) (execBackend, error) {
	return nil, errSeatbeltRequiresDarwin
}
