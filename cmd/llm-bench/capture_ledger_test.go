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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCaptureOllamaURL = "http://ollama.test"

// failingRunner returns one Result per (target, trace) like the real runner,
// failing the trace IDs in fail with a canned error (failErr overrides the
// default when set).
type failingRunner struct {
	fail    map[string]bool
	failErr error
}

func (f *failingRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	var results []Result
	for _, target := range targets {
		for _, tr := range traces {
			r := Result{Model: target.Display, TraceID: tr.ID, CandidateProvider: target.Provider}
			if f.fail[tr.ID] {
				r.Err = fmt.Errorf("replay timeout for %s", tr.ID)
				if f.failErr != nil {
					r.Err = f.failErr
				}
			} else {
				r.Transcript = []Turn{{Role: "assistant", Content: "ans " + tr.ID}}
				r.Score = Score{PromptEvalTokens: 10, GenTokens: 2}
			}
			results = append(results, r)
		}
	}
	return results, nil
}

type captureReturnErrorRunner struct {
	err        error
	retryError bool
	calls      int
}

func (r *captureReturnErrorRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	r.calls++
	if r.retryError && r.calls == 1 {
		results := make([]Result, 0, len(targets)*len(traces))
		for _, target := range targets {
			for _, trace := range traces {
				results = append(results, Result{Model: target.Display, TraceID: trace.ID, Err: fmt.Errorf("context deadline exceeded")})
			}
		}
		return results, nil
	}
	return nil, r.err
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
		OllamaURL:  testCaptureOllamaURL,
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
	if got, want := m.RequestedTraces, requestedTracesForLedger(m.Expected); !reflect.DeepEqual(got, want) {
		t.Fatalf("requested_traces = %+v; want the ordered result-independent trace universe %+v", got, want)
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
		// A cell that fails its first attempt is retried once in-run (round-2
		// review P2): captured-first-try rows record attempts 1, rows that
		// stayed failed record the retry as attempts 2.
		wantAttempts := 1
		if w.status == "failed" {
			wantAttempts = 2
		}
		if row.Model != "m" || row.Status != w.status || row.PairID != w.pairID || row.Arm != w.arm || row.Attempts != wantAttempts {
			t.Errorf("row %s = %+v; want model m, status %s, pair %q, arm %q, attempts %d", id, row, w.status, w.pairID, w.arm, wantAttempts)
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

func TestCaptureRejectsCanonicalDuplicateTargets(t *testing.T) {
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{},
		Targets: []ModelTarget{
			{Display: "m", Provider: "ollama", Model: "m"},
			{Display: "ollama/M", Provider: "ollama", Model: "M"},
		},
		Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
		OutputPath: filepath.Join(t.TempDir(), "artifacts.jsonl"),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate model target") {
		t.Fatalf("err = %v; want canonical duplicate target rejection", err)
	}
}

// flakyRunner is a stateful calibrationRunner: failFirst trace IDs fail on
// the first invocation only, failAlways ones fail every time, and drop ones
// never produce a Result at all. calls records each invocation's trace IDs
// so tests can assert the in-run retry re-runs ONLY the failed cells.
type flakyRunner struct {
	failFirst  map[string]bool
	failAlways map[string]bool
	drop       map[string]bool
	calls      [][]string
}

func (f *flakyRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	var ids []string
	for _, tr := range traces {
		ids = append(ids, tr.ID)
	}
	f.calls = append(f.calls, ids)
	firstAttempt := len(f.calls) == 1
	var results []Result
	for _, target := range targets {
		for _, tr := range traces {
			if f.drop[tr.ID] {
				continue
			}
			r := Result{Model: target.Display, TraceID: tr.ID, CandidateProvider: target.Provider}
			if f.failAlways[tr.ID] || (f.failFirst[tr.ID] && firstAttempt) {
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

// TestCaptureRetriesFailedCellsOnce pins the round-2 review P2 in-run retry:
// a failed (trace x model) cell is retried exactly once within the same
// invocation — the retry pass carries ONLY the failed cells — and the ledger
// records attempts 1 (first-try capture) or 2 (retried). A cell the runner
// never returned a Result for still gets a universe ledger row.
func TestCaptureRetriesFailedCellsOnce(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("r-legacy", "pair-r", AssemblyLegacy),
		pairedCaptureTrace("r-mixed", "pair-r", AssemblyMixed),
		pairedCaptureTrace("d-legacy", "pair-d", AssemblyLegacy),
		pairedCaptureTrace("d-mixed", "pair-d", AssemblyMixed),
		{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	runner := &flakyRunner{
		failFirst: map[string]bool{"r-mixed": true},
		drop:      map[string]bool{"d-mixed": true},
	}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     runner,
		Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:     traces,
		OutputPath: out,
		OllamaURL:  testCaptureOllamaURL,
		Stdout:     io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("RunAll invocations = %d; want 2 (initial + one retry)", len(runner.calls))
	}
	retried := append([]string(nil), runner.calls[1]...)
	sort.Strings(retried)
	if want := []string{"d-mixed", "r-mixed"}; !reflect.DeepEqual(retried, want) {
		t.Fatalf("retry pass ran %v; want only the failed cells %v", runner.calls[1], want)
	}
	arts := readArtifactsFile(t, out)
	hashByID := map[string]string{}
	for _, a := range arts {
		hashByID[a.TraceID] = a.ArtifactHash
	}
	if len(arts) != 4 || hashByID["r-mixed"] == "" {
		t.Fatalf("artifacts = %d (%v); want 4 including the retried r-mixed", len(arts), hashByID)
	}
	raw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ExpectedCount != 5 || m.ArtifactCount != 4 {
		t.Fatalf("expected/artifact counts = %d/%d; want 5/4", m.ExpectedCount, m.ArtifactCount)
	}
	rows := map[string]captureExpectedRow{}
	for _, row := range m.Expected {
		rows[row.TraceID] = row
	}
	type want struct {
		status string
		att    int
	}
	wants := map[string]want{
		"r-legacy": {"captured", 1},
		"r-mixed":  {"captured", 2}, // retry succeeded on the second attempt
		"d-legacy": {"captured", 1},
		"d-mixed":  {"failed", 2}, // dropped by the runner both times: universe row persists
		"plain-t":  {"captured", 1},
	}
	for id, w := range wants {
		row, ok := rows[id]
		if !ok {
			t.Errorf("no ledger row for %s", id)
			continue
		}
		if row.Status != w.status || row.Attempts != w.att {
			t.Errorf("row %s = %+v; want status %s attempts %d", id, row, w.status, w.att)
		}
		if w.status == "captured" && row.ArtifactHash != hashByID[id] {
			t.Errorf("row %s hash = %q; want the written artifact hash %q", id, row.ArtifactHash, hashByID[id])
		}
	}
	if rows["d-mixed"].Error == "" || !strings.HasPrefix(rows["d-mixed"].Error, "<error: ") {
		t.Errorf("d-mixed error = %q; want a categorized no-result stub", rows["d-mixed"].Error)
	}
	// The manifest with a retried row loads for report verification
	// (attempts 2 is within the 1..2 policy range).
	if _, _, err := loadCaptureManifestForReport(out + ".manifest.json"); err != nil {
		t.Fatalf("loadCaptureManifestForReport: %v", err)
	}
}

func TestCaptureAllFailedReplacesStaleArtifactsWithLoadableEmptyFile(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("g-legacy", "pair-g", AssemblyLegacy),
		pairedCaptureTrace("g-mixed", "pair-g", AssemblyMixed),
	}
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{name: "fresh output is created"},
		{name: "stale output is replaced", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "artifacts.jsonl")
			if tc.stale {
				if err := os.WriteFile(out, []byte("stale artifacts\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "labels.jsonl"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
				Runner:     &flakyRunner{failAlways: map[string]bool{"g-legacy": true, "g-mixed": true}},
				Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
				Traces:     traces,
				OutputPath: out,
				OllamaURL:  testCaptureOllamaURL,
				Stdout:     io.Discard,
			})
			if err == nil || !strings.Contains(err.Error(), "no artifacts written") {
				t.Fatalf("err = %v; want the loud no-artifacts failure", err)
			}
			rawArtifacts, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read empty artifacts: %v", err)
			}
			if len(rawArtifacts) != 0 {
				t.Fatalf("artifacts = %q; want an empty JSONL file", rawArtifacts)
			}
			artifacts, err := loadArtifacts(out)
			if err != nil {
				t.Fatalf("loadArtifacts: %v", err)
			}
			if len(artifacts) != 0 {
				t.Fatalf("loadArtifacts returned %d artifacts; want 0", len(artifacts))
			}

			raw, err := os.ReadFile(out + ".manifest.json")
			if err != nil {
				t.Fatalf("manifest must persist for an all-failed assembly run: %v", err)
			}
			var m captureManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if m.SchemaVersion != captureManifestSchemaVersionV2 || m.ExpectedCount != 2 || len(m.Expected) != 2 || m.ArtifactCount != 0 || len(m.PerArtifact) != 0 {
				t.Fatalf("manifest = version %q expected %d/%d artifacts %d/%d; want v2 with the full 2-row ledger and zero artifacts",
					m.SchemaVersion, m.ExpectedCount, len(m.Expected), m.ArtifactCount, len(m.PerArtifact))
			}
			for _, row := range m.Expected {
				if row.Status != "failed" || row.Attempts != 2 || row.Error == "" {
					t.Errorf("row %+v; want failed with attempts 2 and a recorded error", row)
				}
			}
			if _, _, err := loadCaptureManifestForReport(out + ".manifest.json"); err != nil {
				t.Fatalf("loadCaptureManifestForReport: %v", err)
			}
			report, err := runAssemblyReport(assemblyReportOptions{
				LabelsPath:          filepath.Join(dir, "labels.jsonl"),
				ArtifactsPath:       out,
				CaptureManifestPath: out + ".manifest.json",
			})
			if err != nil {
				t.Fatalf("runAssemblyReport: %v", err)
			}
			if !strings.Contains(report, "missing-legacy-arm") {
				t.Fatalf("assembly report did not synthesize the failed pair exclusion: %s", report)
			}
		})
	}
}

// TestCaptureLedgerErrorsAreClassifiedStubs pins the committed-evidence
// redaction chokepoint (external PR review round 2 P1): an openai-compat
// backend error can embed kilobytes of arbitrary server response — API keys
// included — and path redaction alone would write it into the committed
// manifest. Ledger errors must route through redactErrorMessage, so only a
// categorized stub survives.
func TestCaptureLedgerErrorsAreRedactedOnStderr(t *testing.T) {
	const secret = "sk-FAKE-SECRET-MARKER-0000"
	traces := []Trace{
		pairedCaptureTrace("leak-legacy", "pair-leak", AssemblyLegacy),
		pairedCaptureTrace("leak-mixed", "pair-leak", AssemblyMixed),
	}
	runner := &failingRunner{fail: map[string]bool{"leak-mixed": true}}
	runner.failErr = fmt.Errorf("backend 500: {\"error\":\"invalid key %s\"} at /Users/keith/models", secret)
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = stderrFile.Close()
	})
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     runner,
		Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:     traces,
		OutputPath: out,
		OllamaURL:  testCaptureOllamaURL,
		Stdout:     io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	os.Stderr = oldStderr
	if err := stderrFile.Close(); err != nil {
		t.Fatalf("close stderr capture: %v", err)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr capture: %v", err)
	}
	if strings.Contains(string(stderr), secret) || strings.Contains(string(stderr), "/Users/") {
		t.Fatalf("stderr carries the raw backend error: %s", stderr)
	}
	raw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("manifest carries the raw backend error (secret marker survived):\n%s", raw)
	}
	if strings.Contains(string(raw), "/Users/") {
		t.Fatalf("manifest carries a local path:\n%s", raw)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, row := range m.Expected {
		if row.Status != "failed" {
			continue
		}
		if !strings.HasPrefix(row.Error, "<error: ") {
			t.Errorf("failed row %s error = %q; want a redactErrorMessage stub", row.TraceID, row.Error)
		}
	}
}

