package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	nestedOllamaModel = "ollama/openai-compat/m"
	compatModel       = "openai-compat/m"
)

func TestCanonicalCandidateModelKeyProviderCollisions(t *testing.T) {
	tests := []struct {
		selector string
		want     string
	}{
		{"m", "m"},
		{"ollama/m", "m"},
		{"OLLAMA / M", "m"},
		{"openai-compat/M", compatModel},
		{"ollama/openai-compat/M", nestedOllamaModel},
		{"OLLAMA / OPENAI-COMPAT/M", nestedOllamaModel},
		{"ollama/ollama/M", "ollama/ollama/m"},
	}
	for _, tc := range tests {
		t.Run(strings.ReplaceAll(tc.selector, "/", "_"), func(t *testing.T) {
			if got := canonicalCandidateModelKey(tc.selector); got != tc.want {
				t.Fatalf("canonicalCandidateModelKey(%q) = %q; want %q", tc.selector, got, tc.want)
			}
		})
	}
	if canonicalCandidateModelKey(nestedOllamaModel) == canonicalCandidateModelKey(compatModel) {
		t.Fatal("provider-distinct targets have the same canonical candidate-model key")
	}
}

func TestCanonicalCandidateModelKeyRoundTripsParsedTargets(t *testing.T) {
	selectors := []string{
		"m",
		"ollama/m",
		compatModel,
		nestedOllamaModel,
		"ollama/ollama/m",
		"openai-compat/ollama/m",
		"openai-compat/openai-compat/m",
	}
	seen := map[string][2]string{}
	for _, selector := range selectors {
		target, err := parseModelTarget(selector)
		if err != nil {
			t.Fatalf("parseModelTarget(%q): %v", selector, err)
		}
		wantTarget := canonicalModelTargetKey(target)
		key := canonicalCandidateModelKey(selector)
		roundTrip, err := parseModelTarget(key)
		if err != nil {
			t.Fatalf("parseModelTarget(canonicalCandidateModelKey(%q)=%q): %v", selector, key, err)
		}
		if gotTarget := canonicalModelTargetKey(roundTrip); gotTarget != wantTarget {
			t.Fatalf("target %q round-tripped as %v; want %v", selector, gotTarget, wantTarget)
		}
		if got := canonicalCandidateModelKey(key); got != key {
			t.Fatalf("canonical key %q is not stable: second pass = %q", key, got)
		}
		if prior, ok := seen[key]; ok && prior != wantTarget {
			t.Fatalf("supported targets %v and %v collide at key %q", prior, wantTarget, key)
		}
		seen[key] = wantTarget
	}
}

func TestCalibrateCaptureProviderLikeOllamaModelWritesLoadableV2Ledger(t *testing.T) {
	targets, err := parseModelTargets(nestedOllamaModel + "," + compatModel)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:              &orderRecordingRunner{},
		Targets:             targets,
		Traces:              []Trace{pairedCaptureTrace("pair-a-legacy", "pair-a", AssemblyLegacy)},
		OutputPath:          out,
		OllamaURL:           "https://ollama.test",
		OpenAICompatBaseURL: "https://compat.test/v1",
		Stdout:              io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}

	raw, err := os.ReadFile(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	var manifest captureManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != captureManifestSchemaVersionV2 || len(manifest.Expected) != 2 {
		t.Fatalf("manifest schema/ledger = %q/%d rows; want v2 with two rows", manifest.SchemaVersion, len(manifest.Expected))
	}
	_, verify, err := loadCaptureManifestForReport(captureManifestPath(out))
	if err != nil {
		t.Fatalf("loadCaptureManifestForReport rejected capture output: %v", err)
	}
	if verify == nil || len(verify.expectedPairs) != 2 {
		t.Fatalf("expected pair keys = %+v; want one key per provider-distinct target", verify)
	}
	gotModels := map[string]bool{}
	for _, pair := range verify.expectedPairs {
		gotModels[pair.model] = true
	}
	for _, want := range []string{nestedOllamaModel, compatModel} {
		if !gotModels[want] {
			t.Errorf("loadable ledger pair keys = %+v; missing model %q", verify.expectedPairs, want)
		}
	}
}

