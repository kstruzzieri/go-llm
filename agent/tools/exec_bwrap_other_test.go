//go:build !linux

package tools

import (
	"strings"
	"testing"
)

// TestBwrapConstructorFailsClosedOffLinux pins the !linux stub directly:
// selecting the bwrap runtime on a non-Linux host must fail construction,
// never degrade to host execution.
func TestBwrapConstructorFailsClosedOffLinux(t *testing.T) {
	backend, err := newBwrapExecBackend(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if backend != nil || err == nil {
		t.Fatalf("off-Linux bwrap constructor returned backend=%T err=%v", backend, err)
	}
	if !strings.Contains(err.Error(), "requires Linux") ||
		!strings.Contains(err.Error(), `"bwrap"`) {
		t.Fatalf("error must name runtime and platform requirement: %v", err)
	}
}

// TestNewExecBackendBwrapFailsClosedOffLinux pins the dispatch path off
// Linux: selecting bwrap must surface the stub's fail-closed error, never a
// host backend.
func TestNewExecBackendBwrapFailsClosedOffLinux(t *testing.T) {
	got, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err == nil || got.execBackend != nil {
		t.Fatalf("bwrap dispatch off Linux returned backend=%T err=%v", got.execBackend, err)
	}
	if !strings.Contains(err.Error(), "requires Linux") {
		t.Fatalf("error must state platform requirement: %v", err)
	}
}
