package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// blindGoldenWorksheet is the byte-exact pre-Task-6 renderBlindWorksheet
// output for goldenBlindArtifacts(), captured at HEAD 1b1024f before the
// promptless treatment landed. Non-assembly and 3a (flat/progressive) blocks
// must stay byte-identical to it forever.
const blindGoldenWorksheet = "# llm-bench — blind labeling worksheet\n#\n# Score each candidate output from the text alone. Model identity is hidden.\n# For each block, fill the score: line with 0, 0.5, or 1, and notes: optionally.\n# Leave score: blank to skip a block (it will be reported as unscored, not labeled).\n# Then run: llm-bench -blind-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -labels-out <labels.jsonl>\n\n=== ARTIFACT sha256:plain ===\ntrace: t-plain\n\n[prompt]\nsystem for t-plain\n\n<user>\nquestion for t-plain\n\n<assistant>\nassistant turn for t-plain\n\n<user>\nfollow-up for t-plain\n\n[rubric]\nrubric for t-plain\n\n[candidate output]\nplain answer\n\n--- fill below (score: 0 | 0.5 | 1) ---\nscore: \nnotes: \n=== END ===\n\n=== ARTIFACT sha256:flat ===\n[prompt]\nsystem for p3a-flat\n\n<user>\nquestion for p3a-flat\n\n[rubric]\nrubric for p3a-flat\n\n[candidate output]\nflat answer\n\n--- fill below (score: 0 | 0.5 | 1) ---\nscore: \nnotes: \n=== END ===\n\n=== ARTIFACT sha256:prog ===\n[prompt]\nsystem for p3a-progressive\n\n<user>\nquestion for p3a-progressive\n\n[rubric]\nrubric for p3a-progressive\n\n[candidate output]\nprogressive answer\n\n--- fill below (score: 0 | 0.5 | 1) ---\nscore: \nnotes: \n=== END ===\n\n"

// goldenBlindArtifacts is the fixed artifact set behind blindGoldenWorksheet:
// one plain multi-turn artifact plus a 3a flat/progressive pair.
func goldenBlindArtifacts() []Artifact {
	plain := testCalibrationArtifact("t-plain", "plain answer")
	plain.Trace.Turns = append(plain.Trace.Turns,
		Turn{Role: "assistant", Content: "assistant turn for t-plain"},
		Turn{Role: "user", Content: "follow-up for t-plain"})
	plain.ArtifactHash = "sha256:plain"
	flat := testCalibrationArtifact("p3a-flat", "flat answer")
	flat.Trace.AssemblyEval = &AssemblyEval{PairID: "p3a", Mode: AssemblyFlat, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10}
	flat.ArtifactHash = "sha256:flat"
	prog := testCalibrationArtifact("p3a-progressive", "progressive answer")
	prog.Trace.AssemblyEval = &AssemblyEval{PairID: "p3a", Mode: AssemblyProgressive, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10}
	prog.ArtifactHash = "sha256:prog"
	return []Artifact{plain, flat, prog}
}

// testPromptlessArtifact builds one prefilled-mode assembly artifact whose
// prompt (system, earlier turns, tool content) is salted with PROMPT-SENTINEL;
// the final user turn carries the question. hash is assigned verbatim.
func testPromptlessArtifact(traceID, hash string, mode AssemblyMode, question, answer string) Artifact {
	trace := Trace{
		ID:     traceID,
		System: "PROMPT-SENTINEL system for " + traceID,
		Turns: []Turn{
			{Role: "user", Content: "PROMPT-SENTINEL earlier turn for " + traceID},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "c1", Content: "PROMPT-SENTINEL tool content for " + traceID},
			{Role: "user", Content: question},
		},
		Golden: Golden{FinalAnswerCriteria: "rubric for " + traceID},
	}
	ae := &AssemblyEval{PairID: "pair-" + traceID, Mode: mode, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10}
	if mode == AssemblyLegacy || mode == AssemblyMixed {
		ae.Budget = 100
		ae.StateDigest = "sha256:state"
	}
	trace.AssemblyEval = ae
	return Artifact{
		TraceID:           traceID,
		CandidateModel:    "ollama/c",
		ArtifactHash:      hash,
		Trace:             trace,
		ActualFinalAnswer: answer,
		ActualTranscript:  []Turn{{Role: "assistant", Content: answer}},
	}
}

// fillWorksheetField sets the first "<field>:" line inside the fill region of
// the block opened by blockHeader (exact line match). Works for primary, DUP,
// PAIR, and adjudication blocks.
func fillWorksheetField(t *testing.T, worksheet, blockHeader, field, value string) string {
	t.Helper()
	lines := strings.Split(worksheet, "\n")
	inBlock, afterMarker := false, false
	for i, line := range lines {
		switch {
		case line == blockHeader:
			inBlock = true
		case inBlock && strings.HasPrefix(line, "--- fill below"):
			afterMarker = true
		case inBlock && strings.HasPrefix(line, blindEndMarker):
			inBlock, afterMarker = false, false
		case inBlock && afterMarker && strings.HasPrefix(line, field+":"):
			lines[i] = field + ": " + value
			return strings.Join(lines, "\n")
		}
	}
	t.Fatalf("fillWorksheetField: no %q line in block %q", field, blockHeader)
	return ""
}

// testBlindSalt is the fixed opaque-block salt every test render uses, so
// opaque BLOCK ids (and therefore worksheets) are deterministic and headers
// are computable via testBlockHeader.
const testBlindSalt = "0123456789abcdef0123456789abcdef"

func mustRenderBlind(t *testing.T, arts []Artifact, dupN int) string {
	t.Helper()
	out, _, err := renderBlindWorksheet(arts, dupN, testBlindSalt)
	if err != nil {
		t.Fatalf("renderBlindWorksheet: %v", err)
	}
	return out
}

// testBlindBlockmap rebuilds the block map a testBlindSalt render emits for
// arts (nil when no promptless artifact is present) — byte-equivalent to the
// map renderBlindWorksheet returns under the same salt.
func testBlindBlockmap(arts []Artifact) *blindBlockmapFile {
	bm := &blindBlockmapFile{SchemaVersion: blindBlockmapSchemaVersion, Salt: testBlindSalt, Blocks: map[string]string{}}
	for _, a := range arts {
		if prefilledAssemblyMode(a.Trace) {
			bm.Blocks[blindOpaqueID(a.ArtifactHash, testBlindSalt)] = a.ArtifactHash
		}
	}
	if len(bm.Blocks) == 0 {
		return nil
	}
	return bm
}

