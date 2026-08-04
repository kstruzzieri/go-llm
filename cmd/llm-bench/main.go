// Command llm-bench replays captured MCP/chat traces against one or more
// candidate models and produces a comparative quality + latency report.
//
// See docs/llm/benchmark-plan.md for design rationale and metrics.
//
// Example:
//
//	llm-bench \
//	  -traces docs/llm/traces/*.json \
//	  -models "ollama/qwen3-coder-next:latest,ollama/gemma4:31b" \
//	  -scorer exact-match \
//	  -report ./bench-report.md
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
)

func main() {
	capture := flag.Bool("capture", false, "Export persisted conversations as redacted benchmark traces and exit")
	captureDB := flag.String("capture-db", "", "SQLite database path containing conversation tables (required with -capture)")
	captureOut := flag.String("capture-out", filepath.Join("docs", "llm", "traces"), "Directory for captured trace JSON files")
	captureLimit := flag.Int("capture-limit", 0, "Maximum conversations to export in capture mode (0 = all)")
	captureSource := flag.String("capture-source", defaultCaptureSource, "Trace source label for captured conversations")
	captureSystem := flag.String("capture-system", "", "Fallback system prompt when a conversation has no stored system message")
	mcpStdioCommand := flag.String("mcp-stdio-command", "", "Run this command and snapshot its MCP tools/list (mutually exclusive with -mcp-url)")
	mcpURL := flag.String("mcp-url", "", "Snapshot MCP tools/list from this HTTP endpoint (mutually exclusive with -mcp-stdio-command)")
	captureSample := flag.String("capture-sample", "", "Stratified capture spec, e.g. n=50,stratify=token-length:turn-count (mutually exclusive with -capture-limit)")
	captureSampleSeed := flag.Int64("capture-sample-seed", 0, "Deterministic seed for -capture-sample (0 = time.Now().UnixNano())")

	importXlam := flag.String("import-xlam", "", "Convert a MadeAgents/xlam-irrelevance JSON file into golden-empty restraint traces and exit (uses -import-xlam-out, -import-xlam-manifest, -import-xlam-n, -import-xlam-seed, -import-xlam-min-tools)")
	importXlamOut := flag.String("import-xlam-out", filepath.Join("docs", "llm", "traces", "xlam-irrelevance-local"), "Output directory for -import-xlam trace files")
	importXlamManifest := flag.String("import-xlam-manifest", filepath.Join("docs", "llm", "calibration", "xlam-irrelevance-manifest.jsonl"), "Output manifest path for -import-xlam")
	importXlamN := flag.Int("import-xlam-n", 300, "Number of eligible records to sample for -import-xlam (<=0 = all eligible)")
	importXlamSeed := flag.Int64("import-xlam-seed", 42, "Deterministic sampling seed for -import-xlam")
	assemblyBuildPath := flag.String("assembly-build", "", "Build assembly traces from this case-fixture JSON (#331): a JSON array is the 3a flat/progressive corpus; a JSON object is the 3c mixed-assembly fixture (paired legacy/mixed arms + optional topline)")
	assemblyOut := flag.String("assembly-out", filepath.Join("docs", "llm", "assembly-corpus", "traces"), "Output directory for -assembly-build with a 3a JSON-array fixture")
	assemblyOutMixed := flag.String("assembly-out-mixed", filepath.Join("docs", "llm", "assembly-corpus", "mixed", "traces"), "Output directory for -assembly-build with a 3c mixed-assembly object fixture (the mixed corpus keeps its own manifest, separate from -assembly-out)")

	importXlamMinTools := flag.Int("import-xlam-min-tools", 1, "Drop -import-xlam records offering fewer than this many tools")

	calibrateCapture := flag.Bool("calibrate-capture", false, "Phase 1: replay candidates and write frozen artifacts.jsonl")
	calibrate := flag.Bool("calibrate", false, "Phase 2: re-score frozen labeled artifacts with the judge model")
	manualReport := flag.Bool("manual-report", false, "Score frozen labeled artifacts with human labels (manual scorer) and emit a quality baseline report (uses -labels, -artifacts, -report)")
	pairedReport := flag.Bool("paired-report", false, "Emit the paired-label report: paired-complete means, completeness worklist, win/loss/tie matrix, bootstrap delta CIs, resolution diagnostic (uses -labels, -artifacts, -baseline, -report)")
	assemblyReport := flag.Bool("assembly-report", false, "Emit the paired assembly (flat vs progressive) report using -labels, -artifacts, and -report (#331)")
	discriminationReport := flag.Bool("discrimination-report", false, "Classify each trace (spec §9.1: valid-discriminator/saturated/unsolved/floor-only/no-signal/unpaired), emit the per-stratum funnel + K-gate, and write the derived valid-discriminator manifest (uses -labels, -artifacts, -corpus-manifest, -top-models, -floor-model, -gate-source, -discriminator-manifest-out, -report; does NOT honor -corpus-sources/-partitions/-categories — it always classifies all strata so the funnel shows every source)")
	topModels := flag.String("top-models", "", "Comma-separated top-cluster selectors for -discrimination-report (transport prefix optional; e.g. gemma4:31b or ollama/gemma4:31b)")
	floorModel := flag.String("floor-model", "", "Floor model selector for -discrimination-report (transport prefix optional)")
	gateSource := flag.String("gate-source", "", "Provenance source the §9.2 K-gate is decided on for -discrimination-report (empty = round3-challenge)")
	discriminatorManifestOut := flag.String("discriminator-manifest-out", "", "Output path for the derived valid-discriminator manifest (gitignored); contains all valid-discriminator sources, so pair with -corpus-sources for stratum-specific -manual-report/-paired-report views")
	fimLatency := flag.Bool("fim-latency", false, "Measure FIM / inline-completion latency separately from chat latency (uses -fim-cases, -models, -fim-num-predict, -fim-warmup, -report)")
	fimCases := flag.String("fim-cases", "", "Glob for FIM case JSON files (prefix/suffix), required with -fim-latency")
	fimNumPredict := flag.Int("fim-num-predict", defaultFIMNumPredict, "Max tokens to generate per FIM completion (interactive regime is short)")
	fimWarmup := flag.Bool("fim-warmup", true, "Issue one discarded generate per model before timing so FIM latency reflects the warm regime")
	baseline := flag.String("baseline", "", "Baseline model selector for -paired-report deltas (default: first lineup model, derived from artifacts)")
	labelsPath := flag.String("labels", filepath.Join("docs", "llm", "calibration", "labels.jsonl"), "Path to labels.jsonl (Phase 2)")
	labelsOut := flag.String("labels-out", filepath.Join("docs", "llm", "calibration", "artifacts.jsonl"), "Output path for -calibrate-capture artifacts.jsonl, or labels.jsonl for -blind-ingest (must differ from -artifacts)")
	artifactsPath := flag.String("artifacts", filepath.Join("docs", "llm", "calibration", "artifacts.jsonl"), "Path to artifacts.jsonl (Phase 2)")
	calibrateReportDir := flag.String("calibrate-report-dir", filepath.Join("docs", "llm", "calibration", "reports"), "Directory for calibration reports")
	calibrateAgreement := flag.String("calibrate-agreement", string(calibrationAgreementExact), "Calibration agreement mode: exact or tolerance")
	judgeStabilityRuns := flag.Int("judge-stability-runs", 0, "When >1, run the judge that many times per artifact (cache bypassed) and report spread as a diagnostic")

	tracesGlob := flag.String("traces", "", "Glob pattern for trace JSON files (required for normal runs and -calibrate-capture)")
	modelsArg := flag.String("models", "", "Comma-separated model selectors (provider/model or bare model name; required)")
	scorerName := flag.String("scorer", "exact-match", "Scoring strategy: exact-match, llm-judge, manual")
	judgeModel := flag.String("judge-model", "", "Judge model/selector used by -scorer llm-judge; default uses models.json role judge or gemma4:31b")
	judgeOllamaURL := flag.String("judge-ollama-url", "", "Ollama base URL for the Ollama judge transport (default: -ollama-url)")
	judgeTimeout := flag.Duration("judge-timeout", 5*time.Minute, "Timeout for each llm-judge scoring request")
	judgeCachePath := flag.String("judge-cache", defaultJudgeCachePath(), "SQLite path for judge response cache; empty disables")
	judgeTransport := flag.String("judge-transport", "", "Judge backend for -scorer llm-judge: ollama (default), openai-compat, or claude-cli (headless `claude -p`, subscription)")
	judgeBaseURL := flag.String("judge-base-url", "", "Base URL for -judge-transport openai-compat (server root, no /v1 suffix)")
	judgeAPIKey := flag.String("judge-api-key", "", "Bearer token for -judge-transport openai-compat; falls back to $"+judgeAPIKeyEnvVar)
	candidateBaseURL := flag.String("candidate-base-url", "", "Base URL for openai-compat candidate targets (server root, no /v1 suffix)")
	candidateAPIKey := flag.String("candidate-api-key", "", "Bearer token for openai-compat candidate targets; falls back to $"+candidateAPIKeyEnvVar)
	corpusManifestPath := flag.String("corpus-manifest", "", "Path to a corpus manifest JSONL (partition/category per trace); enables partition-separated reporting")
	corpusPartitions := flag.String("corpus-partitions", "", "Comma-separated partitions to include from the corpus manifest (natural, challenge, judge-validation; empty = all)")
	corpusCategories := flag.String("corpus-categories", "", "Comma-separated categories to include from the corpus manifest (empty = all)")
	corpusSources := flag.String("corpus-sources", "", "Comma-separated provenance sources to include from the corpus manifest (e.g. round3-challenge, round2-challenge, first-accepted-run; empty = all)")
	corpusOnlyEvidence := flag.Bool("corpus-only-evidence", false, "Restrict the corpus run to entries flagged allowed_as_model_evidence")
	blindRender := flag.Bool("blind-render", false, "Render a blind labeling worksheet from -artifacts (model identity hidden) to -report; legacy/mixed/topline assembly blocks are promptless (#331 slice 3c)")
	blindIngest := flag.Bool("blind-ingest", false, "Parse a filled blind worksheet (-worksheet) into labels.jsonl (-labels-out), rejoining model on artifact_hash from -artifacts; grounding-check flags and DUP intra-rater scores are summarized on stderr")
	blindDups := flag.Int("blind-dups", 0, "With -blind-render: append this many duplicate promptless blocks (deterministic selection over legacy/mixed artifacts) as intra-rater controls")
	dupsOut := flag.String("dups-out", "", "With -blind-ingest: optional JSONL output path for intra-rater dup pairs {artifact_hash, primary_score, dup_score}")
	blindBlockmapOut := flag.String("blind-blockmap-out", "", "With -blind-render: output path for the opaque block map {schema_version, salt, blocks} — REQUIRED when any promptless (legacy/mixed/topline) block renders; the map is the only join from worksheet BLOCK ids to artifact hashes")
	blindBlockmapPath := flag.String("blind-blockmap", "", "With -blind-ingest: the -blind-blockmap-out map the worksheet was rendered with; required when the worksheet holds opaque BLOCK blocks")
	fcRender := flag.Bool("fc-render", false, "Render a forced-choice worksheet (complete legacy/mixed pairs, arm identity hidden behind the sealed -fc-sidemap A/B sides) from -artifacts to -report (#331 slice 3c)")
	fcIngest := flag.Bool("fc-ingest", false, "Parse a filled forced-choice worksheet (-worksheet) into a preference sidecar JSONL (-fc-out), validating pairs against -artifacts and the sealed -fc-sidemap")
	fcOut := flag.String("fc-out", "", "Output JSONL path for -fc-ingest preference rows (required with -fc-ingest)")
	fcPreferences := flag.String("fc-preferences", "", "With -assembly-report: optional -fc-ingest sidecar JSONL; adds the registered forced-choice sign-test secondary analysis to each legacy-mixed model section (never feeds the primary decision)")
	fcSidemapGenerate := flag.Bool("fc-sidemap-generate", false, "Generate the sealed random forced-choice side map over the complete legacy/mixed pairs in -artifacts, write it to -fc-sidemap-out, and print its sha256 (commit the digest BEFORE labeling)")
	fcSidemapOut := flag.String("fc-sidemap-out", "", "Output path for -fc-sidemap-generate (required with it)")
	fcSidemapPath := flag.String("fc-sidemap", "", "Sealed side-map JSON path: REQUIRED with -fc-render and -fc-ingest; optional with -assembly-report, where it replaces the pre-sidemap parity side resolution and enables the descriptive arm-guess audit")
	fcSidemapDigest := flag.String("fc-sidemap-digest", "", "With -fc-render, -fc-ingest, or -assembly-report (alongside -fc-sidemap): the committed sha256 of the sidemap file; a mismatch is a hard error")
	fcRequireComplete := flag.Bool("fc-require-complete", false, "With -fc-ingest: any rendered pair block left blank is a loud error listing the pairs (the registered workflow)")
	captureManifestFlag := flag.String("capture-manifest", "", "With -assembly-report: the -calibrate-capture run manifest; embeds its digest and excludes legacy/mixed pairs it cannot verify (unverified-capture / temperature-mismatch)")
	modelFilter := flag.String("model", "", "With -blind-render, -fc-render, or -adjudicate-render: only render artifacts whose candidate model matches this selector (the registered workflow labels one model at a time); empty renders all")
	adjudicateRender := flag.Bool("adjudicate-render", false, "Render the grounding adjudication worksheet (full prompt shown) for labels flagged grounding-check, from -artifacts and -labels to -report (#331 slice 3c)")
	adjudicateIngest := flag.Bool("adjudicate-ingest", false, "Parse a filled adjudication worksheet (-worksheet) and emit the full updated label set (-labels-out) from -artifacts and -labels; every flagged label must be adjudicated with a reason")
	worksheetPath := flag.String("worksheet", "", "Path to a filled worksheet (required with -blind-ingest, -fc-ingest, -adjudicate-ingest)")
	labelerName := flag.String("labeler", "manual", "Labeler name stamped on -blind-ingest and -fc-ingest output rows")
	reportPath := flag.String("report", "", "Output report path (default: stdout)")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "Per-replay timeout for candidate model calls")
	flag.Parse()
	providedFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) {
		providedFlags[f.Name] = true
	})

	ct := resolveCandidateTransportConfig(*candidateBaseURL, *candidateAPIKey, os.Getenv)

	modes := 0
	if *capture {
		modes++
	}
	if *calibrateCapture {
		modes++
	}
	if *calibrate {
		modes++
	}
	if *manualReport {
		modes++
	}
	if *pairedReport {
		modes++
	}
	if *assemblyReport {
		modes++
	}
	if *fimLatency {
		modes++
	}
	if *blindRender {
		modes++
	}
	if *blindIngest {
		modes++
	}
	if *fcRender {
		modes++
	}
	if *fcIngest {
		modes++
	}
	if *fcSidemapGenerate {
		modes++
	}
	if *adjudicateRender {
		modes++
	}
	if *adjudicateIngest {
		modes++
	}
	if *discriminationReport {
		modes++
	}
	if *importXlam != "" {
		modes++
	}
	if *assemblyBuildPath != "" {
		modes++
	}
	if modes > 1 {
		log.Fatalf("llm-bench: -capture, -calibrate-capture, -calibrate, -manual-report, -paired-report, -assembly-report, -fim-latency, -blind-render, -blind-ingest, -fc-render, -fc-ingest, -fc-sidemap-generate, -adjudicate-render, -adjudicate-ingest, -discrimination-report, -import-xlam, -assembly-build are mutually exclusive")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *assemblyBuildPath != "" {
		if err := assemblyBuildDispatch(ctx, *assemblyBuildPath, *assemblyOut, *assemblyOutMixed); err != nil {
			log.Fatalf("llm-bench: assembly-build: %v", err)
		}
		return
	}

	if *capture {
		if err := validateCaptureSampleAndLimit(*captureLimit, *captureSample); err != nil {
			log.Fatalf("llm-bench: %v", err)
		}
		sampleSpec, err := resolveCaptureSample(*captureSample)
		if err != nil {
			log.Fatalf("llm-bench: %v", err)
		}
		src, err := resolveToolSchemaSource(*mcpStdioCommand, *mcpURL)
		if err != nil {
			log.Fatalf("llm-bench: %v", err)
		}
		if src == nil {
			fmt.Fprintln(os.Stderr, "llm-bench: no -mcp-stdio-command or -mcp-url set; captured traces will have empty trace.Tools and ToolArgsValid will be reported as not-computed")
		}
		result, err := runCapture(ctx, captureOptions{
			DBPath:           *captureDB,
			OutputDir:        *captureOut,
			Limit:            *captureLimit,
			Source:           *captureSource,
			FallbackSystem:   *captureSystem,
			ToolSchemaSource: src,
			SampleSpec:       sampleSpec,
			SampleSeed:       *captureSampleSeed,
		})
		if err != nil {
			log.Fatalf("llm-bench: capture: %v", err)
		}
		for _, skipped := range result.Skipped {
			fmt.Fprintf(os.Stderr, "llm-bench: capture skipped %s\n", skipped)
		}
		if len(result.Written) == 0 {
			log.Fatalf("llm-bench: capture wrote no traces")
		}
		fmt.Fprintf(os.Stderr, "llm-bench: capture wrote %d trace(s) to %s\n", len(result.Written), *captureOut)
		return
	}

	if *importXlam != "" {
		refuseOutputAlias("import-xlam", "-import-xlam-manifest", *importXlamManifest,
			[][2]string{{"-import-xlam", *importXlam}})
		res, err := importXlamIrrelevance(xlamImportOptions{
			SrcPath:      *importXlam,
			OutDir:       *importXlamOut,
			ManifestPath: *importXlamManifest,
			N:            *importXlamN,
			Seed:         *importXlamSeed,
			MinTools:     *importXlamMinTools,
		})
		if err != nil {
			log.Fatalf("llm-bench: import-xlam: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: import-xlam wrote %d of %d eligible trace(s) (filtered %d ineligible) to %s, manifest %s\n", res.Written, res.Eligible, res.Filtered, *importXlamOut, *importXlamManifest)
		return
	}

	if *calibrateCapture {
		if *tracesGlob == "" || *modelsArg == "" {
			log.Fatalf("llm-bench: -calibrate-capture requires -traces and -models")
		}
		tracePaths, err := filepath.Glob(*tracesGlob)
		if err != nil {
			log.Fatalf("llm-bench: glob: %v", err)
		}
		// The artifacts output (and its manifest sibling) must never clobber
		// a trace input.
		refuseOutputAlias("calibrate-capture", "-labels-out", *labelsOut, pathListInputs("-traces", tracePaths))
		refuseOutputAlias("calibrate-capture", "-labels-out capture manifest", captureManifestPath(*labelsOut), pathListInputs("-traces", tracePaths))
		// ... and the two outputs must differ from EACH OTHER (round-2 review
		// P1): a pre-existing hardlink/symlink between <labels-out> and its
		// .manifest.json sibling would leave the artifacts path holding the
		// manifest bytes after the second write.
		refuseOutputAlias("calibrate-capture", "-labels-out capture manifest", captureManifestPath(*labelsOut),
			[][2]string{{"-labels-out", *labelsOut}})
		traces, err := loadTraces(tracePaths)
		if err != nil {
			log.Fatalf("llm-bench: load traces: %v", err)
		}
		targets, err := parseModelTargets(*modelsArg)
		if err != nil {
			log.Fatalf("llm-bench: parse models: %v", err)
		}
		runner := &Runner{
			OllamaURL:           *ollamaURL,
			OpenAICompatBaseURL: ct.baseURL,
			OpenAICompatAPIKey:  ct.apiKey,
			Timeout:             *timeout,
			Scorer:              &CaptureScorer{},
		}
		// Model digests feed assembly-artifact capture provenance only, so
		// resolution (an /api/show round-trip per ollama target) is skipped
		// entirely when the trace set has no prefilled assembly arms.
		var modelDigests map[string]string
		if anyPrefilledAssemblyTrace(traces) {
			modelDigests = resolveCaptureModelDigests(ctx, *ollamaURL, targets)
		}
		if err := runCalibrateCapture(ctx, calibrateCaptureOptions{
			Runner:              runner,
			Targets:             targets,
			Traces:              traces,
			OutputPath:          *labelsOut,
			ModelDigests:        modelDigests,
			OllamaURL:           *ollamaURL,
			OpenAICompatBaseURL: ct.baseURL,
		}); err != nil {
			log.Fatalf("llm-bench: calibrate-capture: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: calibrate-capture wrote artifacts to %s\n", *labelsOut)
		return
	}

	if *calibrate {
		judgeURL := strings.TrimSpace(*judgeOllamaURL)
		if judgeURL == "" {
			judgeURL = *ollamaURL
		}
		judgeName := strings.TrimSpace(*judgeModel)
		if judgeName == "" {
			judgeName = defaultJudgeModelName()
		}
		// Typed-nil interface trap: openJudgeCache may return
		// (*sqliteJudgeCache)(nil), which would still satisfy the
		// judgeCacheStore interface as a non-nil interface value.
		// Only assign the interface variable when the concrete
		// pointer is genuinely non-nil.
		var cacheStore judgeCacheStore
		if c, err := openJudgeCache(*judgeCachePath); err != nil {
			fmt.Fprintf(os.Stderr, "llm-bench: judge cache disabled (%s)\n", redactErrorMessage(err.Error()))
		} else if c != nil {
			cacheStore = c
			defer func() { _ = c.Close() }()
		}
		jt := resolveJudgeTransportConfig(*judgeTransport, *judgeBaseURL, *judgeAPIKey, os.Getenv)
		scorer, err := newScorer(ctx, "llm-judge", scorerOptions{
			ollamaURL:      judgeURL,
			judgeModel:     judgeName,
			judgeTimeout:   *judgeTimeout,
			judgeCache:     cacheStore,
			judgeTransport: jt.transport,
			judgeBaseURL:   jt.baseURL,
			judgeAPIKey:    jt.apiKey,
			// Primary calibration run is cached; stability runs flip
			// the flag internally (Task 23).
			bypassCache: false,
		})
		if err != nil {
			log.Fatalf("llm-bench: calibrate: %v", err)
		}
		judgeProviderName := defaultBenchProvider
		if js, ok := scorer.(*LLMJudgeScorer); ok {
			judgeProviderName = js.JudgeProvider
		}
		res, err := runCalibrate(ctx, calibrateOptions{
			LabelsPath:    *labelsPath,
			ArtifactsPath: *artifactsPath,
			Scorer:        scorer,
			JudgeModel:    judgeName,
			JudgeProvider: judgeProviderName,
			ReportDir:     *calibrateReportDir,
			StabilityRuns: *judgeStabilityRuns,
			AgreementMode: calibrationAgreementMode(*calibrateAgreement),
		})
		if err != nil {
			log.Fatalf("llm-bench: calibrate: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: calibrate verdict=%s agreement=%d/%d report=%s\n",
			res.Verdict, res.AgreeCount, res.MatchedCount, res.ReportPath)
		if code := calibrationExitCode(res.Verdict); code != 0 {
			os.Exit(code)
		}
		return
	}

	if *manualReport {
		refuseReportAlias("manual-report", *reportPath, [][2]string{
			{"-labels", *labelsPath}, {"-artifacts", *artifactsPath}, {"-corpus-manifest", *corpusManifestPath}})
		filter, err := offlineCorpusFilter(*corpusManifestPath, *corpusPartitions, *corpusCategories, *corpusSources, *corpusOnlyEvidence)
		if err != nil {
			log.Fatalf("llm-bench: manual-report: %v", err)
		}
		report, err := runManualReport(ctx, *labelsPath, *artifactsPath, filter)
		if err != nil {
			log.Fatalf("llm-bench: manual-report: %v", err)
		}
		if *reportPath == "" {
			fmt.Print(report)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
			log.Fatalf("llm-bench: write report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: manual quality report written to %s\n", *reportPath)
		return
	}

	if *pairedReport {
		refuseReportAlias("paired-report", *reportPath, [][2]string{
			{"-labels", *labelsPath}, {"-artifacts", *artifactsPath}, {"-corpus-manifest", *corpusManifestPath}})
		filter, err := offlineCorpusFilter(*corpusManifestPath, *corpusPartitions, *corpusCategories, *corpusSources, *corpusOnlyEvidence)
		if err != nil {
			log.Fatalf("llm-bench: paired-report: %v", err)
		}
		report, err := runPairedReport(*labelsPath, *artifactsPath, *baseline, filter)
		if err != nil {
			log.Fatalf("llm-bench: paired-report: %v", err)
		}
		if *reportPath == "" {
			fmt.Print(report)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
			log.Fatalf("llm-bench: write report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: paired-label report written to %s\n", *reportPath)
		return
	}

	if *assemblyReport {
		// The review reproduced -report clobbering the artifacts JSONL here.
		refuseReportAlias("assembly-report", *reportPath, [][2]string{
			{"-labels", *labelsPath}, {"-artifacts", *artifactsPath},
			{"-fc-preferences", *fcPreferences}, {"-fc-sidemap", *fcSidemapPath},
			{"-capture-manifest", *captureManifestFlag}})
		report, err := runAssemblyReport(assemblyReportOptions{
			LabelsPath:          *labelsPath,
			ArtifactsPath:       *artifactsPath,
			FCPrefsPath:         *fcPreferences,
			FCSidemapPath:       *fcSidemapPath,
			FCSidemapDigest:     *fcSidemapDigest,
			CaptureManifestPath: *captureManifestFlag,
		})
		if err != nil {
			log.Fatalf("llm-bench: assembly-report: %v", err)
		}
		if *reportPath == "" {
			fmt.Print(report)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
			log.Fatalf("llm-bench: write assembly report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: assembly report written to %s\n", *reportPath)
		return
	}

	if *discriminationReport {
		if strings.TrimSpace(*corpusManifestPath) == "" {
			log.Fatalf("llm-bench: -discrimination-report requires -corpus-manifest")
		}
		if strings.TrimSpace(*corpusSources) != "" {
			log.Printf("llm-bench: ignoring -corpus-sources in -discrimination-report mode (all strata are classified so the funnel shows every source)")
		}
		discriminationInputs := [][2]string{
			{"-labels", *labelsPath}, {"-artifacts", *artifactsPath}, {"-corpus-manifest", *corpusManifestPath}}
		if strings.TrimSpace(*discriminatorManifestOut) != "" {
			refuseOutputAlias("discrimination-report", "-discriminator-manifest-out", *discriminatorManifestOut, discriminationInputs)
		}
		// Output/output collision: both files are written by this invocation.
		refuseReportAlias("discrimination-report", *reportPath,
			append(discriminationInputs, [2]string{"-discriminator-manifest-out", *discriminatorManifestOut}))
		report, err := runDiscriminationReport(discriminationOptions{
			LabelsPath:               *labelsPath,
			ArtifactsPath:            *artifactsPath,
			ManifestPath:             *corpusManifestPath,
			TopModels:                splitCommaList(*topModels),
			FloorModel:               *floorModel,
			GateSource:               *gateSource,
			DiscriminatorManifestOut: *discriminatorManifestOut,
		})
		if err != nil {
			log.Fatalf("llm-bench: discrimination-report: %v", err)
		}
		if *reportPath == "" {
			fmt.Print(report)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
			log.Fatalf("llm-bench: write report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: discrimination report written to %s\n", *reportPath)
		return
	}

	if *blindRender {
		refuseReportAlias("blind-render", *reportPath, [][2]string{{"-artifacts", *artifactsPath}})
		if strings.TrimSpace(*blindBlockmapOut) != "" {
			// Two-output collision (round-2 review P2): the map is written
			// first and the worksheet second, so an aliased pair loses the
			// only opaque-id join. Cleaned-path equality alone missed
			// hardlinks; refuseOutputAlias adds the os.SameFile backstop and
			// fires before any input loads.
			refuseOutputAlias("blind-render", "-blind-blockmap-out", *blindBlockmapOut,
				[][2]string{{"-artifacts", *artifactsPath}, {"-report", *reportPath}})
		}
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: blind-render: load artifacts: %v", err)
		}
		if len(arts) == 0 {
			log.Fatalf("llm-bench: blind-render: no artifacts in %q", *artifactsPath)
		}
		arts, err = filterArtifactsByModel(arts, *modelFilter)
		if err != nil {
			log.Fatalf("llm-bench: blind-render: %v", err)
		}
		worksheet, blockmap, err := renderBlindWorksheet(arts, *blindDups, "")
		if err != nil {
			log.Fatalf("llm-bench: blind-render: %v", err)
		}
		// The block map is the only opaque-id join; a promptless render that
		// discards it produces an un-ingestable worksheet, and the flag on a
		// promptless-free render is operator confusion — both loud.
		switch {
		case blockmap != nil && strings.TrimSpace(*blindBlockmapOut) == "":
			log.Fatalf("llm-bench: blind-render: promptless blocks rendered; -blind-blockmap-out is required (the block map is the only join from opaque BLOCK ids to artifact hashes)")
		case blockmap == nil && strings.TrimSpace(*blindBlockmapOut) != "":
			log.Fatalf("llm-bench: blind-render: no promptless blocks rendered; drop -blind-blockmap-out")
		}
		if blockmap != nil {
			// Aliasing was refused up front, before any input loaded.
			if err := writeBlindBlockmap(*blindBlockmapOut, *blockmap); err != nil {
				log.Fatalf("llm-bench: blind-render: %v", err)
			}
			fmt.Fprintf(os.Stderr, "llm-bench: blind block map (%d block(s)) written to %s\n", len(blockmap.Blocks), *blindBlockmapOut)
		}
		if *reportPath == "" {
			fmt.Print(worksheet)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(worksheet), 0o600); err != nil {
			log.Fatalf("llm-bench: write worksheet: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: blind worksheet written to %s\n", *reportPath)
		return
	}

	if *fcSidemapGenerate {
		if strings.TrimSpace(*fcSidemapOut) == "" {
			log.Fatalf("llm-bench: -fc-sidemap-generate requires -fc-sidemap-out")
		}
		refuseOutputAlias("fc-sidemap-generate", "-fc-sidemap-out", *fcSidemapOut,
			[][2]string{{"-artifacts", *artifactsPath}})
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: fc-sidemap-generate: load artifacts: %v", err)
		}
		sidemap, err := generateFCSidemap(arts)
		if err != nil {
			log.Fatalf("llm-bench: fc-sidemap-generate: %v", err)
		}
		digest, err := writeFCSidemap(*fcSidemapOut, sidemap)
		if err != nil {
			log.Fatalf("llm-bench: fc-sidemap-generate: %v", err)
		}
		fmt.Printf("fc-sidemap sha256:%s\n", digest)
		fmt.Fprintf(os.Stderr, "llm-bench: fc-sidemap-generate wrote %d pair assignment(s) to %s\n", len(sidemap.Pairs), *fcSidemapOut)
		fmt.Fprintln(os.Stderr, "llm-bench: commit this digest before labeling; reveal the map after preferences are ingested")
		return
	}

	if *fcRender {
		if strings.TrimSpace(*fcSidemapPath) == "" {
			log.Fatalf("llm-bench: -fc-render requires -fc-sidemap (generate one with -fc-sidemap-generate)")
		}
		refuseReportAlias("fc-render", *reportPath, [][2]string{
			{"-artifacts", *artifactsPath}, {"-fc-sidemap", *fcSidemapPath}})
		sidemap := mustLoadVerifiedFCSidemap("fc-render", *fcSidemapPath, *fcSidemapDigest)
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: fc-render: load artifacts: %v", err)
		}
		arts, err = filterArtifactsByModel(arts, *modelFilter)
		if err != nil {
			log.Fatalf("llm-bench: fc-render: %v", err)
		}
		worksheet, err := renderForcedChoiceWorksheet(arts, sidemap)
		if err != nil {
			log.Fatalf("llm-bench: fc-render: %v", err)
		}
		if *reportPath == "" {
			fmt.Print(worksheet)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(worksheet), 0o600); err != nil {
			log.Fatalf("llm-bench: write worksheet: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: forced-choice worksheet written to %s\n", *reportPath)
		return
	}

	if *fcIngest {
		if strings.TrimSpace(*worksheetPath) == "" {
			log.Fatalf("llm-bench: -fc-ingest requires -worksheet")
		}
		if strings.TrimSpace(*fcOut) == "" {
			log.Fatalf("llm-bench: -fc-ingest requires -fc-out")
		}
		if strings.TrimSpace(*fcSidemapPath) == "" {
			log.Fatalf("llm-bench: -fc-ingest requires -fc-sidemap (the sealed map the worksheet was rendered under)")
		}
		refuseOutputAlias("fc-ingest", "-fc-out", *fcOut,
			[][2]string{{"-artifacts", *artifactsPath}, {"-worksheet", *worksheetPath}, {"-fc-sidemap", *fcSidemapPath}})
		sidemap := mustLoadVerifiedFCSidemap("fc-ingest", *fcSidemapPath, *fcSidemapDigest)
		worksheet, err := os.ReadFile(*worksheetPath)
		if err != nil {
			log.Fatalf("llm-bench: fc-ingest: read worksheet: %v", err)
		}
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: fc-ingest: load artifacts: %v", err)
		}
		rows, skipped, err := ingestForcedChoiceWorksheet(string(worksheet), arts, strings.TrimSpace(*labelerName), sidemap, *fcRequireComplete)
		if err != nil {
			log.Fatalf("llm-bench: fc-ingest: %v", err)
		}
		if len(rows) == 0 {
			log.Fatalf("llm-bench: fc-ingest: worksheet has no filled prefer: lines (%d skipped); fill them before ingesting", skipped)
		}
		if err := writeJSONLRows(*fcOut, "fc-preferences", rows); err != nil {
			log.Fatalf("llm-bench: fc-ingest: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: fc-ingest wrote %d preference row(s) to %s (%d block(s) skipped)\n", len(rows), *fcOut, skipped)
		return
	}

	if *adjudicateRender {
		refuseReportAlias("adjudicate-render", *reportPath, [][2]string{
			{"-artifacts", *artifactsPath}, {"-labels", *labelsPath}})
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-render: load artifacts: %v", err)
		}
		arts, err = filterArtifactsByModel(arts, *modelFilter)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-render: %v", err)
		}
		labels, err := loadLabels(*labelsPath)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-render: load labels: %v", err)
		}
		worksheet, err := renderAdjudicationWorksheet(arts, labels)
		if err != nil {
			// The labels path names WHICH file lacked grounding-check flags.
			log.Fatalf("llm-bench: adjudicate-render: %v (labels: %s)", err, *labelsPath)
		}
		if *reportPath == "" {
			fmt.Print(worksheet)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(worksheet), 0o600); err != nil {
			log.Fatalf("llm-bench: write worksheet: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: adjudication worksheet written to %s\n", *reportPath)
		return
	}

	if *adjudicateIngest {
		if strings.TrimSpace(*worksheetPath) == "" {
			log.Fatalf("llm-bench: -adjudicate-ingest requires -worksheet")
		}
		// Same footgun as -blind-ingest, plus the primary label file: the
		// adjudicated set must not clobber -artifacts or the pre-adjudication
		// -labels (the primary pass is audit evidence for the 3c protocol).
		if !providedFlags["labels-out"] || strings.TrimSpace(*labelsOut) == "" {
			log.Fatalf("llm-bench: -adjudicate-ingest requires an explicit -labels-out that differs from -artifacts and -labels")
		}
		refuseOutputAlias("adjudicate-ingest", "-labels-out", *labelsOut,
			[][2]string{{"-artifacts", *artifactsPath}, {"-labels", *labelsPath}, {"-worksheet", *worksheetPath}})
		worksheet, err := os.ReadFile(*worksheetPath)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-ingest: read worksheet: %v", err)
		}
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-ingest: load artifacts: %v", err)
		}
		labels, err := loadLabels(*labelsPath)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-ingest: load labels: %v", err)
		}
		updated, err := ingestAdjudicationWorksheet(string(worksheet), arts, labels)
		if err != nil {
			log.Fatalf("llm-bench: adjudicate-ingest: %v", err)
		}
		if err := writeLabelsJSONL(*labelsOut, updated); err != nil {
			log.Fatalf("llm-bench: adjudicate-ingest: write labels: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: adjudicate-ingest wrote %d label(s) to %s\n", len(updated), *labelsOut)
		return
	}

	if *blindIngest {
		if strings.TrimSpace(*worksheetPath) == "" {
			log.Fatalf("llm-bench: -blind-ingest requires -worksheet")
		}
		// -labels-out defaults to the calibrate-capture artifacts path; in
		// blind-ingest that default is always wrong, even when -artifacts points
		// somewhere else. Require an explicit -labels-out distinct from
		// -artifacts. Compare cleaned paths so spellings like "./artifacts.jsonl"
		// do not slip past.
		if !providedFlags["labels-out"] || strings.TrimSpace(*labelsOut) == "" || filepath.Clean(*labelsOut) == filepath.Clean(*artifactsPath) {
			log.Fatalf("llm-bench: -blind-ingest requires an explicit -labels-out that differs from -artifacts (its default points at artifacts.jsonl and would overwrite the artifacts)")
		}
		// Cleaned-string equality misses case-insensitive filesystems (APFS),
		// symlinks, and hardlinks. If both paths resolve to the same existing
		// file, refuse for the same reason.
		if outInfo, statErr := os.Stat(*labelsOut); statErr == nil {
			if artInfo, statErr := os.Stat(*artifactsPath); statErr == nil && os.SameFile(outInfo, artInfo) {
				log.Fatalf("llm-bench: -blind-ingest: -labels-out resolves to the same file as -artifacts; choose a different output path")
			}
		}
		// The hand-rolled -labels-out vs -artifacts checks above keep their
		// pinned error strings; refuseOutputAlias covers the remaining pairs.
		// The loaded block map is an input like the worksheet: either output
		// aliasing it would truncate the only opaque-id join for this run.
		labelsOutInputs := [][2]string{{"-worksheet", *worksheetPath}}
		dupsOutInputs := [][2]string{{"-artifacts", *artifactsPath}, {"-labels-out", *labelsOut}, {"-worksheet", *worksheetPath}}
		if strings.TrimSpace(*blindBlockmapPath) != "" {
			labelsOutInputs = append(labelsOutInputs, [2]string{"-blind-blockmap", *blindBlockmapPath})
			dupsOutInputs = append(dupsOutInputs, [2]string{"-blind-blockmap", *blindBlockmapPath})
		}
		refuseOutputAlias("blind-ingest", "-labels-out", *labelsOut, labelsOutInputs)
		if strings.TrimSpace(*dupsOut) != "" {
			refuseOutputAlias("blind-ingest", "-dups-out", *dupsOut, dupsOutInputs)
		}
		worksheet, err := os.ReadFile(*worksheetPath)
		if err != nil {
			log.Fatalf("llm-bench: blind-ingest: read worksheet: %v", err)
		}
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: blind-ingest: load artifacts: %v", err)
		}
		if len(arts) == 0 {
			log.Fatalf("llm-bench: blind-ingest: no artifacts in %q", *artifactsPath)
		}
		var blockmap *blindBlockmapFile
		if strings.TrimSpace(*blindBlockmapPath) != "" {
			bm, err := loadBlindBlockmap(*blindBlockmapPath)
			if err != nil {
				log.Fatalf("llm-bench: blind-ingest: %v", err)
			}
			blockmap = &bm
		}
		res, err := ingestBlindWorksheet(string(worksheet), arts, strings.TrimSpace(*labelerName), blockmap)
		if err != nil {
			log.Fatalf("llm-bench: blind-ingest: %v", err)
		}
		// An entirely unscored worksheet is a labeler error (nothing filled in);
		// erroring also keeps an existing -labels-out from being truncated to an
		// empty file.
		if len(res.Labels) == 0 {
			log.Fatalf("llm-bench: blind-ingest: worksheet has no scored blocks (%d unscored); fill the score: lines before ingesting", res.Skipped)
		}
		if err := writeLabelsJSONL(*labelsOut, res.Labels); err != nil {
			log.Fatalf("llm-bench: blind-ingest: write labels: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest wrote %d label(s) to %s (%d block(s) unscored/skipped)\n", len(res.Labels), *labelsOut, res.Skipped)
		if len(res.FlaggedHashes) > 0 {
			fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest: %d block(s) flagged grounding-check (pending adjudication):\n", len(res.FlaggedHashes))
			for _, hash := range res.FlaggedHashes {
				fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest:   %s\n", hash)
			}
		}
		if len(res.DupPairs) > 0 || len(res.DupUnpaired) > 0 {
			agree, disagree, meanAbs := blindDupSummary(res.DupPairs)
			fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest intra-rater: %d dup pair(s), agree=%d disagree=%d mean|delta|=%.3f, unpaired=%d\n",
				len(res.DupPairs), agree, disagree, meanAbs, len(res.DupUnpaired))
			for _, hash := range res.DupUnpaired {
				fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest:   unpaired dup %s\n", hash)
			}
		}
		if strings.TrimSpace(*dupsOut) != "" {
			if err := writeJSONLRows(*dupsOut, "dups", res.DupPairs); err != nil {
				log.Fatalf("llm-bench: blind-ingest: %v", err)
			}
			fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest wrote %d dup pair(s) to %s\n", len(res.DupPairs), *dupsOut)
		}
		return
	}

	if *fimLatency {
		if *fimCases == "" || *modelsArg == "" {
			log.Fatalf("llm-bench: -fim-latency requires -fim-cases and -models")
		}
		casePaths, err := filepath.Glob(*fimCases)
		if err != nil {
			log.Fatalf("llm-bench: glob %q: %v", *fimCases, err)
		}
		if len(casePaths) == 0 {
			log.Fatalf("llm-bench: no FIM cases matched %q", *fimCases)
		}
		refuseReportAlias("fim-latency", *reportPath, pathListInputs("-fim-cases", casePaths))
		cases, err := loadFIMCases(casePaths)
		if err != nil {
			log.Fatalf("llm-bench: load FIM cases: %v", err)
		}
		targets, err := parseModelTargets(*modelsArg)
		if err != nil {
			log.Fatalf("llm-bench: parse models: %v", err)
		}
		if err := validateFIMTargets(targets); err != nil {
			log.Fatalf("llm-bench: %v", err)
		}
		client, err := newOllamaClient(*ollamaURL, ollama.WithTimeout(*timeout))
		if err != nil {
			log.Fatalf("llm-bench: client: %v", err)
		}
		models := make([]string, 0, len(targets))
		for _, target := range targets {
			models = append(models, target.Display)
		}
		results := runFIMLatency(ctx, fimRunOptions{
			Generator:  ollamaFIMGenerator{client: client},
			Models:     models,
			Cases:      cases,
			NumPredict: *fimNumPredict,
			Warmup:     *fimWarmup,
		})
		report := formatFIMReport(models, results)
		if *reportPath == "" {
			fmt.Print(report)
			return
		}
		if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
			log.Fatalf("llm-bench: write report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: FIM latency report written to %s\n", *reportPath)
		return
	}

	if *tracesGlob == "" || *modelsArg == "" {
		flag.Usage()
		os.Exit(2)
	}

	tracePaths, err := filepath.Glob(*tracesGlob)
	if err != nil {
		log.Fatalf("llm-bench: glob %q: %v", *tracesGlob, err)
	}
	if len(tracePaths) == 0 {
		log.Fatalf("llm-bench: no traces matched %q", *tracesGlob)
	}

	runInputs := append(pathListInputs("-traces", tracePaths),
		[2]string{"-corpus-manifest", *corpusManifestPath},
		// Round-2 review P2: the report write could truncate the judge cache
		// SQLite file while it is OPEN — the cache belongs in the guard set.
		[2]string{"-judge-cache", *judgeCachePath})
	if *scorerName == "manual" {
		runInputs = append(runInputs, [2]string{"-labels", *labelsPath})
	}
	refuseReportAlias("run", *reportPath, runInputs)

	targets, err := parseModelTargets(*modelsArg)
	if err != nil {
		log.Fatalf("llm-bench: parse models: %v", err)
	}

	traces, err := loadTraces(tracePaths)
	if err != nil {
		log.Fatalf("llm-bench: load traces: %v", err)
	}

	var corpusData *corpusReportData
	if strings.TrimSpace(*corpusManifestPath) != "" {
		manifest, err := loadManifest(*corpusManifestPath)
		if err != nil {
			log.Fatalf("llm-bench: load corpus manifest: %v", err)
		}
		parts, err := parseCorpusPartitions(*corpusPartitions)
		if err != nil {
			log.Fatalf("llm-bench: %v", err)
		}
		sel := corpusSelection{Partitions: parts, Categories: splitCommaList(*corpusCategories), Sources: splitCommaList(*corpusSources), OnlyModelEvidence: *corpusOnlyEvidence}
		run, data, missing := buildCorpusRun(manifest, sel, traces)
		for _, id := range missing {
			fmt.Fprintf(os.Stderr, "llm-bench: corpus manifest selects trace %q not present in -traces; skipping\n", id)
		}
		if len(run) == 0 {
			log.Fatalf("llm-bench: corpus selection matched no loaded traces")
		}
		traces = run
		corpusData = data
	}

	traceSetHash := traceSetManifestHash(traces)
	log.Printf("llm-bench: trace set manifest hash %s (n=%d)", traceSetHash, len(traces))

	toolCallExpected := make(map[string]bool, len(traces))
	for _, tr := range traces {
		toolCallExpected[tr.ID] = expectsToolCalls(tr)
	}

	resolvedJudgeModel := strings.TrimSpace(*judgeModel)
	if *scorerName == "llm-judge" && resolvedJudgeModel == "" {
		resolvedJudgeModel = defaultJudgeModelName()
	}
	resolvedJudgeURL := strings.TrimSpace(*judgeOllamaURL)
	if *scorerName == "llm-judge" && resolvedJudgeURL == "" {
		resolvedJudgeURL = *ollamaURL
	}

	// Typed-nil interface trap: openJudgeCache may return
	// (*sqliteJudgeCache)(nil) for an empty path, which would still satisfy
	// the judgeCacheStore interface as a non-nil interface value. Only
	// assign the interface variable when the concrete pointer is genuinely
	// non-nil.
	var cacheStore judgeCacheStore
	if c, err := openJudgeCache(*judgeCachePath); err != nil {
		fmt.Fprintf(os.Stderr, "llm-bench: judge cache disabled (%s)\n", redactErrorMessage(err.Error()))
	} else if c != nil {
		cacheStore = c
		defer func() { _ = c.Close() }()
	}

	jt := resolveJudgeTransportConfig(*judgeTransport, *judgeBaseURL, *judgeAPIKey, os.Getenv)
	scorer, err := newScorer(ctx, *scorerName, scorerOptions{
		ollamaURL:        resolvedJudgeURL,
		judgeModel:       resolvedJudgeModel,
		judgeTimeout:     *judgeTimeout,
		judgeCache:       cacheStore,
		judgeTransport:   jt.transport,
		judgeBaseURL:     jt.baseURL,
		judgeAPIKey:      jt.apiKey,
		manualLabelsPath: *labelsPath,
	})
	if err != nil {
		log.Fatalf("llm-bench: scorer: %v", err)
	}
	judgeProviderName := defaultBenchProvider
	if js, ok := scorer.(*LLMJudgeScorer); ok {
		judgeProviderName = js.JudgeProvider
	}

	runner := &Runner{
		OllamaURL:           *ollamaURL,
		OpenAICompatBaseURL: ct.baseURL,
		OpenAICompatAPIKey:  ct.apiKey,
		Timeout:             *timeout,
		Scorer:              scorer,
	}

	results, err := runner.RunAll(ctx, targets, traces)
	if err != nil {
		log.Fatalf("llm-bench: run: %v", err)
	}
	candidateProviders := make(map[string]string, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.CandidateProvider) != "" {
			candidateProviders[result.Model] = result.CandidateProvider
		}
	}

	var judgeCacheHits, judgeCacheMisses int64
	if cacheStore != nil {
		judgeCacheHits, judgeCacheMisses = cacheStore.Stats()
	}

	modelNames := make([]string, 0, len(targets))
	for _, target := range targets {
		modelNames = append(modelNames, target.Display)
	}

	report := formatReport(modelNames, results, reportOptions{
		Scorer:               *scorerName,
		JudgeModel:           resolvedJudgeModel,
		JudgeProvider:        judgeProviderName,
		JudgeCacheHits:       judgeCacheHits,
		JudgeCacheMisses:     judgeCacheMisses,
		TraceSetManifestHash: traceSetHash,
		CandidateProviders:   candidateProviders,
		Corpus:               corpusData,
		ToolCallExpected:     toolCallExpected,
	})

	if *reportPath == "" {
		fmt.Print(report)
		return
	}

	if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
		log.Fatalf("llm-bench: write report: %v", err)
	}
	fmt.Fprintf(os.Stderr, "llm-bench: report written to %s\n", *reportPath)
}

