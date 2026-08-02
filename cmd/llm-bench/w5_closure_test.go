package main

// Wave 5 closure tests (#331 slice 3c, round-3 review): answer-relevant
// pressure targets, mandatory scenario families on non-control cases, and
// completed tool-call equality in the subsequence matcher. The opaque
// worksheet-ID tests live with the worksheet flows (blind_flows_test.go,
// w3_report_test.go).

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// (2) The pressure target must be answer-relevant: equal to one of the
// case's required_evidence entries (domain AND literal), or — on
// stale_vs_fresh only — present case-sensitively in a rag abstract/overview
// or a memory record's content (the stale carrier).
func TestMixedPressureTargetAnswerRelevance(t *testing.T) {
	t.Run("non-evidence target rejected off stale_vs_fresh", func(t *testing.T) {
		c := mixedConvCase("rel-conv")
		// "settled on" is contained in conversation turn 0 and absent from the
		// question/system, so every pre-existing scan passes; only the
		// relevance rule can reject it.
		c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "settled on"}
		err := validateMixedPressureTarget(c)
		if err == nil || !strings.Contains(err.Error(), "answer-relevant") {
			t.Errorf("non-evidence target = %v; want an answer-relevance rejection", err)
		}
	})
	t.Run("required_evidence target accepted", func(t *testing.T) {
		if err := validateMixedPressureTarget(mixedConvCase("rel-ok")); err != nil {
			t.Errorf("evidence-anchored target = %v; want nil", err)
		}
	})
	t.Run("evidence literal under the wrong domain is not relevant", func(t *testing.T) {
		c := mixedJoinCase("rel-dom")
		// "port 7443" is registered as MEMORY evidence; declaring the target in
		// another domain must not count as the evidence anchor. Plant the
		// literal in a rag path-free spot so containment alone would pass.
		c.RagSources[0].Content = "package gw\n\nconst retryCeiling = 6 // retry ceiling 6\n// see port 7443\n"
		c.RequiredEvidence = c.RequiredEvidence[1:2] // keep only the rag anchor
		c.AnswerHome = "rag"
		c.Stratum = "chain_retention"
		c.PressureTarget = &mixedEvidence{Domain: "rag", Literal: "port 7443"}
		err := validateMixedPressureTarget(c)
		if err == nil || !strings.Contains(err.Error(), "answer-relevant") {
			t.Errorf("wrong-domain evidence literal = %v; want an answer-relevance rejection", err)
		}
	})
	t.Run("stale_vs_fresh memory-content stale carrier accepted", func(t *testing.T) {
		c := mixedStaleCase("rel-stale-mem")
		c.PressureTarget = &mixedEvidence{Domain: "memory", Literal: "09:30 on wednesdays"}
		if err := validateMixedPressureTarget(c); err != nil {
			t.Errorf("stale memory carrier target = %v; want nil", err)
		}
	})
	t.Run("stale_vs_fresh target outside evidence and stale carriers rejected", func(t *testing.T) {
		c := mixedStaleCase("rel-stale-bad")
		c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "settled on"}
		err := validateMixedPressureTarget(c)
		if err == nil || !strings.Contains(err.Error(), "answer-relevant") {
			t.Errorf("irrelevant stale-stratum target = %v; want an answer-relevance rejection", err)
		}
	})
}

// (3) scenario_family is mandatory at author time on non-control cases;
// controls stay optional.
func TestMixedScenarioFamilyMandatory(t *testing.T) {
	c := mixedConvCase("fam-req")
	c.ScenarioFamily = ""
	err := validateMixedCase(c)
	if err == nil || !strings.Contains(err.Error(), "scenario_family is required") {
		t.Errorf("family-free non-control case = %v; want the scenario_family requirement", err)
	}
	ctl := mixedControlCase("fam-ctl")
	ctl.ScenarioFamily = ""
	if err := validateMixedCase(ctl); err != nil {
		t.Errorf("family-free control case = %v; want nil (controls stay optional)", err)
	}
}

// (4) The subsequence matcher's tool-call branch compares per-call Name and
// argument BYTES, not just IDs: a same-ID call with a different name or
// different argument bytes presents a different prompt and is drift.
func TestMixedSubsequenceToolCallNameAndArgsTightened(t *testing.T) {
	full := mixedTestAsst("c1", `{"query":"x"}`)

	if !mixedSubsequenceMatch(full, mixedTestAsst("c1", `{"query":"x"}`)) {
		t.Errorf("identical tool-call messages must match")
	}

	renamed := mixedTestAsst("c1", `{"query":"x"}`)
	renamed.ToolCalls[0].Function.Name = "agent_memory_search"
	if mixedSubsequenceMatch(full, renamed) {
		t.Errorf("same-ID call with a different Name must not match")
	}

	reargued := mixedTestAsst("c1", `{"query":"x"}`)
	reargued.ToolCalls[0].Function.Arguments = json.RawMessage(`{"query":"y"}`)
	if mixedSubsequenceMatch(full, reargued) {
		t.Errorf("same-ID same-Name call with different argument bytes must not match")
	}

	// Gate integration: the drift is rejected end to end.
	q := mixedTestUser("q")
	err := mixedArmSubsequenceGate("sub-w5", AssemblyMixed, mixedTestState(full, q), mixedTestState(renamed, q))
	if err == nil || !strings.Contains(err.Error(), "sub-w5") {
		t.Errorf("renamed tool call passed the subsequence gate = %v; want rejection", err)
	}
}