// testBlockHeader / testBlockDupHeader compute a promptless artifact's
// worksheet header under testBlindSalt.
func testBlockHeader(hash string) string {
	return blindBlockHeaderPrefix + blindOpaqueID(hash, testBlindSalt) + " ==="
}

func testBlockDupHeader(hash string) string {
	return blindBlockHeaderPrefix + blindOpaqueID(hash, testBlindSalt) + " DUP ==="
}

// blindTestPrimaryHeader is the primary-block header for any artifact:
// opaque BLOCK for promptless, hash-addressed ARTIFACT otherwise.
func blindTestPrimaryHeader(a Artifact) string {
	if prefilledAssemblyMode(a.Trace) {
		return testBlockHeader(a.ArtifactHash)
	}
	return blindArtifactHeaderPrefix + a.ArtifactHash + " ==="
}

func TestBlindWorksheetPromptlessPrimary(t *testing.T) {
	for _, mode := range []AssemblyMode{AssemblyLegacy, AssemblyMixed, AssemblyTopline} {
		art := testPromptlessArtifact("t-"+string(mode), "sha256:pl-"+string(mode), mode, "final question for "+string(mode)+"?", "answer body")
		out := mustRenderBlind(t, []Artifact{art}, 0)
		for _, want := range []string{
			testBlockHeader(art.ArtifactHash),
			"[question]\nfinal question for " + string(mode) + "?",
			"[rubric]\nrubric for t-" + string(mode),
			"[candidate output]\nanswer body",
			"flag: ",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s worksheet missing %q:\n%s", mode, want, out)
			}
		}
		// ZERO prompt bytes anywhere in a promptless-only worksheet: no
		// sentinel, no [prompt] section, no trace line — and (W5) no artifact
		// hash either: "sha256:" is the unblinding-join sentinel, since a
		// header hash resolves the arm through the committed artifacts JSONL.
		for _, leaked := range []string{"PROMPT-SENTINEL", "[prompt]", "trace:", "sha256:", blindArtifactHeaderPrefix} {
			if strings.Contains(out, leaked) {
				t.Errorf("%s worksheet leaked %q:\n%s", mode, leaked, out)
			}
		}
	}

	// Non-assembly and 3a flat/progressive rendering is frozen byte-for-byte,
	// and a promptless-free render emits no block map.
	got, bm, err := renderBlindWorksheet(goldenBlindArtifacts(), 0, testBlindSalt)
	if err != nil {
		t.Fatalf("renderBlindWorksheet: %v", err)
	}
	if got != blindGoldenWorksheet {
		t.Errorf("non-promptless worksheet drifted from pre-change golden:\ngot:\n%s\nwant:\n%s", got, blindGoldenWorksheet)
	}
	if bm != nil {
		t.Errorf("promptless-free render returned a block map: %+v; want nil", bm)
	}

	t.Run("promptless render returns the id->hash block map", func(t *testing.T) {
		art := testPromptlessArtifact("t-map", "sha256:pl-map", AssemblyMixed, "q?", "ans")
		_, bm, err := renderBlindWorksheet([]Artifact{art}, 0, testBlindSalt)
		if err != nil {
			t.Fatalf("renderBlindWorksheet: %v", err)
		}
		if bm == nil || bm.SchemaVersion != blindBlockmapSchemaVersion || bm.Salt != testBlindSalt ||
			bm.Blocks[blindOpaqueID(art.ArtifactHash, testBlindSalt)] != art.ArtifactHash {
			t.Fatalf("block map = %+v; want the schema-versioned salt map holding the artifact", bm)
		}
	})

	t.Run("final non-user turn is a loud render error", func(t *testing.T) {
		art := testPromptlessArtifact("t-bad", "sha256:bad", AssemblyMixed, "q?", "ans")
		art.Trace.Turns = append(art.Trace.Turns, Turn{Role: "assistant", Content: "assistant tail"})
		if _, _, err := renderBlindWorksheet([]Artifact{art}, 0, testBlindSalt); err == nil {
			t.Fatalf("promptless artifact with non-user final turn accepted; [question] would leak prompt bytes")
		}
	})
}

