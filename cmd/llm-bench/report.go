package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// formatReport renders per-model aggregates as a Markdown table.
func formatReport(models []string, results []Result, opts reportOptions) string {
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	if opts.Scorer != "" {
		fmt.Fprintf(&b, "Scorer: `%s`\n\n", markdownCell(opts.Scorer))
	}
	if opts.Scorer == "llm-judge" && opts.JudgeModel != "" {
		fmt.Fprintf(&b, "Judge model: `%s`\n\n", markdownCell(opts.JudgeModel))
	}

	if opts.TraceSetManifestHash != "" || opts.JudgeCacheHits+opts.JudgeCacheMisses > 0 {
		fmt.Fprintf(&b, "## Provenance\n\n")
		if opts.TraceSetManifestHash != "" {
			fmt.Fprintf(&b, "- Trace set manifest hash: `%s`\n", markdownCell(opts.TraceSetManifestHash))
		}
		if opts.JudgeCacheHits+opts.JudgeCacheMisses > 0 {
			total := opts.JudgeCacheHits + opts.JudgeCacheMisses
			rate := 100 * float64(opts.JudgeCacheHits) / float64(total)
			fmt.Fprintf(&b, "- Judge cache hit rate: %.1f%% (%d/%d)\n", rate, opts.JudgeCacheHits, total)
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "## Results\n\n")
	fmt.Fprintf(&b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | LatencyMs (p50 / p90) | TotalTokens | n |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors

		qCell := fmt.Sprintf("%.2f / %.2f / %.2f / %.2f / %.2f",
			agg.meanQuality, agg.qualityP25, agg.qualityP50, agg.qualityP75, agg.qualityP90)
		toolArgsCell := fmt.Sprintf("%s (computed=%d)",
			metricCell(agg.meanToolArgs, agg.toolArgsComputed > 0), agg.toolArgsComputed)
		latencyCell := fmt.Sprintf("%d / %d", agg.latencyP50, agg.latencyP90)
		tokensCell := "n/a"
		if agg.totalTokensAvailable > 0 {
			tokensCell = fmt.Sprintf("%d", agg.totalTokensSum)
		}

		fmt.Fprintf(&b, "| %s | %s | %.2f | %s | %s | %s | %d |\n",
			markdownCell(m), qCell, agg.meanToolSeq, toolArgsCell, latencyCell, tokensCell, scored)
	}

	fmt.Fprintf(&b, "\n## Per-trace detail\n\n")
	for _, m := range models {
		fmt.Fprintf(&b, "### %s\n\n", markdownCell(m))
		fmt.Fprintf(&b, "| Trace | Quality | ToolSeq | ToolArgs | Replay Latency (ms) | Scorer Latency (ms) | Status |\n")
		fmt.Fprintf(&b, "|---|---|---|---|---|---|---|\n")
		rs := byModel[m]
		sort.Slice(rs, func(i, j int) bool { return rs[i].TraceID < rs[j].TraceID })
		for _, r := range rs {
			if r.Err != nil {
				fmt.Fprintf(&b, "| %s | — | — | — | — | — | %s |\n",
					markdownCell(r.TraceID), redactErrorMessage(r.Err.Error()))
				continue
			}
			fmt.Fprintf(&b, "| %s | %.2f | %.2f | %s | %d | %d | %s |\n",
				markdownCell(r.TraceID), r.Score.AnswerQuality, r.Score.ToolSequenceMatch,
				metricCell(r.Score.ToolArgsValid, r.Score.ToolArgsValidComputed),
				r.Score.LatencyMs, r.Score.ScorerLatencyMs,
				markdownCell(redactString(r.Score.Notes)))
		}
		fmt.Fprintln(&b)
	}

	out := b.String()
	out = pathPattern.ReplaceAllString(out, "<redacted-path>")
	return out
}

// reportOptions carries metadata that formatReport embeds in report headers.
type reportOptions struct {
	Scorer               string
	JudgeModel           string
	JudgeCacheHits       int64
	JudgeCacheMisses     int64
	TraceSetManifestHash string
}

// modelAggregate holds computed statistics for one model's result set.
type modelAggregate struct {
	errors               int
	meanQuality          float64
	qualityP25           float64
	qualityP50           float64
	qualityP75           float64
	qualityP90           float64
	meanToolSeq          float64
	meanToolArgs         float64
	toolArgsComputed     int
	meanLatencyMs        int64
	latencyP50           int64
	latencyP90           int64
	meanScorerLatencyMs  int64
	totalTokensSum       int
	totalTokensAvailable int
}

func aggregate(rs []Result) modelAggregate {
	if len(rs) == 0 {
		return modelAggregate{}
	}
	var a modelAggregate
	var qVals []float64
	var lVals []int64
	var qSum, tsSum, taSum float64
	var latSum, scorerLatSum int64
	var scored int
	for _, r := range rs {
		if r.Err != nil {
			a.errors++
			continue
		}
		qVals = append(qVals, r.Score.AnswerQuality)
		lVals = append(lVals, r.Score.LatencyMs)
		qSum += r.Score.AnswerQuality
		tsSum += r.Score.ToolSequenceMatch
		latSum += r.Score.LatencyMs
		scorerLatSum += r.Score.ScorerLatencyMs
		scored++
		if r.Score.ToolArgsValidComputed {
			taSum += r.Score.ToolArgsValid
			a.toolArgsComputed++
		}
		if r.Score.TotalTokens > 0 {
			a.totalTokensSum += r.Score.TotalTokens
			a.totalTokensAvailable++
		}
	}
	if scored > 0 {
		a.meanQuality = qSum / float64(scored)
		a.meanToolSeq = tsSum / float64(scored)
		a.meanLatencyMs = latSum / int64(scored)
		a.meanScorerLatencyMs = scorerLatSum / int64(scored)
	}
	if a.toolArgsComputed > 0 {
		a.meanToolArgs = taSum / float64(a.toolArgsComputed)
	}
	qPs := percentiles(qVals, 0.25, 0.5, 0.75, 0.9)
	a.qualityP25 = qPs[0]
	a.qualityP50 = qPs[1]
	a.qualityP75 = qPs[2]
	a.qualityP90 = qPs[3]
	lPs := int64Percentiles(lVals, 0.5, 0.9)
	a.latencyP50 = lPs[0]
	a.latencyP90 = lPs[1]
	return a
}

// metricCell renders a metric value as either a 2-decimal number (when
// computed) or "n/a" (when not). Used for ToolArgsValid so consumers can
// distinguish "no schema source / no calls to validate" from a real 0.
func metricCell(v float64, computed bool) string {
	if !computed {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

func markdownCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", `\|`)
	if s == "" {
		return ""
	}
	return s
}