// mustLoadVerifiedFCSidemap loads the sealed side map for a worksheet mode
// and, when the operator supplied -fc-sidemap-digest, verifies the file
// against the committed digest — the same gate -assembly-report applies, so
// a swapped sidemap cannot slip into render or ingest either. Fatal on any
// failure. Returns the whole map: render and ingest resolve assignments AND
// arm hashes through it (W5 — worksheet headers are hash-free).
func mustLoadVerifiedFCSidemap(mode, path, committedDigest string) fcSidemapFile {
	sidemap, _, digest, err := loadFCSidemap(path)
	if err != nil {
		log.Fatalf("llm-bench: %s: %v", mode, err)
	}
	if strings.TrimSpace(committedDigest) != "" {
		if err := verifyFCSidemapDigest(digest, committedDigest); err != nil {
			log.Fatalf("llm-bench: %s: %v", mode, err)
		}
	}
	return sidemap
}

// refuseOutputAlias fatals when an output path aliases an input file (or a
// sibling output of the same invocation), via cleaned-path equality or the
// os.SameFile backstop (case-insensitive filesystems, symlinks, hardlinks).
// Mirrors the hardened -blind-ingest -labels-out guard across EVERY output
// flag (external PR review P1: -report could clobber the artifacts JSONL);
// inputs are ordered (flag, path) pairs so the failure is deterministic.
// Blank input paths (unset optional flags) are skipped so call sites can
// list every mode input unconditionally.
func refuseOutputAlias(mode, outFlag, outPath string, inputs [][2]string) {
	for _, in := range inputs {
		inFlag, inPath := in[0], in[1]
		if strings.TrimSpace(inPath) == "" {
			continue
		}
		if filepath.Clean(outPath) == filepath.Clean(inPath) {
			log.Fatalf("llm-bench: %s: %s must differ from %s (it would overwrite the input)", mode, outFlag, inFlag)
		}
		if outInfo, err := os.Stat(outPath); err == nil {
			if inInfo, err := os.Stat(inPath); err == nil && os.SameFile(outInfo, inInfo) {
				log.Fatalf("llm-bench: %s: %s resolves to the same file as %s; choose a different output path", mode, outFlag, inFlag)
			}
		}
	}
}

