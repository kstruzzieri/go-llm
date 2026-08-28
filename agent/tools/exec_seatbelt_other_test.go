//go:build !darwin

package tools

import (
	"strings"
	"testing"
)

// TestSeatbeltFailsClosedOffDarwin pins the off-platform contract: selecting
// the seatbelt runtime anywhere but macOS fails construction — never a silent
// host fallback.
func TestSeatbeltFailsClosedOffDarwin(t *testing.T) {
	got, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	if err == nil || got.execBackend != nil {
		t.Fatalf("seatbelt off darwin returned backend=%T err=%v", got.execBackend, err)
	}
	if !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("error must say the runtime requires macOS: %v", err)
	}
}
