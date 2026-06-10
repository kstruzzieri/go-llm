package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func keepSorted(keep map[string]struct{}) []string {
	out := make([]string, 0, len(keep))
	for id := range keep {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func TestCorpusEvidenceFilter_DropsJudgeValidationAndNonEvidence(t *testing.T) {
	m := sampleCorpusManifest()
	traces := []Trace{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}
	keep, data, excl := corpusEvidenceFilter(m, corpusSelection{}, traces)
	if got := keepSorted(keep); !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Fatalf("keep = %v; want [t1 t2] (judge-validation t3 dropped)", got)
	}
	if data == nil || data.Counts.Total != 3 {
		t.Fatalf("report data = %+v; want diagnostics for all selected traces", data)
	}
	if excl.JudgeValidation != 1 {
		t.Errorf("excl.JudgeValidation = %d; want 1", excl.JudgeValidation)
	}
}

func TestCorpusEvidenceFilter_RestrictsToSelectedPartition(t *testing.T) {
	m := sampleCorpusManifest()
	traces := []Trace{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}
	keep, _, _ := corpusEvidenceFilter(m, corpusSelection{Partitions: []CorpusPartition{PartitionChallenge}}, traces)
	if got := keepSorted(keep); !reflect.DeepEqual(got, []string{"t2"}) {
		t.Fatalf("keep = %v; want [t2] (challenge only)", got)
	}
}

func TestCorpusEvidenceFilter_SurfacesSelectedButMissingTrace(t *testing.T) {
	m := sampleCorpusManifest()
	traces := []Trace{{ID: "t1"}, {ID: "t2"}} // t3 selected by manifest but absent from artifacts
	_, data, _ := corpusEvidenceFilter(m, corpusSelection{}, traces)
	if data == nil || !reflect.DeepEqual(data.MissingSelected, []string{"t3"}) {
		t.Fatalf("MissingSelected = %+v; want [t3] so offline reports cannot silently shrink the run", data)
	}
}

func TestSplitMissingByEvidence(t *testing.T) {
	m := sampleCorpusManifest()
	// t3 is judge-validation/non-evidence: its absence cannot shrink a
	// model-evidence report. t1 is natural evidence: its absence must block.
	// An ID with no manifest entry is treated as evidence (fail loud).
	evidence, nonEvidence := splitMissingByEvidence(m, []string{"t1", "t3", "ghost"})
	if !reflect.DeepEqual(evidence, []string{"t1", "ghost"}) {
		t.Fatalf("evidence = %v; want [t1 ghost] (t3 canary excluded)", evidence)
	}
	if !reflect.DeepEqual(nonEvidence, []string{"t3"}) {
		t.Fatalf("nonEvidence = %v; want [t3] (surfaced for the report note)", nonEvidence)
	}
	if evidence, _ := splitMissingByEvidence(m, []string{"t3"}); len(evidence) != 0 {
		t.Fatalf("splitMissingByEvidence(canary only) evidence = %v; want empty", evidence)
	}
}

func TestMissingCorpusNote(t *testing.T) {
	if got := missingCorpusNote(nil); got != "" {
		t.Fatalf("missingCorpusNote(nil) = %q; want empty", got)
	}
	got := missingCorpusNote([]string{"tool-canary-01"})
	if !strings.Contains(got, "tool-canary-01") || !strings.Contains(got, "non-evidence") {
		t.Fatalf("missingCorpusNote = %q; want canary ID and non-evidence wording", got)
	}
}

func TestTracesFromArtifacts_DedupesByTraceID(t *testing.T) {
	arts := []Artifact{
		{Trace: Trace{ID: "t1"}},
		{Trace: Trace{ID: "t1"}}, // second model for same trace
		{Trace: Trace{ID: "t2"}},
	}
	got := tracesFromArtifacts(arts)
	if len(got) != 2 || got[0].ID != "t1" || got[1].ID != "t2" {
		t.Fatalf("tracesFromArtifacts = %+v; want distinct [t1 t2] in first-seen order", got)
	}
}