func TestBlindIngestGroundingFlag(t *testing.T) {
	art := testPromptlessArtifact("t1", "sha256:f1", AssemblyMixed, "q?", "ans")
	arts := []Artifact{art}
	header := testBlockHeader(art.ArtifactHash)
	base := mustRenderBlind(t, arts, 0)
	bm := testBlindBlockmap(arts)

	t.Run("grounding-check flag prefixes notes and is summarized", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "score", "1")
		ws = fillWorksheetField(t, ws, header, "notes", "shaky claim")
		ws = fillWorksheetField(t, ws, header, "flag", "grounding-check")
		res, err := ingestBlindWorksheet(ws, arts, "tester", bm)
		if err != nil {
			t.Fatalf("ingestBlindWorksheet: %v", err)
		}
		if len(res.Labels) != 1 || res.Labels[0].LabelNotes != "grounding-check;shaky claim" {
			t.Fatalf("labels = %+v; want one label with notes %q", res.Labels, "grounding-check;shaky claim")
		}
		if !reflect.DeepEqual(res.FlaggedHashes, []string{art.ArtifactHash}) {
			t.Fatalf("FlaggedHashes = %v; want [%s]", res.FlaggedHashes, art.ArtifactHash)
		}
	})

	t.Run("blank flag emits a normal label", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "score", "0.5")
		res, err := ingestBlindWorksheet(ws, arts, "tester", bm)
		if err != nil {
			t.Fatalf("ingestBlindWorksheet: %v", err)
		}
		if len(res.Labels) != 1 || res.Labels[0].LabelNotes != "" || len(res.FlaggedHashes) != 0 {
			t.Fatalf("res = %+v; want one unflagged label with empty notes", res)
		}
	})

	t.Run("unknown flag value is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "score", "1")
		ws = fillWorksheetField(t, ws, header, "flag", "verify-me")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", bm); err == nil || !strings.Contains(err.Error(), "flag") {
			t.Fatalf("err = %v; want loud unknown-flag error", err)
		}
	})

	t.Run("flag on an unscored block is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "flag", "grounding-check")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", bm); err == nil {
			t.Fatalf("flag on unscored block accepted; want error")
		}
	})

	t.Run("spoofed grounding-check notes prefix with blank flag is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "score", "1")
		ws = fillWorksheetField(t, ws, header, "notes", "grounding-check;sneaky")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", bm); err == nil {
			t.Fatalf("spoofed grounding-check prefix in notes accepted; want error")
		}
	})

	t.Run("flag on a non-promptless block is a loud error", func(t *testing.T) {
		plain := testCalibrationArtifact("t-plain", "ans")
		plain.ArtifactHash = artifactHash(plain)
		ws := "=== ARTIFACT " + plain.ArtifactHash + " ===\n" +
			blindFillMarker + "\nscore: 1\nflag: grounding-check\n" + blindEndMarker + "\n"
		if _, err := ingestBlindWorksheet(ws, []Artifact{plain}, "tester", nil); err == nil || !strings.Contains(err.Error(), "flag") {
			t.Fatalf("err = %v; want loud flag-on-non-promptless error", err)
		}
	})

	t.Run("BLOCK header without a block map is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, base, header, "score", "1")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", nil); err == nil || !strings.Contains(err.Error(), "-blind-blockmap") {
			t.Fatalf("err = %v; want the loud blockmap requirement", err)
		}
	})

	t.Run("BLOCK id absent from the map is a loud error", func(t *testing.T) {
		other := testBlindBlockmap([]Artifact{testPromptlessArtifact("t9", "sha256:f9", AssemblyMixed, "q?", "a")})
		ws := fillWorksheetField(t, base, header, "score", "1")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", other); err == nil || !strings.Contains(err.Error(), "not in the block map") {
			t.Fatalf("err = %v; want the loud unknown-BLOCK-id error", err)
		}
	})

	t.Run("hash-addressed promptless block is a loud error", func(t *testing.T) {
		// A pre-W5 worksheet spelled promptless headers with the artifact
		// hash — unblinded by inspection; ingest must refuse it.
		ws := "=== ARTIFACT " + art.ArtifactHash + " ===\n" +
			blindFillMarker + "\nscore: 1\n" + blindEndMarker + "\n"
		if _, err := ingestBlindWorksheet(ws, arts, "tester", bm); err == nil || !strings.Contains(err.Error(), "opaque BLOCK") {
			t.Fatalf("err = %v; want the loud hash-addressed-promptless rejection", err)
		}
	})
}

// blindDupPool: topline sha256:d0 sorts before all eligible hashes and MUST
// NOT be selected (only legacy/mixed are dup-eligible); flat sha256:zz-flat is
// likewise ineligible. Eligible sorted: d1 d2 d3 d4 d5; N=2 => step
// ceil(5/2)=3 => dups d1 and d4.
func blindDupPool() []Artifact {
	arts := []Artifact{
		testPromptlessArtifact("t-top", "sha256:d0", AssemblyTopline, "top q?", "top answer"),
		testPromptlessArtifact("t1", "sha256:d1", AssemblyMixed, "q1?", "a1"),
		testPromptlessArtifact("t2", "sha256:d2", AssemblyLegacy, "q2?", "a2"),
		testPromptlessArtifact("t3", "sha256:d3", AssemblyMixed, "q3?", "a3"),
		testPromptlessArtifact("t4", "sha256:d4", AssemblyLegacy, "q4?", "a4"),
		testPromptlessArtifact("t5", "sha256:d5", AssemblyMixed, "q5?", "a5"),
	}
	flat := testCalibrationArtifact("t-flat", "flat answer")
	flat.Trace.AssemblyEval = &AssemblyEval{PairID: "pf", Mode: AssemblyFlat, CandidateIDs: []string{"c1"}, EstimatedPromptTokens: 10}
	flat.ArtifactHash = "sha256:zz-flat"
	return append(arts, flat)
}

