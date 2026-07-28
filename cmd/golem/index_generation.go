package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/rag"
)

const (
	activePointerSchemaVersion = 1
	generationSchemaVersion    = 1
	generationCleanupTimeout   = 5 * time.Second
)

type activeGenerationPointer struct {
	SchemaVersion int    `json:"schemaVersion"`
	WorkspaceID   string `json:"workspaceID"`
	Generation    string `json:"generation"`
}

type generationMetadata struct {
	SchemaVersion           int    `json:"schemaVersion"`
	Generation              string `json:"generation"`
	WorkspaceID             string `json:"workspaceID"`
	RequestedEmbeddingModel string `json:"requestedEmbeddingModel"`
	VectorSpaceID           string `json:"vectorSpaceID"`
	IndexedAt               string `json:"indexedAt"`
	Status                  string `json:"status"`
	ErrorCount              int    `json:"errorCount"`
	SourceCount             int    `json:"sourceCount"`
	ChunkCount              int    `json:"chunkCount"`
}

type indexGeneration struct {
	id           string
	dbPath       string
	metadataPath string
	metadata     generationMetadata
	legacy       bool
}

type generationBuildOptions struct {
	root                string
	dbPath              string
	workspaceID         string
	requestedModel      string
	actualVectorSpace   string
	embedder            rag.Embedder
	summarize           rag.SourceSummaryGenerator
	full                bool
	pruneDeleted        bool
	refuseInvalidActive bool
	out                 io.Writer
	finalizationHook    finalizationHook
}

type generationBuildResult struct {
	generation indexGeneration
	sidecar    indexSidecar
	stats      rag.StoreStats
	index      indexResult
	activeErr  error
	gcWarn     string // non-fatal superseded-generation removal failure
	summaryErr error  // non-fatal optional summary generation failures
}

