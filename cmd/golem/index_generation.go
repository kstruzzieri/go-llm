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
	activePointerSchemaVersion  = 1
	generationSchemaVersion     = 1
	generationCheckpointTimeout = 5 * time.Second
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

func checkpointGeneration(ctx context.Context, store *rag.SQLiteStore) error {
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), generationCheckpointTimeout)
	defer cancel()
	if _, err := store.DB().ExecContext(checkpointCtx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint staging generation: %w", err)
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

func createStagingGeneration(dbPath string) (indexGeneration, error) {
	if err := os.MkdirAll(generationsPath(dbPath), 0o700); err != nil {
		return indexGeneration{}, fmt.Errorf("create generations directory: %w", err)
	}
	for range 4 {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return indexGeneration{}, fmt.Errorf("create generation id: %w", err)
		}
		gen := generationPaths(dbPath, hex.EncodeToString(token[:]))
		gen.dbPath = filepath.Join(generationsPath(dbPath), ".staging-"+gen.id, "index.db")
		gen.metadataPath = filepath.Join(filepath.Dir(gen.dbPath), "index.json")
		if err := os.Mkdir(filepath.Dir(gen.dbPath), 0o700); err == nil {
			return gen, nil
		} else if !os.IsExist(err) {
			return indexGeneration{}, fmt.Errorf("create staging generation: %w", err)
		}
	}
	return indexGeneration{}, fmt.Errorf("create staging generation: repeated id collision")
}

func finalizeStagingGeneration(dbPath string, staging indexGeneration) (indexGeneration, error) {
	final := generationPaths(dbPath, staging.id)
	if err := os.Rename(filepath.Dir(staging.dbPath), filepath.Dir(final.dbPath)); err != nil {
		return indexGeneration{}, fmt.Errorf("finalize staging generation: %w", err)
	}
	dir, err := os.Open(generationsPath(dbPath))
	if err != nil {
		return indexGeneration{}, fmt.Errorf("open generations directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return indexGeneration{}, fmt.Errorf("sync generations directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return indexGeneration{}, fmt.Errorf("close generations directory: %w", err)
	}
	final.metadata = staging.metadata
	return final, nil
}

func writeGenerationMetadata(path string, metadata generationMetadata) error {
	return writeAtomicJSON(path, metadata, nil)
}

func readStrictJSON(path string, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %q: %w", path, err)
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return fmt.Errorf("decode %q: %w", path, err)
	}
	return nil
}

func readActivePointer(dbPath string) (activeGenerationPointer, error) {
	var pointer activeGenerationPointer
	if err := readStrictJSON(activePointerPath(dbPath), &pointer); err != nil {
		return activeGenerationPointer{}, fmt.Errorf("read active generation pointer: %w", err)
	}
	return pointer, nil
}

func validatePointer(pointer activeGenerationPointer, workspaceID string) error {
	if pointer.SchemaVersion != activePointerSchemaVersion {
		return fmt.Errorf("active generation pointer schemaVersion %d unsupported", pointer.SchemaVersion)
	}
	if pointer.WorkspaceID != workspaceID {
		return fmt.Errorf("active generation pointer workspaceID %q does not match %q", pointer.WorkspaceID, workspaceID)
	}
	if !validGenerationID(pointer.Generation) {
		return fmt.Errorf("active generation pointer has invalid generation %q", pointer.Generation)
	}
	return nil
}

func validateGenerationMetadata(metadata generationMetadata, pointer activeGenerationPointer) error {
	if metadata.SchemaVersion != generationSchemaVersion {
		return fmt.Errorf("generation metadata schemaVersion %d unsupported", metadata.SchemaVersion)
	}
	if metadata.Generation != pointer.Generation {
		return fmt.Errorf("generation metadata id %q does not match pointer %q", metadata.Generation, pointer.Generation)
	}
	if metadata.WorkspaceID != pointer.WorkspaceID {
		return fmt.Errorf("generation metadata workspaceID %q does not match pointer %q", metadata.WorkspaceID, pointer.WorkspaceID)
	}
	if metadata.VectorSpaceID == "" {
		return fmt.Errorf("generation metadata vector space is empty")
	}
	if metadata.SourceCount < 1 || metadata.ChunkCount < 1 {
		return fmt.Errorf("generation metadata has unusable counts sources=%d chunks=%d", metadata.SourceCount, metadata.ChunkCount)
	}
	switch metadata.Status {
	case "complete":
		if metadata.ErrorCount != 0 {
			return fmt.Errorf("complete generation has %d errors", metadata.ErrorCount)
		}
	case "partial":
		if metadata.ErrorCount < 1 {
			return fmt.Errorf("partial generation has no errors")
		}
	default:
		return fmt.Errorf("generation metadata status %q is invalid", metadata.Status)
	}
	if _, err := time.Parse(time.RFC3339, metadata.IndexedAt); err != nil {
		return fmt.Errorf("generation metadata indexedAt: %w", err)
	}
	return nil
}

