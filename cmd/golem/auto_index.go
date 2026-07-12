package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// autoIndexProbeTimeout bounds the startup embed probe so a hung backend
// cannot stall auto-index classification indefinitely.
const autoIndexProbeTimeout = 30 * time.Second

// probeAutoIndexEmbedder verifies the exact indexing path (routing, fallback
// chain, model load) with one real embed call before any background indexing
// starts. Errors are concise and safe to surface in a startup warning.
func probeAutoIndexEmbedder(ctx context.Context, embedder rag.Embedder, model string) (string, error) {
	child, cancel := context.WithTimeout(ctx, autoIndexProbeTimeout)
	defer cancel()
	res, err := embedder.Embed(child, model, []string{"golem startup index probe"})
	if err != nil {
		return "", fmt.Errorf("embed probe (%s): %w", model, err)
	}
	if len(res.Embeddings) != 1 || len(res.Embeddings[0]) == 0 {
		return "", fmt.Errorf("embed probe (%s): want one non-empty vector, got %d vector(s)", model, len(res.Embeddings))
	}
	vectorSpaceID := res.VectorSpaceID
	if vectorSpaceID == "" && res.Provider != "" && res.Model != "" {
		vectorSpaceID = res.Provider + "/" + res.Model
	}
	if vectorSpaceID == "" {
		return "", fmt.Errorf("embed probe (%s): successful route returned no vector-space identity", model)
	}
	return vectorSpaceID, nil
}

// autoIndexJob carries the background auto-index runner's dependencies. The
// embedder is pre-built by the caller (the same chain embedder used for the
// probe and the index run) and embChain doubles as the expected vector-space
// set: expectedVectorSpaces(cfg) and embeddingChain(cfg) return the identical
// chain, so no separate expected field is carried.
type autoIndexJob struct {
	root        string
	dbPath      string
	workspaceID string
	cfg         *config.Config
	router      *provider.Router
	embedder    rag.Embedder
	embChain    []string
	feedbackDB  string
	ready       *readyRetrieve
	notice      func(string)

	// openRetriever is a test seam: nil selects the real buildGatedRetriever
	// over cfg/router; tests inject a fake because buildGatedRetriever needs
	// a live provider router to construct the query-time chain embedder.
	openRetriever func(context.Context, string) (*retrievalReader, string, vsDecision, rag.StoreStats, error)
}

