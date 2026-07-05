package main

import (
	"context"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/rag"
)

// autoIndexProbeTimeout bounds the startup embed probe so a hung backend
// cannot stall auto-index classification indefinitely.
const autoIndexProbeTimeout = 30 * time.Second

// probeAutoIndexEmbedder verifies the exact indexing path (routing, fallback
// chain, model load) with one real embed call before any background indexing
// starts. Errors are concise and safe to surface in a startup warning.
func probeAutoIndexEmbedder(ctx context.Context, embedder rag.Embedder, model string) error {
	child, cancel := context.WithTimeout(ctx, autoIndexProbeTimeout)
	defer cancel()
	res, err := embedder.Embed(child, model, []string{"golem startup index probe"})
	if err != nil {
		return fmt.Errorf("embed probe (%s): %w", model, err)
	}
	if len(res.Embeddings) != 1 || len(res.Embeddings[0]) == 0 {
		return fmt.Errorf("embed probe (%s): got %d vectors, want exactly 1 non-empty", model, len(res.Embeddings))
	}
	return nil
}

// autoIndexClass is classifyAutoIndex's outcome. full=true means the runner
// must pass full=true to prepareIndexStore (which removes the artifacts —
// classification itself deletes nothing); reason feeds the self-heal rebuild
// notice and is empty for incremental runs.
type autoIndexClass struct {
	full   bool
	reason string
}

// classifyAutoIndex decides whether the startup auto refresh runs incremental
// or full on the PRIVATE autoDBPath store. It mirrors preflightExistingIndex's
// checks but maps every failure to "full rebuild" instead of refusing: the
// private store is golem-owned and disposable (self-heal policy). It must
// never be pointed at a user-supplied -rag-db, which never self-heals.
//
// The vector-space probe opens the DB read-only so classification never
// creates WAL/SHM (matching preflightExistingIndex). An existing DB we cannot
// even open or probe is not trustworthy for an incremental run either, so
// those errors also select a full rebuild.
func classifyAutoIndex(ctx context.Context, dbPath, sidecarPath, workspaceID string, expected []string) autoIndexClass {
	if !fileExists(dbPath) {
		// First build: incremental on an empty store is a full build anyway.
		return autoIndexClass{}
	}
	sc, err := readSidecar(sidecarPath)
	if err != nil {
		return autoIndexClass{full: true, reason: "existing index has no valid sidecar"}
	}
	if err := validateSidecar(sc, workspaceID); err != nil {
		return autoIndexClass{full: true, reason: fmt.Sprintf("index sidecar invalid: %v", err)}
	}
	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		return autoIndexClass{full: true, reason: fmt.Sprintf("cannot open existing index: %v", err)}
	}
	defer func() { _ = store.Close() }()
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		return autoIndexClass{full: true, reason: fmt.Sprintf("cannot probe existing index: %v", err)}
	}
	if dec := vsGateDecision(probe.KnownIDs, probe.HasUnknown, expected); !dec.register {
		return autoIndexClass{full: true, reason: fmt.Sprintf("index vector space %s does not match embedding chain %v",
			describeStored(probe), expected)}
	}
	return autoIndexClass{}
}
