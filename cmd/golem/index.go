package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// errIndexFailed is the sentinel returned to main() for an already-rendered
// non-zero index outcome, so main() exits non-zero WITHOUT printing an extra
// "golem: <err>" line (runIndex owns its own output).
var errIndexFailed = errors.New("golem index failed")

// indexJob carries the dependencies executeIndex needs. The indexer + store are
// pre-built (with the chain embedder) so tests can inject a fake embedder.
type indexJob struct {
	indexer        *rag.Indexer
	store          *rag.SQLiteStore
	root           string
	dbPath         string
	displayDBPath  string
	sidecarPath    string
	workspaceID    string
	requestedModel string // embChain[0], the configured primary selector
	out            io.Writer
	pruneDeleted   bool // drop stored sources missing from the walk (startup auto refresh only)
}

// indexResult is executeIndex's outcome. exitErr is nil on a clean run,
// errIndexFailed on any rendered non-zero outcome (partial, integrity, fatal).
type indexResult struct {
	exitErr error
}

// indexExcludes is the union of the read-tools ignoreDirs and the rag defaults,
// so golem's own private dir is never indexed.
var indexExcludes = []string{".git", "vendor", "node_modules", ".superpowers", "dist", "__pycache__"}

// executeIndex runs the index, probes the stored vector space, writes the
// sidecar only for a usable+clean store, prints a summary, and returns the exit
// outcome. (Preflight and -full removal are layered in Task 7.)
func executeIndex(ctx context.Context, job indexJob) indexResult {
	dirOpts := []rag.IndexDirOption{rag.WithIncremental(), rag.WithExclude(indexExcludes...)}
	if job.pruneDeleted {
		dirOpts = append(dirOpts, rag.WithPruneDeleted())
	}
	status, indexErr := job.indexer.IndexDirectoryWithStatus(ctx, job.root, dirOpts...)

	// A walk failure / cancellation aborts the command: error but no completed
	// queue. Distinguish it from a completed run with per-file errors.
	if ctx.Err() != nil {
		_, _ = fmt.Fprintf(job.out, "golem index: cancelled: %v\n", ctx.Err())
		return indexResult{exitErr: errIndexFailed}
	}
	if indexErr != nil && len(status.Errors) == 0 {
		_, _ = fmt.Fprintf(job.out, "golem index: %v\n", indexErr)
		return indexResult{exitErr: errIndexFailed}
	}

	stats, statErr := job.store.Stats(ctx)
	if statErr != nil {
		_, _ = fmt.Fprintf(job.out, "golem index: read stats: %v\n", statErr)
		return indexResult{exitErr: errIndexFailed}
	}

	// "Usable store" gate (spec §5.7): >=1 source AND a clean single-vsid probe.
	probe, probeErr := job.store.ProbeVectorSpaces(ctx)
	if probeErr != nil {
		_, _ = fmt.Fprintf(job.out, "golem index: probe vector space: %v\n", probeErr)
		return indexResult{exitErr: errIndexFailed}
	}
	clean := len(probe.KnownIDs) == 1 && !probe.HasUnknown
	if stats.TotalSources < 1 || !clean {
		_, _ = fmt.Fprintf(job.out, "golem index: corpus not usable (sources=%d, vector spaces=%v, legacy=%v); not writing index marker\n",
			stats.TotalSources, probe.KnownIDs, probe.HasUnknown)
		return indexResult{exitErr: errIndexFailed}
	}
	if err := chmodIndexDBFiles(job.dbPath); err != nil {
		_, _ = fmt.Fprintf(job.out, "golem index: %v\n", err)
		return indexResult{exitErr: errIndexFailed}
	}

	errCount := len(status.Errors)
	sidecarStatus := "complete"
	if errCount > 0 {
		sidecarStatus = "partial"
	}
	sc := indexSidecar{
		SchemaVersion:           indexSchemaVersion,
		WorkspaceID:             job.workspaceID,
		RequestedEmbeddingModel: job.requestedModel,
		VectorSpaceID:           probe.KnownIDs[0],
		IndexedAt:               time.Now().UTC().Format(time.RFC3339),
		Status:                  sidecarStatus,
		ErrorCount:              errCount,
	}
	if err := writeSidecar(job.sidecarPath, sc); err != nil {
		_, _ = fmt.Fprintf(job.out, "golem index: %v\n", err)
		return indexResult{exitErr: errIndexFailed}
	}

	displayPath := job.dbPath
	if job.displayDBPath != "" {
		displayPath = job.displayDBPath
	}
	_, _ = fmt.Fprintf(job.out, "indexed %d sources, %d chunks (%d errors) -> %s\n",
		stats.TotalSources, stats.TotalChunks, errCount, displayPath)
	if errCount > 0 {
		printCappedErrors(job.out, status.Errors, 10)
		// Partial run: usable index persisted + marker written, but exit non-zero.
		return indexResult{exitErr: errIndexFailed}
	}
	return indexResult{}
}

