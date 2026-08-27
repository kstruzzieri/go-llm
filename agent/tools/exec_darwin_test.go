//go:build darwin

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeSandboxExec writes an executable shell script standing in for
// /usr/bin/sandbox-exec in probe tests.
func writeFakeSandboxExec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sandbox-exec")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeSeatbeltFileChecks(t *testing.T) {
	dir := t.TempDir()
	nonexec := filepath.Join(dir, "sandbox-exec")
	if err := os.WriteFile(nonexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"missing":        filepath.Join(dir, "missing"),
		"directory":      dir,
		"non-executable": nonexec,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			err := probeSeatbelt(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("probe must fail naming the path: %v", err)
			}
		})
	}
}

// TestProbeSeatbeltNestedSandboxShape drives the real probe through a stand-in
// binary reproducing the observed nested-sandbox failure: exists, executable,
// but cannot apply a profile. A stat-only check would pass here; the active
// probe must fail and carry the diagnostic.
func TestProbeSeatbeltNestedSandboxShape(t *testing.T) {
	fake := writeFakeSandboxExec(t,
		"echo 'sandbox_apply: Operation not permitted' >&2\nexit 1\n")
	err := probeSeatbelt(context.Background(), fake)
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("probe must surface the sandbox_apply diagnostic: %v", err)
	}
}

func TestProbeSeatbeltTimeout(t *testing.T) {
	fake := writeFakeSandboxExec(t, "sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := probeSeatbelt(ctx, fake); err == nil {
		t.Fatal("hung probe must fail, not block construction forever")
	}
}

func TestProbeSeatbeltSucceedsOnFakeApplier(t *testing.T) {
	fake := writeFakeSandboxExec(t, "exit 0\n")
	if err := probeSeatbelt(context.Background(), fake); err != nil {
		t.Fatalf("probe of a working applier failed: %v", err)
	}
}

func TestSeatbeltConstructorRejectsUnenforceableBeforeProbe(t *testing.T) {
	probeCalled := false
	_, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { probeCalled = true; return nil },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt, MemoryCapMB: 512})
	if err == nil || !strings.Contains(err.Error(), "MemoryCapMB") {
		t.Fatalf("unenforceable field accepted: %v", err)
	}
	if probeCalled {
		t.Fatal("probe must not run for a rejected config")
	}
}

func TestSeatbeltConstructorFailsClosedOnProbeError(t *testing.T) {
	probeErr := errors.New("sandbox_apply: Operation not permitted")
	backend, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { return probeErr },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	if backend != nil || !errors.Is(err, probeErr) {
		t.Fatalf("probe failure must fail construction: backend=%T err=%v", backend, err)
	}
}

func TestSeatbeltConstructorSucceedsOnProbeSuccess(t *testing.T) {
	backend, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { return nil },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	sb, ok := backend.(*seatbeltBackend)
	if !ok {
		t.Fatalf("backend = %T, want *seatbeltBackend", backend)
	}
	if !sb.allowNetwork || sb.execPath != "/usr/bin/sandbox-exec" {
		t.Fatalf("backend misconfigured: %+v", sb)
	}
	if sb.runner == nil || sb.starter == nil {
		t.Fatal("backend must carry both host delegates")
	}
}

// TestNewExecBackendSeatbeltMatchesRealCapability is the characterization pin:
// public construction succeeds exactly when the real active probe succeeds on
// this host. On an unsandboxed macOS host both succeed; inside a nested
// sandbox both must fail.
func TestNewExecBackendSeatbeltMatchesRealCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	probeErr := probeSeatbelt(ctx, seatbeltExecPath)
	got, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	if (err == nil) != (probeErr == nil) {
		t.Fatalf("constructor outcome (err=%v) disagrees with real capability (probe=%v)", err, probeErr)
	}
	if err != nil {
		return // incapable host: fail-closed is the correct outcome
	}
	if _, ok := got.execBackend.(*seatbeltBackend); !ok {
		t.Fatalf("backend = %T, want *seatbeltBackend", got.execBackend)
	}
	if !strings.HasPrefix(got.approval.keyComponent, "sb:") {
		t.Fatalf("seatbelt must get a sandbox key namespace, got %q", got.approval.keyComponent)
	}
	want := `runtime="seatbelt" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private`
	if got.approval.preview != want {
		t.Fatalf("preview = %q, want %q", got.approval.preview, want)
	}
}
