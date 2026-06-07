package main

import (
	"errors"
	"strings"
	"testing"
)

// TestFormatReport_FailureAwareLatency pins that latency is reported
// failure-aware: a per-model failures/total (timeout) count is shown, and the
// latency column is labeled successful-only (since errored/timed-out replays
// are excluded from the latency aggregate, making p90 otherwise optimistic).
func TestFormatReport_FailureAwareLatency(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0, LatencyMs: 100}},
		{Model: "m", TraceID: "t2", Err: errors.New("context deadline exceeded")}, // timeout
		{Model: "m", TraceID: "t3", Err: errors.New("connection refused")},        // non-timeout
	}
	out := formatReport([]string{"m"}, results, reportOptions{Scorer: "exact-match"})
	for _, want := range []string{"successful-only", "Failures/total (timeout)", "2/3 (1 timeout)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

// TestFormatReportIncludesToolArgsMetric verifies that ToolArgsValid is
// rendered in both the summary and per-trace sections when it is computed.
func TestFormatReportIncludesToolArgsMetric(t *testing.T) {
	report := formatReport([]string{"model-a"}, []Result{
		{
			Model:   "model-a",
			TraceID: "trace-1",
			Score: Score{
				AnswerQuality:         1.0,
				ToolSequenceMatch:     1.0,
				ToolArgsValid:         0.5,
				ToolArgsValidComputed: true,
			},
		},
	}, reportOptions{Scorer: "exact-match"})

	for _, want := range []string{
		"ToolArgsValid (computed=N)",
		"| Trace | Quality | ToolSeq | ToolArgs | Replay Latency (ms) | Scorer Latency (ms) | Status |",
		"| trace-1 | 1.00 | 1.00 | 0.50 |",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

// TestFormatReportRendersToolArgsNAWhenNotComputed verifies that ToolArgsValid
// renders as "n/a" in both the summary and per-trace sections when not computed.
func TestFormatReportRendersToolArgsNAWhenNotComputed(t *testing.T) {
	report := formatReport([]string{"model-a"}, []Result{
		{
			Model:   "model-a",
			TraceID: "trace-1",
			Score: Score{
				AnswerQuality:         1.0,
				ToolSequenceMatch:     0.0,
				ToolArgsValid:         0.0,
				ToolArgsValidComputed: false,
			},
		},
	}, reportOptions{Scorer: "exact-match"})

	// Summary: ToolArgsValid column should show "n/a (computed=0)"
	if !strings.Contains(report, "n/a (computed=0)") {
		t.Fatalf("summary should render ToolArgs as n/a (computed=0) when not computed:\n%s", report)
	}
	// Per-trace: first four cells still follow the same format
	if !strings.Contains(report, "| trace-1 | 1.00 | 0.00 | n/a |") {
		t.Fatalf("detail should render ToolArgs as n/a when not computed:\n%s", report)
	}
}

func TestAggregateToolArgsIgnoresNotComputedRows(t *testing.T) {
	agg := aggregate([]Result{
		{Model: "m", Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}},
		{Model: "m", Score: Score{ToolArgsValid: 0.0, ToolArgsValidComputed: false}},
		{Model: "m", Score: Score{ToolArgsValid: 0.0, ToolArgsValidComputed: true}},
	})
	if agg.toolArgsComputed != 2 {
		t.Fatalf("toolArgsComputed=%d; want 2", agg.toolArgsComputed)
	}
	if agg.meanToolArgs != 0.5 {
		t.Fatalf("meanToolArgs=%v; want 0.5", agg.meanToolArgs)
	}
}

func TestAggregateComputesQualityPercentiles(t *testing.T) {
	rs := []Result{
		{Score: Score{AnswerQuality: 0.1}},
		{Score: Score{AnswerQuality: 0.2}},
		{Score: Score{AnswerQuality: 0.3}},
		{Score: Score{AnswerQuality: 0.4}},
	}
	agg := aggregate(rs)
	if got := agg.qualityP25; got != 0.1 {
		t.Errorf("qualityP25=%v; want 0.1", got)
	}
	if got := agg.qualityP50; got != 0.2 {
		t.Errorf("qualityP50=%v; want 0.2", got)
	}
	if got := agg.qualityP90; got != 0.4 {
		t.Errorf("qualityP90=%v; want 0.4", got)
	}
}

func TestAggregateComputesLatencyPercentiles(t *testing.T) {
	rs := []Result{
		{Score: Score{LatencyMs: 100}},
		{Score: Score{LatencyMs: 200}},
		{Score: Score{LatencyMs: 300}},
		{Score: Score{LatencyMs: 1000}},
	}
	agg := aggregate(rs)
	if agg.latencyP50 != 200 {
		t.Errorf("latencyP50=%d; want 200", agg.latencyP50)
	}
	if agg.latencyP90 != 1000 {
		t.Errorf("latencyP90=%d; want 1000", agg.latencyP90)
	}
}

func TestAggregateSumsTokens(t *testing.T) {
	rs := []Result{
		{Score: Score{TotalTokens: 100}},
		{Score: Score{TotalTokens: 0}}, // unavailable — does NOT count toward "available"
		{Score: Score{TotalTokens: 50}},
	}
	agg := aggregate(rs)
	if agg.totalTokensSum != 150 {
		t.Errorf("totalTokensSum=%d; want 150", agg.totalTokensSum)
	}
	if agg.totalTokensAvailable != 2 {
		t.Errorf("totalTokensAvailable=%d; want 2", agg.totalTokensAvailable)
	}
}

func TestAggregateSumsGenAndPromptEvalTokens(t *testing.T) {
	rs := []Result{
		{Score: Score{GenTokens: 100, PromptEvalTokens: 40, TotalTokens: 140}},
		{Score: Score{GenTokens: 0, PromptEvalTokens: 0, TotalTokens: 0}}, // unavailable — does NOT count
		{Score: Score{GenTokens: 50, PromptEvalTokens: 20, TotalTokens: 70}},
	}
	agg := aggregate(rs)
	if agg.genTokensSum != 150 || agg.genTokensAvailable != 2 {
		t.Errorf("gen tokens sum/available = %d/%d; want 150/2", agg.genTokensSum, agg.genTokensAvailable)
	}
	if agg.promptEvalTokensSum != 60 || agg.promptEvalTokensAvailable != 2 {
		t.Errorf("prompt-eval tokens sum/available = %d/%d; want 60/2", agg.promptEvalTokensSum, agg.promptEvalTokensAvailable)
	}
}

func TestAggregateSumsThinkingTokensOnlyWhenComputed(t *testing.T) {
	rs := []Result{
		{Score: Score{GenTokens: 100, ThinkingTokens: 30, ThinkingTokensComputed: true}},
		{Score: Score{GenTokens: 50, ThinkingTokens: 0, ThinkingTokensComputed: false}}, // Ollama: not exposed
	}
	agg := aggregate(rs)
	if agg.thinkingTokensSum != 30 || agg.thinkingTokensAvailable != 1 {
		t.Errorf("thinking tokens sum/available = %d/%d; want 30/1 (only computed rows count)", agg.thinkingTokensSum, agg.thinkingTokensAvailable)
	}
}

func TestFormatTokenBreakdown_ShowsGenVsPromptEval(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{GenTokens: 100, PromptEvalTokens: 40, TotalTokens: 140}},
		{Model: "m", TraceID: "t2", Score: Score{GenTokens: 50, PromptEvalTokens: 20, TotalTokens: 70}},
	}
	out := formatTokenBreakdown([]string{"m"}, results)
	for _, want := range []string{
		"## Token breakdown (gen vs prompt-eval)",
		"thinking",
		"| m | 150 / 75 (n=2) | 60 / 30 (n=2) | n/a | 2 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("token breakdown missing %q:\n%s", want, out)
		}
	}
}