func TestCaptureRunnerReturnErrorsAreRedacted(t *testing.T) {
	const secret = "sk-FAKE-RUNNER-SECRET-0000"
	for _, tc := range []struct {
		name       string
		retryError bool
	}{
		{name: "initial run"},
		{name: "retry run", retryError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
				Runner: &captureReturnErrorRunner{
					err:        fmt.Errorf("backend response %s at /Users/keith/models", secret),
					retryError: tc.retryError,
				},
				Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
				Traces:     []Trace{{ID: "t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}}},
				OutputPath: filepath.Join(t.TempDir(), "artifacts.jsonl"),
			})
			if err == nil {
				t.Fatal("runCalibrateCapture returned nil; want a sanitized runner error")
			}
			if got := err.Error(); strings.Contains(got, secret) || strings.Contains(got, "/Users/") || !strings.Contains(got, "<error: other>") {
				t.Fatalf("runner error = %q; want only a categorized stub", got)
			}
		})
	}
}

func TestCaptureDigestResolutionErrorsAreRedactedOnStderr(t *testing.T) {
	const secret = "sk-FAKE-DIGEST-SECRET-0000"
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("create stderr capture: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = stderrFile.Close()
	})
	digests := resolveCandidateDigests(context.Background(), &fakeShowModeler{
		err: fmt.Errorf("backend response %s at /Users/keith/models", secret),
	}, []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}})
	os.Stderr = oldStderr
	if err := stderrFile.Close(); err != nil {
		t.Fatalf("close stderr capture: %v", err)
	}
	if len(digests) != 0 {
		t.Fatalf("digests = %v; want no digest after resolver failure", digests)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("read stderr capture: %v", err)
	}
	if got := string(stderr); strings.Contains(got, secret) || strings.Contains(got, "/Users/") || !strings.Contains(got, "<error: other>") {
		t.Fatalf("stderr = %q; want only a categorized stub", got)
	}
}

