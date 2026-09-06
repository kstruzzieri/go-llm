package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		return "", fmt.Errorf("golem: embed probe (%s): %w", model, err)
	}
	if len(res.Embeddings) != 1 || len(res.Embeddings[0]) == 0 {
		return "", fmt.Errorf("golem: embed probe (%s): want one non-empty vector, got %d vector(s)", model, len(res.Embeddings))
	}
	vectorSpaceID := res.VectorSpaceID
	if vectorSpaceID == "" && res.Provider != "" && res.Model != "" {
		vectorSpaceID = res.Provider + "/" + res.Model
	}
	if vectorSpaceID == "" {
		return "", fmt.Errorf("golem: embed probe (%s): successful route returned no vector-space identity", model)
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
	weighter    rag.BehavioralWeighter
	progressive bool
	summarize   rag.SourceSummaryGenerator
	recorder    *evidenceRecorder // -grounding evidence capture; nil => no capture
	ready       *readyRetrieve
	notice      func(string)

	// openRetriever is a test seam: nil selects the real buildGatedRetriever
	// over cfg/router; tests inject a fake because buildGatedRetriever needs
	// a live provider router to construct the query-time chain embedder.
	openRetriever func(context.Context, string) (*retrievalReader, vsDecision, rag.StoreStats, error)
}

// runAutoIndex acquires the workspace writer lease, probes the actual vector
// space, builds and validates an immutable staging generation, publishes its
// active pointer, then swaps the read-only delegate. It is synchronous; the
// caller launches the goroutine.
// Cancellation is silent unless required policy retirement fails. Once policy
// detection occurred, cleanup still retires the pointer and detaches the reader.
func runAutoIndex(ctx context.Context, job autoIndexJob) {
	// The background entry point has no error receiver; notices render failures.
	_ = executeAutoIndex(ctx, job)
}

// executeAutoIndex retains internal causes separately from safe public notices.
func executeAutoIndex(ctx context.Context, job autoIndexJob) error {
	fail := func(err error) error {
		if ctx.Err() != nil {
			return err
		}
		reason := indexDiagnostic(err.Error())
		if job.ready.hasReader() {
			job.notice("warning: retrieve auto-index refresh failed: " + reason + "; serving active generation")
			return err
		}
		job.ready.markFailed("retrieve: the workspace auto-index failed (" + reason + "); " +
			"use read_file, search, glob, and list instead.")
		job.notice("warning: retrieve auto-index failed: " + reason + "; using file/search tools")
		return err
	}
	if len(job.embChain) == 0 {
		return fail(errors.New("no embedding chain configured"))
	}
	contended := false
	lease, err := acquireIndexWriterLease(job.dbPath)
	if errors.Is(err, errIndexWriterLeaseHeld) {
		if job.ready.hasReader() {
			job.notice("warning: retrieve auto-index writer lease already held; serving active generation")
			return nil
		}
		// A first run waits for the competing writer, then adopts its generation.
		contended = true
		lease, err = awaitIndexWriterLease(ctx, job.dbPath)
	}
	if err != nil {
		return fail(err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil && ctx.Err() == nil {
			job.notice("warning: retrieve auto-index writer lease close failed: " + indexDiagnostic(closeErr.Error()))
		}
	}()
	if contended && adoptActiveGeneration(ctx, job) {
		return nil
	}
	// Probe before any filesystem scan: failure preserves the active generation.
	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, job.embedder, job.embChain[0])
	if err != nil {
		return fail(err)
	}

	job.ready.mu.RLock()
	affectedReader := job.ready.reader
	job.ready.mu.RUnlock()
	var buf bytes.Buffer
	built, err := buildIndexGeneration(ctx, generationBuildOptions{
		root: job.root, dbPath: job.dbPath, workspaceID: job.workspaceID,
		requestedModel: job.embChain[0], actualVectorSpace: actualVectorSpace,
		embedder: job.embedder, summarize: job.summarize, pruneDeleted: true, out: &buf,
	})
	failBuilt := func(cause error, retire bool) error {
		if !built.index.policyAffected {
			return fail(cause)
		}
		if retire {
			built.retirementErr = retirePolicyGeneration(ctx, job.dbPath, job.workspaceID)
			cause = errors.Join(cause, built.retirementErr)
		}
		// Local admission stops even if the cross-process pointer write failed.
		job.ready.detach(affectedReader)
		if ctx.Err() == nil || built.retirementErr != nil {
			line := "warning: retrieve auto-index unavailable after sensitive-content policy; using file/search tools"
			if built.retirementErr != nil {
				line += "; " + indexRetirementFailureMessage
			}
			job.notice(line)
		}
		return errors.Join(built.index.exitErr, cause)
	}
	if built.activeErr != nil && !errors.Is(built.activeErr, errNoActiveGeneration) && ctx.Err() == nil {
		detail := indexDiagnostic(built.activeErr.Error())
		if built.index.policyAffected {
			detail = "prior index unavailable"
		}
		job.notice("warning: retrieve auto-index rebuilding private store: " + detail)
	}
	if built.gcWarn != "" && ctx.Err() == nil {
		warning := indexDiagnostic(built.gcWarn)
		if built.index.policyAffected {
			warning = "superseded generation cleanup incomplete"
		}
		job.notice("warning: retrieve auto-index " + warning)
	}
	if err != nil {
		if !built.index.policyAffected {
			// Retain ordinary background classification from executeIndex output.
			_ = fail(errors.New(autoIndexFailReason(&buf, err))) // Render the concise reason; return the original tree.
			return err
		}
		return failBuilt(err, false) // builder already attempted retirement
	}
	final := built.generation
	reader, dec, openedStats, err := job.open(ctx, final.dbPath)
	if err == nil && reader == nil {
		err = fmt.Errorf("golem: index vector space %s does not match embedding chain %v", dec.stored, job.embChain)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		closeErr := reader.closeAfterDrain()
		cleanupErr := removeGenerationPath(context.WithoutCancel(ctx), filepath.Dir(final.dbPath))
		return failBuilt(errors.Join(err, closeErr, cleanupErr), true)
	}
	reader.bindGeneration(job.dbPath, job.workspaceID, final)
	if err := built.publish(ctx, job.dbPath, job.workspaceID, nil); err != nil {
		return failBuilt(errors.Join(err, reader.closeAfterDrain()), false)
	}
	if err := ctx.Err(); err != nil {
		return failBuilt(errors.Join(err, reader.closeAfterDrain()), true)
	}
	line := autoIndexReadyLine(built.sidecar, openedStats)
	if !job.ready.install(reader, line) || ctx.Err() != nil {
		return nil
	}
	job.notice(line)
	if built.summaryErr != nil {
		detail := indexDiagnostic(firstLine(built.summaryErr.Error()))
		if built.index.policyAffected {
			detail = "sensitive-content policy affected this index"
		}
		job.notice("warning: retrieve auto-index progressive summaries incomplete: " + detail)
	}
	return nil
}

