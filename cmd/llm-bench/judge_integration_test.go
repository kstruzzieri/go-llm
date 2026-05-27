//go:build llmbench_integration

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCalibrateAgainstLiveOllama exercises the full calibrate pipeline against
// a live local Ollama. Skipped unless built with -tags llmbench_integration.
// Set LLM_BENCH_JUDGE_MODEL (default "gemma4:31b") and LLM_BENCH_OLLAMA_URL
// (default http://localhost:11434).
func TestCalibrateAgainstLiveOllama(t *testing.T) {
	judgeModel := os.Getenv("LLM_BENCH_JUDGE_MODEL")
	if judgeModel == "" {
		judgeModel = "gemma4:31b"
	}
	ollamaURL := os.Getenv("LLM_BENCH_OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://localhost:11434"
	}

	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	reports := filepath.Join(dir, "reports")

	// Build a tiny 10-artifact micro-set with hand-crafted easy correctness.
	var artifacts, labelRecs []any
	for i := 0; i < 10; i++ {
		traceID := "easy-" + strings.Repeat("a", i+1)
		trace := Trace{
			ID:     traceID,
			System: "answer arithmetic questions exactly",
			Turns:  []Turn{{Role: "user", Content: "what is 2+2?"}},
			Golden: Golden{FinalAnswerCriteria: "answer exactly 4"},
		}
		a := Artifact{
			TraceID:           traceID,
			CandidateModel:    "ollama/probe-candidate",
			Trace:             trace,
			ActualFinalAnswer: "4",
			ActualToolCalls:   nil,
			ActualTranscript: []Turn{
				{Role: "user", Content: "what is 2+2?"},
				{Role: "assistant", Content: "4"},
			},
			CapturedAt: time.Now().UTC(),
		}
		a.ArtifactHash = artifactHash(a)
		artifacts = append(artifacts, a)
		labelRecs = append(labelRecs, Label{
			TraceID: a.TraceID, CandidateModel: a.CandidateModel, ArtifactHash: a.ArtifactHash,
			ExpectedAnswerQuality: 1.0, Labeler: "integration",
		})
	}
	seed := func(path string, recs []any) {
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		enc := json.NewEncoder(f)
		for _, r := range recs {
			if err := enc.Encode(r); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(arts, artifacts)
	seed(labels, labelRecs)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cache, _ := openJudgeCache(filepath.Join(dir, "judge.db"))
	defer func() { _ = cache.Close() }()

	scorer, err := newScorer(ctx, "llm-judge", scorerOptions{
		ollamaURL:    ollamaURL,
		judgeModel:   judgeModel,
		judgeTimeout: 90 * time.Second,
		judgeCache:   cache,
	})
	if err != nil {
		t.Fatalf("newScorer: %v", err)
	}
	res, err := runCalibrate(ctx, calibrateOptions{
		LabelsPath:    labels,
		ArtifactsPath: arts,
		Scorer:        scorer,
		JudgeModel:    judgeModel,
		ReportDir:     reports,
		MinLabels:     10,
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if res.Verdict == "" {
		t.Fatalf("no verdict produced")
	}
	if res.MatchedCount != 10 {
		t.Fatalf("MatchedCount=%d; want 10", res.MatchedCount)
	}
	t.Logf("integration calibrate: %d/%d (%.0f%%) → %s", res.AgreeCount, res.MatchedCount, res.AgreementRate*100, res.Verdict)
}