func TestBlindDupInjection(t *testing.T) {
	arts := blindDupPool()
	bm := testBlindBlockmap(arts)
	out := mustRenderBlind(t, arts, 2)

	if got := strings.Count(out, " DUP ==="); got != 2 {
		t.Fatalf("DUP block count = %d; want 2:\n%s", got, out)
	}
	for _, want := range []string{testBlockDupHeader("sha256:d1"), testBlockDupHeader("sha256:d4")} {
		if !strings.Contains(out, want) {
			t.Fatalf("worksheet missing %q (deterministic selection broken):\n%s", want, out)
		}
	}
	// DUP blocks render after ALL primary blocks (idx >= 0 required — a
	// missing primary must fail here, not satisfy the < comparison at -1).
	firstDup := strings.Index(out, " DUP ===")
	if firstDup < 0 {
		t.Fatalf("no DUP block in worksheet:\n%s", out)
	}
	for _, a := range arts {
		idx := strings.Index(out, blindTestPrimaryHeader(a))
		if idx < 0 {
			t.Fatalf("primary block %s missing from worksheet", a.ArtifactHash)
		}
		if idx > firstDup {
			t.Fatalf("primary block %s at %d renders after first DUP at %d", a.ArtifactHash, idx, firstDup)
		}
	}
	// flag: lines belong to promptless PRIMARY blocks only (6 here: d0..d5);
	// DUP blocks must not render one.
	if got := strings.Count(out, "\nflag: \n"); got != 6 {
		t.Fatalf("flag: line count = %d; want 6 (promptless primaries only, never DUP blocks)", got)
	}

	// Fill every primary; dup d1 agrees (1 vs 1), dup d4 disagrees (0.5 vs 1).
	ws := out
	scores := map[string]string{"sha256:d0": "0", "sha256:d1": "1", "sha256:d2": "0", "sha256:d3": "0.5", "sha256:d4": "0.5", "sha256:d5": "1", "sha256:zz-flat": "0"}
	headerFor := func(hash string) string {
		if hash == "sha256:zz-flat" {
			return "=== ARTIFACT " + hash + " ==="
		}
		return testBlockHeader(hash)
	}
	for hash, score := range scores {
		ws = fillWorksheetField(t, ws, headerFor(hash), "score", score)
	}
	ws = fillWorksheetField(t, ws, testBlockDupHeader("sha256:d1"), "score", "1")
	ws = fillWorksheetField(t, ws, testBlockDupHeader("sha256:d4"), "score", "1")

	res, err := ingestBlindWorksheet(ws, arts, "tester", bm)
	if err != nil {
		t.Fatalf("ingestBlindWorksheet: %v", err)
	}
	if len(res.Labels) != 7 {
		t.Fatalf("labels = %d; want 7 (DUP scores must never become labels)", len(res.Labels))
	}
	seen := map[string]int{}
	for _, l := range res.Labels {
		seen[l.ArtifactHash]++
	}
	for hash, n := range seen {
		if n != 1 {
			t.Fatalf("hash %s labeled %d times; DUP score leaked into labels", hash, n)
		}
	}
	wantPairs := []blindDupPair{
		{ArtifactHash: "sha256:d1", PrimaryScore: 1, DupScore: 1},
		{ArtifactHash: "sha256:d4", PrimaryScore: 0.5, DupScore: 1},
	}
	if !reflect.DeepEqual(res.DupPairs, wantPairs) {
		t.Fatalf("DupPairs = %+v; want %+v", res.DupPairs, wantPairs)
	}
	agree, disagree, meanAbs := blindDupSummary(res.DupPairs)
	if agree != 1 || disagree != 1 || meanAbs != 0.25 {
		t.Fatalf("summary = agree %d disagree %d mean|d| %v; want 1/1/0.25", agree, disagree, meanAbs)
	}
	if len(res.DupUnpaired) != 0 {
		t.Fatalf("DupUnpaired = %v; want none", res.DupUnpaired)
	}

	// -dups-out JSONL rows pinned.
	path := filepath.Join(t.TempDir(), "dups.jsonl")
	if err := writeJSONLRows(path, "dups", res.DupPairs); err != nil {
		t.Fatalf("writeJSONLRows: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"artifact_hash":"sha256:d1","primary_score":1,"dup_score":1}` + "\n" +
		`{"artifact_hash":"sha256:d4","primary_score":0.5,"dup_score":1}` + "\n"
	if string(data) != want {
		t.Fatalf("dups-out = %q; want %q", data, want)
	}

	t.Run("dup without a scored primary is unpaired", func(t *testing.T) {
		ws := out
		for hash, score := range scores {
			if hash == "sha256:d1" {
				continue // primary d1 left unscored
			}
			ws = fillWorksheetField(t, ws, headerFor(hash), "score", score)
		}
		ws = fillWorksheetField(t, ws, testBlockDupHeader("sha256:d1"), "score", "1")
		ws = fillWorksheetField(t, ws, testBlockDupHeader("sha256:d4"), "score", "1")
		res, err := ingestBlindWorksheet(ws, arts, "tester", bm)
		if err != nil {
			t.Fatalf("ingestBlindWorksheet: %v", err)
		}
		if res.Skipped != 1 || !reflect.DeepEqual(res.DupUnpaired, []string{"sha256:d1"}) {
			t.Fatalf("skipped=%d unpaired=%v; want 1 skipped, unpaired [sha256:d1]", res.Skipped, res.DupUnpaired)
		}
		if len(res.DupPairs) != 1 || res.DupPairs[0].ArtifactHash != "sha256:d4" {
			t.Fatalf("DupPairs = %+v; want only sha256:d4", res.DupPairs)
		}
	})

	t.Run("flag on a DUP block is a loud error", func(t *testing.T) {
		// DUP blocks render no flag: line, so a labeler would have to
		// hand-write one; ingest must still reject it.
		dupIdx := strings.Index(out, testBlockDupHeader("sha256:d1"))
		if dupIdx < 0 {
			t.Fatalf("missing d1 DUP block")
		}
		ws := out[:dupIdx] + strings.Replace(out[dupIdx:], "notes: ", "notes: \nflag: grounding-check", 1)
		ws = fillWorksheetField(t, ws, testBlockDupHeader("sha256:d1"), "score", "1")
		if _, err := ingestBlindWorksheet(ws, arts, "tester", bm); err == nil {
			t.Fatalf("flag on DUP block accepted; want error")
		}
	})

	t.Run("dup count exceeding eligible artifacts is a loud error", func(t *testing.T) {
		if _, _, err := renderBlindWorksheet(arts, 6, testBlindSalt); err == nil {
			t.Fatalf("blind-dups 6 over 5 eligible accepted; want error")
		}
	})
}

// fcPairArtifacts builds complete legacy/mixed pairs for pair-alpha (FNV-1a-64
// of "pair-alpha|c|fc" = 18218199419608068020, even => legacy is A) and
// pair-gamma ("pair-gamma|c|fc" = 15644729119194098327, odd => mixed is A).
func fcPairArtifacts() []Artifact {
	mk := func(traceID, hash, pairID string, mode AssemblyMode, question, answer string) Artifact {
		a := testPromptlessArtifact(traceID, hash, mode, question, answer)
		a.Trace.AssemblyEval.PairID = pairID
		a.Trace.Golden.FinalAnswerCriteria = "rubric for " + pairID
		return a
	}
	return []Artifact{
		mk("alpha-legacy", "sha256:al", "pair-alpha", AssemblyLegacy, "alpha question?", "legacy alpha answer"),
		mk("alpha-mixed", "sha256:am", "pair-alpha", AssemblyMixed, "alpha question?", "mixed alpha answer"),
		mk("gamma-legacy", "sha256:gl", "pair-gamma", AssemblyLegacy, "gamma question?", "legacy gamma answer"),
		mk("gamma-mixed", "sha256:gm", "pair-gamma", AssemblyMixed, "gamma question?", "mixed gamma answer"),
	}
}

func TestForcedChoiceRenderIngest(t *testing.T) {
	arts := fcPairArtifacts()
	sidemap := parityFCSidemap(arts, "c")
	out, err := renderForcedChoiceWorksheet(arts, sidemap)
	if err != nil {
		t.Fatalf("renderForcedChoiceWorksheet: %v", err)
	}

	// pair-alpha: legacy is A (even parity in the parity-mirroring map), so
	// answer A is the legacy answer. Block shape pinned exactly — the header
	// carries pairID and modelKey ONLY (W5: hashes would unblind via the
	// artifacts JSONL join).
	wantAlpha := "=== PAIR pair-alpha c ===\n" +
		"[question]\nalpha question?\n\n" +
		"[rubric]\nrubric for pair-alpha\n\n" +
		"[answer A]\nlegacy alpha answer\n\n" +
		"[answer B]\nmixed alpha answer\n\n" +
		fcFillMarker + "\nprefer: \narm_guess: \n" + blindEndMarker + "\n"
	if !strings.Contains(out, wantAlpha) {
		t.Errorf("worksheet missing exact pair-alpha block:\nwant:\n%s\ngot:\n%s", wantAlpha, out)
	}
	// pair-gamma: mixed is A (odd parity).
	if want := "=== PAIR pair-gamma c ==="; !strings.Contains(out, want) {
		t.Errorf("worksheet missing %q (parity assignment broken):\n%s", want, out)
	}
	if !strings.Contains(out, "[answer A]\nmixed gamma answer") {
		t.Errorf("pair-gamma answer A is not the mixed arm:\n%s", out)
	}
	// The worksheet carries the sidemap's sha256 identity, but never either
	// arm's artifact hash (the unblinding join).
	for _, a := range arts {
		if strings.Contains(out, a.ArtifactHash) {
			t.Errorf("forced-choice worksheet leaks artifact hash %q:\n%s", a.ArtifactHash, out)
		}
	}
	for _, leaked := range []string{"legacy", "mixed", "topline"} {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "=== PAIR ") && strings.Contains(line, leaked) {
				t.Errorf("PAIR header leaks arm name %q: %s", leaked, line)
			}
		}
	}

	t.Run("whitespace in a header field is a loud render error", func(t *testing.T) {
		bad := fcPairArtifacts()
		bad[0].Trace.AssemblyEval.PairID = "pair alpha"
		bad[1].Trace.AssemblyEval.PairID = "pair alpha"
		if _, err := renderForcedChoiceWorksheet(bad, parityFCSidemap(bad, "c")); err == nil {
			t.Fatalf("whitespace pair id accepted; the space-delimited PAIR header would misparse")
		}
	})

	t.Run("question mismatch across arms is a loud error", func(t *testing.T) {
		bad := fcPairArtifacts()
		bad[1].Trace.Turns[len(bad[1].Trace.Turns)-1].Content = "different question?"
		if _, err := renderForcedChoiceWorksheet(bad, parityFCSidemap(bad, "c")); err == nil {
			t.Fatalf("question mismatch accepted; want error")
		}
	})

	t.Run("incomplete pairs are excluded", func(t *testing.T) {
		extra := append(fcPairArtifacts(),
			testPromptlessArtifact("delta-legacy", "sha256:dl", AssemblyLegacy, "delta q?", "delta answer"))
		blank := testPromptlessArtifact("eps-legacy", "sha256:el", AssemblyLegacy, "eps q?", "")
		blankMixed := testPromptlessArtifact("eps-mixed", "sha256:em", AssemblyMixed, "eps q?", "eps mixed answer")
		blank.Trace.AssemblyEval.PairID = "pair-eps"
		blankMixed.Trace.AssemblyEval.PairID = "pair-eps"
		extra = append(extra, blank, blankMixed)
		got, err := renderForcedChoiceWorksheet(extra, parityFCSidemap(extra, "c"))
		if err != nil {
			t.Fatalf("renderForcedChoiceWorksheet: %v", err)
		}
		for _, absent := range []string{"pair-delta-legacy", "pair-eps"} {
			if strings.Contains(got, absent) {
				t.Errorf("incomplete pair %q rendered:\n%s", absent, got)
			}
		}
	})

	t.Run("ingest happy path emits pinned preference rows", func(t *testing.T) {
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "A")
		ws = fillWorksheetField(t, ws, "=== PAIR pair-gamma c ===", "prefer", "tie")
		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false)
		if err != nil {
			t.Fatalf("ingestForcedChoiceWorksheet: %v", err)
		}
		if skipped != 0 || len(rows) != 2 {
			t.Fatalf("rows=%d skipped=%d; want 2 rows, 0 skipped", len(rows), skipped)
		}
		alpha, gamma := rows[0], rows[1]
		if alpha.PairID != "pair-alpha" || alpha.CandidateModel != "c" || alpha.ArtifactHashA != "sha256:al" ||
			alpha.ArtifactHashB != "sha256:am" || alpha.Preference != "a" || alpha.Labeler != "tester" || alpha.LabeledAt.IsZero() {
			t.Fatalf("alpha row = %+v; want a-preference with full join fields and labeler stamp", alpha)
		}
		if gamma.PairID != "pair-gamma" || gamma.ArtifactHashA != "sha256:gm" || gamma.ArtifactHashB != "sha256:gl" || gamma.Preference != "tie" {
			t.Fatalf("gamma row = %+v; want tie with mixed-first hashes", gamma)
		}
		// JSON row shape: labeled_at must be RFC3339 (time.Time marshals so).
		raw, err := json.Marshal(alpha)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"pair_id", "candidate_model", "artifact_hash_a", "artifact_hash_b", "preference", "labeler", "labeled_at"} {
			if _, ok := decoded[key]; !ok {
				t.Errorf("row JSON missing key %q: %s", key, raw)
			}
		}
		stamp, ok := decoded["labeled_at"].(string)
		if !ok {
			t.Fatalf("labeled_at is not a string: %s", raw)
		}
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Errorf("labeled_at %q is not RFC3339: %v", stamp, err)
		}
	})

	t.Run("blank prefer is skipped and counted", func(t *testing.T) {
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "B")
		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false)
		if err != nil {
			t.Fatalf("ingestForcedChoiceWorksheet: %v", err)
		}
		if len(rows) != 1 || rows[0].Preference != "b" || skipped != 1 {
			t.Fatalf("rows=%+v skipped=%d; want one b-row and one skipped", rows, skipped)
		}
	})

	t.Run("invalid prefer value is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "C")
		if _, _, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false); err == nil {
			t.Fatalf("prefer C accepted; want error")
		}
	})

	t.Run("unknown pair in header is a loud error", func(t *testing.T) {
		ws := strings.Replace(out, "=== PAIR pair-alpha c ===", "=== PAIR pair-forged c ===", 1)
		ws = fillWorksheetField(t, ws, "=== PAIR pair-forged c ===", "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false)
		if err == nil || !strings.Contains(err.Error(), "pair-forged") {
			t.Fatalf("forged pair id = %v; want a loud no-sidemap-entry error naming it", err)
		}
	})

	t.Run("sidemap hashes disagreeing with its side assignment are a loud error", func(t *testing.T) {
		// A tampered map (assignment flipped, hashes left) is internally
		// inconsistent; ingest must refuse it rather than mis-assign sides.
		tampered := parityFCSidemap(arts, "c")
		e := tampered.Pairs["pair-alpha|c"]
		e.LegacyIsA = !e.LegacyIsA
		tampered.Pairs["pair-alpha|c"] = e
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "tester", tampered, false)
		if err == nil || !strings.Contains(err.Error(), "side assignment") {
			t.Fatalf("tampered sidemap = %v; want the side-assignment rejection", err)
		}
	})

	t.Run("sidemap hash outside the artifacts is a loud error", func(t *testing.T) {
		forged := parityFCSidemap(arts, "c")
		e := forged.Pairs["pair-alpha|c"]
		e.HashA = "sha256:forged"
		forged.Pairs["pair-alpha|c"] = e
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "A")
		_, _, err := ingestForcedChoiceWorksheet(ws, arts, "tester", forged, false)
		if err == nil || !strings.Contains(err.Error(), "sha256:forged") {
			t.Fatalf("forged sidemap hash = %v; want a loud not-in-artifacts error", err)
		}
	})

	t.Run("duplicate pair block is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "A")
		if _, _, err := ingestForcedChoiceWorksheet(ws+"\n"+ws, arts, "tester", sidemap, false); err == nil {
			t.Fatalf("duplicate pair block accepted; want error")
		}
	})
}

