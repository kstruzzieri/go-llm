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
	outPath := flags.String("out", "", "Path to write report JSON (required)")
	warmRuns := flags.Int("warm-runs", 3, "Baseline warm retrieval runs per query")
	samples := flags.Int("samples", 5, "Outline measured samples per query")
	noLatency := flags.Bool("no-latency", false, "Disable baseline wall-clock latency measurement")
	dimensions := flags.Int("dimensions", 768, "Outline embedding dimensions")
	candidateLimit := flags.Int("candidate-m", 50, "Outline candidate limit")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	var warmRunsSet, samplesSet, outSet bool
	flags.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "warm-runs":
			warmRunsSet = true
		case "samples":
			samplesSet = true
		case "out":
			outSet = true
		}
	})
	// An explicit -out is required for every experiment so no invocation can
	// silently overwrite the committed baseline or any other tracked report.
	if !outSet {
		return fmt.Errorf("rag-eval requires an explicit -out path")
	}

	switch *experiment {
	case "outline":
		measured := *samples
		// -warm-runs is accepted as a deprecated alias for -samples in outline
		// mode so pre-split invocations keep working; -samples wins if both set.
		if !samplesSet && warmRunsSet {
			measured = *warmRuns
		}
		report, err := rageval.RunOutlineExperiment(context.Background(), rageval.OutlineOptions{
			Dimensions:     *dimensions,
			Samples:        measured,
			CandidateLimit: *candidateLimit,
		})
		if err != nil {
			return err
		}
		return rageval.WriteOutlineReport(*outPath, report)
	case "baseline":
		fixture, err := rageval.LoadFixture(*fixturePath)
		if err != nil {
			return err
		}
		report, err := rageval.Run(context.Background(), fixture, rageval.RunOptions{
			WarmRuns:       *warmRuns,
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
