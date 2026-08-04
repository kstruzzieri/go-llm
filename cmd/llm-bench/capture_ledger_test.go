package main

// Capture-manifest ledger (#331 slice 3c, external PR review P1): failed
// capture runs previously vanished from BOTH the artifacts and the manifest,
// so a pair whose two arms both failed disappeared with no missing-arm
// exclusion (the report discovers pairs from artifacts alone). The v2
// manifest schema records an `expected` ledger row per (trace x model);
// the report cross-checks it and synthesizes vanished pairs into the
// registered missing-arm exclusions. v1 manifests keep exactly the sealed
// run's semantics.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingRunner returns one Result per (target, trace) like the real runner,
// failing the trace IDs in fail with a canned error.
type failingRunner struct{ fail map[string]bool }

func (f *failingRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	var results []Result
	for _, target := range targets {
		for _, tr := range traces {
			r := Result{Model: target.Display, TraceID: tr.ID, CandidateProvider: target.Provider}
			if f.fail[tr.ID] {
				r.Err = fmt.Errorf("replay timeout for %s", tr.ID)
			} else {
				r.Transcript = []Turn{{Role: "assistant", Content: "ans " + tr.ID}}
				r.Score = Score{PromptEvalTokens: 10, GenTokens: 2}
			}
			results = append(results, r)
		}
	}
	return results, nil
}

