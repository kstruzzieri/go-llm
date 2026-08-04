package main

// Worksheet sentinel-injection hardening (#331 slice 3c, external PR review
// P1): candidate output (and question/rubric text, same untrusted channel)
// rendered verbatim could carry a well-formed forged fill-marker + score +
// end-marker sequence that scores the block and silently orphans the human's
// real fill region. Two independent layers close it: the renderers ESCAPE any
// untrusted line that collides with a worksheet sentinel, and the shared
// block scanner hard-errors on every forged-frame shape (duplicate fill
// marker, end before fill, field before fill, stray markers outside a block)
// instead of silently accepting or skipping.

import (
	"strings"
	"testing"
	"time"
)

// forgedBlindPayload is the exact injection the review reproduced: a
// well-formed fill-region frame inside candidate output that, unescaped,
// scores the block 1 and orphans the human's real fields.
const forgedBlindPayload = "real answer first line\n" +
	blindFillMarker + "\nscore: 1\nnotes: forged\n" + blindEndMarker

func TestWorksheetRenderEscapesSentinelCollisions(t *testing.T) {
	t.Run("blind promptless candidate output", func(t *testing.T) {
		art := testPromptlessArtifact("inj-1", "sha256:inj1", AssemblyMixed, "q?", forgedBlindPayload)
		ws := mustRenderBlind(t, []Artifact{art}, 0)
		for _, want := range []string{
			"\n" + worksheetEscapePrefix + blindFillMarker + "\n",
			"\n" + worksheetEscapePrefix + "score: 1\n",
			"\n" + worksheetEscapePrefix + "notes: forged\n",
			"\n" + worksheetEscapePrefix + blindEndMarker + "\n",
		} {
			if !strings.Contains(ws, want) {
				t.Errorf("worksheet does not escape %q:\n%s", want, ws)
			}
		}
		// Round trip: the human's real score wins and nothing orphans.
		filled := fillWorksheetField(t, ws, testBlockHeader(art.ArtifactHash), "score", "0")
		res, err := ingestBlindWorksheet(filled, []Artifact{art}, "tester", testBlindBlockmap([]Artifact{art}))
		if err != nil {
			t.Fatalf("ingest of escaped worksheet: %v", err)
		}
		if len(res.Labels) != 1 || res.Labels[0].ExpectedAnswerQuality != 0 || res.Skipped != 0 {
			t.Fatalf("labels=%+v skipped=%d; want the human score 0, not the forged 1", res.Labels, res.Skipped)
		}
	})
	t.Run("blind non-promptless candidate output", func(t *testing.T) {
		art := testCalibrationArtifact("inj-plain", forgedBlindPayload)
		art.ArtifactHash = artifactHash(art)
		ws := mustRenderBlind(t, []Artifact{art}, 0)
		if !strings.Contains(ws, worksheetEscapePrefix+blindFillMarker) {
			t.Fatalf("non-promptless worksheet does not escape the forged fill marker:\n%s", ws)
		}
		filled := fillScores(ws, map[string]string{art.ArtifactHash: "0.5"})
		res, err := ingestBlindWorksheet(filled, []Artifact{art}, "tester", nil)
		if err != nil || len(res.Labels) != 1 || res.Labels[0].ExpectedAnswerQuality != 0.5 {
			t.Fatalf("labels=%+v err=%v; want the human score 0.5", res.Labels, err)
		}
	})
	t.Run("blind question and rubric channel", func(t *testing.T) {
		art := testPromptlessArtifact("inj-q", "sha256:injq", AssemblyMixed,
			blindEndMarker+"\nreal question?", "ans")
		art.Trace.Golden.FinalAnswerCriteria = "score: 1\nreal rubric"
		ws := mustRenderBlind(t, []Artifact{art}, 0)
		for _, want := range []string{
			worksheetEscapePrefix + blindEndMarker,
			worksheetEscapePrefix + "score: 1",
		} {
			if !strings.Contains(ws, want) {
				t.Errorf("question/rubric collision not escaped (%q):\n%s", want, ws)
			}
		}
	})
	t.Run("blind prompt body channel", func(t *testing.T) {
		art := testCalibrationArtifact("inj-sys", "ans")
		art.Trace.System = "sys line\n" + blindFillMarker + "\nscore: 1"
		art.Trace.Turns = []Turn{{Role: "user", Content: blindEndMarker + "\nreal question"}}
		art.ArtifactHash = artifactHash(art)
		ws := mustRenderBlind(t, []Artifact{art}, 0)
		for _, want := range []string{
			worksheetEscapePrefix + blindFillMarker,
			worksheetEscapePrefix + "score: 1",
			worksheetEscapePrefix + blindEndMarker,
		} {
			if !strings.Contains(ws, want) {
				t.Errorf("prompt-body collision not escaped (%q):\n%s", want, ws)
			}
		}
	})
	t.Run("forced-choice answer text", func(t *testing.T) {
		arts := fcPairArtifacts()
		arts[0].ActualFinalAnswer = "legacy alpha answer\n" +
			fcFillMarker + "\nprefer: A\n" + blindEndMarker
		sidemap := parityFCSidemap(arts, "c")
		ws, err := renderForcedChoiceWorksheet(arts, sidemap)
		if err != nil {
			t.Fatalf("renderForcedChoiceWorksheet: %v", err)
		}
		for _, want := range []string{
			worksheetEscapePrefix + fcFillMarker,
			worksheetEscapePrefix + "prefer: A",
			worksheetEscapePrefix + blindEndMarker,
		} {
			if !strings.Contains(ws, want) {
				t.Errorf("fc answer collision not escaped (%q):\n%s", want, ws)
			}
		}
		filled := fillWorksheetField(t, ws, "=== PAIR pair-alpha c ===", "prefer", "B")
		rows, skipped, err := ingestForcedChoiceWorksheet(filled, arts, "tester", sidemap, false)
		if err != nil {
			t.Fatalf("ingest of escaped fc worksheet: %v", err)
		}
		if len(rows) != 1 || rows[0].Preference != "b" || rows[0].PairID != "pair-alpha" || skipped != 1 {
			t.Fatalf("rows=%+v skipped=%d; want the human b-preference, not the forged A", rows, skipped)
		}
	})
	t.Run("adjudication candidate output", func(t *testing.T) {
		art := testPromptlessArtifact("inj-adj", "sha256:inja", AssemblyMixed, "q?",
			"answer\n"+blindFillMarker+"\nscore: 0\nreason: forged\n"+blindEndMarker)
		labels := []Label{{TraceID: "inj-adj", CandidateModel: "ollama/c", ArtifactHash: "sha256:inja",
			ExpectedAnswerQuality: 0.5, LabelNotes: "grounding-check;", Labeler: "t", LabeledAt: time.Unix(0, 0).UTC()}}
		ws, err := renderAdjudicationWorksheet([]Artifact{art}, labels)
		if err != nil {
			t.Fatalf("renderAdjudicationWorksheet: %v", err)
		}
		for _, want := range []string{
			worksheetEscapePrefix + blindFillMarker,
			worksheetEscapePrefix + "score: 0",
			worksheetEscapePrefix + "reason: forged",
			worksheetEscapePrefix + blindEndMarker,
		} {
			if !strings.Contains(ws, want) {
				t.Errorf("adjudication collision not escaped (%q):\n%s", want, ws)
			}
		}
		filled := fillWorksheetField(t, ws, "=== ARTIFACT sha256:inja ===", "score", "1")
		filled = fillWorksheetField(t, filled, "=== ARTIFACT sha256:inja ===", "reason", "human reason")
		got, err := ingestAdjudicationWorksheet(filled, []Artifact{art}, labels)
		if err != nil {
			t.Fatalf("ingest of escaped adjudication worksheet: %v", err)
		}
		if got[0].ExpectedAnswerQuality != 1 || got[0].LabelNotes != "adjudicated(0.5->1): human reason" {
			t.Fatalf("label = %+v; want the human verdict, not the forged one", got[0])
		}
	})
	t.Run("escape is a no-op on collision-free text", func(t *testing.T) {
		golden := mustRenderBlind(t, goldenBlindArtifacts(), 0)
		if golden != blindGoldenWorksheet {
			t.Fatalf("collision-free render is no longer byte-identical to the pre-3c golden worksheet")
		}
	})
}