// TestFCSideIsLegacyA pins the registered A/B assignment: FNV-1a-64 over
// pairID+"|"+modelKey+"|fc" (offset 14695981039346656037, prime
// 1099511628211), even sum => legacy renders as answer A. Hash literals were
// computed independently with hash/fnv outside this package.
func TestFCSideIsLegacyA(t *testing.T) {
	cases := []struct {
		pairID, modelKey string
		fnv              uint64
		want             bool
	}{
		{"pair-alpha", "c", 18218199419608068020, true},
		{"pair-beta", "c", 3565649076032149826, true},
		{"pair-gamma", "c", 15644729119194098327, false},
		{"pair-delta", "c", 10176336996731909166, true},
		{"p1", "gemma4:31b", 13823460209950699956, true},
		{"p1", "qwen3:8b", 7483125776458196605, false},
	}
	for _, tc := range cases {
		if tc.want != (tc.fnv%2 == 0) {
			t.Fatalf("test-case inconsistency for %s|%s: literal %d parity disagrees with want %v", tc.pairID, tc.modelKey, tc.fnv, tc.want)
		}
		if got := fcSideIsLegacyA(tc.pairID, tc.modelKey); got != tc.want {
			t.Errorf("fcSideIsLegacyA(%q, %q) = %v; want %v (fnv %d)", tc.pairID, tc.modelKey, got, tc.want, tc.fnv)
		}
	}
}

