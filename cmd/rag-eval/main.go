package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/kstruzzieri/go-llm/internal/rageval"
)

func main() {
	fixturePath := flag.String("fixtures", "internal/rageval/testdata/fixtures.json", "Path to RAG evaluation fixture JSON")
	outPath := flag.String("out", "internal/rageval/testdata/baseline.json", "Path to write baseline report JSON")
	warmRuns := flag.Int("warm-runs", 3, "Warm retrieval runs per query")
	noLatency := flag.Bool("no-latency", false, "Disable wall-clock latency measurement")
	flag.Parse()

	fixture, err := rageval.LoadFixture(*fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	report, err := rageval.Run(context.Background(), fixture, rageval.RunOptions{
		WarmRuns:       *warmRuns,
		MeasureLatency: !*noLatency,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if err := rageval.WriteReport(*outPath, report); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
