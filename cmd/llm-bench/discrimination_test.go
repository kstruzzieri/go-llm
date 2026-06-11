package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeArtifactsJSONL(t *testing.T, path string, arts []Artifact) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, a := range arts {
		if err := enc.Encode(a); err != nil {
			t.Fatal(err)
		}
	}
}

func writeDiscriminationLabelsJSONL(t *testing.T, path string, arts []Artifact, quals []float64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for i, a := range arts {
		if err := enc.Encode(Label{
			TraceID:               a.TraceID,
			CandidateModel:        a.CandidateModel,
			ArtifactHash:          a.ArtifactHash,
			ExpectedAnswerQuality: quals[i],
			Labeler:               "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
}

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
			name:  "saturated — every top model 1.0",
			q:     map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.0, fok: true, want: stateSaturated,
		},
		{
			name:  "valid discriminator — top splits and one solved it",
			q:     map[string]float64{"gemma": 1.0, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateValidDiscriminator,
		},
		{
			name:  "unsolved — top splits but none reached 1.0",
			q:     map[string]float64{"gemma": 0.5, "coder": 0.0, "qwen36": 0.5, "glm": 0.0},
			floor: 0.0, fok: true, want: stateUnsolved,
		},
		{
			name:  "floor-only — top tied below 1.0, floor differs",
			q:     map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: true, want: stateFloorOnly,
		},
		{
			name:  "no-signal — all five tied below 1.0",
			q:     map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.5, fok: true, want: stateNoSignal,
		},
		{
			name:  "unpaired — a top model has no label",
			q:     map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0},
			floor: 0.0, fok: true, want: stateUnpaired,
		},
		{
			name:  "unpaired — floor missing at step 4 (cannot separate floor-only from no-signal)",
			q:     map[string]float64{"gemma": 0.5, "coder": 0.5, "qwen36": 0.5, "glm": 0.5},
			floor: 0.0, fok: false, want: stateUnpaired,
		},
		{
			name:  "precedence — saturated takes priority over any floor reading",
			q:     map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 1.0, "glm": 1.0},
			floor: 0.5, fok: true, want: stateSaturated,
		},
		{
			name:  "valid discriminator — split with two at 1.0",
			q:     map[string]float64{"gemma": 1.0, "coder": 1.0, "qwen36": 0.5, "glm": 0.0},
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

func TestClassifyTrace_EmptyTopModelsIsUnpaired(t *testing.T) {
	// No declared top cluster cannot be classified; the vacuous "all top == 1.0"
	// must not masquerade as saturated.
	if got := classifyTrace(map[string]float64{"x": 1.0}, nil, 0.0, true); got != stateUnpaired {
		t.Fatalf("classifyTrace(empty top) = %q; want %q", got, stateUnpaired)
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
	return mkArtifactSourced(t, traceID, model, "round3-challenge")
}

// mkArtifactSourced is mkArtifact with an explicit trace source, for tests that
// exercise per-stratum funnel and K-gate behavior.
func mkArtifactSourced(t *testing.T, traceID, model, source string) Artifact {
	t.Helper()
	a := Artifact{
		TraceID:        traceID,
		CandidateModel: model,
		Trace:          Trace{ID: traceID, Source: source, System: "s", Turns: []Turn{{Role: "user", Content: "q"}}},
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

func TestRunDiscriminationReport_ClassifiesFunnelAndWritesDerivedManifest(t *testing.T) {
	dir := t.TempDir()

	// Two R3-fresh traces: one valid discriminator, one saturated.
	arts := []Artifact{
		mkArtifact(t, "r3c-type-semantics-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3-coder-next:latest"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3.6:35b-a3b"),
		mkArtifact(t, "r3c-type-semantics-01", "openai-compat/glm-5.1"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3:8b"),
		mkArtifact(t, "r3c-stdlib-contract-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r3c-stdlib-contract-01", "ollama/qwen3-coder-next:latest"),
		mkArtifact(t, "r3c-stdlib-contract-01", "ollama/qwen3.6:35b-a3b"),
		mkArtifact(t, "r3c-stdlib-contract-01", "openai-compat/glm-5.1"),
		mkArtifact(t, "r3c-stdlib-contract-01", "ollama/qwen3:8b"),
	}
	quals := []float64{1.0, 0.5, 0.5, 0.5, 0.0 /*sat*/, 1.0, 1.0, 1.0, 1.0, 1.0}

	artPath := filepath.Join(dir, "artifacts.jsonl")
	labelPath := filepath.Join(dir, "labels.jsonl")
	writeArtifactsJSONL(t, artPath, arts)
	writeDiscriminationLabelsJSONL(t, labelPath, arts, quals)

	mPath := filepath.Join(dir, "m.jsonl")
	if err := writeManifest(mPath, Manifest{Entries: []ManifestEntry{
		{TraceID: "r3c-type-semantics-01", Partition: PartitionChallenge, Category: "type-semantics", Source: "round3-challenge", AllowedAsModelEvidence: true},
		{TraceID: "r3c-stdlib-contract-01", Partition: PartitionChallenge, Category: "stdlib-contract", Source: "round3-challenge", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "discriminators.jsonl")
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:               labelPath,
		ArtifactsPath:            artPath,
		ManifestPath:             mPath,
		TopModels:                []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:               "ollama/qwen3:8b",
		DiscriminatorManifestOut: outPath,
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "valid-discriminator") || !strings.Contains(report, "saturated") {
		t.Fatalf("report missing classification states:\n%s", report)
	}
	if !strings.Contains(report, "round3-challenge") {
		t.Fatalf("report missing R3-fresh stratum")
	}
	// K-gate line present with the count (1 valid of 2).
	if !strings.Contains(report, "VALID_DISCRIMINATORS=1") {
		t.Fatalf("report missing valid-discriminator count line:\n%s", report)
	}
	if !strings.Contains(report, "| r3c-stdlib-contract-01 | round3-challenge | saturated |  |") {
		t.Fatalf("saturated trace must appear as a per-trace row:\n%s", report)
	}

	derived, err := loadManifest(outPath)
	if err != nil {
		t.Fatalf("derived manifest must load: %v", err)
	}
	if len(derived.Entries) != 1 || derived.Entries[0].TraceID != "r3c-type-semantics-01" {
		t.Fatalf("derived manifest = %+v; want only the valid discriminator", derived.Entries)
	}
}

func TestRunDiscriminationReport_FailsLoudOnMissingTopLabel(t *testing.T) {
	dir := t.TempDir()
	// glm label absent for the trace -> unpaired/missing, surfaced loudly.
	arts := []Artifact{
		mkArtifact(t, "r3c-type-semantics-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3-coder-next:latest"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3.6:35b-a3b"),
		mkArtifact(t, "r3c-type-semantics-01", "ollama/qwen3:8b"),
	}
	quals := []float64{1.0, 0.5, 0.5, 0.0}
	artPath := filepath.Join(dir, "artifacts.jsonl")
	labelPath := filepath.Join(dir, "labels.jsonl")
	writeArtifactsJSONL(t, artPath, arts)
	writeDiscriminationLabelsJSONL(t, labelPath, arts, quals)
	mPath := filepath.Join(dir, "m.jsonl")
	if err := writeManifest(mPath, Manifest{Entries: []ManifestEntry{
		{TraceID: "r3c-type-semantics-01", Partition: PartitionChallenge, Category: "type-semantics", Source: "round3-challenge", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labelPath,
		ArtifactsPath: artPath,
		ManifestPath:  mPath,
		TopModels:     []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:    "ollama/qwen3:8b",
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "| r3c-type-semantics-01 | round3-challenge | unpaired/missing | missing: openai-compat/glm-5.1 |") {
		t.Fatalf("missing top label must be surfaced loudly with the model name:\n%s", report)
	}
}

func TestRunDiscriminationReport_KGateNANoRound3Stratum(t *testing.T) {
	dir := t.TempDir()
	arts := []Artifact{
		mkArtifact(t, "r2c-subtle-correctness-01", "ollama/gemma4:31b"),
		mkArtifact(t, "r2c-subtle-correctness-01", "ollama/qwen3-coder-next:latest"),
		mkArtifact(t, "r2c-subtle-correctness-01", "ollama/qwen3.6:35b-a3b"),
		mkArtifact(t, "r2c-subtle-correctness-01", "openai-compat/glm-5.1"),
		mkArtifact(t, "r2c-subtle-correctness-01", "ollama/qwen3:8b"),
	}
	quals := []float64{1.0, 0.5, 0.5, 0.5, 0.0}
	artPath := filepath.Join(dir, "artifacts.jsonl")
	labelPath := filepath.Join(dir, "labels.jsonl")
	writeArtifactsJSONL(t, artPath, arts)
	writeDiscriminationLabelsJSONL(t, labelPath, arts, quals)
	mPath := filepath.Join(dir, "m.jsonl")
	if err := writeManifest(mPath, Manifest{Entries: []ManifestEntry{
		{TraceID: "r2c-subtle-correctness-01", Partition: PartitionChallenge, Category: "subtle-correctness", Source: "round2-challenge", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "discriminators.jsonl")

	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:               labelPath,
		ArtifactsPath:            artPath,
		ManifestPath:             mPath,
		TopModels:                []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:               "ollama/qwen3:8b",
		DiscriminatorManifestOut: outPath,
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if strings.Contains(report, "VALID_DISCRIMINATORS=0 of K=10") {
		t.Fatalf("Round-2-only report must not print a fake Round-3 K-gate failure:\n%s", report)
	}
	if !strings.Contains(report, "K-gate (spec §9.2): N/A — no round3-challenge stratum present") {
		t.Fatalf("Round-2-only report missing K-gate N/A line:\n%s", report)
	}
	if !strings.Contains(report, "Derived manifest note: contains every valid-discriminator trace across all reported sources") {
		t.Fatalf("report missing derived manifest consumer note:\n%s", report)
	}
	if !strings.Contains(report, "sources seen:") || !strings.Contains(report, "round2-challenge") {
		t.Fatalf("N/A K-gate must name the sources actually present so a manifest typo is auditable:\n%s", report)
	}
}

func TestRunDiscriminationReport_RejectsOutOfDomainLabelAtLoad(t *testing.T) {
	// The {0,0.5,1.0} domain that classifyTrace's == 1.0 logic depends on is
	// enforced once, at load, for every mode. An out-of-domain quality (0.9) must
	// abort the discrimination report at load — never reach the classifier.
	labels, artifacts, manifest := discriminationFixture(t, "r3c-bad-01", "round3-challenge",
		[5]float64{0.9, 0.5, 0.5, 0.5, 0.0})
	_, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:    "ollama/qwen3:8b",
	})
	if err == nil || !strings.Contains(err.Error(), "expected_answer_quality") {
		t.Fatalf("out-of-domain label must be rejected at load; got %v", err)
	}
}

func TestResolvePanelSelector(t *testing.T) {
	present := map[string]struct{}{"ollama/gemma4:31b": {}, "openai-compat/glm-5.1": {}}
	cases := []struct {
		sel     string
		want    string
		wantErr string
	}{
		{sel: "ollama/gemma4:31b", want: "ollama/gemma4:31b"}, // exact
		{sel: "gemma4:31b", want: "ollama/gemma4:31b"},        // prefix omitted
		{sel: "GLM-5.1", want: "openai-compat/glm-5.1"},       // prefix omitted + case
		{sel: "nope:1", want: "nope:1"},                       // unresolved -> normalized, surfaces as unpaired
	}
	for _, c := range cases {
		got, err := resolvePanelSelector(c.sel, present)
		if err != nil {
			t.Fatalf("resolvePanelSelector(%q) error: %v", c.sel, err)
		}
		if got != c.want {
			t.Fatalf("resolvePanelSelector(%q) = %q; want %q", c.sel, got, c.want)
		}
	}

	// Ambiguous: same base model under two transports, unprefixed selector.
	amb := map[string]struct{}{"ollama/qwen3.6:35b-a3b": {}, "openai-compat/qwen3.6:35b-a3b": {}}
	if _, err := resolvePanelSelector("qwen3.6:35b-a3b", amb); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous selector must error loudly; got %v", err)
	}
}

// discriminationFixture writes a single-trace, five-model artifact+label+manifest
// triple and returns the paths, so the source/quality-specific cases stay terse.
func discriminationFixture(t *testing.T, traceID, source string, quals [5]float64) (labels, artifacts, manifest string) {
	t.Helper()
	dir := t.TempDir()
	models := []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1", "ollama/qwen3:8b"}
	arts := make([]Artifact, len(models))
	for i, m := range models {
		arts[i] = mkArtifactSourced(t, traceID, m, source)
	}
	artifacts = filepath.Join(dir, "artifacts.jsonl")
	labels = filepath.Join(dir, "labels.jsonl")
	manifest = filepath.Join(dir, "m.jsonl")
	writeArtifactsJSONL(t, artifacts, arts)
	writeDiscriminationLabelsJSONL(t, labels, arts, quals[:])
	if err := writeManifest(manifest, Manifest{Entries: []ManifestEntry{
		{TraceID: traceID, Partition: PartitionChallenge, Category: "x", Source: source, AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}
	return labels, artifacts, manifest
}

func TestRunDiscriminationReport_ResolvesPrefixOmittedPanelSelectors(t *testing.T) {
	// The rest of the tool treats "ollama/gemma4:31b" and "gemma4:31b" as equal
	// (modelSelectorsEqual). Discrimination must too: a prefix-omitted panel
	// selector must resolve to the prefixed artifact key, not silently classify
	// the trace as unpaired and zero the gate.
	labels, artifacts, manifest := discriminationFixture(t, "r3c-type-semantics-01", "round3-challenge",
		[5]float64{1.0, 0.5, 0.5, 0.5, 0.0})
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"gemma4:31b", "qwen3-coder-next:latest", "qwen3.6:35b-a3b", "glm-5.1"}, // no transport prefixes
		FloorModel:    "qwen3:8b",
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "VALID_DISCRIMINATORS=1") {
		t.Fatalf("prefix-omitted selectors must resolve and the trace must count as a valid discriminator:\n%s", report)
	}
	if strings.Contains(report, "unpaired/missing | missing:") {
		t.Fatalf("prefix-omitted selectors must not surface as missing labels:\n%s", report)
	}
}

func TestRunDiscriminationReport_GateSourceOverride(t *testing.T) {
	// A non-default round (e.g. an interim round2 anchor re-gate) can assert the
	// gate stratum via GateSource without a code change.
	labels, artifacts, manifest := discriminationFixture(t, "r2c-subtle-correctness-01", "round2-challenge",
		[5]float64{1.0, 0.5, 0.5, 0.5, 0.0})
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:    "ollama/qwen3:8b",
		GateSource:    "round2-challenge",
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "VALID_DISCRIMINATORS=1 of K=10") {
		t.Fatalf("GateSource must move the K-gate onto round2-challenge:\n%s", report)
	}
}

func TestRunDiscriminationReport_FlagsFloorInversion(t *testing.T) {
	// The floor model outscoring the whole top cluster is a data anomaly, not a
	// routine floor-only spread; it must be surfaced, not silently bucketed.
	labels, artifacts, manifest := discriminationFixture(t, "r3c-anomaly-01", "round3-challenge",
		[5]float64{0.5, 0.5, 0.5, 0.5, 1.0}) // floor (qwen3:8b) = 1.0 > top = 0.5
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:    "ollama/qwen3:8b",
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "outscored top cluster") {
		t.Fatalf("floor-beats-top inversion must be flagged in the per-trace detail:\n%s", report)
	}
}

func TestRunDiscriminationReport_FlagsFloorInversionInUnsolved(t *testing.T) {
	// The scarier inversion: the top cluster splits, no top model solves it
	// (unsolved), yet the cheap floor model scored 1.0. This must be flagged just
	// like the floor-only inversion, not hidden because the state differs.
	labels, artifacts, manifest := discriminationFixture(t, "r3c-unsolved-anomaly-01", "round3-challenge",
		[5]float64{0.5, 0.0, 0.5, 0.0, 1.0}) // top splits, none 1.0; floor (qwen3:8b) = 1.0
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:    "ollama/qwen3:8b",
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if !strings.Contains(report, "| r3c-unsolved-anomaly-01 | round3-challenge | unsolved | anomaly: floor outscored top cluster |") {
		t.Fatalf("floor beating an unsolved top cluster must be flagged:\n%s", report)
	}
}

func TestRunDiscriminationReport_AmbiguousSelectorErrors(t *testing.T) {
	// Same base model captured under two transports + an unprefixed selector =
	// genuinely ambiguous operator input. Fail loud rather than silently miss and
	// deflate the gate.
	dir := t.TempDir()
	arts := []Artifact{
		mkArtifactSourced(t, "r3c-amb-01", "ollama/qwen3.6:35b-a3b", "round3-challenge"),
		mkArtifactSourced(t, "r3c-amb-01", "openai-compat/qwen3.6:35b-a3b", "round3-challenge"),
	}
	artifacts := filepath.Join(dir, "a.jsonl")
	labels := filepath.Join(dir, "l.jsonl")
	manifest := filepath.Join(dir, "m.jsonl")
	writeArtifactsJSONL(t, artifacts, arts)
	writeDiscriminationLabelsJSONL(t, labels, arts, []float64{1.0, 0.5})
	if err := writeManifest(manifest, Manifest{Entries: []ManifestEntry{
		{TraceID: "r3c-amb-01", Partition: PartitionChallenge, Category: "x", Source: "round3-challenge", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}
	_, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:    labels,
		ArtifactsPath: artifacts,
		ManifestPath:  manifest,
		TopModels:     []string{"qwen3.6:35b-a3b"}, // ambiguous: matches both transports
		FloorModel:    "ollama/qwen3:8b",
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous panel selector must error loudly; got %v", err)
	}
}

func TestRunDiscriminationReport_SuppressesNoteWhenZeroDiscriminators(t *testing.T) {
	// With zero valid discriminators the derived manifest is empty (loadManifest
	// rejects empty), so the "go consume it" note would send the operator into an
	// error. Suppress it and state the empty result instead.
	labels, artifacts, manifest := discriminationFixture(t, "r3c-saturated-01", "round3-challenge",
		[5]float64{1.0, 1.0, 1.0, 1.0, 1.0}) // saturated -> zero valid discriminators
	dir := t.TempDir()
	outPath := filepath.Join(dir, "discriminators.jsonl")
	report, err := runDiscriminationReport(discriminationOptions{
		LabelsPath:               labels,
		ArtifactsPath:            artifacts,
		ManifestPath:             manifest,
		TopModels:                []string{"ollama/gemma4:31b", "ollama/qwen3-coder-next:latest", "ollama/qwen3.6:35b-a3b", "openai-compat/glm-5.1"},
		FloorModel:               "ollama/qwen3:8b",
		DiscriminatorManifestOut: outPath,
	})
	if err != nil {
		t.Fatalf("runDiscriminationReport: %v", err)
	}
	if strings.Contains(report, "Derived manifest note: contains every valid-discriminator") {
		t.Fatalf("zero-discriminator report must not tell the operator to consume an empty manifest:\n%s", report)
	}
	if !strings.Contains(report, "0 valid discriminators") {
		t.Fatalf("zero-discriminator report must state the empty result:\n%s", report)
	}
}

func TestBuildStratumFunnels_DeduplicatesTraceAcrossEntries(t *testing.T) {
	// A trace ID appearing in more than one manifest entry must be counted once,
	// or Authored/Captured silently inflate past the unique trace count.
	m := Manifest{Entries: []ManifestEntry{
		{TraceID: "dup-01", Partition: PartitionChallenge, Category: "a", Source: "round3-challenge", AllowedAsModelEvidence: true},
		{TraceID: "dup-01", Partition: PartitionChallenge, Category: "b", Source: "round3-challenge", AllowedAsModelEvidence: true},
	}}
	captured := map[string]bool{"dup-01": true}
	cls := []traceClassification{{TraceID: "dup-01", Source: "round3-challenge", State: stateSaturated}}
	funnels := buildStratumFunnels(m, captured, cls)
	if len(funnels) != 1 {
		t.Fatalf("want 1 stratum; got %d", len(funnels))
	}
	if funnels[0].Authored != 1 || funnels[0].Captured != 1 {
		t.Fatalf("authored/captured = %d/%d; want 1/1 (deduplicated by trace ID)", funnels[0].Authored, funnels[0].Captured)
	}
}
