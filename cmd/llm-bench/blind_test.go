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
