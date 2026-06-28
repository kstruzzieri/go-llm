package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// workspaceID derives the stable per-workspace identity (sha256 prefix of the
// canonical root), matching resolveSessionID's default branch so session and
// memory agree on workspace identity.
func workspaceID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "workspace:" + hex.EncodeToString(sum[:])[:16]
}

// memoryDBPath locates the memory DB OUTSIDE the repo, under the per-user data
// dir ($XDG_DATA_HOME/golem/memories.db, else ~/.local/share/golem/memories.db).
// It is a separate file from the session DB.
func memoryDBPath(getenv func(string) string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "golem", "memories.db"), nil
}

func memoryDBPathForWorkspace(getenv func(string) string, root string) (string, error) {
	p, err := memoryDBPath(getenv)
	if err != nil {
		return "", err
	}
	if err := validatePathOutsideWorkspace(p, root); err != nil {
		return "", err
	}
	return p, nil
}

// memorySearchRedactedMarker replaces a memory_search tool result when the turn
// is persisted, so raw retrieved memory rows do not enter session history or get
// folded into the pinned durable summary. The live turn already consumed the real
// result; this only affects what is stored.
const memorySearchRedactedMarker = "memory_search result omitted from session history"
