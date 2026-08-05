package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureProvenanceHashBindsEveryExcludedField(t *testing.T) {
	temp, seed := 0.0, 7
	base := Artifact{
		TraceID:        "pair-a-legacy",
		CandidateModel: "ollama/m",
		CapturedAt:     time.Date(2026, 8, 5, 12, 0, 0, 123, time.UTC),
		Capture: &CaptureProvenance{
			OrderIndex: 0, Temperature: &temp, Seed: &seed,
			Transport: "ollama", Model: "ollama/m",
			ModelDigest:  "sha256:" + strings.Repeat("a", 64),
			PromptTokens: 10, GenTokens: 2, CapturedOrder: "legacy-first",
		},
	}
	artifactIdentity := artifactHash(base)
	want := captureProvenanceHash(base)
	mutations := map[string]func(*Artifact){
		"captured_at":    func(a *Artifact) { a.CapturedAt = a.CapturedAt.Add(time.Nanosecond) },
		"capture_nil":    func(a *Artifact) { a.Capture = nil },
		"order_index":    func(a *Artifact) { a.Capture.OrderIndex++ },
		"temperature":    func(a *Artifact) { v := 0.5; a.Capture.Temperature = &v },
		"seed":           func(a *Artifact) { v := 8; a.Capture.Seed = &v },
		"transport":      func(a *Artifact) { a.Capture.Transport = "openai-compat" },
		"model":          func(a *Artifact) { a.Capture.Model = "ollama/other" },
		"model_digest":   func(a *Artifact) { a.Capture.ModelDigest = "sha256:" + strings.Repeat("b", 64) },
		"prompt_tokens":  func(a *Artifact) { a.Capture.PromptTokens++ },
		"gen_tokens":     func(a *Artifact) { a.Capture.GenTokens++ },
		"captured_order": func(a *Artifact) { a.Capture.CapturedOrder = "mixed-first" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			got := cloneCaptureArtifacts(t, []Artifact{base})[0]
			mutate(&got)
			if hash := captureProvenanceHash(got); hash == want {
				t.Fatalf("captureProvenanceHash unchanged after %s mutation", name)
			}
			if hash := artifactHash(got); hash != artifactIdentity {
				t.Fatalf("artifactHash changed after output-only provenance mutation: %s != %s", hash, artifactIdentity)
			}
		})
	}
}