func TestCaptureInvalidModelDigestIsNotPersisted(t *testing.T) {
	const secret = "sk-FAKE-PROVIDER-DIGEST-SECRET-0000"
	target := ModelTarget{Display: "ollama/m", Provider: "ollama", Model: "m"}
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() { os.Stderr = oldStderr })
	digests := map[string]string{"ollama/m": secret}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: []ModelTarget{target},
		Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
		OutputPath: out, OllamaURL: testCaptureOllamaURL, ModelDigests: digests, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	os.Stderr = oldStderr
	if err := stderrFile.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"stderr": stderr, "artifacts": artifacts, "manifest": manifest} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaked the invalid provider digest", name)
		}
	}
	if got := string(stderr); !strings.Contains(got, `digest resolution skipped for "ollama/m": invalid digest`) {
		t.Fatalf("stderr = %q; want the fixed warning naming only the target selector", got)
	}
	if got := readArtifactsFile(t, out)[0].Capture.ModelDigest; got != "" {
		t.Fatalf("artifact model_digest = %q; want empty", got)
	}
	var m captureManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ModelTargets[0].ResolvedDigest != "" || len(m.ServerProbe.OllamaDigests) != 0 {
		t.Fatalf("manifest persisted invalid digest: targets=%+v probe=%+v", m.ModelTargets, m.ServerProbe)
	}
}

