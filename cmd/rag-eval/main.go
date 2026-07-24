package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/kstruzzieri/go-llm/internal/rageval"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("rag-eval: ")
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("rag-eval", flag.ContinueOnError)
	experiment := flags.String("experiment", "baseline", "Experiment to run: baseline or outline")
	fixturePath := flags.String("fixtures", "internal/rageval/testdata/fixtures.json", "Path to baseline fixture JSON")
	outPath := flags.String("out", "internal/rageval/testdata/baseline.json", "Path to write report JSON")
	warmRuns := flags.Int("warm-runs", 0, "Baseline warm runs or outline measured samples per query (default: baseline 3, outline 5)")
	noLatency := flags.Bool("no-latency", false, "Disable baseline wall-clock latency measurement")
	dimensions := flags.Int("dimensions", 768, "Outline embedding dimensions")
	candidateLimit := flags.Int("candidate-m", 50, "Outline candidate limit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	warmRunsSet, outSet := false, false
	flags.Visit(func(f *flag.Flag) {
		warmRunsSet = warmRunsSet || f.Name == "warm-runs"
		outSet = outSet || f.Name == "out"
	})

	switch *experiment {
	case "outline":
		if !outSet {
			return fmt.Errorf("outline experiment requires explicit -out")
		}
		samples := 5
		if warmRunsSet {
			samples = *warmRuns
		}
		report, err := rageval.RunOutlineExperiment(context.Background(), rageval.OutlineOptions{
			Dimensions:     *dimensions,
			Samples:        samples,
			CandidateLimit: *candidateLimit,
		})
		if err != nil {
			return err
		}
		return rageval.WriteOutlineReport(*outPath, report)
	case "baseline":
		baselineWarmRuns := 3
		if warmRunsSet {
			baselineWarmRuns = *warmRuns
		}
		fixture, err := rageval.LoadFixture(*fixturePath)
		if err != nil {
			return err
		}
		report, err := rageval.Run(context.Background(), fixture, rageval.RunOptions{
			WarmRuns:       baselineWarmRuns,
			MeasureLatency: !*noLatency,
		})
		if err != nil {
			return err
		}
		return rageval.WriteReport(*outPath, report)
	default:
		return fmt.Errorf("unknown experiment %q (want baseline or outline)", *experiment)
	}
}