func TestAssemblyReportV2RejectsCaptureProvenanceTampering(t *testing.T) {
	baseArts, labels, baseManifest := v2BindingFixture(t)
	run := func(t *testing.T, arts []Artifact, manifest captureManifest) error {
		t.Helper()
		dir := t.TempDir()
		artifactsPath := filepath.Join(dir, "artifacts.jsonl")
		labelsPath := filepath.Join(dir, "labels.jsonl")
		if err := writeJSONLRows(artifactsPath, "artifacts", arts); err != nil {
			t.Fatal(err)
		}
		if err := writeLabelsJSONL(labelsPath, labels); err != nil {
			t.Fatal(err)
		}
		_, err := runAssemblyReport(assemblyReportOptions{
			LabelsPath: labelsPath, ArtifactsPath: artifactsPath,
			CaptureManifestPath: writeLedgerManifest(t, dir, "manifest.json", manifest),
		})
		return err
	}
	if err := run(t, baseArts, baseManifest); err != nil {
		t.Fatalf("valid v2 fixture: %v", err)
	}

	rawMutations := map[string]func(*Artifact){
		"captured_at":    func(a *Artifact) { a.CapturedAt = a.CapturedAt.Add(time.Second) },
		"capture_nil":    func(a *Artifact) { a.Capture = nil },
		"order_index":    func(a *Artifact) { a.Capture.OrderIndex++ },
		"temperature":    func(a *Artifact) { v := 0.5; a.Capture.Temperature = &v },
		"seed":           func(a *Artifact) { v := 1; a.Capture.Seed = &v },
		"transport":      func(a *Artifact) { a.Capture.Transport = "openai-compat" },
		"model":          func(a *Artifact) { a.Capture.Model = "ollama/other" },
		"model_digest":   func(a *Artifact) { a.Capture.ModelDigest = "sha256:" + strings.Repeat("b", 64) },
		"prompt_tokens":  func(a *Artifact) { a.Capture.PromptTokens++ },
		"gen_tokens":     func(a *Artifact) { a.Capture.GenTokens++ },
		"captured_order": func(a *Artifact) { a.Capture.CapturedOrder = "mixed-first" },
	}
	for name, mutate := range rawMutations {
		t.Run("hash binds "+name, func(t *testing.T) {
			arts := cloneCaptureArtifacts(t, baseArts)
			mutate(&arts[0])
			if err := run(t, arts, cloneCaptureManifest(baseManifest)); err == nil || !strings.Contains(err.Error(), "provenance_hash") {
				t.Fatalf("err = %v; want provenance_hash rejection", err)
			}
		})
	}

	semanticCases := []struct {
		name, want string
		mutate     func([]Artifact, *captureManifest)
	}{
		{"order index", "order_index", func(arts []Artifact, _ *captureManifest) { arts[0].Capture.OrderIndex++ }},
		{"usage present", "usage_present", func(arts []Artifact, _ *captureManifest) { arts[0].Capture.PromptTokens = 0 }},
		{"model", "model", func(arts []Artifact, _ *captureManifest) { arts[0].Capture.Model = "ollama/other" }},
		{"digest", "model_digest", func(arts []Artifact, _ *captureManifest) {
			arts[0].Capture.ModelDigest = "sha256:" + strings.Repeat("b", 64)
		}},
		{"bare transport identity", "transport", func(arts []Artifact, _ *captureManifest) { arts[0].Capture.Transport = "openai-compat" }},
		{"different endpoint transport identity", "transport", func(arts []Artifact, _ *captureManifest) { arts[0].Capture.Transport = "openai-compat:ffffffffffff" }},
		{"target", "model_targets", func(_ []Artifact, m *captureManifest) { m.ModelTargets[0].Selector = "ollama/m" }},
		{"registered decoding", "temperature", func(arts []Artifact, m *captureManifest) {
			m.Decoding.Temperature = 0.5
			for i := range arts {
				v := 0.5
				arts[i].Capture.Temperature = &v
			}
		}},
		{"unsupported seed", "seed", func(arts []Artifact, m *captureManifest) {
			m.Decoding.SeedSupported = true
			for i := range arts {
				v := 1
				arts[i].Capture.Seed = &v
			}
		}},
		{"derived captured order", "captured_order", func(arts []Artifact, _ *captureManifest) {
			for i := range arts {
				arts[i].Capture.CapturedOrder = "mixed-first"
			}
		}},
	}
	for _, tc := range semanticCases {
		t.Run("reconciles "+tc.name, func(t *testing.T) {
			arts := cloneCaptureArtifacts(t, baseArts)
			manifest := cloneCaptureManifest(baseManifest)
			tc.mutate(arts, &manifest)
			for i := range arts {
				manifest.PerArtifact[i].ProvenanceHash = captureProvenanceHash(arts[i])
			}
			if err := run(t, arts, manifest); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want semantic rejection containing %q", err, tc.want)
			}
		})
	}
}

