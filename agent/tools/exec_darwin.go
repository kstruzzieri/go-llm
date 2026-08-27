//go:build darwin

package tools

// macOS Seatbelt (sandbox-exec) backend (#442): Darwin-only capability probe
// and process wiring. The security-critical SBPL rendering lives in the
// cross-platform seatbelt.go so it is unit-tested on every CI platform.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// seatbeltExecPath is the system sandbox-exec binary. Deprecated by Apple
	// but functional; its absence or inability to apply a profile fails
	// construction — never a silent host fallback.
	seatbeltExecPath = "/usr/bin/sandbox-exec"
	// seatbeltProbeTimeout bounds the active capability probe.
	seatbeltProbeTimeout = 5 * time.Second
	// seatbeltProbeOutputCap bounds captured probe diagnostics.
	seatbeltProbeOutputCap = 1024
)

// probeSeatbelt actively verifies the host can apply a Seatbelt profile:
// file checks first, then a harmless allow-default run of /usr/bin/true.
// Existence does not imply usability — a process that is already sandboxed
// has an executable sandbox-exec that fails with "sandbox_apply: Operation
// not permitted", so a stat-only check would be a false positive.
func probeSeatbelt(ctx context.Context, execPath string) error {
	fi, err := os.Stat(execPath)
	if err != nil {
		return fmt.Errorf("tools: seatbelt requires %s: %w", execPath, err)
	}
	if !isExecutableFile(fi) {
		return fmt.Errorf("tools: seatbelt requires %s: %w", execPath, errNotExecutable)
	}
	cmd := exec.CommandContext(ctx, execPath, "-p", "(version 1)(allow default)", "/usr/bin/true")
	out := &cappedBuffer{cap: seatbeltProbeOutputCap}
	cmd.Stdout = out
	cmd.Stderr = out
	// Bound Wait when a descendant of the probed binary outlives it while
	// holding the stdio pipes open, mirroring the runners.
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(string(out.buf)); detail != "" {
			return fmt.Errorf("tools: seatbelt unavailable on this host: %w: %s", err, detail)
		}
		return fmt.Errorf("tools: seatbelt unavailable on this host: %w", err)
	}
	return nil
}

// seatbeltProbeFunc is the injectable probe seam for deterministic unit tests.
type seatbeltProbeFunc func(context.Context, string) error

// seatbeltBackend runs both command lifetimes under per-invocation Seatbelt
// profiles by rewriting the re-checked spec to sandbox-exec and delegating to
// the host unix implementations.
type seatbeltBackend struct {
	execPath     string
	allowNetwork bool
	runner       commandRunner
	starter      backgroundStarter
}

// newSeatbeltExecBackend constructs the real Seatbelt backend, failing closed
// when the config requests ceilings Seatbelt cannot enforce or when the
// active capability probe fails.
func newSeatbeltExecBackend(cfg SandboxConfig) (execBackend, error) {
	return newSeatbeltExecBackendAt(seatbeltExecPath, probeSeatbelt, cfg)
}

func newSeatbeltExecBackendAt(execPath string, probe seatbeltProbeFunc, cfg SandboxConfig) (execBackend, error) {
	if err := seatbeltConfigSupport(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	if err := probe(ctx, execPath); err != nil {
		return nil, err
	}
	return &seatbeltBackend{
		execPath:     execPath,
		allowNetwork: cfg.AllowNetwork,
		runner:       unixRunner{},
		starter:      unixStarter{},
	}, nil
}

// Run and Start are wired in the follow-up commits of #442; until then the
// backend fails closed rather than ever running a target unsandboxed.
func (b *seatbeltBackend) Run(context.Context, execSpec) (execResult, error) {
	return execResult{}, errors.New("tools: seatbelt foreground execution is not wired yet")
}

func (b *seatbeltBackend) Start(execSpec, io.Writer, io.Writer) (backgroundProcess, error) {
	return nil, errors.New("tools: seatbelt background execution is not wired yet")
}
