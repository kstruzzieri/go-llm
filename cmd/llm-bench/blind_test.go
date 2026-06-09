package main

import (
	"strings"
	"testing"
)

func TestRenderBlindWorksheet_HidesModelAndShowsHash(t *testing.T) {
	a1 := testCalibrationArtifact("r2c-fabrication-01", "the candidate answer text")
	a1.CandidateModel = "ollama/gemma4:31b"
	a1.ArtifactHash = artifactHash(a1)
	out := renderBlindWorksheet([]Artifact{a1})
	for _, want := range []string{
		"=== ARTIFACT " + a1.ArtifactHash + " ===",
		"trace: r2c-fabrication-01",
		"[rubric]",
		"rubric for r2c-fabrication-01",
		"the candidate answer text",
		"--- fill below",
		"score:",
		"notes:",
		"=== END ===",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("worksheet missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gemma4:31b") {
		t.Errorf("worksheet leaked the model identity:\n%s", out)
	}
}

func TestRenderBlindWorksheet_OrdersByTraceThenHash(t *testing.T) {
	b := testCalibrationArtifact("t2", "b")
	b.ArtifactHash = "sha256:bbbb"
	a := testCalibrationArtifact("t1", "a")
	a.ArtifactHash = "sha256:zzzz"
	c := testCalibrationArtifact("t1", "c")
	c.ArtifactHash = "sha256:aaaa"
	out := renderBlindWorksheet([]Artifact{b, a, c})
	// Expect t1 block(s) before t2; within t1, hash aaaa before zzzz.
	iA := strings.Index(out, "sha256:aaaa")
	iZ := strings.Index(out, "sha256:zzzz")
	iB := strings.Index(out, "sha256:bbbb")
	if !(iA < iZ && iZ < iB) {
		t.Fatalf("order wrong: aaaa@%d zzzz@%d bbbb@%d; want aaaa<zzzz<bbbb", iA, iZ, iB)
	}
}

func TestIngestBlindWorksheet_RoundTripRejoinsModelOnHash(t *testing.T) {
	a1 := testCalibrationArtifact("r2c-fabrication-01", "answer one")
	a1.CandidateModel = "ollama/gemma4:31b"
	a1.ArtifactHash = artifactHash(a1)
	a2 := testCalibrationArtifact("r2c-fabrication-01", "answer two")
	a2.CandidateModel = "ollama/qwen3:8b"
	a2.ArtifactHash = artifactHash(a2)
	arts := []Artifact{a1, a2}

	// Render, then simulate the human filling score:/notes: lines.
	worksheet := renderBlindWorksheet(arts)
	filled := fillScores(worksheet, map[string]string{
		a1.ArtifactHash: "1",
		a2.ArtifactHash: "0.5",
	})

	got, skipped, err := ingestBlindWorksheet(filled, arts, "tester")
	if err != nil {
		t.Fatalf("ingestBlindWorksheet: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d; want 0", skipped)
	}
	byHash := map[string]Label{}
	for _, l := range got {
		byHash[l.ArtifactHash] = l
	}
	if l := byHash[a1.ArtifactHash]; l.ExpectedAnswerQuality != 1.0 || l.CandidateModel != "ollama/gemma4:31b" || l.TraceID != "r2c-fabrication-01" || l.Labeler != "tester" || l.LabeledAt.IsZero() {
		t.Fatalf("a1 label = %+v; want quality 1.0, model gemma4:31b, trace rejoined, labeler tester, labeled_at set", l)
	}
	if l := byHash[a2.ArtifactHash]; l.ExpectedAnswerQuality != 0.5 || l.CandidateModel != "ollama/qwen3:8b" {
		t.Fatalf("a2 label = %+v; want quality 0.5, model qwen3:8b", l)
	}
}

func TestIngestBlindWorksheet_SkipsUnscoredAndRejectsBadScore(t *testing.T) {
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	worksheet := renderBlindWorksheet([]Artifact{a})
	// Unscored (blank) -> skipped.
	if labels, skipped, err := ingestBlindWorksheet(worksheet, []Artifact{a}, "tester"); err != nil || len(labels) != 0 || skipped != 1 {
		t.Fatalf("unscored: labels=%d skipped=%d err=%v; want 0 labels, 1 skipped, no error", len(labels), skipped, err)
	}
	// Out-of-range -> error.
	bad := fillScores(worksheet, map[string]string{a.ArtifactHash: "0.7"})
	if _, _, err := ingestBlindWorksheet(bad, []Artifact{a}, "tester"); err == nil {
		t.Fatalf("ingestBlindWorksheet accepted score 0.7; want a loud range error")
	}
}

func TestIngestBlindWorksheet_RejectsDuplicateHashBlock(t *testing.T) {
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	worksheet := renderBlindWorksheet([]Artifact{a})
	dup := worksheet + "\n" + worksheet
	if _, _, err := ingestBlindWorksheet(dup, []Artifact{a}, "tester"); err == nil {
		t.Fatalf("ingestBlindWorksheet accepted duplicate artifact_hash block; want loud error")
	}
}

// fillScores is a test helper that fills the first blank "score: " line after
// each block's fill marker with the score for that block's artifact hash.
func fillScores(worksheet string, scoreByHash map[string]string) string {
	lines := strings.Split(worksheet, "\n")
	var currentHash string
	afterMarker := false
	for i, line := range lines {
		if strings.HasPrefix(line, "=== ARTIFACT ") {
			currentHash = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "=== ARTIFACT "), " ==="))
			afterMarker = false
			continue
		}
		if strings.HasPrefix(line, blindFillMarker) {
			afterMarker = true
			continue
		}
		if afterMarker && strings.HasPrefix(line, "score:") {
			if s, ok := scoreByHash[currentHash]; ok {
				lines[i] = "score: " + s
			}
			afterMarker = false
		}
	}
	return strings.Join(lines, "\n")
}
