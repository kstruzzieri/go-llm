package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner implements calibrationRunner with canned Results so we don't
// need a live Ollama for the capture test.
type fakeRunner struct {
	results []Result
}

func (f *fakeRunner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	return f.results, nil
}

func TestRunCalibrateCapture_WritesOneArtifactPerResult(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "artifacts.jsonl")
	runner := &fakeRunner{results: []Result{
		{Model: "ollama/cand", TraceID: "t1", Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
			{Role: "assistant", Content: "answer one"},
		}},
		{Model: "ollama/cand", TraceID: "t2", Transcript: []Turn{
			{Role: "assistant", Content: "answer two"},
		}},
	}}
	traces := []Trace{
		{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "u1"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
		{ID: "t2", System: "s", Turns: []Turn{{Role: "user", Content: "u2"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "ollama/cand", Provider: "ollama", Model: "cand"}}

	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}

	// Read back artifacts.jsonl: expect 2 records, each with non-empty
	// ArtifactHash, no ExpectedAnswerQuality field set (labels are
	// separate), and ActualFinalAnswer matching the transcript.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var artifacts []Artifact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var a Artifact
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil {
			t.Fatalf("decode: %v", err)
		}
		artifacts = append(artifacts, a)
	}
	if len(artifacts) != 2 {
		t.Fatalf("got %d artifacts; want 2", len(artifacts))
	}
	for i, a := range artifacts {
		if a.ArtifactHash == "" {
			t.Fatalf("artifact %d missing hash", i)
		}
		if a.CandidateModel != "ollama/cand" {
			t.Fatalf("artifact %d candidate = %q", i, a.CandidateModel)
		}
	}
	if artifacts[0].ActualFinalAnswer != "answer one" {
		t.Fatalf("artifact[0].ActualFinalAnswer = %q", artifacts[0].ActualFinalAnswer)
	}
}

func TestArtifactHash_DeterministicAcrossInvocations(t *testing.T) {
	a := Artifact{
		TraceID: "t1", CandidateModel: "ollama/cand",
		ActualFinalAnswer: "the answer is 4",
		ActualToolCalls:   []string{"read_file"},
		ActualTranscript:  []Turn{{Role: "assistant", Content: "the answer is 4"}},
	}
	b := a
	if artifactHash(a) != artifactHash(b) {
		t.Fatalf("artifactHash not deterministic for identical Artifact")
	}
}

func TestArtifactHash_SensitiveToFinalAnswer(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualFinalAnswer = "v1"
	h1 := artifactHash(base)
	base.ActualFinalAnswer = "v2"
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash not sensitive to ActualFinalAnswer")
	}
}

func TestArtifactHash_SensitiveToToolCallSequence(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualToolCalls = []string{"a", "b"}
	h1 := artifactHash(base)
	base.ActualToolCalls = []string{"b", "a"}
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash should be order-sensitive on ActualToolCalls")
	}
}

func TestArtifactHash_PrefixIsSha256(t *testing.T) {
	h := artifactHash(Artifact{TraceID: "t", CandidateModel: "c"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Fatalf("artifactHash %q does not start with sha256:", h)
	}
	// sha256: + 64 hex chars = 71 total
	if len(h) != 71 {
		t.Fatalf("artifactHash length = %d; want 71", len(h))
	}
}

func TestLoadLabelsAndArtifacts_MatchAndStale(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	arts := filepath.Join(dir, "artifacts.jsonl")
	art := Artifact{TraceID: "t1", CandidateModel: "ollama/c", ActualFinalAnswer: "x"}
	art.ArtifactHash = artifactHash(art)
	if err := writeJSONL(arts, []any{art}); err != nil {
		t.Fatalf("seed arts: %v", err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: art.ArtifactHash, ExpectedAnswerQuality: 1.0, Labeler: "manual"},
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: "sha256:stale", ExpectedAnswerQuality: 0.5, Labeler: "manual"},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	matched, stale, err := loadLabelsMatchedAgainst(labels, arts)
	if err != nil {
		t.Fatalf("loadLabelsMatchedAgainst: %v", err)
	}
	if len(matched) != 1 || matched[0].Label.ArtifactHash != art.ArtifactHash {
		t.Fatalf("matched = %+v; want 1 entry with art.ArtifactHash", matched)
	}
	if len(stale) != 1 || stale[0].ArtifactHash != "sha256:stale" {
		t.Fatalf("stale = %+v; want 1 stale label", stale)
	}
}

// writeJSONL is a tiny test helper local to calibration_test.go.
func writeJSONL(path string, records []any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
