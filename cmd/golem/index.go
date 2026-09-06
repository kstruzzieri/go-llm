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

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// errIndexFailed is the sentinel returned to main() for an already-rendered
// non-zero index outcome, so main() exits non-zero WITHOUT printing an extra
// "golem: <err>" line (runIndex owns its own output).
var errIndexFailed = errors.New("golem index failed")

const indexRetirementFailureMessage = "active-pointer retirement could not be persisted; other processes may still serve the prior generation"

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
	exitErr        error
	policyAffected bool
	policyUnsafe   bool
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
	result := indexResult{policyAffected: len(status.PolicyOutcomes) > 0}
	var skipped, redacted, safeSkips int
	for _, outcome := range status.PolicyOutcomes {
		result.policyUnsafe = result.policyUnsafe || outcome.Unsafe
		switch outcome.Action {
		case rag.IndexPolicySkip:
			skipped++
			if !outcome.Unsafe {
				safeSkips++
			}
		case rag.IndexPolicyRedact:
			redacted++
		}
	}
	if result.policyAffected {
		_, _ = fmt.Fprintf(job.out, "index policy: %d skipped, %d redacted (%d indexed files)\n", skipped, redacted, status.IndexedFiles)
	}
	fail := func(err error) indexResult {
		result.exitErr = errors.Join(errIndexFailed, indexErr, err)
		_, _ = fmt.Fprintln(job.out, "golem index: "+indexFailureMessage(result.policyAffected, err))
		return result
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if result.policyUnsafe {
		return fail(errors.New("unsafe policy cleanup"))
	}
	if indexErr != nil && len(status.Errors) == 0 && !rag.IsSafeIndexSkip(indexErr) {
		return fail(indexErr)
	}

	stats, err := job.store.Stats(ctx)
	if err != nil {
		return fail(fmt.Errorf("read stats: %w", err))
	}
	probe, err := job.store.ProbeVectorSpaces(ctx)
	if err != nil {
		return fail(fmt.Errorf("probe vector space: %w", err))
	}
	clean := len(probe.KnownIDs) == 1 && !probe.HasUnknown
	if stats.TotalSources < 1 || !clean {
		return fail(fmt.Errorf("corpus not usable (sources=%d, vector spaces=%v, legacy=%v); not writing index marker", stats.TotalSources, probe.KnownIDs, probe.HasUnknown))
	}
	if err := chmodIndexDBFiles(job.dbPath); err != nil {
		return fail(err)
	}

	errCount := len(status.Errors) + safeSkips
	sidecarStatus := "complete"
	if errCount > 0 {
		sidecarStatus = "partial"
	}
	sc := indexSidecar{
		SchemaVersion: indexSchemaVersion, WorkspaceID: job.workspaceID,
		RequestedEmbeddingModel: job.requestedModel, VectorSpaceID: probe.KnownIDs[0],
		IndexedAt: time.Now().UTC().Format(time.RFC3339), Status: sidecarStatus, ErrorCount: errCount,
	}
	if err := writeSidecar(job.sidecarPath, sc); err != nil {
		return fail(err)
	}
	displayPath := job.dbPath
	if job.displayDBPath != "" {
		displayPath = job.displayDBPath
	}
	_, _ = fmt.Fprintf(job.out, "indexed %d sources, %d chunks (%d errors) -> %s\n", stats.TotalSources, stats.TotalChunks, errCount, indexDiagnostic(displayPath))
	if errCount > 0 {
		if !result.policyAffected {
			printCappedErrors(job.out, status.Errors, 10)
		}
		result.exitErr = errors.Join(errIndexFailed, indexErr)
	}
	return result
}

// indexDiagnostic protects CLI-owned indexing text, including ordinary errors
// whose filenames contain detected values. Internal errors retain their causes.
func indexDiagnostic(text string) string {
	return secretscan.Redact(text, secretscan.Scan(text))
}

func indexFailureMessage(policyAffected bool, err error) string {
	if policyAffected {
		return "sensitive-content policy affected this index; generation unavailable"
	}
	return indexDiagnostic(err.Error())
}