func TestCaptureCanonicalModelDigestMatchesAllSinks(t *testing.T) {
	const rawDigest = "SHA256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
	const wantDigest = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	target := ModelTarget{Display: "ollama/m", Provider: "ollama", Model: "m"}
	digests := resolveCandidateDigests(context.Background(), &fakeShowModeler{
		digests: map[string]string{"m": rawDigest},
	}, []ModelTarget{target})
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: []ModelTarget{target},
		Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
		OutputPath: out, OllamaURL: testCaptureOllamaURL, ModelDigests: digests, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	if got := readArtifactsFile(t, out)[0].Capture.ModelDigest; got != wantDigest {
		t.Fatalf("artifact model_digest = %q; want %q", got, wantDigest)
	}
	raw, err := os.ReadFile(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ModelTargets[0].ResolvedDigest != wantDigest || m.ServerProbe.OllamaDigests["ollama/m"] != wantDigest {
		t.Fatalf("manifest digest sinks disagree: targets=%+v probe=%+v", m.ModelTargets, m.ServerProbe)
	}
}

func TestCaptureOpenAICompatIgnoresInjectedModelDigest(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	target := ModelTarget{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	stderrFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() { os.Stderr = oldStderr })
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: []ModelTarget{target},
		Traces:     []Trace{pairedCaptureTrace("arm-legacy", "pair-a", AssemblyLegacy)},
		OutputPath: out, OpenAICompatBaseURL: "http://compat.test", ModelDigests: map[string]string{"openai-compat/m": digest}, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	os.Stderr = oldStderr
	if err := stderrFile.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stderr), "digest resolution skipped") || strings.Contains(string(stderr), "invalid digest") {
		t.Fatalf("openai-compat digest omission emitted a misleading warning: %q", stderr)
	}
	if got := readArtifactsFile(t, out)[0].Capture.ModelDigest; got != "" {
		t.Fatalf("artifact model_digest = %q; want empty for openai-compat", got)
	}
	raw, err := os.ReadFile(captureManifestPath(out))
	if err != nil {
		t.Fatal(err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ModelTargets[0].ResolvedDigest != "" || len(m.ServerProbe.OllamaDigests) != 0 {
		t.Fatalf("openai-compat digest persisted: targets=%+v probe=%+v", m.ModelTargets, m.ServerProbe)
	}
}

func TestCaptureDoesNotProbeProps(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(srv.Close)
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:              &orderRecordingRunner{},
		Targets:             []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}},
		Traces:              []Trace{pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy)},
		OutputPath:          out,
		OpenAICompatBaseURL: srv.URL,
		Stdout:              io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server received %d request(s); capture must not probe /props", got)
	}
	if _, err := os.Stat(out + ".manifest.json"); err != nil {
		t.Fatalf("manifest missing: %v", err)
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

func requestedTracesForLedger(rows []captureExpectedRow) []captureRequestedTrace {
	seen := make(map[string]bool, len(rows))
	var requested []captureRequestedTrace
	for _, row := range rows {
		if seen[row.TraceID] {
			continue
		}
		seen[row.TraceID] = true
		requested = append(requested, captureRequestedTrace{TraceID: row.TraceID, PairID: row.PairID, Arm: row.Arm})
	}
	return requested
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
		{TraceID: "pair-half-mixed", Model: "m", PairID: "pair-half", Arm: "mixed", Status: "failed", Attempts: 2, Error: "<error: timeout>"},
		{TraceID: "pair-gone-legacy", Model: "m", PairID: "pair-gone", Arm: "legacy", Status: "failed", Attempts: 2, Error: "<error: timeout>"},
		{TraceID: "pair-gone-mixed", Model: "m", PairID: "pair-gone", Arm: "mixed", Status: "failed", Attempts: 2, Error: "<error: timeout>"},
	}
	v2 := writeLedgerManifest(t, dir, "v2.manifest.json", captureManifest{
		SchemaVersion:   captureManifestSchemaVersionV2,
		RequestedTraces: requestedTracesForLedger(ledger),
		ModelTargets:    []captureManifestTarget{{Selector: "m"}},
		ArtifactCount:   1,
		PerArtifact: []captureManifestRow{{
			TraceID: half.TraceID, ArtifactHash: half.ArtifactHash,
			ProvenanceHash: captureProvenanceHash(half), UsagePresent: true,
		}},
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
		reasons := map[string][]string{}
		for _, ex := range rep.LegacyMixedModels[0].Exclusions {
			reasons[ex.PairID] = append(reasons[ex.PairID], ex.Reason)
		}
		if !reflect.DeepEqual(reasons["pair-half"], []string{"missing-mixed-arm"}) {
			t.Errorf("pair-half reason = %q; want missing-mixed-arm", reasons["pair-half"])
		}
		if !reflect.DeepEqual(reasons["pair-gone"], []string{"missing-legacy-arm", "missing-mixed-arm"}) {
			t.Errorf("pair-gone reasons = %q; want both missing-arm exclusions (the pair must be synthesized from the ledger, never vanish)", reasons["pair-gone"])
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

func TestAssemblyReportV2ArtifactBinding(t *testing.T) {
	legacy := withCaptureTemp(mixedArtifact("pair-a", AssemblyLegacy, "ollama/m", nil), 0)
	mixed := withCaptureTemp(mixedArtifact("pair-a", AssemblyMixed, "ollama/m", nil), 0)
	legacy.CapturedAt = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mixed.CapturedAt = legacy.CapturedAt.Add(time.Second)
	legacy.Capture.OrderIndex, mixed.Capture.OrderIndex = 0, 1
	legacy.Capture.PromptTokens, legacy.Capture.GenTokens = 10, 2
	mixed.Capture.PromptTokens, mixed.Capture.GenTokens = 10, 2
	legacy.Capture.CapturedOrder, mixed.Capture.CapturedOrder = "legacy-first", "legacy-first"
	arts := []Artifact{legacy, mixed}
	labels := []Label{labelFor(legacy, 0), labelFor(mixed, 1)}
	base := captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2,
		RequestedTraces: requestedTracesForLedger([]captureExpectedRow{
			{TraceID: legacy.TraceID, PairID: "pair-a", Arm: "legacy"},
			{TraceID: mixed.TraceID, PairID: "pair-a", Arm: "mixed"},
		}),
		Transport:            defaultBenchProvider,
		ModelTargets:         []captureManifestTarget{{Selector: "ollama/m"}},
		Decoding:             captureDecoding{Temperature: assemblyCaptureTemperature},
		CounterbalanceScheme: captureCounterbalanceScheme,
		ArtifactCount:        2,
		PerArtifact: []captureManifestRow{
			{TraceID: legacy.TraceID, ArtifactHash: legacy.ArtifactHash, ProvenanceHash: captureProvenanceHash(legacy), UsagePresent: true},
			{TraceID: mixed.TraceID, ArtifactHash: mixed.ArtifactHash, ProvenanceHash: captureProvenanceHash(mixed), OrderIndex: 1, UsagePresent: true},
		},
		ExpectedCount: 2,
		Expected: []captureExpectedRow{
			{TraceID: legacy.TraceID, Model: "ollama/m", PairID: "pair-a", Arm: "legacy", Status: "captured", Attempts: 1, ArtifactHash: legacy.ArtifactHash},
			{TraceID: mixed.TraceID, Model: "ollama/m", PairID: "pair-a", Arm: "mixed", Status: "captured", Attempts: 1, ArtifactHash: mixed.ArtifactHash},
		},
	}
	run := func(t *testing.T, manifest captureManifest, artifacts []Artifact) error {
		t.Helper()
		dir := t.TempDir()
		artifactsPath := filepath.Join(dir, "artifacts.jsonl")
		labelsPath := filepath.Join(dir, "labels.jsonl")
		if err := writeJSONLRows(artifactsPath, "artifacts", artifacts); err != nil {
			t.Fatal(err)
		}
		if err := writeLabelsJSONL(labelsPath, labels); err != nil {
			t.Fatal(err)
		}
		manifestPath := writeLedgerManifest(t, dir, "manifest.json", manifest)
		_, err := runAssemblyReport(assemblyReportOptions{
			LabelsPath: labelsPath, ArtifactsPath: artifactsPath, CaptureManifestPath: manifestPath,
		})
		return err
	}
	clone := func(mut func(*captureManifest)) captureManifest {
		m := base
		m.PerArtifact = append([]captureManifestRow(nil), base.PerArtifact...)
		m.Expected = append([]captureExpectedRow(nil), base.Expected...)
		m.RequestedTraces = append([]captureRequestedTrace(nil), base.RequestedTraces...)
		mut(&m)
		return m
	}
	withRehashedFirstArtifact := func(mut func(*Artifact)) (captureManifest, []Artifact) {
		changed := append([]Artifact(nil), arts...)
		mut(&changed[0])
		changed[0].ArtifactHash = artifactHash(changed[0])
		manifest := clone(func(m *captureManifest) {
			m.PerArtifact[0].ArtifactHash = changed[0].ArtifactHash
			m.Expected[0].ArtifactHash = changed[0].ArtifactHash
		})
		return manifest, changed
	}
	outerTraceManifest, outerTraceArtifacts := withRehashedFirstArtifact(func(a *Artifact) { a.TraceID = "other-trace" })
	innerTraceManifest, innerTraceArtifacts := withRehashedFirstArtifact(func(a *Artifact) { a.Trace.ID = "other-trace" })
	if err := run(t, base, arts); err != nil {
		t.Fatalf("valid v2 report: %v", err)
	}
	for _, tc := range []struct {
		name, want string
		manifest   captureManifest
		artifacts  []Artifact
	}{
		{"missing expected row", "expected_count", clone(func(m *captureManifest) {
			m.PerArtifact = m.PerArtifact[1:]
			m.Expected = m.Expected[1:]
			m.ArtifactCount, m.ExpectedCount = 1, 1
		}), arts},
		{"swapped hash set", "artifact hash", clone(func(m *captureManifest) {
			hash := "sha256:" + strings.Repeat("f", 64)
			m.PerArtifact[0].ArtifactHash = hash
			m.Expected[0].ArtifactHash = hash
		}), arts},
		{"trace", "trace_id", clone(func(m *captureManifest) {
			m.PerArtifact[0].TraceID = "other-trace"
			m.Expected[0].TraceID = "other-trace"
		}), arts},
		{"artifact outer trace", "trace_id", outerTraceManifest, outerTraceArtifacts},
		{"artifact inner trace", "trace.id", innerTraceManifest, innerTraceArtifacts},
		{"model", "model_targets", clone(func(m *captureManifest) {
			m.Expected[0].Model = "openai-compat/m"
		}), arts},
		{"pair", "pair_id", clone(func(m *captureManifest) {
			m.Expected[0].PairID = "pair-other"
		}), arts},
		{"arm", "arm", clone(func(m *captureManifest) {
			m.Expected[0].Arm = "mixed"
		}), arts},
		{"extra artifact against all-failed v2", "artifact hash", captureManifest{
			SchemaVersion:   captureManifestSchemaVersionV2,
			RequestedTraces: []captureRequestedTrace{{TraceID: "failed-legacy", PairID: "pair-failed", Arm: "legacy"}},
			ModelTargets:    []captureManifestTarget{{Selector: "ollama/m"}},
			ExpectedCount:   1,
			Expected:        []captureExpectedRow{{TraceID: "failed-legacy", Model: "ollama/m", PairID: "pair-failed", Arm: "legacy", Status: "failed", Attempts: 2, Error: "<error: timeout>"}},
		}, []Artifact{legacy}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t, tc.manifest, tc.artifacts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want v2 binding error containing %q", err, tc.want)
			}
		})
	}

	v1 := captureManifest{
		SchemaVersion: captureManifestSchemaVersion,
		ArtifactCount: 2,
		PerArtifact:   append([]captureManifestRow(nil), base.PerArtifact...),
	}
	v1.PerArtifact[0].TraceID = "legacy-v1-does-not-bind-cross-file"
	if err := run(t, v1, arts); err != nil {
		t.Fatalf("v1 compatibility bypass: %v", err)
	}
}

func TestAssemblyReportV2KeepsSameNativeModelDistinctByProvider(t *testing.T) {
	const compatEndpoint = "https://compat.test/v1"
	var arts []Artifact
	var labels []Label
	var perArtifact []captureManifestRow
	var expected []captureExpectedRow
	for i, model := range []string{"ollama/m", "openai-compat/m"} {
		ledgerModel := []string{"ollama / m", "openai-compat / m"}[i]
		for _, mode := range []AssemblyMode{AssemblyLegacy, AssemblyMixed} {
			a := withCaptureTemp(mixedArtifact("pair-a", mode, model, nil), 0)
			a.CapturedAt = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
			a.Capture.OrderIndex = len(perArtifact) % 2
			a.Capture.PromptTokens, a.Capture.GenTokens = 10, 2
			a.Capture.CapturedOrder = "legacy-first"
			if strings.HasPrefix(model, openAICompatTransport+"/") {
				a.Capture.Transport = openAICompatCandidateProviderName(compatEndpoint)
			}
			arts = append(arts, a)
			labels = append(labels, labelFor(a, 1))
			perArtifact = append(perArtifact, captureManifestRow{
				TraceID: a.TraceID, ArtifactHash: a.ArtifactHash,
				ProvenanceHash: captureProvenanceHash(a), OrderIndex: a.Capture.OrderIndex, UsagePresent: true,
			})
			expected = append(expected, captureExpectedRow{TraceID: a.TraceID, Model: ledgerModel, PairID: "pair-a", Arm: string(mode), Status: "captured", Attempts: 1, ArtifactHash: a.ArtifactHash})
		}
	}
	dir := t.TempDir()
	artifactsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONLRows(artifactsPath, "artifacts", arts); err != nil {
		t.Fatal(err)
	}
	if err := writeLabelsJSONL(labelsPath, labels); err != nil {
		t.Fatal(err)
	}
	manifestPath := writeLedgerManifest(t, dir, "manifest.json", captureManifest{
		SchemaVersion:   captureManifestSchemaVersionV2,
		RequestedTraces: requestedTracesForLedger(expected),
		Endpoint:        "https://ollama.test " + compatEndpoint, Transport: "mixed",
		ModelTargets: []captureManifestTarget{{Selector: "ollama/m"}, {Selector: "openai-compat/m"}},
		Decoding:     captureDecoding{Temperature: assemblyCaptureTemperature}, CounterbalanceScheme: captureCounterbalanceScheme,
		ArtifactCount: len(arts), PerArtifact: perArtifact,
		ExpectedCount: len(expected), Expected: expected,
	})
	raw, err := runAssemblyReport(assemblyReportOptions{
		LabelsPath: labelsPath, ArtifactsPath: artifactsPath, CaptureManifestPath: manifestPath,
	})
	if err != nil {
		t.Fatalf("runAssemblyReport: %v", err)
	}
	var report AssemblyReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.LegacyMixedModels) != 2 || report.LegacyMixedModels[0].CandidateModel != "m" || report.LegacyMixedModels[1].CandidateModel != "openai-compat/m" {
		t.Fatalf("legacy_mixed_models = %+v; want distinct Ollama and openai-compat sections", report.LegacyMixedModels)
	}
}

