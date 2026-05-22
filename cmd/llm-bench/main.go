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
	"syscall"
	"time"
)

func main() {
	capture := flag.Bool("capture", false, "Export persisted conversations as redacted benchmark traces and exit")
	captureDB := flag.String("capture-db", "", "SQLite database path containing conversation tables (required with -capture)")
	captureOut := flag.String("capture-out", filepath.Join("docs", "llm", "traces"), "Directory for captured trace JSON files")
	captureLimit := flag.Int("capture-limit", 0, "Maximum conversations to export in capture mode (0 = all)")
	captureSource := flag.String("capture-source", defaultCaptureSource, "Trace source label for captured conversations")
	captureSystem := flag.String("capture-system", "", "Fallback system prompt when a conversation has no stored system message")
	tracesGlob := flag.String("traces", "", "Glob pattern for trace JSON files (required)")
	modelsArg := flag.String("models", "", "Comma-separated model selectors (provider/model or bare model name; required)")
	scorerName := flag.String("scorer", "exact-match", "Scoring strategy: exact-match, llm-judge, manual")
	reportPath := flag.String("report", "", "Output report path (default: stdout)")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "Per-trace timeout")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *capture {
		result, err := runCapture(ctx, captureOptions{
			DBPath:         *captureDB,
			OutputDir:      *captureOut,
			Limit:          *captureLimit,
			Source:         *captureSource,
			FallbackSystem: *captureSystem,
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

	scorer, err := newScorer(*scorerName)
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

	modelNames := make([]string, 0, len(targets))
	for _, target := range targets {
		modelNames = append(modelNames, target.Display)
	}

	report := formatReport(modelNames, results)

	if *reportPath == "" {
		fmt.Print(report)
		return
	}

	if err := os.WriteFile(*reportPath, []byte(report), 0o600); err != nil {
		log.Fatalf("llm-bench: write report: %v", err)
	}
	fmt.Fprintf(os.Stderr, "llm-bench: report written to %s\n", *reportPath)
}