func TestCaptureManifestSanitizesEndpointsBeforePublication(t *testing.T) {
	const (
		ollamaRaw = "https://ollama-user:OLLAMA_SECRET@ollama.test/root?token=OLLAMA_QUERY_SECRET#OLLAMA_FRAGMENT_SECRET"
		compatRaw = "https://compat-user:COMPAT_SECRET@compat.test/v1?api_key=COMPAT_QUERY_SECRET#COMPAT_FRAGMENT_SECRET"
	)
	tests := []struct {
		name, wantEndpoint   string
		targets              []ModelTarget
		ollamaURL, compatURL string
		allFailed            bool
	}{
		{"ollama", "https://ollama.test/root", []ModelTarget{{Display: "ollama/m", Provider: "ollama", Model: "m"}}, ollamaRaw, "", false},
		{"compat", "https://compat.test/v1", []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}}, "", compatRaw, false},
		{"unused malformed compat ignored", "https://ollama.test/root", []ModelTarget{{Display: "ollama/m", Provider: "ollama", Model: "m"}}, ollamaRaw, "https://COMPAT_SECRET@%zz.invalid", false},
		{"unused malformed ollama ignored", "https://compat.test/v1", []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}}, "https://OLLAMA_SECRET@%zz.invalid", compatRaw, false},
		{"mixed", "https://ollama.test/root https://compat.test/v1", []ModelTarget{
			{Display: "ollama/m", Provider: "ollama", Model: "m"},
			{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"},
		}, ollamaRaw, compatRaw, false},
		{"all failed", "https://ollama.test/root", []ModelTarget{{Display: "ollama/m", Provider: "ollama", Model: "m"}}, ollamaRaw, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "artifacts.jsonl")
			var runner calibrationRunner = &orderRecordingRunner{}
			if tc.allFailed {
				runner = &failingRunner{fail: map[string]bool{"arm-legacy": true}}
			}
			err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
				Runner: runner, Targets: tc.targets,
				Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
				OutputPath: out, OllamaURL: tc.ollamaURL, OpenAICompatBaseURL: tc.compatURL, Stdout: io.Discard,
			})
			if tc.allFailed {
				if err == nil || !strings.Contains(err.Error(), "no artifacts written") {
					t.Fatalf("all-failed err = %v", err)
				}
			} else if err != nil {
				t.Fatalf("runCalibrateCapture: %v", err)
			}
			raw, readErr := os.ReadFile(captureManifestPath(out))
			if readErr != nil {
				t.Fatalf("read manifest: %v", readErr)
			}
			var manifest captureManifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Endpoint != tc.wantEndpoint {
				t.Fatalf("endpoint = %q; want %q", manifest.Endpoint, tc.wantEndpoint)
			}
			for _, secret := range []string{"OLLAMA_SECRET", "OLLAMA_QUERY_SECRET", "OLLAMA_FRAGMENT_SECRET", "COMPAT_SECRET", "COMPAT_QUERY_SECRET", "COMPAT_FRAGMENT_SECRET"} {
				if strings.Contains(string(raw), secret) || strings.Contains(errString(err), secret) {
					t.Fatalf("secret %q reached manifest/error", secret)
				}
			}
		})
	}
}

func TestCaptureRejectsMalformedEndpointWithoutEchoOrPublication(t *testing.T) {
	const secret = "ENDPOINT_SECRET_DO_NOT_ECHO"
	runner := &orderRecordingRunner{}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     runner,
		Targets:    []ModelTarget{{Display: "ollama/m", Provider: "ollama", Model: "m"}},
		Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
		OutputPath: out, OllamaURL: "https://user:" + secret + "@%zz.invalid/path?token=" + secret,
		Stdout: io.Discard,
	})
	if err == nil || err.Error() != "calibrate-capture: invalid capture endpoint" || strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %q; want fixed non-echoing endpoint error", errString(err))
	}
	if len(runner.gotTraceIDs) != 0 {
		t.Fatalf("runner called before endpoint validation: %v", runner.gotTraceIDs)
	}
	for _, path := range []string{out, captureManifestPath(out)} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s published after malformed endpoint: %v", path, statErr)
		}
	}
}

func TestCaptureReportTransportIdentitySurvivesForceQuerySanitization(t *testing.T) {
	const rawEndpoint = "https://compat.test/v1?"
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	runner := &providerIdentityRunner{provider: openAICompatCandidateProviderName(rawEndpoint)}
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:  runner,
		Targets: []ModelTarget{{Display: "openai-compat/m", Provider: openAICompatTransport, Model: "m"}},
		Traces: []Trace{
			pairedCaptureTrace("pair-a-legacy", "pair-a", AssemblyLegacy),
			pairedCaptureTrace("pair-a-mixed", "pair-a", AssemblyMixed),
		},
		OutputPath: out, OpenAICompatBaseURL: rawEndpoint, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	arts := readArtifactsFile(t, out)
	labels := make([]Label, 0, len(arts))
	for _, artifact := range arts {
		labels = append(labels, labelFor(artifact, 1))
	}
	labelsPath := filepath.Join(t.TempDir(), "labels.jsonl")
	if err := writeLabelsJSONL(labelsPath, labels); err != nil {
		t.Fatal(err)
	}
	if _, err := runAssemblyReport(assemblyReportOptions{
		LabelsPath: labelsPath, ArtifactsPath: out,
		CaptureManifestPath: captureManifestPath(out),
	}); err != nil {
		t.Fatalf("report rejected its own ForceQuery-sanitized capture: %v", err)
	}
}