// refuseReportAlias applies refuseOutputAlias to the -report flag when it is
// set (an empty -report writes to stdout and can alias nothing).
func refuseReportAlias(mode, reportPath string, inputs [][2]string) {
	if strings.TrimSpace(reportPath) == "" {
		return
	}
	refuseOutputAlias(mode, "-report", reportPath, inputs)
}

// pathListInputs expands a list of concrete paths (globbed traces or FIM
// cases) into refuseOutputAlias (flag, path) pairs under one flag name.
func pathListInputs(flagName string, paths []string) [][2]string {
	out := make([][2]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, [2]string{flagName, p})
	}
	return out
}

func defaultJudgeModelName() string {
	cfg, err := config.Default()
	if err == nil {
		if m := cfg.RoleConfig("judge"); m != nil && strings.TrimSpace(m.Name) != "" {
			return strings.TrimSpace(m.Name)
		}
	}
	return fallbackJudgeModel
}

func calibrationExitCode(verdict string) int {
	if verdict == "PASS" {
		return 0
	}
	return 3
}

// defaultJudgeCachePath returns the platform-appropriate location for the
// judge response cache. Falls back to "" (cache disabled) if the user
// cache dir cannot be resolved.
func defaultJudgeCachePath() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "go-llm", "judge-cache.db")
}

