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
	labelsPath := flag.String("labels", filepath.Join("docs", "llm", "calibration", "labels.jsonl"), "Path to labels.jsonl (Phase 2)")
	labelsOut := flag.String("labels-out", filepath.Join("docs", "llm", "calibration", "artifacts.jsonl"), "Output path for -calibrate-capture artifacts.jsonl")
	artifactsPath := flag.String("artifacts", filepath.Join("docs", "llm", "calibration", "artifacts.jsonl"), "Path to artifacts.jsonl (Phase 2)")
	calibrateReportDir := flag.String("calibrate-report-dir", filepath.Join("docs", "llm", "calibration", "reports"), "Directory for calibration reports")
	judgeStabilityRuns := flag.Int("judge-stability-runs", 0, "When >1, run the judge that many times per artifact (cache bypassed) and report spread as a diagnostic")

	tracesGlob := flag.String("traces", "", "Glob pattern for trace JSON files (required for normal runs and -calibrate-capture)")
	modelsArg := flag.String("models", "", "Comma-separated model selectors (provider/model or bare model name; required)")
	scorerName := flag.String("scorer", "exact-match", "Scoring strategy: exact-match, llm-judge, manual")
	judgeModel := flag.String("judge-model", "", "Ollama model used by -scorer llm-judge; default uses models.json role judge or gemma4:31b")
	judgeOllamaURL := flag.String("judge-ollama-url", "", "Ollama base URL for -scorer llm-judge (default: -ollama-url)")
	judgeTimeout := flag.Duration("judge-timeout", 5*time.Minute, "Timeout for each llm-judge scoring request")
	judgeCachePath := flag.String("judge-cache", defaultJudgeCachePath(), "SQLite path for judge response cache; empty disables")
	reportPath := flag.String("report", "", "Output report path (default: stdout)")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "Per-replay timeout for candidate model calls")
	flag.Parse()

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
	if modes > 1 {
		log.Fatalf("llm-bench: -capture, -calibrate-capture, -calibrate are mutually exclusive")
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
			OllamaURL: *ollamaURL,
			Timeout:   *timeout,
			Scorer:    &CaptureScorer{},
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
		scorer, err := newScorer(ctx, "llm-judge", scorerOptions{
			ollamaURL:    judgeURL,
			judgeModel:   judgeName,
			judgeTimeout: *judgeTimeout,
			judgeCache:   cacheStore,
			// Primary calibration run is cached; stability runs flip
			// the flag internally (Task 23).
			bypassCache: false,
		})
		if err != nil {
			log.Fatalf("llm-bench: calibrate: %v", err)
		}
		res, err := runCalibrate(ctx, calibrateOptions{
			LabelsPath:    *labelsPath,
			ArtifactsPath: *artifactsPath,
			Scorer:        scorer,
			JudgeModel:    judgeName,
			ReportDir:     *calibrateReportDir,
			StabilityRuns: *judgeStabilityRuns,
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

	traceSetHash := traceSetManifestHash(traces)
	log.Printf("llm-bench: trace set manifest hash %s (n=%d)", traceSetHash, len(traces))

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

	scorer, err := newScorer(ctx, *scorerName, scorerOptions{
		ollamaURL:    resolvedJudgeURL,
		judgeModel:   resolvedJudgeModel,
		judgeTimeout: *judgeTimeout,
		judgeCache:   cacheStore,
	})
	if err != nil {
		log.Fatalf("llm-bench: scorer: %v", err)
	}

	runner := &Runner{
		OllamaURL: *ollamaURL,
		Timeout:   *timeout,
		Scorer:    scorer,
	}

	results, err := runner.RunAll(ctx, targets, traces)
	if err != nil {
		log.Fatalf("llm-bench: run: %v", err)
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
		JudgeCacheHits:       judgeCacheHits,
		JudgeCacheMisses:     judgeCacheMisses,
		TraceSetManifestHash: traceSetHash,
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