// indexLeaseRetryInterval paces the first-run writer-lease wait; contention is
// human-scale (another golem session indexing the same workspace).
const indexLeaseRetryInterval = 500 * time.Millisecond

// awaitIndexWriterLease polls for the workspace writer lease until it is
// acquired, a non-contention error occurs, or ctx is cancelled.
func awaitIndexWriterLease(ctx context.Context, dbPath string) (*flockLease, error) {
	ticker := time.NewTicker(indexLeaseRetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
		lease, err := acquireIndexWriterLease(dbPath)
		if !errors.Is(err, errIndexWriterLeaseHeld) {
			return lease, err
		}
	}
}

// open builds the query-time retrieval reader over dbPath, via the injected
// test seam when one is set.
func (job autoIndexJob) open(ctx context.Context, dbPath string) (*retrievalReader, vsDecision, rag.StoreStats, error) {
	if job.openRetriever != nil {
		return job.openRetriever(ctx, dbPath)
	}
	return buildGatedRetriever(ctx, job.cfg, job.router, dbPath, job.embChain, job.weighter, job.progressive, job.recorder)
}

// adoptActiveGeneration installs the generation a competing writer published
// while this run waited for the lease. It returns false when nothing valid was
// published, the open failed, or the vector-space gate rejected it — the
// caller then builds under the lease it holds.
func adoptActiveGeneration(ctx context.Context, job autoIndexJob) bool {
	gen, err := resolveActiveGeneration(ctx, job.dbPath, job.workspaceID)
	if err != nil {
		return false
	}
	reader, _, stats, err := job.open(ctx, gen.dbPath)
	if err != nil || reader == nil {
		return false
	}
	reader.bindGeneration(job.dbPath, job.workspaceID, gen)
	line := autoGenerationLine(gen.metadata, stats)
	if !job.ready.install(reader, line) || ctx.Err() != nil {
		return true
	}
	job.notice(line)
	return true
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
