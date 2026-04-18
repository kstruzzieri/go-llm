package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// formatReport renders per-model aggregates as a Markdown table.
func formatReport(models []string, results []Result) string {
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "| Model | Traces | Errors | Mean Quality | Mean ToolSeq | Mean Latency (ms) |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|\n")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		fmt.Fprintf(&b, "| %s | %d | %d | %.2f | %.2f | %d |\n",
			m, len(rs), agg.errors, agg.meanQuality, agg.meanToolSeq, agg.meanLatencyMs)
	}

	fmt.Fprintf(&b, "\n## Per-trace detail\n\n")
	for _, m := range models {
		fmt.Fprintf(&b, "### %s\n\n", m)
		fmt.Fprintf(&b, "| Trace | Quality | ToolSeq | Latency (ms) | Error |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|\n")
		rs := byModel[m]
		sort.Slice(rs, func(i, j int) bool { return rs[i].TraceID < rs[j].TraceID })
		for _, r := range rs {
			if r.Err != nil {
				// Errored runs: render em-dash in metric cells so a reader
				// never confuses an error with a genuine zero score.
				fmt.Fprintf(&b, "| %s | — | — | — | %s |\n", r.TraceID, r.Err.Error())
				continue
			}
			fmt.Fprintf(&b, "| %s | %.2f | %.2f | %d | |\n",
				r.TraceID, r.Score.AnswerQuality, r.Score.ToolSequenceMatch, r.Score.LatencyMs)
		}
		fmt.Fprintln(&b)
	}

	return b.String()
}

type modelAggregate struct {
	errors         int
	meanQuality    float64
	meanToolSeq    float64
	meanLatencyMs  int64
}

func aggregate(rs []Result) modelAggregate {
	if len(rs) == 0 {
		return modelAggregate{}
	}
	var a modelAggregate
	var qSum, tsSum float64
	var latSum int64
	var scored int
	for _, r := range rs {
		if r.Err != nil {
			a.errors++
			continue
		}
		qSum += r.Score.AnswerQuality
		tsSum += r.Score.ToolSequenceMatch
		latSum += r.Score.LatencyMs
		scored++
	}
	if scored > 0 {
		a.meanQuality = qSum / float64(scored)
		a.meanToolSeq = tsSum / float64(scored)
		a.meanLatencyMs = latSum / int64(scored)
	}
	return a
}