func TestAdjudicationRoundTrip(t *testing.T) {
	m1 := testPromptlessArtifact("adj-1", "sha256:m1", AssemblyMixed, "q1?", "answer one")
	m2 := testPromptlessArtifact("adj-2", "sha256:m2", AssemblyMixed, "q2?", "answer two")
	arts := []Artifact{m1, m2}
	flagged := Label{TraceID: "adj-1", CandidateModel: "ollama/c", ArtifactHash: "sha256:m1",
		ExpectedAnswerQuality: 0.5, LabelNotes: "grounding-check;odd claim", Labeler: "tester", LabeledAt: time.Unix(1753574400, 0).UTC()}
	clean := Label{TraceID: "adj-2", CandidateModel: "ollama/c", ArtifactHash: "sha256:m2",
		ExpectedAnswerQuality: 1, LabelNotes: "clean", Labeler: "tester", LabeledAt: time.Unix(1753574400, 0).UTC()}
	labels := []Label{flagged, clean}

	out, err := renderAdjudicationWorksheet(arts, labels)
	if err != nil {
		t.Fatalf("renderAdjudicationWorksheet: %v", err)
	}
	for _, want := range []string{
		"=== ARTIFACT sha256:m1 ===",
		"[prompt]",
		"PROMPT-SENTINEL system for adj-1", // full prompt IS present here
		"[question]\nq1?",
		"score: 0.5", // pre-filled with the primary score
		"reason: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("adjudication worksheet missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sha256:m2") {
		t.Errorf("unflagged label rendered into adjudication worksheet:\n%s", out)
	}

	header := "=== ARTIFACT sha256:m1 ==="
	t.Run("changed score rewrites the note", func(t *testing.T) {
		ws := fillWorksheetField(t, out, header, "score", "1")
		ws = fillWorksheetField(t, ws, header, "reason", "prompt supports the claim")
		got, err := ingestAdjudicationWorksheet(ws, arts, labels)
		if err != nil {
			t.Fatalf("ingestAdjudicationWorksheet: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("labels = %d; want 2", len(got))
		}
		if got[0].ExpectedAnswerQuality != 1 || got[0].LabelNotes != "adjudicated(0.5->1): prompt supports the claim" {
			t.Fatalf("adjudicated label = %+v; want score 1 and note %q", got[0], "adjudicated(0.5->1): prompt supports the claim")
		}
		if !reflect.DeepEqual(got[1], clean) {
			t.Fatalf("unflagged label mutated: %+v; want byte-identical %+v", got[1], clean)
		}
	})

	t.Run("kept score notes adjudicated(kept)", func(t *testing.T) {
		ws := fillWorksheetField(t, out, header, "reason", "checked against prompt")
		got, err := ingestAdjudicationWorksheet(ws, arts, labels)
		if err != nil {
			t.Fatalf("ingestAdjudicationWorksheet: %v", err)
		}
		if got[0].ExpectedAnswerQuality != 0.5 || got[0].LabelNotes != "adjudicated(kept): checked against prompt" {
			t.Fatalf("kept label = %+v; want unchanged score and note %q", got[0], "adjudicated(kept): checked against prompt")
		}
	})

	t.Run("missing reason is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, out, header, "score", "1")
		if _, err := ingestAdjudicationWorksheet(ws, arts, labels); err == nil || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("err = %v; want loud missing-reason error", err)
		}
	})

	t.Run("flagged label absent from worksheet is a loud error", func(t *testing.T) {
		if _, err := ingestAdjudicationWorksheet("", arts, labels); err == nil {
			t.Fatalf("empty worksheet accepted; adjudication must be complete")
		}
	})

	t.Run("worksheet block for an unflagged label is a loud error", func(t *testing.T) {
		ws := fillWorksheetField(t, out, header, "reason", "fine")
		ws += "=== ARTIFACT sha256:m2 ===\n" + blindFillMarker + "\nscore: 1\nreason: forged\n" + blindEndMarker + "\n"
		if _, err := ingestAdjudicationWorksheet(ws, arts, labels); err == nil {
			t.Fatalf("unflagged worksheet block accepted; want error")
		}
	})

	t.Run("flagged label missing its artifact is a loud render error", func(t *testing.T) {
		if _, err := renderAdjudicationWorksheet([]Artifact{m2}, labels); err == nil {
			t.Fatalf("flagged label without artifact accepted; want error")
		}
	})
}