func TestFormatTokenBreakdown_ShowsTokenAvailabilityDenominators(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "reported", Score: Score{GenTokens: 100, PromptEvalTokens: 40, TotalTokens: 140}},
		{Model: "m", TraceID: "unreported", Score: Score{AnswerQuality: 1.0}},
	}
	out := formatTokenBreakdown([]string{"m"}, results)
	for _, want := range []string{
		"| Model | Gen tokens (sum / mean, n) | Prompt-eval tokens (sum / mean, n) | Thinking tokens (sum / mean, n) | Successful rows |",
		"| m | 100 / 100 (n=1) | 40 / 40 (n=1) | n/a | 2 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("token breakdown missing availability denominator %q:\n%s", want, out)
		}
	}
}

func TestFormatTokenBreakdown_ShowsThinkingWhenComputed(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{GenTokens: 100, PromptEvalTokens: 40, ThinkingTokens: 30, ThinkingTokensComputed: true}},
	}
	out := formatTokenBreakdown([]string{"m"}, results)
	if !strings.Contains(out, "| m | 100 / 100 (n=1) | 40 / 40 (n=1) | 30 / 30 (n=1) | 1 |") {
		t.Errorf("thinking column should populate when computed:\n%s", out)
	}
}

// Thinking has an explicit computed flag, so unlike gen/prompt-eval (where 0 is
// treated as "not reported"), a computed thinking value of 0 is a REAL
// measurement and must render as 0/0, not n/a — this lets a reader distinguish
// "measured zero reasoning" from "provider does not isolate reasoning".
func TestFormatTokenBreakdown_ComputedZeroThinkingIsRealZeroNotNA(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{GenTokens: 100, PromptEvalTokens: 40, ThinkingTokens: 0, ThinkingTokensComputed: true}},
	}
	out := formatTokenBreakdown([]string{"m"}, results)
	if !strings.Contains(out, "| m | 100 / 100 (n=1) | 40 / 40 (n=1) | 0 / 0 (n=1) | 1 |") {
		t.Errorf("computed thinking=0 must render as a real 0/0 (measured), not n/a:\n%s", out)
	}
}

