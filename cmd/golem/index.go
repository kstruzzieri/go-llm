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

	_, _ = fmt.Fprintf(job.out, "indexed %d sources, %d chunks (%d errors) -> %s\n",
		stats.TotalSources, stats.TotalChunks, errCount, job.dbPath)
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

// osRemove and errNotExist are indirected so removal is testable without real files.
var (
	osRemove    = os.Remove
	errNotExist = os.ErrNotExist
)

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// removeIndexArtifacts deletes the DB, its WAL/SHM sidecars, and the marker so a
// -full run cannot inherit an old vector space. Not-exist is fine (first run).
func removeIndexArtifacts(dbPath, sidecar string) error {
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm", sidecar} {
		if err := removeIfExists(p); err != nil {
			return err
		}
	}
	return nil
}

func removeIfExists(p string) error {
	if err := osRemove(p); err != nil && !errors.Is(err, errNotExist) {
		return fmt.Errorf("remove %q: %w", p, err)
	}
	return nil
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

// prepareIndexStore handles the pre-open lifecycle for the index store:
//   - full=true: remove all artifacts (DB, WAL, SHM, sidecar) then open a fresh store.
//   - full=false: if the DB pre-existed, run preflightExistingIndex to enforce
//     spec §5.2 (valid sidecar + matching vector-space id required), then open
//     the write-capable store only after preflight passes.
//
// Preflight and removal always happen BEFORE SQLite is opened so no DB file is
// ever deleted underneath a live handle. The parent directory is created on
// first call so callers need not manage it separately.
func prepareIndexStore(ctx context.Context, dbPath, sidecarPath, workspaceID string, expected []string, full bool) (*rag.SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}
	if full {
		if err := removeIndexArtifacts(dbPath, sidecarPath); err != nil {
			return nil, err
		}
		return rag.NewSQLiteStore(dbPath)
	}
	preexisting := fileExists(dbPath)
	if preexisting {
		if err := preflightExistingIndex(ctx, dbPath, sidecarPath, workspaceID, expected); err != nil {
			return nil, err
		}
	}
	return rag.NewSQLiteStore(dbPath)
}

// preflightExistingIndex enforces spec §5.2 for a DEFAULT (incremental) run
// when dbPath existed before opening SQLite for writes. A matching sidecar is
// required before any DB open; otherwise a copied/foreign empty DB could be
// blessed or modified by an incremental run. The vector-space probe uses a
// read-only store so a mismatch refusal does not create WAL/SHM or migrate.
// Mirrored by classifyAutoIndex (auto_index.go), which maps these refusals to
// a self-heal full rebuild; keep the checks in sync.
func preflightExistingIndex(ctx context.Context, dbPath, sidecarPath, workspaceID string, expected []string) error {
	sc, serr := readSidecar(sidecarPath)
	if serr != nil {
		return fmt.Errorf("existing index has no valid sidecar; run \"golem index -full\" to rebuild")
	}
	if verr := validateSidecar(sc, workspaceID); verr != nil {
		return fmt.Errorf("existing index sidecar invalid (%v); run \"golem index -full\" to rebuild", verr)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("preflight open read-only: %w", err)
	}
	defer func() { _ = store.Close() }()
	probe, perr := store.ProbeVectorSpaces(ctx)
	if perr != nil {
		return fmt.Errorf("preflight probe: %w", perr)
	}
	dec := vsGateDecision(probe.KnownIDs, probe.HasUnknown, expected)
	if !dec.register {
		return fmt.Errorf("existing index was built with vector space %s, current embedding chain is %v; run \"golem index -full\" to rebuild",
			describeStored(probe), expected)
	}
	return nil
}

func describeStored(p rag.VectorSpaceProbe) string {
	if len(p.KnownIDs) == 1 {
		return p.KnownIDs[0]
	}
	return fmt.Sprintf("%v", p.KnownIDs)
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
	// prepareIndexStore creates the parent dir itself; no MkdirAll needed here.
	store, err := prepareIndexStore(ctx, dbPath, sidecarPath(dbPath), workspaceID, embChain, full)
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %v\n", err)
		return errIndexFailed
	}
	defer func() { _ = store.Close() }()

	embedder := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return bundle.Router.Route(rc, rr)
	}, embChain)
	indexer, err := rag.NewIndexerWithEmbedder(embedder, store, rag.WithEmbeddingModel(embChain[0]))
	if err != nil {
		return fmt.Errorf("build indexer: %w", err)
	}

	res := executeIndex(ctx, indexJob{
		indexer:        indexer,
		store:          store,
		root:           root,
		dbPath:         dbPath,
		sidecarPath:    sidecarPath(dbPath),
		workspaceID:    workspaceID,
		requestedModel: embChain[0],
		out:            out,
	})
	return res.exitErr
}