// TestWorksheetFillRegionLoudErrors: in every grammar, a non-blank fill-region
// line that does not start with a recognized field is a loud error — the
// silent-skip alternative drops multi-line notes/reason continuations and
// typo'd fields without a trace.
func TestWorksheetFillRegionLoudErrors(t *testing.T) {
	t.Run("blind: multi-line notes continuation", func(t *testing.T) {
		art := testPromptlessArtifact("t1", "sha256:f1", AssemblyMixed, "q?", "ans")
		header := testBlockHeader(art.ArtifactHash)
		ws := mustRenderBlind(t, []Artifact{art}, 0)
		ws = fillWorksheetField(t, ws, header, "score", "1")
		ws = fillWorksheetField(t, ws, header, "notes", "first line\nsecond line spills over")
		if _, err := ingestBlindWorksheet(ws, []Artifact{art}, "tester", testBlindBlockmap([]Artifact{art})); err == nil || !strings.Contains(err.Error(), "unrecognized fill-region line") {
			t.Fatalf("err = %v; want loud unrecognized-line error", err)
		}
	})
	t.Run("fc: capitalized field typo", func(t *testing.T) {
		arts := fcPairArtifacts()
		sidemap := parityFCSidemap(arts, "c")
		out, err := renderForcedChoiceWorksheet(arts, sidemap)
		if err != nil {
			t.Fatalf("renderForcedChoiceWorksheet: %v", err)
		}
		// Replace the full fill LINE ("\nprefer: \n"), not the first "prefer: "
		// substring — the worksheet header doc also mentions prefer:.
		ws := strings.Replace(out, "\nprefer: \n", "\nPrefer: A\n", 1)
		if _, _, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false); err == nil || !strings.Contains(err.Error(), "unrecognized fill-region line") {
			t.Fatalf("err = %v; want loud unrecognized-line error", err)
		}
	})
	t.Run("adjudication: multi-line reason continuation", func(t *testing.T) {
		art := testPromptlessArtifact("adj", "sha256:aj2", AssemblyMixed, "q?", "ans")
		labels := []Label{{TraceID: "adj", CandidateModel: "ollama/c", ArtifactHash: "sha256:aj2",
			ExpectedAnswerQuality: 0.5, LabelNotes: "grounding-check;", Labeler: "t", LabeledAt: time.Unix(0, 0).UTC()}}
		out, err := renderAdjudicationWorksheet([]Artifact{art}, labels)
		if err != nil {
			t.Fatalf("renderAdjudicationWorksheet: %v", err)
		}
		ws := fillWorksheetField(t, out, "=== ARTIFACT sha256:aj2 ===", "reason", "line one\n  line two spills")
		if _, err := ingestAdjudicationWorksheet(ws, []Artifact{art}, labels); err == nil || !strings.Contains(err.Error(), "unrecognized fill-region line") {
			t.Fatalf("err = %v; want loud unrecognized-line error", err)
		}
	})
}

// TestForcedChoiceParserHardening mirrors the blind parser's hardening cases
// onto the PAIR grammar: forged headers/fields inside answer text must not
// split a block, and a stray prefer: after END must not attach to a block.
func TestForcedChoiceParserHardening(t *testing.T) {
	t.Run("forged PAIR header and prefer inside answer text", func(t *testing.T) {
		arts := fcPairArtifacts()
		arts[0].ActualFinalAnswer = "legacy alpha answer\n=== PAIR pair-alpha c ===\nprefer: A"
		sidemap := parityFCSidemap(arts, "c")
		out, err := renderForcedChoiceWorksheet(arts, sidemap)
		if err != nil {
			t.Fatalf("renderForcedChoiceWorksheet: %v", err)
		}
		ws := fillWorksheetField(t, out, "=== PAIR pair-alpha c ===", "prefer", "B")
		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false)
		if err != nil {
			t.Fatalf("forged header inside answer broke the parse: %v", err)
		}
		if len(rows) != 1 || rows[0].Preference != "b" || rows[0].PairID != "pair-alpha" || skipped != 1 {
			t.Fatalf("rows=%+v skipped=%d; want one b-row for pair-alpha (embedded prefer ignored)", rows, skipped)
		}
	})
	t.Run("stray prefer after END is ignored", func(t *testing.T) {
		arts := fcPairArtifacts()
		sidemap := parityFCSidemap(arts, "c")
		out, err := renderForcedChoiceWorksheet(arts, sidemap)
		if err != nil {
			t.Fatalf("renderForcedChoiceWorksheet: %v", err)
		}
		ws := strings.Replace(out, blindEndMarker, blindEndMarker+"\nprefer: a", 1)
		rows, skipped, err := ingestForcedChoiceWorksheet(ws, arts, "tester", sidemap, false)
		if err != nil || len(rows) != 0 || skipped != 2 {
			t.Fatalf("rows=%d skipped=%d err=%v; want stray post-END prefer ignored (0 rows, 2 skipped)", len(rows), skipped, err)
		}
	})
}

