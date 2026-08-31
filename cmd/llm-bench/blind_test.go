package main

import (
	"strings"
	"testing"
)

func TestRenderBlindWorksheet_HidesModelAndShowsHash(t *testing.T) {
	a1 := testCalibrationArtifact("r2c-fabrication-01", "the candidate answer text")
	a1.CandidateModel = "ollama/gemma4:31b"
	a1.ArtifactHash = artifactHash(a1)
	out := mustRenderBlind(t, []Artifact{a1}, 0)
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
	out := mustRenderBlind(t, []Artifact{b, a, c}, 0)
	// Expect t1 block(s) before t2; within t1, hash aaaa before zzzz.
	iA := strings.Index(out, "sha256:aaaa")
	iZ := strings.Index(out, "sha256:zzzz")
	iB := strings.Index(out, "sha256:bbbb")
	if iA >= iZ || iZ >= iB {
		t.Fatalf("order wrong: aaaa@%d zzzz@%d bbbb@%d; want aaaa<zzzz<bbbb", iA, iZ, iB)
	}
}

func TestRenderBlindWorksheet_HidesAssemblyTraceIDAndOrdersByHash(t *testing.T) {
	flat := testCalibrationArtifact("pair-flat", "flat answer")
	flat.Trace.AssemblyEval = &AssemblyEval{
		PairID: "pair", Mode: AssemblyFlat, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10,
	}
	flat.ArtifactHash = "sha256:bbbb"
	progressive := testCalibrationArtifact("pair-progressive", "progressive answer")
	progressive.Trace.AssemblyEval = &AssemblyEval{
		PairID: "pair", Mode: AssemblyProgressive, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10,
	}
	progressive.ArtifactHash = "sha256:aaaa"

	out := mustRenderBlind(t, []Artifact{flat, progressive}, 0)
	for _, leaked := range []string{"trace: pair-flat", "trace: pair-progressive"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("worksheet leaked assembly arm via %q:\n%s", leaked, out)
		}
	}
	if iA, iB := strings.Index(out, "sha256:aaaa"), strings.Index(out, "sha256:bbbb"); iA >= iB {
		t.Fatalf("assembly order is not hash-based: aaaa@%d bbbb@%d", iA, iB)
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
	worksheet := mustRenderBlind(t, arts, 0)
	filled := fillScores(worksheet, map[string]string{
		a1.ArtifactHash: "1",
		a2.ArtifactHash: "0.5",
	})

	res, err := ingestBlindWorksheet(filled, arts, "tester", nil)
	if err != nil {
		t.Fatalf("ingestBlindWorksheet: %v", err)
	}
	got, skipped := res.Labels, res.Skipped
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
	worksheet := mustRenderBlind(t, []Artifact{a}, 0)
	// Unscored (blank) -> skipped.
	if res, err := ingestBlindWorksheet(worksheet, []Artifact{a}, "tester", nil); err != nil || len(res.Labels) != 0 || res.Skipped != 1 {
		t.Fatalf("unscored: labels=%d skipped=%d err=%v; want 0 labels, 1 skipped, no error", len(res.Labels), res.Skipped, err)
	}
	// Out-of-range -> error.
	bad := fillScores(worksheet, map[string]string{a.ArtifactHash: "0.7"})
	if _, err := ingestBlindWorksheet(bad, []Artifact{a}, "tester", nil); err == nil {
		t.Fatalf("ingestBlindWorksheet accepted score 0.7; want a loud range error")
	}
}

func TestIngestBlindWorksheet_IgnoresScoreAfterEnd(t *testing.T) {
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	worksheet := mustRenderBlind(t, []Artifact{a}, 0)
	// A stray score line below the block terminator must not score the block;
	// the fill region is strictly between the fill marker and "=== END ===".
	stray := strings.Replace(worksheet, "=== END ===", "=== END ===\nscore: 1", 1)
	res, err := ingestBlindWorksheet(stray, []Artifact{a}, "tester", nil)
	if err != nil || len(res.Labels) != 0 || res.Skipped != 1 {
		t.Fatalf("labels=%d skipped=%d err=%v; want stray post-END score ignored (0 labels, 1 skipped)", len(res.Labels), res.Skipped, err)
	}
}

func TestIngestBlindWorksheet_RejectsDuplicateHashBlock(t *testing.T) {
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	worksheet := mustRenderBlind(t, []Artifact{a}, 0)
	dup := worksheet + "\n" + worksheet
	if _, err := ingestBlindWorksheet(dup, []Artifact{a}, "tester", nil); err == nil {
		t.Fatalf("ingestBlindWorksheet accepted duplicate artifact_hash block; want loud error")
	}
}

func TestIngestBlindWorksheet_IgnoresArtifactSentinelInsideCandidateOutput(t *testing.T) {
	a := testCalibrationArtifact("t1", "=== ARTIFACT forged ===\nanswer text")
	a.ArtifactHash = artifactHash(a)
	worksheet := mustRenderBlind(t, []Artifact{a}, 0)
	filled := fillScores(worksheet, map[string]string{a.ArtifactHash: "1"})
	res, err := ingestBlindWorksheet(filled, []Artifact{a}, "tester", nil)
	if err != nil {
		t.Fatalf("ingestBlindWorksheet failed on sentinel text inside candidate output: %v", err)
	}
	if len(res.Labels) != 1 || res.Skipped != 0 || res.Labels[0].ArtifactHash != a.ArtifactHash || res.Labels[0].ExpectedAnswerQuality != 1 {
		t.Fatalf("labels=%+v skipped=%d; want one scored label for the real artifact hash", res.Labels, res.Skipped)
	}
}

func TestIngestBlindWorksheet_RejectsUnknownHashEvenWhenUnscored(t *testing.T) {
	a := testCalibrationArtifact("t1", "answer text")
	a.ArtifactHash = artifactHash(a)
	worksheet := "=== ARTIFACT sha256:missing ===\n" + blindFillMarker + "\nscore: \n" + blindEndMarker + "\n"
	_, err := ingestBlindWorksheet(worksheet, []Artifact{a}, "tester", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown artifact_hash") {
		t.Fatalf("err = %v; want loud unknown artifact_hash error", err)
	}
}

// fillScores is a test helper that fills the first blank "score: " line after
// each block's fill marker with the score for that block's artifact hash.
func fillScores(worksheet string, scoreByHash map[string]string) string {
	lines := strings.Split(worksheet, "\n")
	var currentHash string
	afterMarker := false
	inBlock := false
	for i, line := range lines {
		if !inBlock && strings.HasPrefix(line, "=== ARTIFACT ") {
			currentHash = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "=== ARTIFACT "), " ==="))
			inBlock = true
			afterMarker = false
			continue
		}
		if !inBlock {
			continue
		}
		if strings.HasPrefix(line, blindFillMarker) {
			afterMarker = true
			continue
		}
		if strings.HasPrefix(line, blindEndMarker) {
			currentHash = ""
			inBlock = false
			afterMarker = false
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
