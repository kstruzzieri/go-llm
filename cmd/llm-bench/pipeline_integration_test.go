//go:build llmbench_integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFullPipelineAgainstLiveOllama exercises the normal-run path end to end
// against a live local Ollama: load traces from disk → replay each against a
// candidate model → score with the llm-judge scorer → render the report.
// It is the durable form of the 2026-05-28 synthetic dry run: a plumbing
// smoke test, not a benchmark. It asserts the pipeline runs clean and that the
// report is well-formed and sanitized; it deliberately makes NO assertion
// about answer quality (synthetic traces give no quality discrimination) so
// model nondeterminism cannot make it flaky.
//
// Skipped unless built with -tags llmbench_integration. Configure via:
//
//	LLM_BENCH_CANDIDATE_MODEL (default "qwen3:8b")
//	LLM_BENCH_JUDGE_MODEL     (default "gemma4:31b")
//	LLM_BENCH_OLLAMA_URL      (default "http://localhost:11434")
//
// The candidate and judge must differ (llm-judge rejects self-preference).
func TestFullPipelineAgainstLiveOllama(t *testing.T) {
	candidateModel := envOr("LLM_BENCH_CANDIDATE_MODEL", "qwen3:8b")
	judgeModel := envOr("LLM_BENCH_JUDGE_MODEL", "gemma4:31b")
	ollamaURL := envOr("LLM_BENCH_OLLAMA_URL", "http://localhost:11434")

	dir := t.TempDir()
	// One no-tool trace and one tool trace, exercising both replay paths:
	// the plain final-answer path and the lock-step tool loop with a frozen
	// tool result fed back to the candidate.
	writeTrace(t, filepath.Join(dir, "capital.json"), `{
  "id": "smoke-capital-france",
  "source": "smoke-test",
  "system": "You are a concise factual assistant. Answer in a single short sentence.",
  "turns": [{"role": "user", "content": "What is the capital of France?"}],
  "golden": {
    "final_answer_criteria": "States that the capital of France is Paris.",
    "final_answer_substring": "Paris"
  }
}`)
	writeTrace(t, filepath.Join(dir, "weather.json"), `{
  "id": "smoke-weather-tokyo",
  "source": "smoke-test",
  "system": "You are a weather assistant. When the user asks about the weather, call the get_weather tool with the city name, then report the result in one sentence.",
  "tools": [{
    "type": "function",
    "function": {
      "name": "get_weather",
      "description": "Get the current weather for a city.",
      "parameters": {
        "type": "object",
        "properties": {"city": {"type": "string", "description": "The city to look up."}},
        "required": ["city"]
      }
    }
  }],
  "turns": [
    {"role": "user", "content": "What's the weather in Tokyo right now?"},
    {"role": "assistant", "content": "", "tool_calls": [{"name": "get_weather", "arguments": {"city": "Tokyo"}}]},
    {"role": "tool", "name": "get_weather", "content": "{\"temp_c\": 18, \"conditions\": \"cloudy\"}"},
    {"role": "assistant", "content": "It is currently 18°C and cloudy in Tokyo."}
  ],
  "golden": {
    "tool_calls": ["get_weather"],
    "final_answer_criteria": "Reports, based on the tool result, that Tokyo is about 18 degrees Celsius and cloudy.",
    "final_answer_substring": "cloudy"
  }
}`)

	tracePaths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	traces, err := loadTraces(tracePaths)
	if err != nil {
		t.Fatalf("loadTraces: %v", err)
	}
	if len(traces) != 2 {
		t.Fatalf("loaded %d traces; want 2", len(traces))
	}

	// Manifest hash must be non-empty and deterministic across recomputation.
	hash := traceSetManifestHash(traces)
	if hash == "" {
		t.Fatal("empty trace set manifest hash")
	}
	if again := traceSetManifestHash(traces); again != hash {
		t.Fatalf("manifest hash not deterministic: %q != %q", hash, again)
	}

	targets, err := parseModelTargets(candidateModel)
	if err != nil {
		t.Fatalf("parseModelTargets: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The cache is part of what this test exercises, so a failure to open it
	// is a real defect, not a reason to silently degrade to no-cache.
	cache, err := openJudgeCache(filepath.Join(dir, "judge.db"))
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	var cacheStore judgeCacheStore
	if cache != nil {
		cacheStore = cache
		defer func() { _ = cache.Close() }()
	}

	scorer, err := newScorer(ctx, "llm-judge", scorerOptions{
		ollamaURL:    ollamaURL,
		judgeModel:   judgeModel,
		judgeTimeout: 2 * time.Minute,
		judgeCache:   cacheStore,
	})
	if err != nil {
		t.Fatalf("newScorer: %v", err)
	}

	runner := &Runner{OllamaURL: ollamaURL, Timeout: 5 * time.Minute, Scorer: scorer}
	results, err := runner.RunAll(ctx, targets, traces)
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}

	wantResults := len(targets) * len(traces)
	if len(results) != wantResults {
		t.Fatalf("got %d results; want %d", len(results), wantResults)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Fatalf("result %s/%s errored: %v", r.Model, r.TraceID, r.Err)
		}
	}

	var hits, misses int64
	if cacheStore != nil {
		hits, misses = cacheStore.Stats()
		// Every scored trace must consult the cache exactly once (one Get per
		// judge call), proving the cache path is actually wired rather than a
		// silent no-op that would also report zero hits on a cold run.
		if hits+misses != int64(wantResults) {
			t.Fatalf("judge cache consulted %d times; want %d (one Get per scored trace)", hits+misses, wantResults)
		}
	}

	report := formatReport([]string{candidateModel}, results, reportOptions{
		Scorer:               "llm-judge",
		JudgeModel:           judgeModel,
		JudgeCacheHits:       hits,
		JudgeCacheMisses:     misses,
		TraceSetManifestHash: hash,
	})

	// Well-formed: report carries the provenance and the candidate identity.
	for _, want := range []string{hash, candidateModel, "ToolArgsValid"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n---\n%s", want, report)
		}
	}
	// Sanitized: the renderer must not leak local filesystem paths or the
	// judge's per-trace justification text (prefixed "llm-judge=" in Notes).
	if strings.Contains(report, dir) {
		t.Fatalf("report leaked temp dir path %q\n---\n%s", dir, report)
	}
	if strings.Contains(report, "llm-judge=") {
		t.Fatalf("report leaked judge justification text\n---\n%s", report)
	}

	t.Logf("pipeline smoke ok: %d results, manifest %s, judge cache %d/%d hit",
		len(results), hash, hits, hits+misses)
}

// envOr and writeTrace are shared helpers for llmbench_integration test
// files in this package; keep their names distinct from any future additions.
func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func writeTrace(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
