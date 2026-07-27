package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIndexWriterLeaseHelper(t *testing.T) {
	if os.Getenv("GO_LLM_INDEX_LEASE_HELPER") != "1" {
		return
	}
	lease, err := acquireIndexWriterLease(os.Args[len(os.Args)-1])
	if errors.Is(err, errIndexWriterLeaseHeld) {
		os.Exit(3)
	}
	if err != nil {
		os.Exit(4)
	}
	if err := lease.Close(); err != nil {
		os.Exit(5)
	}
	os.Exit(0)
}

func runLeaseHelper(t *testing.T, dbPath string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestIndexWriterLeaseHelper$", "--", dbPath)
	cmd.Env = append(os.Environ(), "GO_LLM_INDEX_LEASE_HELPER=1")
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func TestIndexWriterLease_ContendsAcrossProcesses(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "k.db")
	lease, err := acquireIndexWriterLease(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := runLeaseHelper(t, dbPath); got != 3 {
		t.Fatalf("contending process exit = %d, want 3", got)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runLeaseHelper(t, dbPath); got != 0 {
		t.Fatalf("process after release exit = %d, want 0", got)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}
