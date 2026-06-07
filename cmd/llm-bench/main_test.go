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
