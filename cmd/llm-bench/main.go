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
	tracesGlob := flag.String("traces", "", "Glob pattern for trace JSON files (required)")
	modelsArg := flag.String("models", "", "Comma-separated model selectors (provider/model or bare model name; required)")
	scorerName := flag.String("scorer", "exact-match", "Scoring strategy: exact-match, llm-judge, manual")
	reportPath := flag.String("report", "", "Output report path (default: stdout)")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama base URL")
	timeout := flag.Duration("timeout", 5*time.Minute, "Per-trace timeout")
	flag.Parse()

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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

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

	if err := os.WriteFile(*reportPath, []byte(report), 0o644); err != nil {
		log.Fatalf("llm-bench: write report: %v", err)
	}
	fmt.Fprintf(os.Stderr, "llm-bench: report written to %s\n", *reportPath)
}