func validateGenerationDatabase(ctx context.Context, gen indexGeneration) error {
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		return fmt.Errorf("generation database open: %w", err)
	}
	defer func() { _ = store.Close() }()
	var integrity string
	if err := store.DB().QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("generation database integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("generation database integrity: %s", integrity)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		return fmt.Errorf("generation database stats: %w", err)
	}
	if stats.TotalSources != gen.metadata.SourceCount {
		return fmt.Errorf("generation source count %d does not match metadata %d", stats.TotalSources, gen.metadata.SourceCount)
	}
	if stats.TotalChunks != gen.metadata.ChunkCount {
		return fmt.Errorf("generation chunk count %d does not match metadata %d", stats.TotalChunks, gen.metadata.ChunkCount)
	}
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		return fmt.Errorf("generation database vector space probe: %w", err)
	}
	if probe.HasUnknown || len(probe.KnownIDs) != 1 || probe.KnownIDs[0] != gen.metadata.VectorSpaceID {
		return fmt.Errorf("generation database vector space known=%v unknown=%v does not match metadata %q", probe.KnownIDs, probe.HasUnknown, gen.metadata.VectorSpaceID)
	}
	return nil
}

func resolveActiveGeneration(ctx context.Context, dbPath, workspaceID string) (indexGeneration, error) {
	pointer, err := readActivePointer(dbPath)
	if err == nil {
		if err := validatePointer(pointer, workspaceID); err != nil {
			return indexGeneration{}, err
		}
		gen := generationPaths(dbPath, pointer.Generation)
		if err := readStrictJSON(gen.metadataPath, &gen.metadata); err != nil {
			return indexGeneration{}, fmt.Errorf("read generation metadata: %w", err)
		}
		if err := validateGenerationMetadata(gen.metadata, pointer); err != nil {
			return indexGeneration{}, err
		}
		if err := validateGenerationDatabase(ctx, gen); err != nil {
			return indexGeneration{}, err
		}
		return gen, nil
	}
	if !os.IsNotExist(errors.Unwrap(err)) && !errors.Is(err, os.ErrNotExist) {
		return indexGeneration{}, err
	}
	if !fileExists(dbPath) || !fileExists(sidecarPath(dbPath)) {
		return indexGeneration{}, fmt.Errorf("%w: no active generation", os.ErrNotExist)
	}
	sidecar, err := readSidecar(sidecarPath(dbPath))
	if err != nil {
		return indexGeneration{}, fmt.Errorf("read legacy generation metadata: %w", err)
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
		return indexGeneration{}, fmt.Errorf("legacy generation database: %w", err)
	}
	stats, err := store.Stats(ctx)
	if err == nil {
		legacy.metadata.SourceCount = stats.TotalSources
		legacy.metadata.ChunkCount = stats.TotalChunks
	}
	_ = store.Close()
	if err != nil {
		return indexGeneration{}, fmt.Errorf("legacy generation database stats: %w", err)
	}
	pseudo := activeGenerationPointer{WorkspaceID: workspaceID}
	if err := validateGenerationMetadataLegacy(legacy.metadata, pseudo); err != nil {
		return indexGeneration{}, err
	}
	if err := validateGenerationDatabase(ctx, legacy); err != nil {
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

func publishActiveGeneration(dbPath string, pointer activeGenerationPointer, hook publicationHook) error {
	if err := validatePointer(pointer, pointer.WorkspaceID); err != nil {
		return err
	}
	return writeAtomicJSON(activePointerPath(dbPath), pointer, func(boundary publicationBoundary) error {
		if hook == nil {
			return nil
		}
		return hook(boundary)
	})
}

func writeAtomicJSON(path string, value any, hook publicationHook) error {
	if hook != nil {
		if err := hook(publicationBeforePointerTemp); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %q: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %q: %w", tmp, err)
	}
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", tmp, err)
	}
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod %q: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %q: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %q: %w", tmp, err)
	}
	if hook != nil {
		if err := hook(publicationAfterPointerTempSync); err != nil {
			cleanup = false
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %q: %w", path, err)
	}
	cleanup = false
	if hook != nil {
		if err := hook(publicationAfterPointerRename); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open publication directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("sync publication directory: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close publication directory: %w", err)
	}
	if hook != nil {
		if err := hook(publicationAfterPointerDirSync); err != nil {
			return err
		}
	}
	return nil
}

func cleanupStaleGenerations(dbPath, activeID string) error {
	entries, err := os.ReadDir(generationsPath(dbPath))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read generations directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".staging-") || !validGenerationID(strings.TrimPrefix(entry.Name(), ".staging-")) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(generationsPath(dbPath), entry.Name())); err != nil {
			return fmt.Errorf("remove stale generation %q: %w", entry.Name(), err)
		}
	}
	if err := os.Remove(activePointerPath(dbPath) + ".tmp"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale active pointer temp: %w", err)
	}
	return nil
}