func buildIndexGeneration(ctx context.Context, opts generationBuildOptions) (result generationBuildResult, err error) {
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if opts.dbPath == "" || opts.workspaceID == "" || opts.requestedModel == "" || opts.actualVectorSpace == "" || opts.embedder == nil || opts.out == nil {
		return result, fmt.Errorf("golem: build index generation: db path, workspace, requested model, actual vector space, embedder, and output are required")
	}
	active, activeErr := resolveActiveGeneration(ctx, opts.dbPath, opts.workspaceID)
	result.activeErr = activeErr
	if opts.refuseInvalidActive && !opts.full {
		switch {
		case activeErr == nil && active.metadata.VectorSpaceID != opts.actualVectorSpace:
			return result, fmt.Errorf("golem: existing index uses vector space %s, successful embedding route is %s; run \"golem index -full\" to rebuild",
				active.metadata.VectorSpaceID, opts.actualVectorSpace)
		case activeErr != nil && !errors.Is(activeErr, os.ErrNotExist):
			return result, fmt.Errorf("golem: existing index is invalid (%v); run \"golem index -full\" to rebuild", activeErr)
		case errors.Is(activeErr, os.ErrNotExist) && (fileExists(opts.dbPath) || fileExists(sidecarPath(opts.dbPath))):
			return result, fmt.Errorf("golem: existing index has no valid sidecar; run \"golem index -full\" to rebuild")
		}
	}
	keepID := ""
	gcFinalized := false
	switch {
	case activeErr == nil:
		// active.id is "" for a legacy pair, so every finalized directory is a
		// pre-publication orphan and collectable.
		gcFinalized, keepID = true, active.id
	case errors.Is(activeErr, os.ErrNotExist):
		// Nothing was ever published: finalized directories are crash orphans.
		gcFinalized = true
	}
	gcWarn, err := cleanupStaleGenerations(ctx, opts.dbPath, keepID, gcFinalized)
	if err != nil {
		return result, err
	}
	result.gcWarn = gcWarn
	staging, err := createStagingGeneration(ctx, opts.dbPath)
	if err != nil {
		return result, err
	}
	cleanupPath := filepath.Dir(staging.dbPath)
	keepGeneration := false
	defer func() {
		if keepGeneration {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generationCleanupTimeout)
		defer cancel()
		if cleanupErr := removeGenerationPath(cleanupCtx, cleanupPath); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()
	if !opts.full && activeErr == nil && active.metadata.VectorSpaceID == opts.actualVectorSpace {
		if err := copyImmutableFile(ctx, active.dbPath, staging.dbPath); err != nil {
			return result, err
		}
	}
	store, err := rag.NewSQLiteStore(staging.dbPath)
	if err != nil {
		return result, fmt.Errorf("golem: open staging database %q: %w", staging.dbPath, err)
	}
	storeClosed := false
	defer func() {
		if !storeClosed {
			if closeErr := store.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("golem: close staging database %q: %w", staging.dbPath, closeErr))
			}
		}
	}()
	indexer, err := rag.NewIndexerWithEmbedder(opts.embedder, store, rag.WithEmbeddingModel(opts.requestedModel))
	if err != nil {
		return result, fmt.Errorf("golem: build staging indexer: %w", err)
	}
	result.index = executeIndex(ctx, indexJob{
		indexer: indexer, store: store, root: opts.root,
		dbPath: staging.dbPath, displayDBPath: opts.dbPath,
		sidecarPath: staging.metadataPath, workspaceID: opts.workspaceID,
		requestedModel: opts.requestedModel, out: opts.out, pruneDeleted: opts.pruneDeleted,
	})
	sc, sidecarErr := readSidecar(staging.metadataPath)
	if sidecarErr != nil {
		return result, fmt.Errorf("golem: read staging metadata %q: %w", staging.metadataPath, sidecarErr)
	}
	if validateErr := validateSidecar(sc, opts.workspaceID); validateErr != nil {
		return result, fmt.Errorf("golem: validate staging metadata %q: %w", staging.metadataPath, validateErr)
	}
	if result.index.exitErr != nil && sc.Status != "partial" {
		return result, fmt.Errorf("golem: indexing did not produce a publishable generation")
	}
	if opts.summarize != nil {
		if err := store.GenerateSourceSummaries(ctx, opts.summarize); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
			result.summaryErr = fmt.Errorf("golem: generate staged source summaries: %w", err)
		}
	}
	if checkpointErr := checkpointGeneration(ctx, store); checkpointErr != nil {
		return result, checkpointErr
	}
	if closeErr := store.Close(); closeErr != nil {
		storeClosed = true
		return result, fmt.Errorf("golem: close staging database %q: %w", staging.dbPath, closeErr)
	}
	storeClosed = true
	if err := ctx.Err(); err != nil {
		return result, err
	}
	readOnly, err := rag.OpenSQLiteStoreReadOnly(staging.dbPath)
	if err != nil {
		return result, fmt.Errorf("golem: open staging generation %q read-only: %w", staging.dbPath, err)
	}
	stats, statsErr := readOnly.Stats(ctx)
	closeErr := readOnly.Close()
	if statsErr != nil {
		err = errors.Join(err, fmt.Errorf("golem: read staging generation stats %q: %w", staging.dbPath, statsErr))
	}
	if closeErr != nil {
		err = errors.Join(err, fmt.Errorf("golem: close staging generation %q after stats: %w", staging.dbPath, closeErr))
	}
	if err != nil {
		return result, err
	}
	if result.index.exitErr != nil && stats.TotalSources < 1 {
		return result, fmt.Errorf("golem: partial staging generation has no usable sources")
	}
	if sc.VectorSpaceID != opts.actualVectorSpace {
		return result, fmt.Errorf("golem: staging vector space %q does not match successful probe %q", sc.VectorSpaceID, opts.actualVectorSpace)
	}
	staging.metadata = generationMetadata{
		SchemaVersion: generationSchemaVersion, Generation: staging.id,
		WorkspaceID: opts.workspaceID, RequestedEmbeddingModel: opts.requestedModel,
		VectorSpaceID: sc.VectorSpaceID, IndexedAt: sc.IndexedAt,
		Status: sc.Status, ErrorCount: sc.ErrorCount,
		SourceCount: stats.TotalSources, ChunkCount: stats.TotalChunks,
	}
	if err := writeGenerationMetadata(ctx, staging.metadataPath, staging.metadata); err != nil {
		return result, err
	}
	pointer := activeGenerationPointer{SchemaVersion: activePointerSchemaVersion, WorkspaceID: opts.workspaceID, Generation: staging.id}
	if err := validateGenerationMetadata(staging.metadata, pointer); err != nil {
		return result, err
	}
	if err := validateGenerationDatabase(ctx, staging, true); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	final, err := finalizeStagingGeneration(ctx, opts.dbPath, staging, opts.finalizationHook)
	if err != nil {
		return result, err
	}
	keepGeneration = true
	result.generation = final
	result.sidecar = sc
	result.stats = stats
	return result, nil
}