func TestCaptureManifestV2ExpectedLedger(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("half-legacy", "pair-half", AssemblyLegacy),
		pairedCaptureTrace("half-mixed", "pair-half", AssemblyMixed),
		pairedCaptureTrace("gone-legacy", "pair-gone", AssemblyLegacy),
		pairedCaptureTrace("gone-mixed", "pair-gone", AssemblyMixed),
		toplineCaptureTrace("top-1"),
		{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     &failingRunner{fail: map[string]bool{"half-mixed": true, "gone-legacy": true, "gone-mixed": true}},
		Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:     traces,
		OutputPath: out,
		Stdout:     io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	raw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != captureManifestSchemaVersionV2 {
		t.Errorf("schema_version = %q; want %q (new captures write the ledger schema)", m.SchemaVersion, captureManifestSchemaVersionV2)
	}
	if m.ExpectedCount != 6 || len(m.Expected) != 6 {
		t.Fatalf("expected_count/len(expected) = %d/%d; want 6/6 (one ledger row per trace x model, failures included)", m.ExpectedCount, len(m.Expected))
	}
	// artifact_count semantics unchanged: successful captures only.
	if m.ArtifactCount != 3 || len(m.PerArtifact) != 3 {
		t.Fatalf("artifact_count/per_artifact = %d/%d; want 3/3 (successes only)", m.ArtifactCount, len(m.PerArtifact))
	}
	arts := readArtifactsFile(t, out)
	hashByID := map[string]string{}
	for _, a := range arts {
		hashByID[a.TraceID] = a.ArtifactHash
	}
	rows := map[string]captureExpectedRow{}
	for _, row := range m.Expected {
		rows[row.TraceID] = row
	}
	type want struct {
		status, pairID, arm string
	}
	wants := map[string]want{
		"half-legacy": {"captured", "pair-half", "legacy"},
		"half-mixed":  {"failed", "pair-half", "mixed"},
		"gone-legacy": {"failed", "pair-gone", "legacy"},
		"gone-mixed":  {"failed", "pair-gone", "mixed"},
		"top-1":       {"captured", "", "topline"},
		"plain-t":     {"captured", "", ""},
	}
	for id, w := range wants {
		row, ok := rows[id]
		if !ok {
			t.Errorf("no ledger row for %s", id)
			continue
		}
		if row.Model != "m" || row.Status != w.status || row.PairID != w.pairID || row.Arm != w.arm || row.Attempts != 1 {
			t.Errorf("row %s = %+v; want model m, status %s, pair %q, arm %q, attempts 1", id, row, w.status, w.pairID, w.arm)
		}
		if w.status == "failed" {
			if row.Error == "" || row.ArtifactHash != "" {
				t.Errorf("failed row %s = %+v; want a recorded error and no artifact hash", id, row)
			}
		} else {
			if row.Error != "" || row.ArtifactHash != hashByID[id] {
				t.Errorf("captured row %s = %+v; want no error and the written artifact hash %q", id, row, hashByID[id])
			}
		}
	}

	// Round trip: the ledger-bearing manifest loads for report verification.
	ref, verify, err := loadCaptureManifestForReport(out + ".manifest.json")
	if err != nil {
		t.Fatalf("loadCaptureManifestForReport on v2: %v", err)
	}
	if ref.ArtifactCount != 3 || verify == nil {
		t.Fatalf("ref/verify = %+v/%v; want the artifact count and a verification set", ref, verify)
	}
	if len(verify.expectedPairs) != 2 {
		t.Fatalf("expectedPairs = %+v; want the two legacy-mixed pairs", verify.expectedPairs)
	}
}

// writeLedgerManifest writes a capture manifest for report-side tests and
// returns its path.
func writeLedgerManifest(t *testing.T, dir, name string, m captureManifest) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAssemblyReportLedgerSynthesizesVanishedPairs(t *testing.T) {
	// One captured artifact: the legacy arm of pair-half. pair-half's mixed
	// arm failed at capture; pair-gone lost BOTH arms — before the ledger it
	// vanished from the report entirely.
	half := withCaptureTemp(mixedArtifact("pair-half", AssemblyLegacy, "m", nil), 0)
	arts := []Artifact{half}
	labels := []Label{labelFor(half, 1)}
	dir := t.TempDir()

	ledger := []captureExpectedRow{
		{TraceID: half.TraceID, Model: "m", PairID: "pair-half", Arm: "legacy", Status: "captured", Attempts: 1, ArtifactHash: half.ArtifactHash},
		{TraceID: "pair-half-mixed", Model: "m", PairID: "pair-half", Arm: "mixed", Status: "failed", Attempts: 1, Error: "replay timeout"},
		{TraceID: "pair-gone-legacy", Model: "m", PairID: "pair-gone", Arm: "legacy", Status: "failed", Attempts: 1, Error: "replay timeout"},
		{TraceID: "pair-gone-mixed", Model: "m", PairID: "pair-gone", Arm: "mixed", Status: "failed", Attempts: 1, Error: "replay timeout"},
	}
	v2 := writeLedgerManifest(t, dir, "v2.manifest.json", captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2,
		ArtifactCount: 1,
		PerArtifact:   []captureManifestRow{{TraceID: half.TraceID, ArtifactHash: half.ArtifactHash, UsagePresent: true}},
		ExpectedCount: len(ledger),
		Expected:      ledger,
	})

	t.Run("v2 ledger yields missing-arm exclusions for vanished pairs", func(t *testing.T) {
		ref, verify, err := loadCaptureManifestForReport(v2)
		if err != nil {
			t.Fatalf("loadCaptureManifestForReport: %v", err)
		}
		rep, err := computeAssemblyReport(arts, labels, 1, 200, nil,
			assemblyReportExtras{capture: verify, captureRef: &ref})
		if err != nil {
			t.Fatalf("computeAssemblyReport: %v", err)
		}
		if len(rep.LegacyMixedModels) != 1 {
			t.Fatalf("legacy_mixed_models = %d; want 1", len(rep.LegacyMixedModels))
		}
		reasons := map[string]string{}
		for _, ex := range rep.LegacyMixedModels[0].Exclusions {
			reasons[ex.PairID] = ex.Reason
		}
		if reasons["pair-half"] != "missing-mixed-arm" {
			t.Errorf("pair-half reason = %q; want missing-mixed-arm", reasons["pair-half"])
		}
		if reasons["pair-gone"] != "missing-legacy-arm" {
			t.Errorf("pair-gone reason = %q; want missing-legacy-arm (the pair must be synthesized from the ledger, never vanish)", reasons["pair-gone"])
		}
	})

	t.Run("v1 manifest keeps the sealed semantics exactly", func(t *testing.T) {
		v1 := writeLedgerManifest(t, dir, "v1.manifest.json", captureManifest{
			SchemaVersion: captureManifestSchemaVersion,
			ArtifactCount: 1,
			PerArtifact:   []captureManifestRow{{TraceID: half.TraceID, ArtifactHash: half.ArtifactHash, UsagePresent: true}},
		})
		ref, verify, err := loadCaptureManifestForReport(v1)
		if err != nil {
			t.Fatalf("loadCaptureManifestForReport: %v", err)
		}
		rep, err := computeAssemblyReport(arts, labels, 1, 200, nil,
			assemblyReportExtras{capture: verify, captureRef: &ref})
		if err != nil {
			t.Fatalf("computeAssemblyReport: %v", err)
		}
		reasons := map[string]string{}
		for _, ex := range rep.LegacyMixedModels[0].Exclusions {
			reasons[ex.PairID] = ex.Reason
		}
		if reasons["pair-half"] != "missing-mixed-arm" {
			t.Errorf("pair-half reason = %q; want missing-mixed-arm (single-arm discovery is pre-ledger behavior)", reasons["pair-half"])
		}
		if _, present := reasons["pair-gone"]; present {
			t.Errorf("pair-gone appears under a v1 manifest; the sealed run's semantics must not change")
		}
	})
}

