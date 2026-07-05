package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
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
func probeAutoIndexEmbedder(ctx context.Context, embedder rag.Embedder, model string) error {
	child, cancel := context.WithTimeout(ctx, autoIndexProbeTimeout)
	defer cancel()
	res, err := embedder.Embed(child, model, []string{"golem startup index probe"})
	if err != nil {
		return fmt.Errorf("embed probe (%s): %w", model, err)
	}
	if len(res.Embeddings) != 1 || len(res.Embeddings[0]) == 0 {
		return fmt.Errorf("embed probe (%s): want one non-empty vector, got %d vector(s)", model, len(res.Embeddings))
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
		// A sidecar without its database (torn artifact removal, or the .db
		// deleted by hand) must not survive: if this build fails before
		// writing a new marker, the stale sidecar would validate and the run
		// would report ready with the old run's metadata. Full removes it.
		if fileExists(sidecarPath) {
			return autoIndexClass{full: true, reason: "sidecar exists without a database"}
		}
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

// autoIndexJob carries the background auto-index runner's dependencies. The
// embedder is pre-built by the caller (the same chain embedder used for the
// probe and the index run) and embChain doubles as the expected vector-space
// set: expectedVectorSpaces(cfg) and embeddingChain(cfg) return the identical
// chain, so no separate expected field is carried.
type autoIndexJob struct {
	root        string
	dbPath      string
	sidecarPath string
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
	openRetriever func(context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error)
}

// runAutoIndex is the startup auto-index pipeline (spec "Startup Flow" step 5):
// probe the embedder, classify/self-heal the private store, run the index with
// deleted-source pruning, close the writer, then open read-only retrieval and
// flip the ready wrapper. It is synchronous; the caller launches the goroutine.
// A cancelled ctx returns silently: the wrapper stays warming and no notices
// print during shutdown.
func runAutoIndex(ctx context.Context, job autoIndexJob) {
	fail := func(reason string) {
		if ctx.Err() != nil {
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

	// Probe FIRST: a dead embedder must not create or write the store.
	if err := probeAutoIndexEmbedder(ctx, job.embedder, job.embChain[0]); err != nil {
		fail(err.Error())
		return
	}

	class := classifyAutoIndex(ctx, job.dbPath, job.sidecarPath, job.workspaceID, job.embChain)
	if class.full && class.reason != "" {
		job.notice("warning: retrieve auto-index rebuilding private store: " + class.reason)
	}
	store, err := prepareIndexStore(ctx, job.dbPath, job.sidecarPath, job.workspaceID, job.embChain, class.full)
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
		dbPath:         job.dbPath,
		sidecarPath:    job.sidecarPath,
		workspaceID:    job.workspaceID,
		requestedModel: job.embChain[0],
		out:            &buf,
		pruneDeleted:   true,
	})
	// Close the writer BEFORE any read-only open: retrieval opens the DB with
	// immutable=1 and must not race a live write handle.
	_ = store.Close()
	if ctx.Err() != nil {
		return
	}

	// Interpret the outcome through the store, not the exit sentinel alone
	// (spec "Partial Indexes"): a partial run still writes a usable sidecar.
	sc, scErr := readSidecar(job.sidecarPath)
	if scErr != nil || validateSidecar(sc, job.workspaceID) != nil {
		fail(autoIndexFailReason(&buf, res.exitErr))
		return
	}

	open := job.openRetriever
	if open == nil {
		open = func(oc context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error) {
			return buildGatedRetriever(oc, job.cfg, job.router, job.dbPath, job.embChain, job.feedbackDB)
		}
	}
	tool, feedback, feedbackWarn, dec, stats, err := open(ctx)
	if err != nil {
		fail(err.Error())
		return
	}
	if tool == nil {
		// The gate refusing a store this run just wrote should be impossible;
		// surface it rather than serving a mismatched corpus.
		fail(fmt.Sprintf("index vector space %s does not match embedding chain %v", dec.stored, job.embChain))
		return
	}
	// A non-nil exitErr with a COMPLETE sidecar means this run failed before
	// writing a new marker and an older valid index survived (a completed
	// partial run writes status "partial" and warns below). Serving the
	// previous corpus is correct, but say so instead of a plain ready line.
	if res.exitErr != nil && sc.Status != "partial" {
		job.notice("warning: retrieve auto-index refresh failed: " + autoIndexFailReason(&buf, res.exitErr) +
			"; serving previous index")
	}
	line := autoIndexReadyLine(sc, stats)
	job.ready.markReady(tool, line, feedback)
	job.notice(line)
	if feedbackWarn != "" {
		job.notice("warning: " + feedbackWarn)
	}
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