func checkpointGeneration(ctx context.Context, store *rag.SQLiteStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Checkpoint time is data-dependent — a full rebuild's WAL is the whole
	// database — so no fixed ceiling; cancellation aborts the build and the
	// cleanup defer discards the staging directory.
	if _, err := store.DB().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("golem: checkpoint staging generation: %w", err)
	}
	return nil
}

func indexPathStem(dbPath string) string {
	return strings.TrimSuffix(dbPath, filepath.Ext(dbPath))
}

func activePointerPath(dbPath string) string { return indexPathStem(dbPath) + ".active.json" }
func generationsPath(dbPath string) string   { return indexPathStem(dbPath) + ".generations" }
func writerLeasePath(dbPath string) string   { return indexPathStem(dbPath) + ".lock" }

func generationPaths(dbPath, generation string) indexGeneration {
	dir := filepath.Join(generationsPath(dbPath), generation)
	return indexGeneration{
		id:           generation,
		dbPath:       filepath.Join(dir, "index.db"),
		metadataPath: filepath.Join(dir, "index.json"),
	}
}

func validGenerationID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func createStagingGeneration(ctx context.Context, dbPath string) (indexGeneration, error) {
	if err := ctx.Err(); err != nil {
		return indexGeneration{}, err
	}
	if err := os.MkdirAll(generationsPath(dbPath), 0o700); err != nil {
		return indexGeneration{}, fmt.Errorf("golem: create generations directory %q: %w", generationsPath(dbPath), err)
	}
	for range 4 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return indexGeneration{}, fmt.Errorf("golem: create generation id: %w", err)
		}
		gen := generationPaths(dbPath, hex.EncodeToString(token[:]))
		gen.dbPath = filepath.Join(generationsPath(dbPath), ".staging-"+gen.id, "index.db")
		gen.metadataPath = filepath.Join(filepath.Dir(gen.dbPath), "index.json")
		if err := os.Mkdir(filepath.Dir(gen.dbPath), 0o700); err == nil {
			return gen, nil
		} else if !os.IsExist(err) {
			return indexGeneration{}, fmt.Errorf("golem: create staging generation under %q: %w", generationsPath(dbPath), err)
		}
	}
	return indexGeneration{}, fmt.Errorf("golem: create staging generation under %q: repeated id collision", generationsPath(dbPath))
}

type finalizationBoundary string

const (
	finalizationBeforeRename finalizationBoundary = "before-generation-rename"
	finalizationAfterRename  finalizationBoundary = "after-generation-rename"
	finalizationAfterDirSync finalizationBoundary = "after-generation-dir-sync"
)

type finalizationHook func(finalizationBoundary) error

