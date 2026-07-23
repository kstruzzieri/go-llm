package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// indexDBPathForWorkspace resolves the per-workspace RAG index DB path and the
// workspace id under the per-user data dir
// (<base>/golem/indexes/<sha16>.db). <sha16> is the same key
// resolveSessionID uses for the default workspace session id, so a workspace's
// session and index share one identity. The DB is validated to live outside the
// workspace so indexing/editing never touches it.
func indexDBPathForWorkspace(getenv func(string) string, root string) (dbPath, workspaceID string, err error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(root))
	key := hex.EncodeToString(sum[:])[:16]
	dbPath = filepath.Join(base, "golem", "indexes", key+".db")
	if err := validatePathOutsideWorkspace(dbPath, root); err != nil {
		return "", "", err
	}
	return dbPath, "workspace:" + key, nil
}

// sidecarPath returns the index-marker JSON path alongside the DB
// (<sha16>.db -> <sha16>.json).
func sidecarPath(dbPath string) string {
	return strings.TrimSuffix(dbPath, ".db") + ".json"
}