func printCappedErrors(w io.Writer, errs []string, limit int) {
	shown := errs
	if len(errs) > limit {
		shown = errs[:limit]
	}
	for _, e := range shown {
		_, _ = fmt.Fprintf(w, "  - %s\n", indexDiagnostic(e))
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
func runIndex(ctx context.Context, args []string, out, errOut io.Writer) (runErr error) {
	defer func() {
		if runErr != nil && !errors.Is(runErr, errIndexFailed) && !errors.Is(runErr, flag.ErrHelp) {
			_, _ = fmt.Fprintln(out, "golem index: "+indexDiagnostic(runErr.Error()))
			runErr = errors.Join(errIndexFailed, runErr)
		}
	}()
	var (
		configPath  string
		rootFlag    string
		ollamaURL   string
		full        bool
		progressive bool
	)
	fs := flag.NewFlagSet("golem index", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&configPath, "config", "", "path to models.json (default: auto-discover)")
	fs.StringVar(&rootFlag, "root", ".", "workspace root to index")
	fs.StringVar(&ollamaURL, "ollama-url", "", "override Ollama base URL")
	fs.BoolVar(&full, "full", false, "clean rebuild (drop the existing index first)")
	fs.BoolVar(&progressive, "progressive", false, "generate opt-in L0/L1 progressive source summaries")
	fs.Bool("no-color", false, "disable dim ANSI footers in summary")
	var allowDest stringSliceFlag
	fs.Var(&allowDest, "allow-destination", "admit a remote model destination: \"<provider>/<canonical base URL>\" (repeatable; this command never prompts)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := filepath.Abs(rootFlag)
	if err != nil {
		return fmt.Errorf("golem: resolve root: %w", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return fmt.Errorf("golem: resolve root: %w", err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	// #477: plan and admit before bootstrap. Embedding is REQUIRED for
	// indexing (unresolvable is fatal, unchanged); the summarize route joins
	// the plan only under -progressive.
	embChain, err := embeddingChain(cfg)
	if err != nil {
		return err
	}
	routes := []providerbootstrap.PlannedRoute{{UseCase: "embedding", Chain: embChain}}
	var summarizeChain []string
	if progressive {
		summarizeRoute, serr := providerbootstrap.PlanOptionalUseCaseRoute(cfg, config.UseCaseSummarize)
		if serr != nil {
			return serr
		}
		routes = append(routes, summarizeRoute)
		summarizeChain = summarizeRoute.Chain
	}
	gate, netPlan, _, err := admitForSubcommand(ctx, cfg, routes,
		providerbootstrap.PlanOptions{}, allowDest, ollamaURL, "", "", errOut)
	if err != nil {
		return err
	}

	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:            cfg,
		OllamaURLOverride: ollamaURL,
		DestinationGate:   gate,
		ActiveProviders:   netPlan.ActiveProviders,
	})
	if err != nil {
		return fmt.Errorf("golem: bootstrap providers: %w", err)
	}
	defer func() {
		if closeErr := bundle.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close provider bundle: %s\n", indexDiagnostic(closeErr.Error()))
		}
	}()

	var summarize rag.SourceSummaryGenerator
	if progressive {
		// #477 D8: the chain comes from the admitted route above.
		if len(summarizeChain) == 0 {
			_, _ = fmt.Fprintln(errOut, "golem index: warning: "+progressiveNoChainWarning(false))
		}
		summarize = routerSourceSummaryGenerator(bundle.Router, summarizeChain)
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
		_, _ = fmt.Fprintf(out, "golem index: %s\n", indexDiagnostic(err.Error()))
		return errIndexFailed
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			_, _ = fmt.Fprintf(errOut, "golem: close index writer lease: %s\n", indexDiagnostic(closeErr.Error()))
		}
	}()

	embedder := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return bundle.Router.Route(rc, rr)
	}, embChain)
	actualVectorSpace, err := probeAutoIndexEmbedder(ctx, embedder, embChain[0])
	if err != nil {
		_, _ = fmt.Fprintf(out, "golem index: %s\n", indexDiagnostic(err.Error()))
		return errIndexFailed
	}
	built, err := buildIndexGeneration(ctx, generationBuildOptions{
		root:                root,
		dbPath:              dbPath,
		workspaceID:         workspaceID,
		requestedModel:      embChain[0],
		actualVectorSpace:   actualVectorSpace,
		embedder:            embedder,
		summarize:           summarize,
		full:                full,
		refuseInvalidActive: true,
		out:                 out,
	})
	if built.gcWarn != "" {
		warning := indexDiagnostic(built.gcWarn)
		if built.index.policyAffected {
			warning = "superseded generation cleanup incomplete"
		}
		_, _ = fmt.Fprintf(errOut, "golem index: warning: %s\n", warning)
	}
	if err == nil {
		err = built.publish(ctx, dbPath, workspaceID, nil)
	}
	if err != nil {
		_, _ = fmt.Fprintln(out, "golem index: "+indexFailureMessage(built.index.policyAffected, err))
		if built.retirementErr != nil {
			_, _ = fmt.Fprintln(errOut, "golem index: "+indexRetirementFailureMessage)
		}
		return errors.Join(errIndexFailed, err)
	}
	if built.summaryErr != nil {
		detail := indexDiagnostic(firstLine(built.summaryErr.Error()))
		if built.index.policyAffected {
			detail = "sensitive-content policy affected this index"
		}
		_, _ = fmt.Fprintf(errOut, "golem index: warning: progressive summaries incomplete: %s\n", detail)
	}
	return built.index.exitErr
}