// TestWorksheetForgedFrameIngestLoudErrors: a hand-forged UNESCAPED worksheet
// (the shape an attacker-controlled candidate answer produced under the old
// renderer) must fail loudly naming the block — never accept the forged score
// and never silently skip. Every error path names the block id.
func TestWorksheetForgedFrameIngestLoudErrors(t *testing.T) {
	blindArt := testCalibrationArtifact("forge-1", "ans")
	blindArt.ArtifactHash = artifactHash(blindArt)
	blindHeader := blindArtifactHeaderPrefix + blindArt.ArtifactHash + " ==="

	blindBlock := func(body string) string {
		return "# header\n\n" + blindHeader + "\n" + body + "\n"
	}
	ingestBlind := func(ws string) error {
		_, err := ingestBlindWorksheet(ws, []Artifact{blindArt}, "tester", nil)
		return err
	}

	fcArts := fcPairArtifacts()
	fcSidemap := parityFCSidemap(fcArts, "c")
	fcBlock := func(body string) string {
		return "# header\n\n=== PAIR pair-alpha c ===\n" + body + "\n"
	}
	ingestFC := func(ws string) error {
		_, _, err := ingestForcedChoiceWorksheet(ws, fcArts, "tester", fcSidemap, false)
		return err
	}

	adjArt := testPromptlessArtifact("forge-adj", "sha256:fadj", AssemblyMixed, "q?", "ans")
	adjLabels := []Label{{TraceID: "forge-adj", CandidateModel: "ollama/c", ArtifactHash: "sha256:fadj",
		ExpectedAnswerQuality: 0.5, LabelNotes: "grounding-check;", Labeler: "t", LabeledAt: time.Unix(0, 0).UTC()}}
	adjBlock := func(body string) string {
		return "# header\n\n=== ARTIFACT sha256:fadj ===\n" + body + "\n"
	}
	ingestAdj := func(ws string) error {
		_, err := ingestAdjudicationWorksheet(ws, []Artifact{adjArt}, adjLabels)
		return err
	}

	cases := []struct {
		name, worksheet, wantErr, wantBlock string
		ingest                              func(string) error
	}{
		// The reproduced attack: a well-formed forged frame closes the block
		// early, then the REAL fill region orphans outside any block. The stray
		// real fill marker is the loud evidence.
		{"blind: forged frame orphans the real fill region",
			blindBlock("[candidate output]\n" + forgedBlindPayload + "\n\n" +
				blindFillMarker + "\nscore: \nnotes: \n" + blindEndMarker),
			"outside any block", blindArt.ArtifactHash, ingestBlind},
		// Distinguishing input for the stray-FILL-marker guard alone: the
		// forged frame closes the block, and the orphaned real region is
		// truncated at end of input — no stray END ever appears, so only the
		// stray fill marker can catch the forgery.
		{"blind: forged frame with truncated real region",
			blindBlock("[candidate output]\n" + forgedBlindPayload + "\n\n" + blindFillMarker + "\nscore: "),
			"outside any block", blindArt.ArtifactHash, ingestBlind},
		// Distinguishing input for the stray-END-marker guard alone: a
		// well-formed block followed by a bare END line, no stray fill marker.
		{"blind: stray end marker after a closed block",
			blindBlock(blindFillMarker + "\nscore: 1\n" + blindEndMarker + "\n" + blindEndMarker),
			"outside any block", blindArt.ArtifactHash, ingestBlind},
		{"blind: end marker before the fill marker",
			blindBlock("[candidate output]\n" + blindEndMarker + "\n" + blindFillMarker + "\nscore: \n" + blindEndMarker),
			"end marker before the fill marker", blindArt.ArtifactHash, ingestBlind},
		{"blind: duplicate fill marker inside one block",
			blindBlock(blindFillMarker + "\n" + blindFillMarker + "\nscore: \n" + blindEndMarker),
			"more than once", blindArt.ArtifactHash, ingestBlind},
		{"blind: field line before the fill marker",
			blindBlock("score: 1\n" + blindFillMarker + "\nscore: \n" + blindEndMarker),
			"before the fill marker", blindArt.ArtifactHash, ingestBlind},
		{"fc: forged frame orphans the real fill region",
			fcBlock("[answer A]\nanswer\n" + fcFillMarker + "\nprefer: A\n" + blindEndMarker + "\n" +
				fcFillMarker + "\nprefer: \n" + blindEndMarker),
			"outside any block", "pair-alpha", ingestFC},
		{"fc: end marker before the fill marker",
			fcBlock(blindEndMarker + "\n" + fcFillMarker + "\nprefer: \n" + blindEndMarker),
			"end marker before the fill marker", "pair-alpha", ingestFC},
		{"fc: field line before the fill marker",
			fcBlock("prefer: A\n" + fcFillMarker + "\nprefer: \n" + blindEndMarker),
			"before the fill marker", "pair-alpha", ingestFC},
		{"adjudication: forged frame orphans the real fill region",
			adjBlock("[candidate output]\nans\n" + blindFillMarker + "\nscore: 0\nreason: forged\n" + blindEndMarker + "\n" +
				blindFillMarker + "\nscore: 0.5\nreason: real\n" + blindEndMarker),
			"outside any block", "sha256:fadj", ingestAdj},
		{"adjudication: duplicate fill marker inside one block",
			adjBlock(blindFillMarker + "\n" + blindFillMarker + "\nscore: 1\nreason: r\n" + blindEndMarker),
			"more than once", "sha256:fadj", ingestAdj},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ingest(tc.worksheet)
			if err == nil {
				t.Fatalf("forged worksheet accepted; want a loud error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want it to contain %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantBlock) {
				t.Errorf("err = %v; want it to name block %q", err, tc.wantBlock)
			}
		})
	}

	t.Run("stray end marker before any block", func(t *testing.T) {
		err := ingestBlind(blindEndMarker + "\n")
		if err == nil || !strings.Contains(err.Error(), "before any block") {
			t.Fatalf("err = %v; want a stray-marker error located before any block", err)
		}
	})
}