func TestAssemblyReportV2AllFailedEmptyArtifactsSucceeds(t *testing.T) {
	dir := t.TempDir()
	artifactsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONLRows(artifactsPath, "artifacts", []Artifact{}); err != nil {
		t.Fatal(err)
	}
	if err := writeLabelsJSONL(labelsPath, nil); err != nil {
		t.Fatal(err)
	}
	expected := []captureExpectedRow{
		{TraceID: "failed-legacy", Model: "ollama / m", PairID: "pair-failed", Arm: "legacy", Status: "failed", Attempts: 2, Error: "<error: timeout>"},
		{TraceID: "failed-mixed", Model: "ollama/m", PairID: "pair-failed", Arm: "mixed", Status: "failed", Attempts: 2, Error: "<error: timeout>"},
	}
	manifestPath := writeLedgerManifest(t, dir, "manifest.json", captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2, Transport: defaultBenchProvider,
		RequestedTraces: requestedTracesForLedger(expected),
		ModelTargets:    []captureManifestTarget{{Selector: "ollama/m"}},
		Decoding:        captureDecoding{Temperature: assemblyCaptureTemperature}, CounterbalanceScheme: captureCounterbalanceScheme,
		ExpectedCount: len(expected), Expected: expected,
	})
	raw, err := runAssemblyReport(assemblyReportOptions{
		LabelsPath: labelsPath, ArtifactsPath: artifactsPath, CaptureManifestPath: manifestPath,
	})
	if err != nil {
		t.Fatalf("runAssemblyReport: %v", err)
	}
	var report AssemblyReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.LegacyMixedModels) != 1 {
		t.Fatalf("legacy_mixed_models = %+v; want one synthesized model section", report.LegacyMixedModels)
	}
	reasons := map[string]bool{}
	for _, exclusion := range report.LegacyMixedModels[0].Exclusions {
		reasons[exclusion.Reason] = true
	}
	if !reasons["missing-legacy-arm"] || !reasons["missing-mixed-arm"] {
		t.Fatalf("exclusions = %+v; want both registered missing-arm reasons", report.LegacyMixedModels[0].Exclusions)
	}
}