func TestFormatReport_EmitsTokenBreakdownAndNoNaN(t *testing.T) {
	out := formatReport([]string{"m"}, []Result{
		{Model: "m", TraceID: "t1", Score: Score{GenTokens: 10, PromptEvalTokens: 5, TotalTokens: 15}},
	}, reportOptions{Scorer: "exact-match"})
	if !strings.Contains(out, "## Token breakdown (gen vs prompt-eval)") {
		t.Errorf("report should include the token breakdown section:\n%s", out)
	}
	if strings.Contains(out, "NaN") {
		t.Errorf("token breakdown must never render NaN:\n%s", out)
	}
}

func TestFormatReport_TokenBreakdownExcludesJudgeValidation(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "natural", Score: Score{GenTokens: 100, PromptEvalTokens: 40, TotalTokens: 140}},
		{Model: "m", TraceID: "challenge", Score: Score{GenTokens: 50, PromptEvalTokens: 20, TotalTokens: 70}},
		{Model: "m", TraceID: "judge", Score: Score{GenTokens: 1000, PromptEvalTokens: 500, TotalTokens: 1500}},
	}
	out := formatReport([]string{"m"}, results, reportOptions{
		Scorer: "exact-match",
		Corpus: &corpusReportData{
			Counts: corpusCounts{
				ByPartition: map[CorpusPartition]int{PartitionNatural: 1, PartitionChallenge: 1, PartitionJudgeValidation: 1},
				ByCategory:  map[string]int{"chat": 3},
				Total:       3,
			},
			TraceToPartition: map[string]CorpusPartition{
				"natural":   PartitionNatural,
				"challenge": PartitionChallenge,
				"judge":     PartitionJudgeValidation,
			},
		},
	})
	if !strings.Contains(out, "| m | 150 / 75 (n=2) | 60 / 30 (n=2) | n/a | 2 |") {
		t.Fatalf("token breakdown should aggregate only natural+challenge rows:\n%s", out)
	}
	if strings.Contains(out, "| m | 1150 /") {
		t.Fatalf("judge-validation tokens leaked into token breakdown:\n%s", out)
	}
}

func TestFormatReportSummaryMatchesTemplateColumns(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Score: Score{AnswerQuality: 0.5, ToolSequenceMatch: 1, ToolArgsValid: 1, ToolArgsValidComputed: true, LatencyMs: 100, TotalTokens: 42}},
	}, reportOptions{Scorer: "llm-judge", JudgeModel: "gemma4:31b"})

	want := "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | LatencyMs (p50 / p90, successful-only) | TotalTokens | n | Failures/total (timeout) |"
	if !strings.Contains(report, want) {
		t.Fatalf("rendered report missing TEMPLATE column header.\nWant: %q\nGot:\n%s", want, report)
	}
}

func TestFormatReportTotalTokensNAWhenAllUnavailable(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Score: Score{AnswerQuality: 0.5, LatencyMs: 100}}, // TotalTokens=0
	}, reportOptions{Scorer: "exact-match"})
	if !strings.Contains(report, "| n/a |") {
		t.Fatalf("TotalTokens=0 should render as n/a; got:\n%s", report)
	}
}