func finalizeStagingGeneration(ctx context.Context, dbPath string, staging indexGeneration, hook finalizationHook) (indexGeneration, error) {
	if err := ctx.Err(); err != nil {
		return indexGeneration{}, err
	}
	if hook != nil {
		if err := hook(finalizationBeforeRename); err != nil {
			return indexGeneration{}, err
		}
	}
	final := generationPaths(dbPath, staging.id)
	if err := os.Rename(filepath.Dir(staging.dbPath), filepath.Dir(final.dbPath)); err != nil {
		return indexGeneration{}, fmt.Errorf("golem: finalize staging generation %q: %w", staging.id, err)
	}
	if hook != nil {
		if err := hook(finalizationAfterRename); err != nil {
			return indexGeneration{}, err
		}
	}
	dir, err := os.Open(generationsPath(dbPath))
	if err != nil {
		return indexGeneration{}, fmt.Errorf("golem: open generations directory %q: %w", generationsPath(dbPath), err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		return indexGeneration{}, errors.Join(fmt.Errorf("golem: sync generations directory %q: %w", generationsPath(dbPath), err), closeErr)
	}
	if err := dir.Close(); err != nil {
		return indexGeneration{}, fmt.Errorf("golem: close generations directory %q: %w", generationsPath(dbPath), err)
	}
	if hook != nil {
		if err := hook(finalizationAfterDirSync); err != nil {
			return indexGeneration{}, err
		}
	}
	final.metadata = staging.metadata
	return final, nil
}

func writeGenerationMetadata(ctx context.Context, path string, metadata generationMetadata) error {
	return writeAtomicJSON(ctx, path, metadata, nil)
}

func readStrictJSON(ctx context.Context, path string, dst any) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("golem: close %q after reading JSON: %w", path, closeErr))
		}
	}()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("golem: decode %q: %w", path, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("golem: decode %q: %w", path, err)
	}
	return nil
}

func readActivePointer(ctx context.Context, dbPath string) (activeGenerationPointer, error) {
	var pointer activeGenerationPointer
	if err := readStrictJSON(ctx, activePointerPath(dbPath), &pointer); err != nil {
		return activeGenerationPointer{}, fmt.Errorf("golem: read active generation pointer %q: %w", activePointerPath(dbPath), err)
	}
	return pointer, nil
}

func validatePointer(pointer activeGenerationPointer, workspaceID string) error {
	if pointer.SchemaVersion != activePointerSchemaVersion {
		return fmt.Errorf("golem: active generation pointer schemaVersion %d unsupported", pointer.SchemaVersion)
	}
	if pointer.WorkspaceID != workspaceID {
		return fmt.Errorf("golem: active generation pointer workspaceID %q does not match %q", pointer.WorkspaceID, workspaceID)
	}
	if !validGenerationID(pointer.Generation) {
		return fmt.Errorf("golem: active generation pointer has invalid generation %q", pointer.Generation)
	}
	return nil
}

func validateGenerationMetadata(metadata generationMetadata, pointer activeGenerationPointer) error {
	if metadata.SchemaVersion != generationSchemaVersion {
		return fmt.Errorf("golem: generation metadata schemaVersion %d unsupported", metadata.SchemaVersion)
	}
	if metadata.Generation != pointer.Generation {
		return fmt.Errorf("golem: generation metadata id %q does not match pointer %q", metadata.Generation, pointer.Generation)
	}
	if metadata.WorkspaceID != pointer.WorkspaceID {
		return fmt.Errorf("golem: generation metadata workspaceID %q does not match pointer %q", metadata.WorkspaceID, pointer.WorkspaceID)
	}
	if metadata.VectorSpaceID == "" {
		return fmt.Errorf("golem: generation metadata vector space is empty")
	}
	if metadata.SourceCount < 1 || metadata.ChunkCount < 1 {
		return fmt.Errorf("golem: generation metadata has unusable counts sources=%d chunks=%d", metadata.SourceCount, metadata.ChunkCount)
	}
	switch metadata.Status {
	case "complete":
		if metadata.ErrorCount != 0 {
			return fmt.Errorf("golem: complete generation has %d errors", metadata.ErrorCount)
		}
	case "partial":
		if metadata.ErrorCount < 1 {
			return fmt.Errorf("golem: partial generation has no errors")
		}
	default:
		return fmt.Errorf("golem: generation metadata status %q is invalid", metadata.Status)
	}
	if _, err := time.Parse(time.RFC3339, metadata.IndexedAt); err != nil {
		return fmt.Errorf("golem: generation metadata indexedAt: %w", err)
	}
	return nil
}