func TestAssemblyReportV1KeepsHistoricalOpenAICompatAlias(t *testing.T) {
	legacy := withCaptureTemp(mixedArtifact("pair-a", AssemblyLegacy, "openai-compat/m", nil), 0)
	mixed := withCaptureTemp(mixedArtifact("pair-a", AssemblyMixed, "openai-compat/m", nil), 0)
	arts := []Artifact{legacy, mixed}
	dir := t.TempDir()
	artifactsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONLRows(artifactsPath, "artifacts", arts); err != nil {
		t.Fatal(err)
	}
	if err := writeLabelsJSONL(labelsPath, []Label{labelFor(legacy, 0), labelFor(mixed, 1)}); err != nil {
		t.Fatal(err)
	}
	manifestPath := writeLedgerManifest(t, dir, "manifest.json", captureManifest{
		SchemaVersion: captureManifestSchemaVersion,
		ArtifactCount: 2,
		PerArtifact: []captureManifestRow{
			{TraceID: legacy.TraceID, ArtifactHash: legacy.ArtifactHash, UsagePresent: true},
			{TraceID: mixed.TraceID, ArtifactHash: mixed.ArtifactHash, UsagePresent: true},
		},
	})
	raw, err := runAssemblyReport(assemblyReportOptions{
		LabelsPath: labelsPath, ArtifactsPath: artifactsPath, CaptureManifestPath: manifestPath,
	})
	if err != nil {
		t.Fatalf("runAssemblyReport: %v", err)
	}
	var report AssemblyReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.LegacyMixedModels) != 1 || report.LegacyMixedModels[0].CandidateModel != "m" {
		t.Fatalf("legacy_mixed_models = %+v; want historical v1 bare model alias", report.LegacyMixedModels)
	}
}

