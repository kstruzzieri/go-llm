package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const round2ChallengeDir = "../../docs/llm/traces/round2-challenge"

func TestRound2ChallengeCorpus_EnforcesAuthoringContract(t *testing.T) {
	tracePaths, err := filepath.Glob(filepath.Join(round2ChallengeDir, "r2c-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracePaths) != 18 {
		t.Fatalf("found %d authored challenge traces; want exactly 18 (canary is tested separately and must not match this glob)", len(tracePaths))
	}
	traces, err := loadTraces(tracePaths)
	if err != nil {
		t.Fatalf("challenge traces must load as valid Traces: %v", err)
	}

	manifest, err := loadManifest(filepath.Join(round2ChallengeDir, "corpus-manifest.jsonl"))
	if err != nil {
		t.Fatalf("load corpus manifest: %v", err)
	}
	partByID := manifest.partitionByTrace()
	catByID := map[string]string{}
	for _, e := range manifest.Entries {
		catByID[e.TraceID] = string(e.Category)
	}

	validChallengeCats := map[string]bool{
		"fabrication": true, "compile-completeness": true, "restraint": true,
		"subtle-correctness": true, "underspecification": true,
	}

	counts := manifest.Counts()
	if counts.ByPartition[PartitionNatural] != 24 {
		t.Fatalf("manifest natural count = %d; want 24", counts.ByPartition[PartitionNatural])
	}
	if counts.ByPartition[PartitionChallenge] != 18 {
		t.Fatalf("manifest challenge count = %d; want 18", counts.ByPartition[PartitionChallenge])
	}
	if counts.ByPartition[PartitionJudgeValidation] != 1 {
		t.Fatalf("manifest judge-validation count = %d; want 1 canary", counts.ByPartition[PartitionJudgeValidation])
	}

	for _, tr := range traces {
		t.Run(tr.ID, func(t *testing.T) {
			if len(tr.Tools) != 0 {
				t.Errorf("tools must be [] for a chat challenge trace; got %d", len(tr.Tools))
			}
			if len(tr.Golden.ToolCalls) != 0 {
				t.Errorf("golden.tool_calls must be [] for a chat challenge trace")
			}
			if strings.TrimSpace(tr.Golden.FinalAnswerSubstring) != "" {
				t.Errorf("final_answer_substring must be empty for challenge traces (rubric-only scoring)")
			}
			if strings.TrimSpace(tr.Golden.FinalAnswerCriteria) == "" {
				t.Errorf("final_answer_criteria (the objective rubric) must be present")
			}
			if !strings.Contains(tr.Golden.FinalAnswerCriteria, "0.0") {
				t.Errorf("rubric must state the 0.0 defect boundary explicitly")
			}
			if !strings.Contains(tr.Golden.FinalAnswerCriteria, "1.0") {
				t.Errorf("rubric must state the 1.0 success boundary explicitly")
			}
			if !strings.Contains(tr.Golden.FinalAnswerCriteria, "0.5") {
				t.Errorf("rubric must state the 0.5 partial-credit boundary explicitly")
			}
			if tr.Source != "round2-challenge" {
				t.Errorf("source = %q; want round2-challenge", tr.Source)
			}
			if part := partByID[tr.ID]; part != PartitionChallenge {
				t.Errorf("trace %q manifest partition = %q; want challenge", tr.ID, part)
			}
			if !validChallengeCats[catByID[tr.ID]] {
				t.Errorf("trace %q manifest category = %q; want one of the 5 challenge axes", tr.ID, catByID[tr.ID])
			}
			if cat := catByID[tr.ID]; !strings.Contains(tr.ID, cat) {
				t.Errorf("trace %q does not embed its manifest axis %q in its ID; a rubric filed under the wrong axis must not pass CI", tr.ID, cat)
			}
		})
	}

	seenChallengeCats := map[string]bool{}
	for _, e := range manifest.Entries {
		if e.Partition == PartitionChallenge {
			seenChallengeCats[string(e.Category)] = true
		}
	}
	for cat := range validChallengeCats {
		if !seenChallengeCats[cat] {
			t.Errorf("challenge axis %q is not represented in the manifest; all 5 axes (fabrication, compile-completeness, restraint, subtle-correctness, underspecification) must be present", cat)
		}
	}

	for _, tr := range traces {
		blob := tr.System
		for _, turn := range tr.Turns {
			blob += "\n" + turn.Content
		}
		if absolutePathPattern.MatchString(blob) {
			t.Errorf("trace %q contains an absolute local path; sanitize before committing", tr.ID)
		}
		if strings.Count(blob, "```")%2 != 0 {
			t.Errorf("trace %q has an unbalanced ``` code fence; a truncated prompt must not ship", tr.ID)
		}
	}
}

// TestRound2ToolCanary_ComputesToolArgsValid proves the tool path end to end:
// the committed canary carries a real MCP tools/list snapshot (frozen into
// trace.Tools by -capture), and the harness can compute ToolArgsValid from it.
// A schema-valid call to the first expected tool must score 1.0. This is the
// R-C4 pipeline-validation gate; it makes no model-quality claim (the canary is
// judge-validation, excluded from model evidence).
func TestRound2ToolCanary_ComputesToolArgsValid(t *testing.T) {
	traces, err := loadTraces([]string{filepath.Join(round2ChallengeDir, "tool-canary-01.json")})
	if err != nil {
		t.Fatalf("load canary: %v", err)
	}
	tr := traces[0]
	if len(tr.Tools) == 0 || len(tr.Golden.ToolCalls) == 0 {
		t.Fatalf("canary must carry tool schemas and expected tool calls; tools=%d calls=%d", len(tr.Tools), len(tr.Golden.ToolCalls))
	}
	// The actual turn is synthesized, not replayed: this gate proves the scoring
	// path computes from the committed schema, not what any model would emit.
	// rag_search's only required argument is "query"; a schema-valid call.
	actual := []Turn{{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			Name:      tr.Golden.ToolCalls[0],
			Arguments: json.RawMessage(`{"query":"provider retry budget"}`),
		}},
	}}
	score, computed, notes, err := scoreToolArguments(tr, actual)
	if err != nil {
		t.Fatalf("scoreToolArguments: %v", err)
	}
	if !computed {
		t.Fatalf("ToolArgsValid not computed from the committed canary schema: %s", notes)
	}
	if score != 1.0 {
		t.Fatalf("ToolArgsValid = %v; want 1.0 for a schema-valid canary call (%s)", score, notes)
	}
}

// TestRound2ToolCanary_ExcludedFromModelEvidence pins the canary to the
// judge-validation partition so it can never enter a model-quality view.
func TestRound2ToolCanary_ExcludedFromModelEvidence(t *testing.T) {
	manifest, err := loadManifest(filepath.Join(round2ChallengeDir, "corpus-manifest.jsonl"))
	if err != nil {
		t.Fatalf("load corpus manifest: %v", err)
	}
	if part := manifest.partitionByTrace()["tool-canary-01"]; part != PartitionJudgeValidation {
		t.Fatalf("tool-canary-01 partition = %q; want judge-validation (never model evidence)", part)
	}
}
