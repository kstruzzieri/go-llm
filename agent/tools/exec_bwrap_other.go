//go:build !linux

package tools

import "fmt"

// newBwrapExecBackend fails closed off Linux, mirroring exec_seatbelt_other.go:
// selecting an unavailable isolation runtime must never degrade to host
// execution.
// errBwrapRequiresLinux is the constant failure of this stub. A named
// variable rather than an inline fmt.Errorf keeps staticcheck from proving
// the caller's error check always true on this platform (SA4023).
var errBwrapRequiresLinux = fmt.Errorf("tools: sandbox runtime %q requires Linux", SandboxRuntimeBwrap)

func newBwrapExecBackend(SandboxConfig) (execBackend, error) {
	return nil, errBwrapRequiresLinux
}