func TestLoadCaptureManifestLedgerValidation(t *testing.T) {
	dir := t.TempDir()
	captured := captureExpectedRow{TraceID: "t1", Model: "m", PairID: "p1", Arm: "legacy", Status: "captured", Attempts: 1, ArtifactHash: "sha256:aa"}
	failed := captureExpectedRow{TraceID: "t2", Model: "m", PairID: "p1", Arm: "mixed", Status: "failed", Attempts: 2, Error: "<error: timeout>"}
	perArtifact := []captureManifestRow{{
		TraceID: "t1", ArtifactHash: "sha256:aa",
		ProvenanceHash: "sha256:" + strings.Repeat("a", 64), UsagePresent: true,
	}}
	valid := captureManifest{
		SchemaVersion: captureManifestSchemaVersionV2,
		RequestedTraces: []captureRequestedTrace{
			{TraceID: "t1", PairID: "p1", Arm: "legacy"},
			{TraceID: "t2", PairID: "p1", Arm: "mixed"},
		},
		ModelTargets:  []captureManifestTarget{{Selector: "m"}},
		ArtifactCount: 1, PerArtifact: perArtifact,
		ExpectedCount: 2, Expected: []captureExpectedRow{captured, failed},
	}

	mutate := func(mut func(*captureManifest)) captureManifest {
		m := valid
		m.PerArtifact = append([]captureManifestRow(nil), valid.PerArtifact...)
		m.Expected = append([]captureExpectedRow(nil), valid.Expected...)
		m.RequestedTraces = append([]captureRequestedTrace(nil), valid.RequestedTraces...)
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
		{"v1 with requested traces rejected", "carries v2 requested_traces", mutate(func(m *captureManifest) {
			m.SchemaVersion = captureManifestSchemaVersion
			m.ExpectedCount, m.Expected = 0, nil
		})},
		{"missing requested trace", "requested_traces", mutate(func(m *captureManifest) {
			m.RequestedTraces = m.RequestedTraces[:1]
		})},
		{"duplicate requested trace", "duplicate", mutate(func(m *captureManifest) {
			m.RequestedTraces[1] = m.RequestedTraces[0]
		})},
		{"blank requested trace", "requested_traces", mutate(func(m *captureManifest) {
			m.RequestedTraces[0].TraceID = ""
		})},
		{"requested trace pair mismatch", "pair_id", mutate(func(m *captureManifest) {
			m.RequestedTraces[0].PairID = "other"
		})},
		{"requested trace arm mismatch", "arm", mutate(func(m *captureManifest) {
			m.RequestedTraces[0].Arm = "mixed"
		})},
		{"whitespace-only requested pair_id", "requires a pair_id", mutate(func(m *captureManifest) {
			m.RequestedTraces[0].PairID = " \t "
			m.Expected[0].PairID = " \t "
		})},
		{"failed row deleted with decremented count", "expected_count", mutate(func(m *captureManifest) {
			m.Expected = m.Expected[:1]
			m.ExpectedCount = 1
		})},
		{"extra expected model", "model_targets", mutate(func(m *captureManifest) {
			m.Expected[1].Model = "other"
		})},
		{"extra expected trace", "requested_traces", mutate(func(m *captureManifest) {
			m.Expected[1].TraceID = "other"
		})},
		{"expected_count mismatch", "expected_count", mutate(func(m *captureManifest) { m.ExpectedCount = 3 })},
		{"unknown status", "status", mutate(func(m *captureManifest) { m.Expected[1].Status = "maybe" })},
		{"blank trace_id", "trace_id", mutate(func(m *captureManifest) { m.Expected[0].TraceID = "" })},
		{"blank model", "model", mutate(func(m *captureManifest) { m.Expected[0].Model = "" })},
		{"attempts zero", "attempts", mutate(func(m *captureManifest) { m.Expected[0].Attempts = 0 })},
		{"attempts beyond the one-retry policy", "attempts", mutate(func(m *captureManifest) { m.Expected[1].Attempts = 3 })},
		{"attempts two accepted (retried cell)", "", mutate(func(m *captureManifest) { m.Expected[1].Attempts = 2 })},
		{"failed row with one attempt", "attempts", mutate(func(m *captureManifest) { m.Expected[1].Attempts = 1 })},
		{"failed row with blank error", "error", mutate(func(m *captureManifest) { m.Expected[1].Error = "" })},
		{"failed row with raw error", "error", mutate(func(m *captureManifest) { m.Expected[1].Error = "timeout while calling backend" })},
		{"duplicate trace x model row", "duplicate", mutate(func(m *captureManifest) {
			m.Expected[1] = m.Expected[0]
		})},
		{"default-provider alias duplicate", "duplicate", mutate(func(m *captureManifest) {
			m.Expected[1] = m.Expected[0]
			m.Expected[1].Model = "ollama/M"
		})},
		{"captured row without a hash", "artifact_hash", mutate(func(m *captureManifest) { m.Expected[0].ArtifactHash = "" })},
		{"captured row carrying an error", "captured", mutate(func(m *captureManifest) { m.Expected[0].Error = "<error: timeout>" })},
		{"captured row hash absent from per_artifact", "per_artifact", mutate(func(m *captureManifest) {
			m.Expected[0].ArtifactHash = "sha256:other"
		})},
		{"duplicate captured hash", "duplicate", mutate(func(m *captureManifest) {
			m.Expected[1] = captured
			m.Expected[1].TraceID = "t2"
		})},
		{"failed row carrying a hash", "failed", mutate(func(m *captureManifest) { m.Expected[1].ArtifactHash = "sha256:aa" })},
		{"legacy arm without pair_id", "pair_id", mutate(func(m *captureManifest) { m.Expected[0].PairID = "" })},
		{"invalid arm", "arm", mutate(func(m *captureManifest) { m.Expected[0].Arm = "sideways" })},
		{"per_artifact blank trace_id", "per_artifact", mutate(func(m *captureManifest) { m.PerArtifact[0].TraceID = "" })},
		{"per_artifact blank hash", "per_artifact", mutate(func(m *captureManifest) { m.PerArtifact[0].ArtifactHash = "" })},
		{"duplicate per_artifact hash", "duplicate", mutate(func(m *captureManifest) {
			m.PerArtifact = append(m.PerArtifact, captureManifestRow{
				TraceID: "t3", ArtifactHash: "sha256:aa",
				ProvenanceHash: "sha256:" + strings.Repeat("b", 64),
			})
			m.ArtifactCount = 2
		})},
		{"captured trace/hash mismatch", "trace_id", mutate(func(m *captureManifest) {
			m.PerArtifact[0].TraceID = "other"
		})},
		{"per_artifact hash missing from the ledger", "ledger", mutate(func(m *captureManifest) {
			m.PerArtifact = append(m.PerArtifact, captureManifestRow{
				TraceID: "t9", ArtifactHash: "sha256:bb",
				ProvenanceHash: "sha256:" + strings.Repeat("b", 64),
			})
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
