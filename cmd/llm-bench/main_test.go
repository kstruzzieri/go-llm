package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("LLM_BENCH_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func TestMainManualScorerUsesLabelsFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": map[string]string{
				"role":    "assistant",
				"content": "smoke-ok",
			},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        1,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONL(labels, []any{
		Label{TraceID: "smoke-minimal-001", CandidateModel: "fake", ExpectedAnswerQuality: 1.0},
	}); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.md")

	cmd := exec.Command(os.Args[0],
		"-traces", "testdata/smoke/minimal.json",
		"-models", "fake",
		"-scorer", "manual",
		"-labels", labels,
		"-ollama-url", server.URL,
		"-judge-cache", "",
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -scorer manual failed: %v\n%s", err, out)
	}
	reportBytes, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(reportBytes), "manual-label") {
		t.Fatalf("report did not include manual scorer notes:\n%s", reportBytes)
	}
}

func TestMainPairedReportEndToEnd(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	reportPath := filepath.Join(dir, "report.md")

	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	if err := writeJSONL(artsPath, []any{a1A, a1B}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labelsPath, []any{l1A, l1B}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0],
		"-paired-report",
		"-labels", labelsPath,
		"-artifacts", artsPath,
		"-baseline", "ollama/a",
		"-report", reportPath,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -paired-report failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{
		"Quality by model (paired-complete)",
		"Model vs baseline (ollama/a)",
		"Bootstrap: seed=1, n=10000",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("paired report missing %q:\n%s", want, body)
		}
	}
}

func TestMainCorpusManifestPartitionsReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]string{"role": "assistant", "content": "smoke-ok"},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        1,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.jsonl")
	if err := writeManifest(manifest, Manifest{Entries: []ManifestEntry{
		{TraceID: "smoke-minimal-001", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.md")

	cmd := exec.Command(os.Args[0],
		"-traces", "testdata/smoke/minimal.json",
		"-models", "fake",
		"-scorer", "exact-match",
		"-corpus-manifest", manifest,
		"-ollama-url", server.URL,
		"-judge-cache", "",
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -corpus-manifest failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{"## Corpus composition", "## Quality by model — by partition", "### natural"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("corpus report missing %q:\n%s", want, body)
		}
	}
}

func TestMainEmitsToolUseSubsetSection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":           map[string]string{"role": "assistant", "content": "smoke-ok"},
			"done":              true,
			"prompt_eval_count": 1,
			"eval_count":        1,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	report := filepath.Join(dir, "report.md")
	cmd := exec.Command(os.Args[0],
		"-traces", "testdata/smoke/minimal.json",
		"-models", "fake",
		"-scorer", "exact-match",
		"-ollama-url", server.URL,
		"-judge-cache", "",
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench run failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	// The smoke trace expects no tool calls, so the subset has 0 expected pairs
	// and the claim is insufficient — but the section must still be emitted.
	// The mock reports eval_count/prompt_eval_count, so the token breakdown must
	// isolate gen vs prompt-eval (not just a combined total).
	for _, want := range []string{
		"## Tool-use (expected-tool-call subset)", "expected-tool-call pairs",
		"## Token breakdown (gen vs prompt-eval)",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("report missing %q:\n%s", want, body)
		}
	}
}

func TestMainOpenAICompatCandidateEndToEnd(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"smoke-ok"},
				"finish_reason":"stop"
			}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	report := filepath.Join(dir, "report.md")
	cmd := exec.Command(os.Args[0],
		"-traces", "testdata/smoke/minimal.json",
		"-models", "openai-compat/fake",
		"-candidate-base-url", srv.URL,
		"-candidate-api-key", "flag-key",
		"-scorer", "exact-match",
		"-judge-cache", "",
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench openai-compat candidate failed: %v\n%s", err, out)
	}
	if gotAuth != "Bearer flag-key" {
		t.Fatalf("Authorization = %q; want Bearer flag-key", gotAuth)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(body), "openai-compat/fake") {
		t.Fatalf("report missing candidate model:\n%s", body)
	}
	if !strings.Contains(string(body), "Candidate providers") {
		t.Fatalf("report missing candidate provider provenance:\n%s", body)
	}
}

func TestMainFIMLatencyEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":      "fake",
			"response":   "  return a + b",
			"done":       true,
			"eval_count": 5,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	casePath := filepath.Join(dir, "c1.json")
	if err := os.WriteFile(casePath, []byte(`{"id":"c1","prefix":"func add(a,b int) int {\n","suffix":"\n}"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "fim.md")
	cmd := exec.Command(os.Args[0],
		"-fim-latency",
		"-fim-cases", casePath,
		"-models", "fake",
		"-ollama-url", server.URL,
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -fim-latency failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, want := range []string{"FIM / inline-completion latency", "separate from chat", "## FIM latency by model"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("FIM report missing %q:\n%s", want, body)
		}
	}
}

func TestMainFIMLatencyRejectsOpenAICompatCandidate(t *testing.T) {
	dir := t.TempDir()
	casePath := filepath.Join(dir, "case.json")
	if err := os.WriteFile(casePath, []byte(`{"id":"c1","prefix":"package main\n","suffix":"\n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0],
		"-fim-latency",
		"-fim-cases", casePath,
		"-models", "openai-compat/fake",
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("llm-bench -fim-latency unexpectedly succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "-fim-latency supports only ollama candidates") {
		t.Fatalf("error missing FIM provider guard:\n%s", out)
	}
}

func TestMainFIMLatencyUsesTimeoutFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(200 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":      "fake",
			"response":   "late",
			"done":       true,
			"eval_count": 1,
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	casePath := filepath.Join(dir, "c1.json")
	if err := os.WriteFile(casePath, []byte(`{"id":"c1","prefix":"func add(a,b int) int {\n","suffix":"\n}"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "fim.md")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-fim-latency",
		"-fim-cases", casePath,
		"-models", "fake",
		"-ollama-url", server.URL,
		"-timeout", "20ms",
		"-fim-warmup=false",
		"-report", report,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("llm-bench -fim-latency ignored -timeout and was killed by test deadline: %v\n%s", ctx.Err(), out)
	}
	if err != nil {
		t.Fatalf("llm-bench -fim-latency failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(body), "| fake | n/a | n/a | 0 | 1/1 |") {
		t.Fatalf("FIM report should record the timed-out generation as a failure:\n%s", body)
	}
}

func TestOfflineCorpusFilter_NilWhenNoManifestPath(t *testing.T) {
	f, err := offlineCorpusFilter("", "", "", false)
	if err != nil || f != nil {
		t.Fatalf("offlineCorpusFilter(no manifest) = (%v, %v); want (nil, nil)", f, err)
	}
}

func TestOfflineCorpusFilter_LoadsManifestAndSelection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	if err := writeManifest(path, sampleCorpusManifest()); err != nil {
		t.Fatal(err)
	}
	f, err := offlineCorpusFilter(path, "challenge", "", true)
	if err != nil {
		t.Fatalf("offlineCorpusFilter: %v", err)
	}
	if f == nil || len(f.Manifest.Entries) != 3 {
		t.Fatalf("filter = %+v; want loaded manifest with 3 entries", f)
	}
	if len(f.Selection.Partitions) != 1 || f.Selection.Partitions[0] != PartitionChallenge || !f.Selection.OnlyModelEvidence {
		t.Fatalf("selection = %+v; want challenge + only-evidence", f.Selection)
	}
}

// TestMainManualReportHonorsCorpusManifest drives -manual-report through main()
// with a manifest that flags one natural (evidence) row and one canary
// judge-validation (non-evidence) row. With -corpus-only-evidence the canary
// must not appear in the report — the CLI dispatch must build and thread the
// filter (R-C1 lived in CLI mode dispatch, not only the pure helper).
func TestMainManualReportHonorsCorpusManifest(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	manifestPath := filepath.Join(dir, "manifest.jsonl")
	reportPath := filepath.Join(dir, "report.md")

	nat := testCalibrationArtifact("nat-row", "natural answer")
	canary := testCalibrationArtifact("canary-row", "canary answer")
	if err := writeJSONL(artsPath, []any{nat, canary}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labelsPath, []any{
		Label{TraceID: "nat-row", CandidateModel: "ollama/c", ArtifactHash: nat.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{TraceID: "canary-row", CandidateModel: "ollama/c", ArtifactHash: canary.ArtifactHash, ExpectedAnswerQuality: 0.0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(manifestPath, Manifest{Entries: []ManifestEntry{
		{TraceID: "nat-row", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
		{TraceID: "canary-row", Partition: PartitionJudgeValidation, Category: "tool-canary", AllowedAsModelEvidence: false},
	}}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0],
		"-manual-report",
		"-labels", labelsPath,
		"-artifacts", artsPath,
		"-corpus-manifest", manifestPath,
		"-corpus-only-evidence",
		"-report", reportPath,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -manual-report -corpus-manifest failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if strings.Contains(string(body), "canary") {
		t.Fatalf("canary (judge-validation, non-evidence) leaked into the manual report:\n%s", body)
	}
}

// TestMainPairedReportHonorsCorpusManifest drives -paired-report through main()
// with -corpus-partitions challenge so only the challenge trace survives. The
// completeness header must read "1 of 1" and the natural trace must not appear.
func TestMainPairedReportHonorsCorpusManifest(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	manifestPath := filepath.Join(dir, "manifest.jsonl")
	reportPath := filepath.Join(dir, "report.md")

	nat := testCalibrationArtifact("nat1", "natural answer")
	nat.CandidateModel = "ollama/gemma4:31b"
	nat.ArtifactHash = artifactHash(nat)
	chal := testCalibrationArtifact("r2c-fabrication-01", "challenge answer")
	chal.CandidateModel = "ollama/gemma4:31b"
	chal.ArtifactHash = artifactHash(chal)
	if err := writeJSONL(artsPath, []any{nat, chal}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labelsPath, []any{
		Label{ArtifactHash: nat.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{ArtifactHash: chal.ArtifactHash, ExpectedAnswerQuality: 0.0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeManifest(manifestPath, Manifest{Entries: []ManifestEntry{
		{TraceID: "nat1", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
		{TraceID: "r2c-fabrication-01", Partition: PartitionChallenge, Category: "fabrication", AllowedAsModelEvidence: true},
	}}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0],
		"-paired-report",
		"-labels", labelsPath,
		"-artifacts", artsPath,
		"-corpus-manifest", manifestPath,
		"-corpus-partitions", "challenge",
		"-report", reportPath,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("llm-bench -paired-report -corpus-manifest failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(body), "Paired-complete traces: 1 of 1") {
		t.Fatalf("paired report did not restrict to exactly the challenge trace:\n%s", body)
	}
	if strings.Contains(string(body), "nat1") {
		t.Fatalf("natural trace leaked into the challenge-only paired report:\n%s", body)
	}
}

// TestMainBlindIngestRejectsCleanedPathCollision: a -labels-out that aliases
// -artifacts through a "./" spelling must still be rejected — the guard
// compares cleaned paths, not raw flag strings.
func TestMainBlindIngestRejectsCleanedPathCollision(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	if err := writeJSONL(artsPath, []any{a}); err != nil {
		t.Fatal(err)
	}
	wsPath := filepath.Join(dir, "worksheet.txt")
	if err := os.WriteFile(wsPath, []byte(renderBlindWorksheet([]Artifact{a})), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0],
		"-blind-ingest",
		"-worksheet", wsPath,
		"-artifacts", artsPath,
		"-labels-out", dir+"/./artifacts.jsonl",
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("blind-ingest accepted -labels-out aliasing -artifacts via ./ spelling:\n%s", out)
	}
	if !strings.Contains(string(out), "differs from -artifacts") {
		t.Fatalf("unexpected failure output:\n%s", out)
	}
}

// TestMainBlindIngestRejectsSameFileViaSymlink: cleaned-string equality
// misses symlinks (and APFS case-insensitivity); a -labels-out that is a
// symlink to -artifacts must be rejected by the os.SameFile backstop.
func TestMainBlindIngestRejectsSameFileViaSymlink(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	if err := writeJSONL(artsPath, []any{a}); err != nil {
		t.Fatal(err)
	}
	wsPath := filepath.Join(dir, "worksheet.txt")
	if err := os.WriteFile(wsPath, []byte(renderBlindWorksheet([]Artifact{a})), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "labels-link.jsonl")
	if err := os.Symlink(artsPath, linkPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	cmd := exec.Command(os.Args[0],
		"-blind-ingest",
		"-worksheet", wsPath,
		"-artifacts", artsPath,
		"-labels-out", linkPath,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("blind-ingest accepted -labels-out symlinked to -artifacts:\n%s", out)
	}
	if !strings.Contains(string(out), "same file as -artifacts") {
		t.Fatalf("unexpected failure output:\n%s", out)
	}
}

// TestMainBlindIngestRejectsFullyUnscoredWorksheet: ingesting a worksheet with
// zero scored blocks is a labeler error and must not truncate an existing
// labels file to empty.
func TestMainBlindIngestRejectsFullyUnscoredWorksheet(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	a := testCalibrationArtifact("t1", "ans")
	a.ArtifactHash = artifactHash(a)
	if err := writeJSONL(artsPath, []any{a}); err != nil {
		t.Fatal(err)
	}
	wsPath := filepath.Join(dir, "worksheet.txt")
	if err := os.WriteFile(wsPath, []byte(renderBlindWorksheet([]Artifact{a})), 0o600); err != nil {
		t.Fatal(err)
	}
	labelsOut := filepath.Join(dir, "labels.jsonl")
	if err := os.WriteFile(labelsOut, []byte("{\"trace_id\":\"prior\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0],
		"-blind-ingest",
		"-worksheet", wsPath,
		"-artifacts", artsPath,
		"-labels-out", labelsOut,
	)
	cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("blind-ingest accepted a fully unscored worksheet:\n%s", out)
	}
	if !strings.Contains(string(out), "no scored blocks") {
		t.Fatalf("unexpected failure output:\n%s", out)
	}
	body, rerr := os.ReadFile(labelsOut)
	if rerr != nil || len(body) == 0 {
		t.Fatalf("pre-existing labels file was clobbered: err=%v len=%d", rerr, len(body))
	}
}

func TestResolveToolSchemaSourceStdio(t *testing.T) {
	src, err := resolveToolSchemaSource("echo mock-server", "")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource: %v", err)
	}
	if src == nil {
		t.Fatalf("nil source for non-empty stdio command")
	}
}

func TestResolveToolSchemaSourceHTTP(t *testing.T) {
	src, err := resolveToolSchemaSource("", "http://localhost:9999")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource: %v", err)
	}
	if src == nil {
		t.Fatalf("nil source for non-empty http endpoint")
	}
}

func TestResolveToolSchemaSourceMutuallyExclusive(t *testing.T) {
	_, err := resolveToolSchemaSource("echo a", "http://x")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutex error; got %v", err)
	}
}

func TestResolveToolSchemaSourceBothEmptyIsNil(t *testing.T) {
	src, err := resolveToolSchemaSource("", "")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource(empty): %v", err)
	}
	if src != nil {
		t.Fatalf("want nil source when no transport configured; got %T", src)
	}
}

func TestResolveJudgeTransportConfig_APIKeyFlagWins(t *testing.T) {
	cfg := resolveJudgeTransportConfig("openai-compat", " https://api.example.com ", "flag-key", func(string) string { return "env-key" })
	if cfg.apiKey != "flag-key" {
		t.Fatalf("apiKey = %q; want flag-key (flag overrides env)", cfg.apiKey)
	}
	if cfg.baseURL != "https://api.example.com" {
		t.Fatalf("baseURL = %q; want trimmed value", cfg.baseURL)
	}
	if cfg.transport != "openai-compat" {
		t.Fatalf("transport = %q; want openai-compat", cfg.transport)
	}
}

func TestResolveJudgeTransportConfig_APIKeyEnvFallback(t *testing.T) {
	calls := 0
	cfg := resolveJudgeTransportConfig("openai-compat", "https://x", "", func(name string) string {
		calls++
		if name != judgeAPIKeyEnvVar {
			t.Errorf("looked up %q; want %q", name, judgeAPIKeyEnvVar)
		}
		return "env-key"
	})
	if cfg.apiKey != "env-key" {
		t.Fatalf("apiKey = %q; want env-key fallback", cfg.apiKey)
	}
	if calls != 1 {
		t.Fatalf("env lookups = %d; want exactly 1", calls)
	}
}

func TestResolveCandidateTransportConfig_APIKeyFlagWins(t *testing.T) {
	cfg := resolveCandidateTransportConfig(" https://api.example.com ", "flag-key", func(string) string { return "env-key" })
	if cfg.apiKey != "flag-key" {
		t.Fatalf("apiKey = %q; want flag-key (flag overrides env)", cfg.apiKey)
	}
	if cfg.baseURL != "https://api.example.com" {
		t.Fatalf("baseURL = %q; want trimmed value", cfg.baseURL)
	}
}

func TestResolveCandidateTransportConfig_APIKeyEnvFallback(t *testing.T) {
	calls := 0
	cfg := resolveCandidateTransportConfig("https://x", "", func(name string) string {
		calls++
		if name != candidateAPIKeyEnvVar {
			t.Errorf("looked up %q; want %q", name, candidateAPIKeyEnvVar)
		}
		return "env-key"
	})
	if cfg.apiKey != "env-key" {
		t.Fatalf("apiKey = %q; want env-key fallback", cfg.apiKey)
	}
	if calls != 1 {
		t.Fatalf("env lookups = %d; want exactly 1", calls)
	}
}

func TestCaptureSampleAndLimitMutuallyExclusive(t *testing.T) {
	if err := validateCaptureSampleAndLimit(10, "n=20"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutex error; got %v", err)
	}
}

func TestResolveCaptureSampleParsesValidSpec(t *testing.T) {
	spec, err := resolveCaptureSample("n=10,stratify=token-length")
	if err != nil {
		t.Fatalf("resolveCaptureSample: %v", err)
	}
	if spec == nil || spec.N != 10 {
		t.Fatalf("spec=%+v; want N=10", spec)
	}
}

func TestResolveCaptureSampleEmptyReturnsNil(t *testing.T) {
	spec, err := resolveCaptureSample("")
	if err != nil {
		t.Fatalf("resolveCaptureSample(empty): %v", err)
	}
	if spec != nil {
		t.Fatalf("want nil spec for empty input; got %+v", spec)
	}
}
