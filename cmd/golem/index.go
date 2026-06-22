package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

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
	status, indexErr := job.indexer.IndexDirectoryWithStatus(ctx, job.root,
		rag.WithIncremental(), rag.WithExclude(indexExcludes...))

	// A walk failure / cancellation aborts the command: error but no completed
	// queue. Distinguish it from a completed run with per-file errors.
	if ctx.Err() != nil {
		fmt.Fprintf(job.out, "golem index: cancelled: %v\n", ctx.Err())
		return indexResult{exitErr: errIndexFailed}
	}
	if indexErr != nil && len(status.Errors) == 0 {
		fmt.Fprintf(job.out, "golem index: %v\n", indexErr)
		return indexResult{exitErr: errIndexFailed}
	}

	stats, statErr := job.store.Stats(ctx)
	if statErr != nil {
		fmt.Fprintf(job.out, "golem index: read stats: %v\n", statErr)
		return indexResult{exitErr: errIndexFailed}
	}

	// "Usable store" gate (spec §5.7): >=1 source AND a clean single-vsid probe.
	probe, probeErr := job.store.ProbeVectorSpaces(ctx)
	if probeErr != nil {
		fmt.Fprintf(job.out, "golem index: probe vector space: %v\n", probeErr)
		return indexResult{exitErr: errIndexFailed}
	}
	clean := len(probe.KnownIDs) == 1 && !probe.HasUnknown
	if stats.TotalSources < 1 || !clean {
		fmt.Fprintf(job.out, "golem index: corpus not usable (sources=%d, vector spaces=%v, legacy=%v); not writing index marker\n",
			stats.TotalSources, probe.KnownIDs, probe.HasUnknown)
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
		fmt.Fprintf(job.out, "golem index: %v\n", err)
		return indexResult{exitErr: errIndexFailed}
	}

	fmt.Fprintf(job.out, "indexed %d sources, %d chunks (%d errors) -> %s\n",
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
		fmt.Fprintf(w, "  - %s\n", e)
	}
	if len(errs) > limit {
		fmt.Fprintf(w, "  (+%d more)\n", len(errs)-limit)
	}
}
