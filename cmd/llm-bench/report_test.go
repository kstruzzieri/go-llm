package main

import (
	"strings"
	"testing"
)

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

	for _, want := range []string{"Mean ToolArgs", "| Trace | Quality | ToolSeq | ToolArgs |", "| trace-1 | 1.00 | 1.00 | 0.50 |"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

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

	if !strings.Contains(report, "| model-a | 1 | 0 | 1.00 | 0.00 | n/a |") {
		t.Fatalf("summary should render ToolArgs as n/a when not computed:\n%s", report)
	}
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