type providerIdentityRunner struct {
	provider string
}

func (r *providerIdentityRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	results := make([]Result, 0, len(targets)*len(traces))
	for _, target := range targets {
		for _, trace := range traces {
			results = append(results, Result{
				Model: target.Display, TraceID: trace.ID, CandidateProvider: r.provider,
				Transcript: []Turn{{Role: "assistant", Content: "answer"}},
				Score:      Score{PromptEvalTokens: 10, GenTokens: 2},
			})
		}
	}
	return results, nil
}

func v2BindingFixture(t *testing.T) ([]Artifact, []Label, captureManifest) {
	t.Helper()
	temp := 0.0
	endpoint := "https://compat.test/v1"
	capturedAt := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	legacy := mixedArtifact("pair-a", AssemblyLegacy, "openai-compat/m", nil)
	mixed := mixedArtifact("pair-a", AssemblyMixed, "openai-compat/m", nil)
	for i, artifact := range []*Artifact{&legacy, &mixed} {
		artifact.CapturedAt = capturedAt.Add(time.Duration(i) * time.Second)
		artifact.Capture = &CaptureProvenance{
			OrderIndex: i, Temperature: &temp,
			Transport: openAICompatCandidateProviderName(endpoint), Model: "openai-compat/m",
			PromptTokens: 10, GenTokens: 2, CapturedOrder: "legacy-first",
		}
	}
	arts := []Artifact{legacy, mixed}
	manifest := captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2,
		CreatedAt:     capturedAt, Endpoint: endpoint, Transport: openAICompatTransport,
		ModelTargets:         []captureManifestTarget{{Selector: "openai-compat/m"}},
		Decoding:             captureDecoding{Temperature: 0, SeedSupported: false},
		CounterbalanceScheme: captureCounterbalanceScheme,
		ArtifactCount:        2,
		ExpectedCount:        2,
	}
	for i, artifact := range arts {
		manifest.PerArtifact = append(manifest.PerArtifact, captureManifestRow{
			TraceID: artifact.TraceID, ArtifactHash: artifact.ArtifactHash,
			OrderIndex: i, UsagePresent: true, ProvenanceHash: captureProvenanceHash(artifact),
		})
		manifest.Expected = append(manifest.Expected, captureExpectedRow{
			TraceID: artifact.TraceID, Model: "openai-compat/m", PairID: "pair-a",
			Arm: string(artifact.Trace.AssemblyEval.Mode), Status: "captured", Attempts: 1,
			ArtifactHash: artifact.ArtifactHash,
		})
	}
	return arts, []Label{labelFor(legacy, 0), labelFor(mixed, 1)}, manifest
}

func cloneCaptureArtifacts(t *testing.T, arts []Artifact) []Artifact {
	t.Helper()
	raw, err := json.Marshal(arts)
	if err != nil {
		t.Fatal(err)
	}
	var clone []Artifact
	if err := json.Unmarshal(raw, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func cloneCaptureManifest(m captureManifest) captureManifest {
	clone := m
	clone.ModelTargets = append([]captureManifestTarget(nil), m.ModelTargets...)
	clone.ServerProbe.OllamaDigests = make(map[string]string, len(m.ServerProbe.OllamaDigests))
	for k, v := range m.ServerProbe.OllamaDigests {
		clone.ServerProbe.OllamaDigests[k] = v
	}
	clone.PerArtifact = append([]captureManifestRow(nil), m.PerArtifact...)
	clone.Expected = append([]captureExpectedRow(nil), m.Expected...)
	return clone
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