// (1) The opaque block map round-trips through disk, and load rejects every
// corruption mode: wrong schema, blank salt, empty blocks, and an id that
// does not derive from its hash under the file's salt.
func TestBlindBlockmapRoundTripAndValidation(t *testing.T) {
	art := testPromptlessArtifact("bm-1", "sha256:bm-1", AssemblyMixed, "q?", "ans")
	_, bm, err := renderBlindWorksheet([]Artifact{art}, 0, testBlindSalt)
	if err != nil || bm == nil {
		t.Fatalf("renderBlindWorksheet: bm=%v err=%v; want a block map", bm, err)
	}
	path := filepath.Join(t.TempDir(), "blockmap.json")
	if err := writeBlindBlockmap(path, *bm); err != nil {
		t.Fatalf("writeBlindBlockmap: %v", err)
	}
	loaded, err := loadBlindBlockmap(path)
	if err != nil {
		t.Fatalf("loadBlindBlockmap: %v", err)
	}
	if loaded.Salt != bm.Salt || len(loaded.Blocks) != 1 ||
		loaded.Blocks[blindOpaqueID(art.ArtifactHash, testBlindSalt)] != art.ArtifactHash {
		t.Fatalf("round-trip map = %+v; want %+v", loaded, *bm)
	}

	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	goodID := blindOpaqueID("sha256:x", "salty")
	cases := []struct {
		name, body, wantErr string
	}{
		{"wrong schema version", `{"schema_version":"blind-blockmap/v9","salt":"s","blocks":{"a":"b"}}`, "schema_version"},
		{"blank salt", `{"schema_version":"blind-blockmap/v1","salt":" ","blocks":{"a":"b"}}`, "salt"},
		{"empty blocks", `{"schema_version":"blind-blockmap/v1","salt":"s","blocks":{}}`, "no blocks"},
		{"non-deriving id", `{"schema_version":"blind-blockmap/v1","salt":"salty","blocks":{"deadbeefdeadbeef":"sha256:x"}}`, "does not derive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadBlindBlockmap(write(t, tc.body)); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want error containing %q", err, tc.wantErr)
			}
		})
	}
	t.Run("deriving id accepted", func(t *testing.T) {
		body := `{"schema_version":"blind-blockmap/v1","salt":"salty","blocks":{"` + goodID + `":"sha256:x"}}`
		if _, err := loadBlindBlockmap(write(t, body)); err != nil {
			t.Errorf("valid hand-written map rejected: %v", err)
		}
	})
}

// (1) CLI enforcement: -blind-render over promptless artifacts REQUIRES
// -blind-blockmap-out (and refuses it when nothing promptless renders); with
// the flag, the map lands on disk and drives a full CLI-shaped ingest join.
func TestMainBlindRenderRequiresBlockmapOut(t *testing.T) {
	dir := t.TempDir()
	art := testPromptlessArtifact("cli-1", "sha256:cli-1", AssemblyMixed, "q?", "ans")
	art.ArtifactHash = artifactHash(art) // loadArtifacts verifies the real hash
	promptlessArts := filepath.Join(dir, "promptless.jsonl")
	if err := writeJSONL(promptlessArts, []any{art}); err != nil {
		t.Fatal(err)
	}
	plain := testCalibrationArtifact("t-plain", "ans")
	plain.ArtifactHash = artifactHash(plain)
	plainArts := filepath.Join(dir, "plain.jsonl")
	if err := writeJSONL(plainArts, []any{plain}); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, error) {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("promptless render without the flag is fatal", func(t *testing.T) {
		out, err := run("-blind-render", "-artifacts", promptlessArts, "-report", filepath.Join(dir, "ws.txt"))
		if err == nil || !strings.Contains(out, "-blind-blockmap-out is required") {
			t.Fatalf("out = %q err = %v; want the loud blockmap-out requirement", out, err)
		}
	})
	t.Run("flag without promptless blocks is fatal", func(t *testing.T) {
		out, err := run("-blind-render", "-artifacts", plainArts,
			"-report", filepath.Join(dir, "ws2.txt"), "-blind-blockmap-out", filepath.Join(dir, "bm2.json"))
		if err == nil || !strings.Contains(out, "no promptless blocks") {
			t.Fatalf("out = %q err = %v; want the no-promptless refusal", out, err)
		}
	})
	t.Run("with the flag the map is written and joins ingest", func(t *testing.T) {
		wsPath := filepath.Join(dir, "ws3.txt")
		bmPath := filepath.Join(dir, "bm3.json")
		if out, err := run("-blind-render", "-artifacts", promptlessArts, "-report", wsPath, "-blind-blockmap-out", bmPath); err != nil {
			t.Fatalf("blind-render failed: %v\n%s", err, out)
		}
		bm, err := loadBlindBlockmap(bmPath)
		if err != nil {
			t.Fatalf("emitted block map invalid: %v", err)
		}
		ws, err := os.ReadFile(wsPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(ws), "sha256:") {
			t.Fatalf("CLI-rendered promptless worksheet leaks hashes:\n%s", ws)
		}
		header := blindBlockHeaderPrefix + blindOpaqueID(art.ArtifactHash, bm.Salt) + " ==="
		filled := fillWorksheetField(t, string(ws), header, "score", "1")
		if err := os.WriteFile(wsPath, []byte(filled), 0o600); err != nil {
			t.Fatal(err)
		}
		labelsOut := filepath.Join(dir, "labels.jsonl")
		if out, err := run("-blind-ingest", "-worksheet", wsPath, "-artifacts", promptlessArts,
			"-labels-out", labelsOut, "-blind-blockmap", bmPath); err != nil {
			t.Fatalf("blind-ingest failed: %v\n%s", err, out)
		}
		labels, err := loadLabels(labelsOut)
		if err != nil || len(labels) != 1 || labels[0].ArtifactHash != art.ArtifactHash || labels[0].ExpectedAnswerQuality != 1 {
			t.Fatalf("labels = %+v err = %v; want one full-join label for the promptless artifact", labels, err)
		}
	})
}
