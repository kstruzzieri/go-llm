package main

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleSidecar(wsID string) indexSidecar {
	return indexSidecar{
		SchemaVersion:           indexSchemaVersion,
		WorkspaceID:             wsID,
		RequestedEmbeddingModel: "ollama/nomic",
		VectorSpaceID:           "ollama/nomic",
		IndexedAt:               "2026-06-22T00:00:00Z",
		Status:                  "complete",
		ErrorCount:              0,
	}
}

func TestSidecarRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idx.json")
	want := sampleSidecar("workspace:abc123")
	if err := writeSidecar(path, want); err != nil {
		t.Fatalf("writeSidecar: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("sidecar mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not survive a successful write")
	}
	got, err := readSidecar(path)
	if err != nil {
		t.Fatalf("readSidecar: %v", err)
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

func TestValidateSidecar(t *testing.T) {
	ws := "workspace:abc123"
	if err := validateSidecar(sampleSidecar(ws), ws); err != nil {
		t.Errorf("valid sidecar rejected: %v", err)
	}
	bad := sampleSidecar(ws)
	bad.SchemaVersion = 999
	if err := validateSidecar(bad, ws); err == nil {
		t.Error("want error for unsupported schemaVersion")
	}
	foreign := sampleSidecar("workspace:other")
	if err := validateSidecar(foreign, ws); err == nil {
		t.Error("want error for foreign workspaceID")
	}
}
