//go:build darwin || linux || freebsd

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Bound regressions that close the FIFO before writing, then block reopening it.
func TestRunCacheProbeModeNamedPipe(t *testing.T) {
	const childEnv = "LLM_BENCH_CACHE_PROBE_FIFO_CHILD"
	if os.Getenv(childEnv) == "" {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCacheProbeModeNamedPipe$")
		cmd.Env = append(os.Environ(), childEnv+"="+filepath.Join(t.TempDir(), "probe.fifo"))
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatal("cache probe blocked writing its table to a named pipe")
		}
		if err != nil {
			t.Fatalf("named-pipe probe: %v\n%s", err, output)
		}
		return
	}

	path := os.Getenv(childEnv)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	type readResult struct {
		data []byte
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		data, err := os.ReadFile(path)
		read <- readResult{data: data, err: err}
	}()
	ps := newProbeServer(t)
	err := runCacheProbeMode(t.Context(), cacheProbeOptions{
		BaseURL: ps.URL, Models: "openai-compat/probe-model", Out: path, Scenario: string(probeUncompacted),
	}, &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	got := <-read
	if got.err != nil {
		t.Fatal(got.err)
	}
	table := string(got.data)
	if !strings.HasPrefix(table, "# Cache probe: scenario uncompacted\n") || strings.Count(table, "\n| ")-1 != 12 {
		t.Fatalf("named-pipe reader did not receive a complete 12-row table:\n%s", table)
	}
}
