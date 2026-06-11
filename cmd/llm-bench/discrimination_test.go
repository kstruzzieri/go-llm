package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestClassifyTrace_AllStates(t *testing.T) {
	top := []string{"gemma", "coder", "qwen36", "glm"}

	cases := []struct {
		name  string
		q     map[string]float64
		floor float64
		fok   bool
		want  discriminationState
	}{
		{
			name: "saturated — every top model 1.0",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.0, fok: true, want: stateSaturated,
		},
		{
			name: "valid discriminator — top splits and one solved it",
			q:    map[string]float64{"gemma": 1.0, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateValidDiscriminator,
		},
		{
			name: "unsolved — top splits but none reached 1.0",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.0, "qwen36": 0.5, "glm": 0.0},
			floor: 0.0, fok: true, want: stateUnsolved,
		},
		{
			name: "floor-only — top tied below 1.0, floor differs",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateFloorOnly,
		},
		{
			name: "no-signal — all five tied below 1.0",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.5, fok: true, want: stateNoSignal,
		},
		{
			name: "unpaired — a top model has no label",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0},
			floor: 0.0, fok: true, want: stateUnpaired,
		},
		{
			name: "unpaired — floor missing at step 4 (cannot separate floor-only from no-signal)",
			q:    map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: false, want: stateUnpaired,
		},
		{
			name: "precedence — saturated takes priority over any floor reading",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.5, fok: true, want: stateSaturated,
		},
		{
			name: "valid discriminator — split with two at 1.0",
			q:    map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 0.5, "glm": 0.0},
			floor: 0.0, fok: true, want: stateValidDiscriminator,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyTrace(c.q, top, c.floor, c.fok)
			if got != c.want {
				t.Fatalf("classifyTrace = %q; want %q", got, c.want)
			}
		})
	}
}

func TestDiscriminationStates_Enumerated(t *testing.T) {
	// Lock the state set so a renamed/added state is a deliberate change.
	want := []discriminationState{
		stateValidDiscriminator, stateSaturated, stateUnsolved,
		stateFloorOnly, stateNoSignal, stateUnpaired,
	}
	if !reflect.DeepEqual(allDiscriminationStates(), want) {
		t.Fatalf("allDiscriminationStates() = %v; want %v", allDiscriminationStates(), want)
	}
}

// mkArtifact builds a hash-self-consistent Artifact for discrimination tests.
// Quality lives on the paired Label (see matchedLabelsForDiscrimination), never on the Artifact.
func mkArtifact(t *testing.T, traceID, model string) Artifact {
	t.Helper()
	a := Artifact{
		TraceID:        traceID,
		CandidateModel: model,
		Trace:          Trace{ID: traceID, Source: "round3-challenge", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}},
	}
	a.ArtifactHash = artifactHash(a)
	return a
}

// matchedLabelsForDiscrimination pairs each artifact with a Label carrying its
// quality, by artifact_hash, returning the []matchedLabel the real entry point consumes.
func matchedLabelsForDiscrimination(arts []Artifact, quals []float64) []matchedLabel {
	out := make([]matchedLabel, len(arts))
	for i, a := range arts {
		out[i] = matchedLabel{
			Artifact: a,
			Label:    Label{ArtifactHash: a.ArtifactHash, ExpectedAnswerQuality: quals[i]},
		}
	}
	return out
}

func TestBuildTraceModelQuality_DropsNonEvidenceAndKeysCanonical(t *testing.T) {
	arts := []Artifact{
		mkArtifact(t, "r3c-type-semantics-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r3c-type-semantics-01", "openai-compat/glm-5.1"),
		mkArtifact(t, "tool-canary-01", "ollama/gemma4:31b"), // judge-validation, must drop
	}
	matched := matchedLabelsForDiscrimination(arts, []float64{1.0, 0.5, 1.0})
	m := Manifest{Entries: []ManifestEntry{
		{TraceID: "r3c-type-semantics-01", Partition: PartitionChallenge, Category: "type-semantics", Source: "round3-challenge", AllowedAsModelEvidence: true},
		{TraceID: "tool-canary-01", Partition: PartitionJudgeValidation, Category: "tool-canary", Source: "round3-challenge", AllowedAsModelEvidence: false},
	}}

	qual, err := buildTraceModelQualityFromMatched(matched, &corpusFilter{Manifest: m, Selection: corpusSelection{}})
	if err != nil {
		t.Fatalf("buildTraceModelQualityFromMatched: %v", err)
	}
	if _, ok := qual["tool-canary-01"]; ok {
		t.Fatalf("canary must be dropped from model-evidence quality map")
	}
	got := qual["r3c-type-semantics-01"]
	want := map[string]float64{"ollama/gemma4:31b": 1.0, "openai-compat/glm-5.1": 0.5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("trace quality = %v; want %v (canonical-model keyed)", got, want)
	}
}

func TestBuildTraceModelQuality_RejectsDuplicatePair(t *testing.T) {
	arts := []Artifact{
		mkArtifact(t, "r3c-x-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r3c-x-01", "ollama/gemma4:31b"), // same (trace, model)
	}
	matched := matchedLabelsForDiscrimination(arts, []float64{1.0, 0.5})
	_, err := buildTraceModelQualityFromMatched(matched, nil)
	if err == nil {
		t.Fatal("want error on duplicate (trace, model); got nil")
	}
	if !strings.Contains(err.Error(), "discrimination:") {
		t.Fatalf("error %q must carry the discrimination: prefix", err)
	}
}
