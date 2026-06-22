package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// indexSchemaVersion is the on-disk version of the index sidecar marker.
const indexSchemaVersion = 1

// indexSidecar is the JSON "index marker" written next to a per-workspace index
// DB after a successful (or partial) golem index. Its presence + validity is the
// auto-discovery gate: a bare/copied DB without a matching sidecar is not trusted.
type indexSidecar struct {
	SchemaVersion           int    `json:"schemaVersion"`
	WorkspaceID             string `json:"workspaceID"`
	RequestedEmbeddingModel string `json:"requestedEmbeddingModel"`
	VectorSpaceID           string `json:"vectorSpaceID"`
	IndexedAt               string `json:"indexedAt"`
	Status                  string `json:"status"` // "complete" | "partial"
	ErrorCount              int    `json:"errorCount"`
}

// writeSidecar atomically writes the marker with mode 0600 (temp file + rename),
// so a crash never leaves a half-written sidecar.
func writeSidecar(path string, s indexSidecar) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("golem: marshal index sidecar: %w", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("golem: write index sidecar: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("golem: chmod index sidecar: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("golem: rename index sidecar: %w", err)
	}
	return nil
}

// readSidecar loads + parses a marker. A not-exist error is returned verbatim so
// callers can distinguish "no index" (os.IsNotExist) from a corrupt sidecar.
func readSidecar(path string) (indexSidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return indexSidecar{}, err
	}
	var s indexSidecar
	if err := json.Unmarshal(data, &s); err != nil {
		return indexSidecar{}, fmt.Errorf("golem: parse index sidecar %q: %w", path, err)
	}
	return s, nil
}

// validateSidecar reports whether the marker may be trusted for auto-discovery
// of workspaceID: known schema version and matching workspace identity.
func validateSidecar(s indexSidecar, workspaceID string) error {
	if s.SchemaVersion != indexSchemaVersion {
		return fmt.Errorf("golem: index sidecar schemaVersion %d unsupported (want %d)", s.SchemaVersion, indexSchemaVersion)
	}
	if s.WorkspaceID != workspaceID {
		return fmt.Errorf("golem: index sidecar workspaceID %q does not match %q", s.WorkspaceID, workspaceID)
	}
	return nil
}