// runAutoIndex acquires the workspace writer lease, probes the actual vector
// space, builds and validates an immutable staging generation, publishes its
// active pointer, then swaps the read-only delegate. It is synchronous; the
// caller launches the goroutine.
// A cancelled ctx returns silently: the wrapper stays warming and no notices
// print during shutdown.
func runAutoIndex(ctx context.Context, job autoIndexJob) {
	fail := func(reason string) {
		if ctx.Err() != nil {
			return
		}
		if job.ready.hasReader() {
			job.notice("warning: retrieve auto-index refresh failed: " + reason + "; serving active generation")
			return
		}
		job.ready.markFailed("retrieve: the workspace auto-index failed (" + reason + "); " +
			"use read_file, search, glob, and list instead.")
		job.notice("warning: retrieve auto-index failed: " + reason + "; using file/search tools")
	}

	// Caller-owned invariant (the job is only built after embeddingChain
	// succeeds), but self-defend: an empty chain must fail, not panic.
	if len(job.embChain) == 0 {
		fail("no embedding chain configured")
		return
	}
	lease, err := acquireIndexWriterLease(job.dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		if job.ready.hasReader() {
			job.notice("warning: retrieve auto-index writer lease already held; serving active generation")
		} else {
			fail("workspace index writer lease already held")
		}
		return
	}
	if err != nil {
		fail(err.Error())
		return
	}
	defer func() { _ = lease.Close() }()

	// Probe FIRST: a dead embedder must not create or write the store.
	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, job.embedder, job.embChain[0])
	if err != nil {
		fail(err.Error())
		return
	}

	active, activeErr := resolveActiveGeneration(ctx, job.dbPath, job.workspaceID)
	if activeErr != nil && !errors.Is(activeErr, os.ErrNotExist) {
		job.notice("warning: retrieve auto-index rebuilding private store: " + activeErr.Error())
	}
	activeID := ""
	if activeErr == nil {
		activeID = active.id
	}
	if err := cleanupStaleGenerations(job.dbPath, activeID); err != nil {
		fail(err.Error())
		return
	}
	staging, err := createStagingGeneration(job.dbPath)
	if err != nil {
		fail(err.Error())
		return
	}
	stagingDir := filepath.Dir(staging.dbPath)
	discardStaging := true
	defer func() {
		if discardStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	if activeErr == nil && active.metadata.VectorSpaceID == actualVectorSpace {
		if err := copyImmutableFile(active.dbPath, staging.dbPath); err != nil {
			fail(err.Error())
			return
		}
	}
	store, err := rag.NewSQLiteStore(staging.dbPath)
	if err != nil {
		fail(err.Error())
		return
	}
	indexer, err := rag.NewIndexerWithEmbedder(job.embedder, store, rag.WithEmbeddingModel(job.embChain[0]))
	if err != nil {
		_ = store.Close()
		fail(err.Error())
		return
	}
	var buf bytes.Buffer
	res := executeIndex(ctx, indexJob{
		indexer:        indexer,
		store:          store,
		root:           job.root,
		dbPath:         staging.dbPath,
		sidecarPath:    staging.metadataPath,
		workspaceID:    job.workspaceID,
		requestedModel: job.embChain[0],
		out:            &buf,
		pruneDeleted:   true,
	})
	// Close the writer BEFORE any read-only open: retrieval opens the DB with
	// immutable=1 and must not race a live write handle.
	if checkpointErr := checkpointGeneration(ctx, store); checkpointErr != nil {
		_ = store.Close()
		fail(checkpointErr.Error())
		return
	}
	if closeErr := store.Close(); closeErr != nil {
		fail(fmt.Sprintf("close staging generation: %v", closeErr))
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Interpret the outcome through the store, not the exit sentinel alone
	// (spec "Partial Indexes"): a partial run still writes a usable sidecar.
	sc, scErr := readSidecar(staging.metadataPath)
	if scErr != nil || validateSidecar(sc, job.workspaceID) != nil {
		fail(autoIndexFailReason(&buf, res.exitErr))
		return
	}

	if res.exitErr != nil && sc.Status != "partial" {
		fail(autoIndexFailReason(&buf, res.exitErr))
		return
	}
	readOnly, err := rag.OpenSQLiteStoreReadOnly(staging.dbPath)
	if err != nil {
		fail(err.Error())
		return
	}
	stats, err := readOnly.Stats(ctx)
	_ = readOnly.Close()
	if err != nil {
		fail(err.Error())
		return
	}
	if res.exitErr != nil && stats.TotalSources < 1 {
		fail(autoIndexFailReason(&buf, res.exitErr))
		return
	}
	if sc.VectorSpaceID != actualVectorSpace {
		fail(fmt.Sprintf("staging vector space %q does not match successful probe %q", sc.VectorSpaceID, actualVectorSpace))
		return
	}
	staging.metadata = generationMetadata{
		SchemaVersion: generationSchemaVersion, Generation: staging.id,
		WorkspaceID: job.workspaceID, RequestedEmbeddingModel: job.embChain[0],
		VectorSpaceID: sc.VectorSpaceID, IndexedAt: sc.IndexedAt,
		Status: sc.Status, ErrorCount: sc.ErrorCount,
		SourceCount: stats.TotalSources, ChunkCount: stats.TotalChunks,
	}
	if err := writeGenerationMetadata(staging.metadataPath, staging.metadata); err != nil {
		fail(err.Error())
		return
	}
	pointer := activeGenerationPointer{SchemaVersion: activePointerSchemaVersion, WorkspaceID: job.workspaceID, Generation: staging.id}
	if err := validateGenerationMetadata(staging.metadata, pointer); err != nil {
		fail(err.Error())
		return
	}
	if err := validateGenerationDatabase(ctx, staging); err != nil {
		fail(err.Error())
		return
	}
	final, err := finalizeStagingGeneration(job.dbPath, staging)
	if err != nil {
		fail(err.Error())
		return
	}
	stagingDir = filepath.Dir(final.dbPath)
	if err := publishActiveGeneration(job.dbPath, pointer, nil); err != nil {
		if current, readErr := readActivePointer(job.dbPath); readErr != nil || current.Generation != final.id {
			_ = os.RemoveAll(stagingDir)
		}
		discardStaging = false
		fail(err.Error())
		return
	}
	discardStaging = false
	open := job.openRetriever
	if open == nil {
		open = func(oc context.Context, dbPath string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
			return buildGatedRetriever(oc, job.cfg, job.router, dbPath, job.embChain, job.feedbackDB)
		}
	}
	reader, feedbackWarn, dec, openedStats, err := open(ctx, final.dbPath)
	if err != nil {
		fail(err.Error())
		return
	}
	if reader == nil {
		fail(fmt.Sprintf("index vector space %s does not match embedding chain %v", dec.stored, job.embChain))
		return
	}
	line := autoIndexReadyLine(sc, openedStats)
	job.ready.install(reader, line)
	job.notice(line)
	if feedbackWarn != "" {
		job.notice("warning: " + feedbackWarn)
	}
}

func copyImmutableFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open active generation for staging copy: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create staging database: %w", err)
	}
	remove := true
	defer func() {
		_ = out.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy active generation: %w", err)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync staging database: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close staging database: %w", err)
	}
	remove = false
	return nil
}

// autoIndexReadyLine renders the completion notice (spec "Notices"): the ready
// line for a complete sidecar, the partial warning for a partial one.
func autoIndexReadyLine(sc indexSidecar, stats rag.StoreStats) string {
	if sc.Status == "partial" {
		return fmt.Sprintf("warning: retrieve auto-index partial, %d sources, %d errors; retrieval enabled",
			stats.TotalSources, sc.ErrorCount)
	}
	return fmt.Sprintf("retrieve: auto-index ready, %d sources, %s, updated %s",
		stats.TotalSources, sc.VectorSpaceID, sc.IndexedAt)
}

// autoIndexFailReason caps the background failure reason to one line: the
// first useful line executeIndex printed, else the exit error.
func autoIndexFailReason(buf *bytes.Buffer, exitErr error) string {
	for _, line := range strings.Split(buf.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if exitErr != nil {
		return exitErr.Error()
	}
	return "index failed"
}