// validateGenerationDatabase cross-checks the generation database against its
// metadata. fullIntegrity additionally runs PRAGMA integrity_check — a full
// page scan reserved for the writer's staging validation; readers of an
// already-validated immutable generation skip it so startup stays O(row
// counts), not O(database bytes).
func validateGenerationDatabase(ctx context.Context, gen indexGeneration, fullIntegrity bool) (err error) {
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		return fmt.Errorf("golem: generation database open: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("golem: close generation database %q after validation: %w", gen.dbPath, closeErr))
		}
	}()
	if fullIntegrity {
		var integrity string
		if err := store.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
			return fmt.Errorf("golem: generation database integrity: %w", err)
		}
		if integrity != "ok" {
			return fmt.Errorf("golem: generation database integrity: %s", integrity)
		}
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		return fmt.Errorf("golem: generation database stats: %w", err)
	}
	if stats.EmbeddingFormat != rag.EmbeddingFormatPackedFloat32 {
		return fmt.Errorf("golem: generation database embedding format %q is unsupported; rebuild as %s", stats.EmbeddingFormat, rag.EmbeddingFormatPackedFloat32)
	}
	if stats.TotalSources != gen.metadata.SourceCount {
		return fmt.Errorf("golem: generation source count %d does not match metadata %d", stats.TotalSources, gen.metadata.SourceCount)
	}
	if stats.TotalChunks != gen.metadata.ChunkCount {
		return fmt.Errorf("golem: generation chunk count %d does not match metadata %d", stats.TotalChunks, gen.metadata.ChunkCount)
	}
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		return fmt.Errorf("golem: generation database vector space probe: %w", err)
	}
	if probe.HasUnknown || len(probe.KnownIDs) != 1 || probe.KnownIDs[0] != gen.metadata.VectorSpaceID {
		return fmt.Errorf("golem: generation database vector space known=%v unknown=%v does not match metadata %q", probe.KnownIDs, probe.HasUnknown, gen.metadata.VectorSpaceID)
	}
	return nil
}

func resolveActiveGeneration(ctx context.Context, dbPath, workspaceID string) (indexGeneration, error) {
	pointer, err := readActivePointer(ctx, dbPath)
	if err == nil {
		if err := validatePointer(pointer, workspaceID); err != nil {
			return indexGeneration{}, err
		}
		gen := generationPaths(dbPath, pointer.Generation)
		if err := readStrictJSON(ctx, gen.metadataPath, &gen.metadata); err != nil {
			return indexGeneration{}, fmt.Errorf("golem: read generation metadata %q: %w", gen.metadataPath, err)
		}
		if err := validateGenerationMetadata(gen.metadata, pointer); err != nil {
			return indexGeneration{}, err
		}
		if err := validateGenerationDatabase(ctx, gen, false); err != nil {
			return indexGeneration{}, err
		}
		return gen, nil
	}
	if !os.IsNotExist(errors.Unwrap(err)) && !errors.Is(err, os.ErrNotExist) {
		return indexGeneration{}, err
	}
	if !fileExists(dbPath) || !fileExists(sidecarPath(dbPath)) {
		return indexGeneration{}, fmt.Errorf("golem: %w: no active generation for %q", os.ErrNotExist, dbPath)
	}
	// A live write-ahead log means an interrupted pre-generation writer: the
	// immutable read-only open would silently serve (and the staging copy
	// would bake in) only the last-checkpointed state, so fail closed instead.
	if info, statErr := os.Stat(dbPath + "-wal"); statErr == nil && info.Size() > 0 {
		return indexGeneration{}, fmt.Errorf("golem: legacy index %q has an unapplied write-ahead log from an interrupted writer; run \"golem index -full\" to rebuild", dbPath)
	}
	sidecar, err := readSidecar(sidecarPath(dbPath))
	if err != nil {
		return indexGeneration{}, fmt.Errorf("golem: read legacy generation metadata %q: %w", sidecarPath(dbPath), err)
	}
	if err := validateSidecar(sidecar, workspaceID); err != nil {
		return indexGeneration{}, err
	}
	legacy := indexGeneration{
		dbPath: dbPath, metadataPath: sidecarPath(dbPath), legacy: true,
		metadata: generationMetadata{
			SchemaVersion:           generationSchemaVersion,
			WorkspaceID:             sidecar.WorkspaceID,
			RequestedEmbeddingModel: sidecar.RequestedEmbeddingModel,
			VectorSpaceID:           sidecar.VectorSpaceID,
			IndexedAt:               sidecar.IndexedAt,
			Status:                  sidecar.Status,
			ErrorCount:              sidecar.ErrorCount,
		},
	}
	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		return indexGeneration{}, fmt.Errorf("golem: open legacy generation database %q: %w", dbPath, err)
	}
	stats, err := store.Stats(ctx)
	if err == nil {
		legacy.metadata.SourceCount = stats.TotalSources
		legacy.metadata.ChunkCount = stats.TotalChunks
	}
	closeErr := store.Close()
	if err != nil {
		return indexGeneration{}, errors.Join(fmt.Errorf("golem: read legacy generation database stats %q: %w", dbPath, err), closeErr)
	}
	if closeErr != nil {
		return indexGeneration{}, fmt.Errorf("golem: close legacy generation database %q: %w", dbPath, closeErr)
	}
	pseudo := activeGenerationPointer{WorkspaceID: workspaceID}
	if err := validateGenerationMetadataLegacy(legacy.metadata, pseudo); err != nil {
		return indexGeneration{}, err
	}
	if err := validateGenerationDatabase(ctx, legacy, false); err != nil {
		return indexGeneration{}, err
	}
	return legacy, nil
}

