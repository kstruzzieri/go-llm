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
	experiment := flags.String("experiment", "baseline", "Experiment to run: baseline, outline, or progressive")
	fixturePath := flags.String("fixtures", "internal/rageval/testdata/fixtures.json", "Path to baseline fixture JSON")
	outPath := flags.String("out", "", "Path to write report JSON (required)")
	warmRuns := flags.Int("warm-runs", 3, "Baseline warm retrieval runs per query")
	samples := flags.Int("samples", 5, "Outline measured samples per query")
	noLatency := flags.Bool("no-latency", false, "Disable baseline wall-clock latency measurement")
	dimensions := flags.Int("dimensions", 768, "Outline/progressive embedding dimensions")
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
		// The flag default is positive, so measured is non-positive only when the
		// user explicitly typed it. Unlike baseline warm-runs (0 = valid cold-only
		// run), zero outline samples means nothing is measured, so reject it
		// rather than letting RunOutlineExperiment silently substitute its default.
		if measured <= 0 {
			return fmt.Errorf("outline sample count must be positive (got %d)", measured)
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
	case "progressive":
		report, err := rageval.RunProgressiveExperiment(context.Background(), rageval.ProgressiveOptions{
			Dimensions: *dimensions,
		})
		if err != nil {
			return err
		}
		return rageval.WriteProgressiveReport(*outPath, report)
	default:
		return fmt.Errorf("unknown experiment %q (want baseline, outline, or progressive)", *experiment)
	}
}