// validateCaptureSampleAndLimit rejects the case where both flags are
// non-zero. Spec §4.4 requires they be mutually exclusive: -capture-limit
// is the simple "first N" path; -capture-sample is the stratified path.
func validateCaptureSampleAndLimit(limit int, sample string) error {
	if limit > 0 && strings.TrimSpace(sample) != "" {
		return fmt.Errorf("-capture-limit and -capture-sample are mutually exclusive")
	}
	return nil
}

// resolveCaptureSample parses the -capture-sample flag into a
// *captureSampleSpec. Returns (nil, nil) when spec is empty (no
// sampling requested). Any parse error is returned as a non-nil error.
func resolveCaptureSample(spec string) (*captureSampleSpec, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	parsed, err := parseCaptureSample(spec)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func validateFIMTargets(targets []ModelTarget) error {
	for _, target := range targets {
		if target.Provider != defaultBenchProvider {
			return fmt.Errorf("-fim-latency supports only ollama candidates; got %q", target.Display)
		}
	}
	return nil
}

// judgeAPIKeyEnvVar is the environment variable consulted for the openai-compat
// judge Bearer token when -judge-api-key is empty. Preferring the env var keeps
// the secret out of shell history and process listings.
const judgeAPIKeyEnvVar = "LLM_BENCH_JUDGE_API_KEY"

// judgeTransportConfig is the resolved judge transport selection fed into
// scorerOptions.
type judgeTransportConfig struct {
	transport string
	baseURL   string
	apiKey    string
}

// resolveJudgeTransportConfig applies flag/env precedence for the judge
// transport. The API key flag takes precedence; when empty it falls back to
// LLM_BENCH_JUDGE_API_KEY via lookupEnv (injected for tests). transport and
// baseURL are trimmed and passed through unchanged — newJudgeTransport
// validates them per backend.
func resolveJudgeTransportConfig(transport, baseURL, apiKey string, lookupEnv func(string) string) judgeTransportConfig {
	cfg := judgeTransportConfig{
		transport: strings.TrimSpace(transport),
		baseURL:   strings.TrimSpace(baseURL),
		apiKey:    strings.TrimSpace(apiKey),
	}
	if cfg.apiKey == "" {
		cfg.apiKey = strings.TrimSpace(lookupEnv(judgeAPIKeyEnvVar))
	}
	return cfg
}

// candidateAPIKeyEnvVar is the environment variable consulted for
// openai-compat candidate Bearer tokens when -candidate-api-key is empty.
const candidateAPIKeyEnvVar = "LLM_BENCH_CANDIDATE_API_KEY"

// candidateTransportConfig is the resolved OpenAI-compatible candidate
// endpoint configuration. Ollama candidates ignore these values.
type candidateTransportConfig struct {
	baseURL string
	apiKey  string
}

// resolveCandidateTransportConfig applies flag/env precedence for candidate
// OpenAI-compatible endpoints. The API key flag takes precedence; when empty
// it falls back to LLM_BENCH_CANDIDATE_API_KEY via lookupEnv.
func resolveCandidateTransportConfig(baseURL, apiKey string, lookupEnv func(string) string) candidateTransportConfig {
	cfg := candidateTransportConfig{
		baseURL: strings.TrimSpace(baseURL),
		apiKey:  strings.TrimSpace(apiKey),
	}
	if cfg.apiKey == "" {
		cfg.apiKey = strings.TrimSpace(lookupEnv(candidateAPIKeyEnvVar))
	}
	return cfg
}

// offlineCorpusFilter builds the corpus filter for the offline scorers from the
// corpus flags. Returns nil (no filtering) when manifestPath is empty.
func offlineCorpusFilter(manifestPath, partitions, categories, sources string, onlyEvidence bool) (*corpusFilter, error) {
	if strings.TrimSpace(manifestPath) == "" {
		return nil, nil
	}
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("load corpus manifest: %w", err)
	}
	parts, err := parseCorpusPartitions(partitions)
	if err != nil {
		return nil, fmt.Errorf("corpus partitions: %w", err)
	}
	return &corpusFilter{
		Manifest:  manifest,
		Selection: corpusSelection{Partitions: parts, Categories: splitCommaList(categories), Sources: splitCommaList(sources), OnlyModelEvidence: onlyEvidence},
	}, nil
}

// resolveToolSchemaSource picks a tool-schema source from CLI flags.
// Returns nil if neither flag is set; an error if both are.
func resolveToolSchemaSource(stdioCmd, httpURL string) (toolSchemaSource, error) {
	stdioCmd = strings.TrimSpace(stdioCmd)
	httpURL = strings.TrimSpace(httpURL)
	if stdioCmd != "" && httpURL != "" {
		return nil, fmt.Errorf("-mcp-stdio-command and -mcp-url are mutually exclusive")
	}
	if stdioCmd != "" {
		return newMCPToolSchemaSourceStdio(stdioCmd)
	}
	if httpURL != "" {
		return newMCPToolSchemaSourceHTTP(httpURL)
	}
	return nil, nil
}