func printCappedErrors(w io.Writer, errs []string, limit int) {
	shown := errs
	if len(errs) > limit {
		shown = errs[:limit]
	}
	for _, e := range shown {
		_, _ = fmt.Fprintf(w, "  - %s\n", e)
	}
	if len(errs) > limit {
		_, _ = fmt.Fprintf(w, "  (+%d more)\n", len(errs)-limit)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func chmodIndexDBFiles(dbPath string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("golem: chmod index db artifact %q: %w", p, err)
		}
	}
	return nil
}

// runIndex is the `golem index` entry point: parse flags, bootstrap providers,
// build the chain embedder + per-workspace store + indexer, and run executeIndex.
// Output (summary, warnings, errors) is written to out/errOut; on a non-zero
// outcome it returns errIndexFailed so main() exits 1 without double-printing.
func runIndex(ctx context.Context, args []string, out, errOut io.Writer) error {
	var (
		configPath string
		rootFlag   string
		ollamaURL  string
		full       bool
	)
	fs := flag.NewFlagSet("golem index", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&rootFlag, "root", ".", "workspace root to index")
	fs.StringVar(&ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.BoolVar(&full, "full", false, "clean rebuild (drop the existing index first)")
	fs.Bool("no-color", false, "disable dim ANSI footers in summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := filepath.Abs(rootFlag)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:            cfg,
		OllamaURLOverride: ollamaURL,
	})
	if err != nil {
		return fmt.Errorf("bootstrap providers: %w", err)
	}
	defer func() { _ = bundle.Close() }()

	embChain, err := embeddingChain(bundle.Config)
	if err != nil {
		return err
	}
	dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, root)
	if err != nil {
		return err
	}
	lease, err := acquireIndexWriterLease(dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		_, _ = fmt.Fprintln(out, "golem index: workspace index writer lease already held")
		return errIndexFailed
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	defer func() { _ = lease.Close() }()

	embedder := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return bundle.Router.Route(rc, rr)
	}, embChain)
	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, embedder, embChain[0])
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	active, activeErr := resolveActiveGeneration(ctx, dbPath, workspaceID)
	if !full {
		switch {
		case activeErr == nil && active.metadata.VectorSpaceID != actualVectorSpace:
			_, _ = fmt.Fprintf(out, "golem index: existing index uses vector space %s, successful embedding route is %s; run \"golem index -full\" to rebuild\n",
				active.metadata.VectorSpaceID, actualVectorSpace)
			return errIndexFailed
		case activeErr != nil && !errors.Is(activeErr, os.ErrNotExist):
			_, _ = fmt.Fprintf(out, "golem index: existing index is invalid (%v); run \"golem index -full\" to rebuild\n", activeErr)
			return errIndexFailed
		case errors.Is(activeErr, os.ErrNotExist) && (fileExists(dbPath) || fileExists(sidecarPath(dbPath))):
			_, _ = fmt.Fprintln(out, "golem index: existing index has no valid sidecar; run \"golem index -full\" to rebuild")
			return errIndexFailed
		}
	}
	activeID := ""
	if activeErr == nil {
		activeID = active.id
	}
	if err := cleanupStaleGenerations(dbPath, activeID); err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	staging, err := createStagingGeneration(dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	stagingDir := filepath.Dir(staging.dbPath)
	discardStaging := true
	defer func() {
		if discardStaging {
			_ = os.RemoveAll(stagingDir)
		}
	}()
	if !full && activeErr == nil {
		if err := copyImmutableFile(active.dbPath, staging.dbPath); err != nil {
			_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
			return errIndexFailed
		}
	}
	store, err := rag.NewSQLiteStore(staging.dbPath)
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	indexer, err := rag.NewIndexerWithEmbedder(embedder, store, rag.WithEmbeddingModel(embChain[0]))
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("build indexer: %w", err)
	}

	res := executeIndex(ctx, indexJob{
		indexer:        indexer,
		store:          store,
		root:           root,
		dbPath:         staging.dbPath,
		displayDBPath:  dbPath,
		sidecarPath:    staging.metadataPath,
		workspaceID:    workspaceID,
		requestedModel: embChain[0],
		out:            out,
	})
	if checkpointErr := checkpointGeneration(ctx, store); checkpointErr != nil {
		_ = store.Close()
		_, _ = fmt.Fprintf(out, "golem index: %v\n", checkpointErr)
		return errIndexFailed
	}
	if closeErr := store.Close(); closeErr != nil {
		_, _ = fmt.Fprintf(out, "golem index: close staging generation: %v\n", closeErr)
		return errIndexFailed
	}
	if ctx.Err() != nil {
		return res.exitErr
	}
	sc, err := readSidecar(staging.metadataPath)
	if err != nil || validateSidecar(sc, workspaceID) != nil {
		if res.exitErr != nil {
			return res.exitErr
		}
		_, _ = fmt.Fprintln(out, "golem index: staging generation did not produce valid metadata")
		return errIndexFailed
	}
	readOnly, err := rag.OpenSQLiteStoreReadOnly(staging.dbPath)
	if err != nil {
		return fmt.Errorf("validate staging generation: %w", err)
	}
	stats, err := readOnly.Stats(ctx)
	_ = readOnly.Close()
	if err != nil {
		return fmt.Errorf("validate staging generation: %w", err)
	}
	if sc.VectorSpaceID != actualVectorSpace {
		_, _ = fmt.Fprintf(out, "golem index: staging vector space %q does not match successful probe %q\n", sc.VectorSpaceID, actualVectorSpace)
		return errIndexFailed
	}
	staging.metadata = generationMetadata{
		SchemaVersion: generationSchemaVersion, Generation: staging.id,
		WorkspaceID: workspaceID, RequestedEmbeddingModel: embChain[0],
		VectorSpaceID: sc.VectorSpaceID, IndexedAt: sc.IndexedAt,
		Status: sc.Status, ErrorCount: sc.ErrorCount,
		SourceCount: stats.TotalSources, ChunkCount: stats.TotalChunks,
	}
	if err := writeGenerationMetadata(staging.metadataPath, staging.metadata); err != nil {
		return fmt.Errorf("write generation metadata: %w", err)
	}
	pointer := activeGenerationPointer{SchemaVersion: activePointerSchemaVersion, WorkspaceID: workspaceID, Generation: staging.id}
	if err := validateGenerationMetadata(staging.metadata, pointer); err != nil {
		return err
	}
	if err := validateGenerationDatabase(ctx, staging); err != nil {
		return err
	}
	final, err := finalizeStagingGeneration(dbPath, staging)
	if err != nil {
		return err
	}
	stagingDir = filepath.Dir(final.dbPath)
	if err := publishActiveGeneration(dbPath, pointer, nil); err != nil {
		if current, readErr := readActivePointer(dbPath); readErr != nil || current.Generation != final.id {
			_ = os.RemoveAll(stagingDir)
		}
		discardStaging = false
		return err
	}
	discardStaging = false
	return res.exitErr
}