func TestFormatReportDoesNotLeakRawErrorOrJustification(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Err: errors.New("dial tcp /tmp/foo: connection refused")},
		{Model: "m1", TraceID: "t2", Score: Score{Notes: `justification: "model picked the wrong tool"`, AnswerQuality: 0.7}},
		{Model: "m1", TraceID: "t3", Score: Score{Notes: `llm-judge=gemma4:31b: exposed judge rationale`, AnswerQuality: 0.8}},
	}, reportOptions{Scorer: "llm-judge", JudgeModel: "gemma4:31b"})

	forbidden := []string{"/tmp/", "/Users/", "justification:", "connection refused", "exposed judge rationale"}
	for _, f := range forbidden {
		if strings.Contains(report, f) {
			t.Errorf("forbidden substring %q leaked into report:\n%s", f, report)
		}
	}
	if !strings.Contains(report, "<error:") {
		t.Error("expected categorized error stub in rendered table")
	}
}

func TestFormatReportAllErrorsRowDoesNotRenderNaN(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Err: errors.New("context deadline exceeded")},
		{Model: "m1", TraceID: "t2", Err: errors.New("connection refused")},
	}, reportOptions{Scorer: "llm-judge", JudgeModel: "gemma4:31b"})
	if strings.Contains(report, "NaN") {
		t.Fatalf("report contains NaN with all errors:\n%s", report)
	}
	// Strengthened: zero-observation rows must render n/a in the
	// quality, toolseq, and latency cells so a casual reader cannot
	// mistake them for genuine zero scores.
	if !strings.Contains(report, "| m1 | n/a | n/a | n/a (computed=0) | n/a | n/a | 0 |") {
		t.Errorf("expected all-errors row to render n/a across metric cells:\n%s", report)
	}
}

func TestFormatReportProvenanceBlock(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Score: Score{AnswerQuality: 0.5}},
	}, reportOptions{
		Scorer:               "llm-judge",
		JudgeModel:           "gemma4:31b",
		JudgeProvider:        "ollama",
		JudgeCacheHits:       7,
		JudgeCacheMisses:     3,
		TraceSetManifestHash: "sha256:abc123",
	})
	if !strings.Contains(report, "Judge provider: `ollama`") {
		t.Error("missing judge provider in provenance")
	}
	if !strings.Contains(report, "Judge cache hit rate: 70.0%") {
		t.Error("missing judge cache hit rate in provenance")
	}
	if !strings.Contains(report, "Trace set manifest hash: `sha256:abc123`") {
		t.Error("missing trace set manifest hash in provenance")
	}
}

func TestFormatReportProvenanceReportsCandidateProviders(t *testing.T) {
	report := formatReport([]string{"ollama/a", "openai-compat/b"}, []Result{
		{Model: "ollama/a", TraceID: "t1", CandidateProvider: "ollama", Score: Score{AnswerQuality: 1}},
		{Model: "openai-compat/b", TraceID: "t1", CandidateProvider: "openai-compat:abc123", Score: Score{AnswerQuality: 1}},
	}, reportOptions{
		TraceSetManifestHash: "sha256:test",
		CandidateProviders: map[string]string{
			"ollama/a":        "ollama",
			"openai-compat/b": "openai-compat:abc123",
		},
	})

	if !strings.Contains(report, "Candidate providers") {
		t.Fatalf("missing candidate providers provenance:\n%s", report)
	}
	if !strings.Contains(report, "`ollama/a: ollama`") {
		t.Fatalf("missing ollama candidate provider:\n%s", report)
	}
	if !strings.Contains(report, "`openai-compat/b: openai-compat:abc123`") {
		t.Fatalf("missing openai-compat candidate provider:\n%s", report)
	}
}

func TestFormatReportProvenanceReportsCacheDisabledForLLMJudge(t *testing.T) {
	report := formatReport([]string{"m1"}, []Result{
		{Model: "m1", TraceID: "t1", Score: Score{AnswerQuality: 0.5}},
	}, reportOptions{
		Scorer:               "llm-judge",
		JudgeModel:           "gemma4:31b",
		TraceSetManifestHash: "sha256:abc123",
		// JudgeCacheHits/Misses both zero → cache disabled or unused
	})
	if !strings.Contains(report, "Judge cache: no activity recorded") {
		t.Fatalf("expected cache-disabled signal in provenance:\n%s", report)
	}
}