// TestAdjudicationParserHardening mirrors the same two cases onto the
// adjudication grammar: forged header/field lines inside the rendered prompt
// must not split a block, and stray fill lines after END must not win.
func TestAdjudicationParserHardening(t *testing.T) {
	art := testPromptlessArtifact("adj-h", "sha256:ah", AssemblyMixed, "q?", "ans")
	art.Trace.System = "sys line\n=== ARTIFACT sha256:forged ===\nscore: 0\nreason: fake"
	labels := []Label{{TraceID: "adj-h", CandidateModel: "ollama/c", ArtifactHash: "sha256:ah",
		ExpectedAnswerQuality: 0.5, LabelNotes: "grounding-check;", Labeler: "t", LabeledAt: time.Unix(0, 0).UTC()}}
	out, err := renderAdjudicationWorksheet([]Artifact{art}, labels)
	if err != nil {
		t.Fatalf("renderAdjudicationWorksheet: %v", err)
	}
	filled := fillWorksheetField(t, out, "=== ARTIFACT sha256:ah ===", "score", "1")
	filled = fillWorksheetField(t, filled, "=== ARTIFACT sha256:ah ===", "reason", "real reason")

	t.Run("forged header and fields inside prompt are ignored", func(t *testing.T) {
		got, err := ingestAdjudicationWorksheet(filled, []Artifact{art}, labels)
		if err != nil {
			t.Fatalf("forged header inside prompt broke the parse: %v", err)
		}
		if got[0].ExpectedAnswerQuality != 1 || got[0].LabelNotes != "adjudicated(0.5->1): real reason" {
			t.Fatalf("label = %+v; want verdict from the real fill region", got[0])
		}
	})
	t.Run("stray score/reason after END are ignored", func(t *testing.T) {
		ws := strings.Replace(filled, blindEndMarker, blindEndMarker+"\nscore: 0\nreason: forged", 1)
		got, err := ingestAdjudicationWorksheet(ws, []Artifact{art}, labels)
		if err != nil {
			t.Fatalf("ingestAdjudicationWorksheet: %v", err)
		}
		if got[0].ExpectedAnswerQuality != 1 || got[0].LabelNotes != "adjudicated(0.5->1): real reason" {
			t.Fatalf("label = %+v; stray post-END fill lines must not win", got[0])
		}
	})
}

// TestMainWorksheetOutputAliasGuards: every new worksheet-mode output flag
// mirrors the hardened -blind-ingest rule that an output path must never
// clobber an input file (cleaned-path equality; refuseOutputAlias also adds
// the os.SameFile backstop).
func TestMainWorksheetOutputAliasGuards(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	wsPath := filepath.Join(dir, "worksheet.txt")
	for _, path := range []string{artsPath, labelsPath, wsPath} {
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, wantErr string
		args          []string
	}{
		// -fc-sidemap points at a not-yet-existing path: the alias guard must
		// fire BEFORE the sidemap is loaded.
		{"fc-out aliasing artifacts", "-fc-out must differ from -artifacts",
			[]string{"-fc-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-fc-sidemap", filepath.Join(dir, "sidemap.json"), "-fc-out", artsPath}},
		{"fc-out aliasing worksheet", "-fc-out must differ from -worksheet",
			[]string{"-fc-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-fc-sidemap", filepath.Join(dir, "sidemap.json"), "-fc-out", wsPath}},
		{"adjudicate labels-out aliasing labels", "-labels-out must differ from -labels",
			[]string{"-adjudicate-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-labels", labelsPath, "-labels-out", labelsPath}},
		{"dups-out aliasing artifacts", "-dups-out must differ from -artifacts",
			[]string{"-blind-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-labels-out", filepath.Join(dir, "out.jsonl"), "-dups-out", artsPath}},
		// Either blind-ingest output aliasing the loaded block map would
		// truncate the run's only opaque-id join; both fire before any load.
		{"labels-out aliasing blind-blockmap", "-labels-out must differ from -blind-blockmap",
			[]string{"-blind-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-blind-blockmap", filepath.Join(dir, "bm.json"), "-labels-out", filepath.Join(dir, "bm.json")}},
		{"dups-out aliasing blind-blockmap", "-dups-out must differ from -blind-blockmap",
			[]string{"-blind-ingest", "-worksheet", wsPath, "-artifacts", artsPath, "-blind-blockmap", filepath.Join(dir, "bm.json"), "-labels-out", filepath.Join(dir, "out.jsonl"), "-dups-out", filepath.Join(dir, "bm.json")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], tc.args...)
			cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("output aliasing an input was accepted:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantErr) {
				t.Fatalf("output missing %q:\n%s", tc.wantErr, out)
			}
		})
	}
}

func TestBlindFlowsRenderDeterminism(t *testing.T) {
	arts := blindDupPool()
	// Deterministic under a FIXED salt; a fresh render (salt "") draws a new
	// salt by design, so worksheet determinism is per-salt.
	if a, b := mustRenderBlind(t, arts, 2), mustRenderBlind(t, arts, 2); a != b {
		t.Errorf("blind worksheet render is not deterministic")
	}
	fcArts := fcPairArtifacts()
	sidemap := parityFCSidemap(fcArts, "c")
	fc1, err1 := renderForcedChoiceWorksheet(fcArts, sidemap)
	fc2, err2 := renderForcedChoiceWorksheet(fcArts, sidemap)
	if err1 != nil || err2 != nil || fc1 != fc2 {
		t.Errorf("forced-choice render not deterministic (err1=%v err2=%v)", err1, err2)
	}
	adjArt := testPromptlessArtifact("adj", "sha256:aj", AssemblyMixed, "q?", "ans")
	adjLabels := []Label{{TraceID: "adj", CandidateModel: "ollama/c", ArtifactHash: "sha256:aj",
		ExpectedAnswerQuality: 0, LabelNotes: "grounding-check;", Labeler: "t", LabeledAt: time.Unix(0, 0).UTC()}}
	ad1, err1 := renderAdjudicationWorksheet([]Artifact{adjArt}, adjLabels)
	ad2, err2 := renderAdjudicationWorksheet([]Artifact{adjArt}, adjLabels)
	if err1 != nil || err2 != nil || ad1 != ad2 {
		t.Errorf("adjudication render not deterministic (err1=%v err2=%v)", err1, err2)
	}
}
