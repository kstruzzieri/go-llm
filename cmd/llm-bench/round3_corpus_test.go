package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const round3ChallengeDir = "../../docs/llm/traces/round3-challenge"

var validR3Families = map[string]bool{
	"type-semantics":       true,
	"concurrency-lifetime": true,
	"stdlib-contract":      true,
	"contract-edge":        true,
	"algorithmic":          true,
}

func TestRound3ChallengeCorpus_EnforcesAuthoringContract(t *testing.T) {
	tracePaths, err := filepath.Glob(filepath.Join(round3ChallengeDir, "r3c-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tracePaths) == 0 {
		t.Fatal("no r3c-*.json fresh traces found")
	}
	traces, err := loadTraces(tracePaths)
	if err != nil {
		t.Fatalf("challenge traces must load as valid Traces: %v", err)
	}

	manifest, err := loadManifest(filepath.Join(round3ChallengeDir, "corpus-manifest.jsonl"))
	if err != nil {
		t.Fatalf("load corpus manifest: %v", err)
	}
	if len(tracePaths) != 24 {
		t.Fatalf("found %d fresh r3c traces; want exactly 24", len(tracePaths))
	}
	counts := manifest.Counts()
	for fam, want := range map[string]int{
		"type-semantics":       6,
		"concurrency-lifetime": 5,
		"stdlib-contract":      5,
		"contract-edge":        4,
		"algorithmic":          4,
	} {
		if got := counts.ByCategory[fam]; got != want {
			t.Errorf("%s count = %d; want %d", fam, got, want)
		}
	}
	if part := manifest.partitionByTrace()["tool-canary-01"]; part != PartitionJudgeValidation {
		t.Errorf("tool-canary-01 partition = %q; want judge-validation", part)
	}
	catByID := map[string]string{}
	srcByID := map[string]string{}
	evidenceByID := map[string]bool{}
	for _, e := range manifest.Entries {
		catByID[e.TraceID] = e.Category
		srcByID[e.TraceID] = e.Source
		evidenceByID[e.TraceID] = e.AllowedAsModelEvidence
	}
	if evidenceByID["tool-canary-01"] {
		t.Errorf("tool-canary-01 must not be allowed as model evidence")
	}

	for _, tr := range traces {
		t.Run(tr.ID, func(t *testing.T) {
			if len(tr.Tools) != 0 {
				t.Errorf("tools must be [] for a chat challenge trace; got %d", len(tr.Tools))
			}
			if len(tr.Golden.ToolCalls) != 0 {
				t.Errorf("golden.tool_calls must be []")
			}
			if strings.TrimSpace(tr.Golden.FinalAnswerSubstring) != "" {
				t.Errorf("final_answer_substring must be empty (rubric-only scoring)")
			}
			crit := tr.Golden.FinalAnswerCriteria
			for _, tier := range []string{"1.0", "0.5", "0.0"} {
				if !strings.Contains(crit, tier) {
					t.Errorf("rubric must state the %s boundary explicitly (gradient pre-registration)", tier)
				}
			}
			if !strings.Contains(strings.ToLower(crit), "restraint") {
				t.Errorf("rubric must enumerate concrete restraint/provenance fail conditions")
			}
			if tr.Source != "round3-challenge" {
				t.Errorf("trace source = %q; want round3-challenge", tr.Source)
			}
			if srcByID[tr.ID] != "round3-challenge" {
				t.Errorf("manifest source = %q; want round3-challenge", srcByID[tr.ID])
			}
			if part := manifest.partitionByTrace()[tr.ID]; part != PartitionChallenge {
				t.Errorf("manifest partition = %q; want challenge", part)
			}
			fam := catByID[tr.ID]
			if !validR3Families[fam] {
				t.Errorf("manifest category %q is not one of the 5 R3 families", fam)
			}
			if !strings.Contains(tr.ID, fam) {
				t.Errorf("trace ID %q must embed its family %q (no misfiled rubric)", tr.ID, fam)
			}

			notePath := filepath.Join(round3ChallengeDir, "screen-notes", tr.ID+".json")
			data, err := os.ReadFile(notePath)
			if err != nil {
				t.Fatalf("missing oracle-screen note %s: %v", notePath, err)
			}
			var note struct {
				TraceID             string   `json:"trace_id"`
				Oracles             []string `json:"oracles"`
				NonTrivial          bool     `json:"non_trivial"`
				SolvableFromContext bool     `json:"solvable_from_context"`
			}
			if err := json.Unmarshal(data, &note); err != nil {
				t.Fatalf("screen note %s must parse: %v", notePath, err)
			}
			if note.TraceID != tr.ID {
				t.Errorf("screen note trace_id = %q; want %q", note.TraceID, tr.ID)
			}
			if len(note.Oracles) == 0 || !note.NonTrivial || !note.SolvableFromContext {
				t.Errorf("screen note must assert >=1 oracle, non_trivial, solvable_from_context")
			}

			blob := tr.System
			for _, turn := range tr.Turns {
				blob += "\n" + turn.Content
			}
			if absolutePathPattern.MatchString(blob) {
				t.Errorf("trace %q contains an absolute local path; sanitize", tr.ID)
			}
			if strings.Count(blob, "```")%2 != 0 {
				t.Errorf("trace %q has an unbalanced ``` fence", tr.ID)
			}
		})
	}
}