func providerCollisionArtifacts() []Artifact {
	var arts []Artifact
	for _, model := range []string{nestedOllamaModel, compatModel} {
		for _, mode := range []AssemblyMode{AssemblyLegacy, AssemblyMixed} {
			arts = append(arts, mixedArtifact("pair-collision", mode, model, nil))
		}
	}
	return arts
}

func TestFilterArtifactsByModelIsolatesProviderLikeOllamaModel(t *testing.T) {
	arts := providerCollisionArtifacts()
	for _, selector := range []string{nestedOllamaModel, compatModel} {
		got, err := filterArtifactsByModel(arts, selector)
		if err != nil {
			t.Fatalf("filterArtifactsByModel(%q): %v", selector, err)
		}
		if len(got) != 2 {
			t.Fatalf("filterArtifactsByModel(%q) returned %d artifacts; want 2", selector, len(got))
		}
		for _, artifact := range got {
			if artifact.CandidateModel != selector {
				t.Errorf("filterArtifactsByModel(%q) crossed into %q", selector, artifact.CandidateModel)
			}
		}
	}
}

func TestForcedChoiceKeepsProviderLikeOllamaModelSeparate(t *testing.T) {
	arts := providerCollisionArtifacts()
	pairs, err := collectFCPairs(arts)
	if err != nil {
		t.Fatalf("collectFCPairs: %v", err)
	}
	if len(pairs) != 2 || pairs[0].model == pairs[1].model {
		t.Fatalf("pairs = %+v; want two provider-distinct model groups", pairs)
	}

	sidemap, err := generateFCSidemap(arts)
	if err != nil {
		t.Fatalf("generateFCSidemap: %v", err)
	}
	if len(sidemap.Pairs) != 2 {
		t.Fatalf("sidemap pairs = %+v; want two provider-distinct entries", sidemap.Pairs)
	}
	for _, model := range []string{nestedOllamaModel, compatModel} {
		if _, ok := sidemap.Pairs[fcSidemapKey("pair-collision", model)]; !ok {
			t.Errorf("sidemap missing provider-distinct model %q", model)
		}
	}
	sidemap.verifiedDigest = testFCSidemapDigest
	worksheet, err := renderForcedChoiceWorksheet(arts, sidemap)
	if err != nil {
		t.Fatalf("renderForcedChoiceWorksheet: %v", err)
	}
	for _, model := range []string{nestedOllamaModel, compatModel} {
		header := "=== PAIR pair-collision " + model + " ==="
		if strings.Count(worksheet, header) != 1 {
			t.Fatalf("worksheet contains %d copies of %q; want 1", strings.Count(worksheet, header), header)
		}
		worksheet = fillWorksheetField(t, worksheet, header, "prefer", "A")
	}
	rows, skipped, err := ingestForcedChoiceWorksheet(worksheet, arts, "tester", sidemap, true)
	if err != nil {
		t.Fatalf("ingestForcedChoiceWorksheet: %v", err)
	}
	if len(rows) != 2 || skipped != 0 {
		t.Fatalf("ingested rows/skipped = %d/%d; want 2/0", len(rows), skipped)
	}
	artifactModels := make(map[string]string, len(arts))
	for _, artifact := range arts {
		artifactModels[artifact.ArtifactHash] = modelKey(artifact.CandidateModel)
	}
	seenModels := map[string]bool{}
	for _, row := range rows {
		seenModels[row.CandidateModel] = true
		if artifactModels[row.ArtifactHashA] != row.CandidateModel || artifactModels[row.ArtifactHashB] != row.CandidateModel {
			t.Errorf("row %q crossed provider artifacts: %q/%q", row.CandidateModel, artifactModels[row.ArtifactHashA], artifactModels[row.ArtifactHashB])
		}
	}
	for _, want := range []string{nestedOllamaModel, compatModel} {
		if !seenModels[want] {
			t.Errorf("ingested models = %v; missing %q", seenModels, want)
		}
	}
}