func validateGenerationMetadataLegacy(metadata generationMetadata, pointer activeGenerationPointer) error {
	metadata.Generation = strings.Repeat("0", 32)
	pointer.Generation = metadata.Generation
	return validateGenerationMetadata(metadata, pointer)
}

type publicationBoundary string

const (
	publicationBeforePointerTemp    publicationBoundary = "before-pointer-temp"
	publicationAfterPointerTempSync publicationBoundary = "after-pointer-temp-sync"
	publicationAfterPointerRename   publicationBoundary = "after-pointer-rename"
	publicationAfterPointerDirSync  publicationBoundary = "after-pointer-dir-sync"
)

type publicationHook func(publicationBoundary) error

func publishActiveGeneration(ctx context.Context, dbPath string, pointer activeGenerationPointer, hook publicationHook) error {
	if err := validatePointer(pointer, pointer.WorkspaceID); err != nil {
		return err
	}
	return writeAtomicJSON(ctx, activePointerPath(dbPath), pointer, func(boundary publicationBoundary) error {
		if hook == nil {
			return nil
		}
		return hook(boundary)
	})
}

func writeAtomicJSON(ctx context.Context, path string, value any, hook publicationHook) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		if err := hook(publicationBeforePointerTemp); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("golem: marshal %q: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("golem: create publication directory %q: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("golem: create %q: %w", tmp, err)
	}
	cleanup := true
	closed := false
	defer func() {
		if !closed {
			if closeErr := f.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("golem: close publication temp %q: %w", tmp, closeErr))
			}
		}
		if cleanup {
			if removeErr := os.Remove(tmp); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("golem: remove publication temp %q: %w", tmp, removeErr))
			}
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("golem: write %q: %w", tmp, err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("golem: chmod %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("golem: sync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		closed = true
		return fmt.Errorf("golem: close %q: %w", tmp, err)
	}
	closed = true
	if hook != nil {
		if err := hook(publicationAfterPointerTempSync); err != nil {
			cleanup = false
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("golem: rename active metadata %q: %w", path, err)
	}
	cleanup = false
	if hook != nil {
		if err := hook(publicationAfterPointerRename); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("golem: open publication directory %q: %w", filepath.Dir(path), err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		return errors.Join(fmt.Errorf("golem: sync publication directory %q: %w", filepath.Dir(path), err), closeErr)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("golem: close publication directory %q: %w", filepath.Dir(path), err)
	}
	if hook != nil {
		if err := hook(publicationAfterPointerDirSync); err != nil {
			return err
		}
	}
	return nil
}

// cleanupStaleGenerations removes interrupted staging directories, the stale
// pointer temp, and — when gcFinalized is set — superseded finalized
// generations (every valid generation directory except keepID). Finalized
// removal is best-effort: on POSIX an unlinked directory stays readable
// through a draining reader's open descriptors, and on Windows the removal
// fails closed while any handle is open and is retried by the next lease
// holder. The first such failure is returned as a warning so a persistently
// failing removal (and its disk growth) stays visible without aborting the
// build.
func cleanupStaleGenerations(ctx context.Context, dbPath, keepID string, gcFinalized bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(generationsPath(dbPath))
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("golem: read generations directory %q: %w", generationsPath(dbPath), err)
	}
	gcWarn := ""
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return gcWarn, err
		}
		if !entry.IsDir() {
			continue
		}
		if gcFinalized && validGenerationID(entry.Name()) && entry.Name() != keepID {
			if removeErr := os.RemoveAll(filepath.Join(generationsPath(dbPath), entry.Name())); removeErr != nil && gcWarn == "" {
				gcWarn = fmt.Sprintf("could not remove superseded generation %q (will retry under the next writer lease): %v", entry.Name(), removeErr)
			}
			continue
		}
		if !strings.HasPrefix(entry.Name(), ".staging-") || !validGenerationID(strings.TrimPrefix(entry.Name(), ".staging-")) {
			continue
		}
		if err := removeGenerationPath(ctx, filepath.Join(generationsPath(dbPath), entry.Name())); err != nil {
			return gcWarn, err
		}
	}
	if err := os.Remove(activePointerPath(dbPath) + ".tmp"); err != nil && !os.IsNotExist(err) {
		return gcWarn, fmt.Errorf("golem: remove stale active pointer temp %q: %w", activePointerPath(dbPath)+".tmp", err)
	}
	return gcWarn, nil
}

