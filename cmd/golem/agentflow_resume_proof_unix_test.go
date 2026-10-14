//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReadResumeProof_RejectsSymlinks(t *testing.T) {
	for _, name := range []string{"regular target", "dangling"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			target := filepath.Join(root, "target.json")
			if name == "regular target" {
				if err := os.WriteFile(target, []byte("retained proof"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(root, "proof-pack.json")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if _, err := readResumeProof(path); err == nil || !strings.Contains(err.Error(), "regular file") {
				t.Errorf("readResumeProof(%s symlink) = %v, want regular-file refusal", name, err)
			}
		})
	}
}

// A subprocess bounds regressions that block while opening a FIFO.
func TestReadResumeProof_RejectsFIFOWithoutBlocking(t *testing.T) {
	const childEnv = "GOLEM_RESUME_PROOF_FIFO_CHILD"
	const rootEnv = "GOLEM_RESUME_PROOF_FIFO_ROOT"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReadResumeProof_RejectsFIFOWithoutBlocking$")
		cmd.Env = append(os.Environ(), childEnv+"=1", rootEnv+"="+t.TempDir())
		output, err := cmd.CombinedOutput()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("readResumeProof(FIFO) blocked, want immediate regular-file refusal")
		}
		if err != nil {
			t.Fatalf("readResumeProof(FIFO) subprocess = %v, want success\n%s", err, output)
		}
		return
	}
	path := filepath.Join(os.Getenv(rootEnv), "proof-pack.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readResumeProof(path); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("readResumeProof(FIFO) = %v, want regular-file refusal", err)
	}
}
