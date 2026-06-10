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

	calibrateCapture := flag.Bool("calibrate-capture", false, "Phase 1: replay candidates and write frozen artifacts.jsonl")
	calibrate := flag.Bool("calibrate", false, "Phase 2: re-score frozen labeled artifacts with the judge model")
	manualReport := flag.Bool("manual-report", false, "Score frozen labeled artifacts with human labels (manual scorer) and emit a quality baseline report (uses -labels, -artifacts, -report)")
	pairedReport := flag.Bool("paired-report", false, "Emit the paired-label report: paired-complete means, completeness worklist, win/loss/tie matrix, bootstrap delta CIs, resolution diagnostic (uses -labels, -artifacts, -baseline, -report)")
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
	corpusOnlyEvidence := flag.Bool("corpus-only-evidence", false, "Restrict the corpus run to entries flagged allowed_as_model_evidence")
	blindRender := flag.Bool("blind-render", false, "Render a blind labeling worksheet from -artifacts (model identity hidden) to -report")
	blindIngest := flag.Bool("blind-ingest", false, "Parse a filled blind worksheet (-worksheet) into labels.jsonl (-labels-out), rejoining model on artifact_hash from -artifacts")
	worksheetPath := flag.String("worksheet", "", "Path to a filled blind worksheet (required with -blind-ingest)")
	labelerName := flag.String("labeler", "manual", "Labeler name stamped on -blind-ingest output labels")
	reportPath := flag.String("report", "", "Output report path (default: stdout)")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "Per-replay timeout for candidate model calls")
	flag.Parse()

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
	if *fimLatency {
		modes++
	}
	if *blindRender {
		modes++
	}
	if *blindIngest {
		modes++
	}
	if modes > 1 {
		log.Fatalf("llm-bench: -capture, -calibrate-capture, -calibrate, -manual-report, -paired-report, -fim-latency, -blind-render, -blind-ingest are mutually exclusive")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

	if *calibrateCapture {
		if *tracesGlob == "" || *modelsArg == "" {
			log.Fatalf("llm-bench: -calibrate-capture requires -traces and -models")
		}
		tracePaths, err := filepath.Glob(*tracesGlob)
		if err != nil {
			log.Fatalf("llm-bench: glob: %v", err)
		}
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
		if err := runCalibrateCapture(ctx, calibrateCaptureOptions{
			Runner:     runner,
			Targets:    targets,
			Traces:     traces,
			OutputPath: *labelsOut,
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
		filter, err := offlineCorpusFilter(*corpusManifestPath, *corpusPartitions, *corpusCategories, *corpusOnlyEvidence)
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
		filter, err := offlineCorpusFilter(*corpusManifestPath, *corpusPartitions, *corpusCategories, *corpusOnlyEvidence)
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

	if *blindRender {
		arts, err := loadArtifacts(*artifactsPath)
		if err != nil {
			log.Fatalf("llm-bench: blind-render: load artifacts: %v", err)
		}
		if len(arts) == 0 {
			log.Fatalf("llm-bench: blind-render: no artifacts in %q", *artifactsPath)
		}
		worksheet := renderBlindWorksheet(arts)
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

	if *blindIngest {
		if strings.TrimSpace(*worksheetPath) == "" {
			log.Fatalf("llm-bench: -blind-ingest requires -worksheet")
		}
		// -labels-out defaults to the calibrate-capture artifacts path, which is
		// also -artifacts' default; writing labels there would overwrite the
		// artifacts file this mode reads from (the R-D3 rejoin source). Require
		// an explicit -labels-out distinct from -artifacts. Compare cleaned
		// paths so spellings like "./artifacts.jsonl" do not slip past.
		if strings.TrimSpace(*labelsOut) == "" || filepath.Clean(*labelsOut) == filepath.Clean(*artifactsPath) {
			log.Fatalf("llm-bench: -blind-ingest requires an explicit -labels-out that differs from -artifacts (its default points at artifacts.jsonl and would overwrite the artifacts)")
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
		labels, skipped, err := ingestBlindWorksheet(string(worksheet), arts, strings.TrimSpace(*labelerName))
		if err != nil {
			log.Fatalf("llm-bench: blind-ingest: %v", err)
		}
		// An entirely unscored worksheet is a labeler error (nothing filled in);
		// erroring also keeps an existing -labels-out from being truncated to an
		// empty file.
		if len(labels) == 0 {
			log.Fatalf("llm-bench: blind-ingest: worksheet has no scored blocks (%d unscored); fill the score: lines before ingesting", skipped)
		}
		if err := writeLabelsJSONL(*labelsOut, labels); err != nil {
			log.Fatalf("llm-bench: blind-ingest: write labels: %v", err)
		}
		fmt.Fprintf(os.Stderr, "llm-bench: blind-ingest wrote %d label(s) to %s (%d block(s) unscored/skipped)\n", len(labels), *labelsOut, skipped)
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
		sel := corpusSelection{Partitions: parts, Categories: splitCommaList(*corpusCategories), OnlyModelEvidence: *corpusOnlyEvidence}
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
func offlineCorpusFilter(manifestPath, partitions, categories string, onlyEvidence bool) (*corpusFilter, error) {
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
		Selection: corpusSelection{Partitions: parts, Categories: splitCommaList(categories), OnlyModelEvidence: onlyEvidence},
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