func TestLoadCaptureManifestLedgerValidation(t *testing.T) {
	dir := t.TempDir()
	captured := captureExpectedRow{TraceID: "t1", Model: "m", PairID: "p1", Arm: "legacy", Status: "captured", Attempts: 1, ArtifactHash: "sha256:aa"}
	failed := captureExpectedRow{TraceID: "t2", Model: "m", PairID: "p1", Arm: "mixed", Status: "failed", Attempts: 1, Error: "boom"}
	perArtifact := []captureManifestRow{{TraceID: "t1", ArtifactHash: "sha256:aa", UsagePresent: true}}
	valid := captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2,
		ArtifactCount: 1, PerArtifact: perArtifact,
		ExpectedCount: 2, Expected: []captureExpectedRow{captured, failed},
	}

	mutate := func(mut func(*captureManifest)) captureManifest {
		m := valid
		m.PerArtifact = append([]captureManifestRow(nil), valid.PerArtifact...)
		m.Expected = append([]captureExpectedRow(nil), valid.Expected...)
		mut(&m)
		return m
	}
	cases := []struct {
		name, wantErr string
		manifest      captureManifest
	}{
		{"valid v2 accepted", "", valid},
		{"v1 with a ledger rejected", "carries a v2 expected ledger", mutate(func(m *captureManifest) {
			m.SchemaVersion = captureManifestSchemaVersion
		})},
		{"expected_count mismatch", "expected_count", mutate(func(m *captureManifest) { m.ExpectedCount = 3 })},
		{"unknown status", "status", mutate(func(m *captureManifest) { m.Expected[1].Status = "maybe" })},
		{"duplicate trace x model row", "duplicate", mutate(func(m *captureManifest) {
			m.Expected[1] = m.Expected[0]
		})},
		{"captured row without a hash", "artifact_hash", mutate(func(m *captureManifest) { m.Expected[0].ArtifactHash = "" })},
		{"captured row hash absent from per_artifact", "per_artifact", mutate(func(m *captureManifest) {
			m.Expected[0].ArtifactHash = "sha256:other"
		})},
		{"failed row carrying a hash", "failed", mutate(func(m *captureManifest) { m.Expected[1].ArtifactHash = "sha256:aa" })},
		{"legacy arm without pair_id", "pair_id", mutate(func(m *captureManifest) { m.Expected[0].PairID = "" })},
		{"invalid arm", "arm", mutate(func(m *captureManifest) { m.Expected[0].Arm = "sideways" })},
		{"per_artifact hash missing from the ledger", "ledger", mutate(func(m *captureManifest) {
			m.PerArtifact = append(m.PerArtifact, captureManifestRow{TraceID: "t9", ArtifactHash: "sha256:bb"})
			m.ArtifactCount = 2
		})},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLedgerManifest(t, dir, fmt.Sprintf("m%d.json", i), tc.manifest)
			_, verify, err := loadCaptureManifestForReport(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("valid manifest rejected: %v", err)
				}
				if len(verify.expectedPairs) != 1 {
					t.Fatalf("expectedPairs = %+v; want the one p1/m pair", verify.expectedPairs)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v; want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestAssemblyCommittedReportByteIdentity is the sealed-evidence gate for the
// registered #331 slice-3c run: the committed report.json must regenerate
// byte-identically from the committed inputs (artifacts, labels, preference
// sidecar, sealed sidemap + digest, v1 capture manifest). Any report-side
// change that would alter the sealed verdict's bytes fails here.
func TestAssemblyCommittedReportByteIdentity(t *testing.T) {
	root := corpusRepoRoot(t)
	mixed := filepath.Join(root, "docs", "llm", "assembly-corpus", "mixed")
	digestRaw, err := os.ReadFile(filepath.Join(mixed, "fc-sidemap.json.sha256"))
	if err != nil {
		t.Fatalf("read committed sidemap digest: %v", err)
	}
	fields := strings.Fields(string(digestRaw)) // "fc-sidemap sha256:<hex>"
	digest := fields[len(fields)-1]
	got, err := runAssemblyReport(assemblyReportOptions{
		LabelsPath:          filepath.Join(mixed, "labels-qwen3-coder-next.jsonl"),
		ArtifactsPath:       filepath.Join(mixed, "artifacts-qwen3-coder-next.jsonl"),
		FCPrefsPath:         filepath.Join(mixed, "pair-preferences.jsonl"),
		FCSidemapPath:       filepath.Join(mixed, "fc-sidemap.json"),
		FCSidemapDigest:     digest,
		CaptureManifestPath: filepath.Join(mixed, "artifacts-qwen3-coder-next.jsonl.manifest.json"),
	})
	if err != nil {
		t.Fatalf("runAssemblyReport over the committed inputs: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(mixed, "report.json"))
	if err != nil {
		t.Fatalf("read committed report: %v", err)
	}
	if got != string(want) {
		t.Fatalf("regenerated report is not byte-identical to the committed docs/llm/assembly-corpus/mixed/report.json (%d vs %d bytes); the sealed run must reproduce exactly", len(got), len(want))
	}
}