// shouldRemoveUnpublished reports whether a generation whose publication
// failed can be removed: only when the active pointer is readable and names a
// different generation. An unreadable pointer keeps the directory — a failed
// publish may already have renamed the pointer onto this generation, and
// deleting the target of a possibly-live pointer is worse than leaking one
// directory to the next lease holder's garbage collection.
func shouldRemoveUnpublished(ctx context.Context, dbPath, generationID string) bool {
	current, err := readActivePointer(ctx, dbPath)
	return err == nil && current.Generation != generationID
}

func removeGenerationPath(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("golem: remove generation path %q: %w", path, err)
	}
	return nil
}

func copyImmutableFile(ctx context.Context, source, destination string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("golem: open active generation %q for staging copy: %w", source, err)
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("golem: close active generation %q after staging copy: %w", source, closeErr))
		}
	}()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("golem: create staging database %q: %w", destination, err)
	}
	outClosed := false
	keep := false
	defer func() {
		if !outClosed {
			if closeErr := out.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("golem: close staging database %q: %w", destination, closeErr))
			}
		}
		if !keep {
			if removeErr := os.Remove(destination); removeErr != nil && !os.IsNotExist(removeErr) {
				err = errors.Join(err, fmt.Errorf("golem: remove failed staging database %q: %w", destination, removeErr))
			}
		}
	}()
	buf := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, writeErr := out.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("golem: copy active generation %q to %q: %w", source, destination, writeErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("golem: read active generation %q: %w", source, readErr)
		}
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("golem: sync staging database %q: %w", destination, err)
	}
	if err := out.Close(); err != nil {
		outClosed = true
		return fmt.Errorf("golem: close staging database %q: %w", destination, err)
	}
	outClosed = true
	keep = true
	return nil
}
