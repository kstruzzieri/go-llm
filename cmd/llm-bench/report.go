package main

import (
	"fmt"
	"math"
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

	emitCacheLine := opts.Scorer == "llm-judge"
	emitProvenance := opts.TraceSetManifestHash != "" || emitCacheLine
	if emitProvenance {
		fmt.Fprintf(&b, "## Provenance\n\n")
		if opts.TraceSetManifestHash != "" {
			fmt.Fprintf(&b, "- Trace set manifest hash: `%s`\n", markdownCell(opts.TraceSetManifestHash))
		}
		if opts.Scorer == "llm-judge" && opts.JudgeProvider != "" {
			fmt.Fprintf(&b, "- Judge provider: `%s`\n", markdownCell(opts.JudgeProvider))
		}
		if emitCacheLine {
			total := opts.JudgeCacheHits + opts.JudgeCacheMisses
			if total > 0 {
				rate := 100 * float64(opts.JudgeCacheHits) / float64(total)
				fmt.Fprintf(&b, "- Judge cache hit rate: %.1f%% (%d/%d)\n", rate, opts.JudgeCacheHits, total)
			} else {
				fmt.Fprintf(&b, "- Judge cache: no activity recorded (cache may be disabled or no traces ran)\n")
			}
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "## Results\n\n")
	fmt.Fprintf(&b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | LatencyMs (p50 / p90, successful-only) | TotalTokens | n | Failures/total (timeout) |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|\n")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors

		qCell := "n/a"
		if scored > 0 {
			qCell = fmt.Sprintf("%.2f / %.2f / %.2f / %.2f / %.2f",
				agg.meanQuality, agg.qualityP25, agg.qualityP50, agg.qualityP75, agg.qualityP90)
		}
		toolSeqCell := "n/a"
		if scored > 0 {
			toolSeqCell = fmt.Sprintf("%.2f", agg.meanToolSeq)
		}
		toolArgsCell := fmt.Sprintf("%s (computed=%d)",
			metricCell(agg.meanToolArgs, agg.toolArgsComputed > 0), agg.toolArgsComputed)
		latencyCell := "n/a"
		if scored > 0 {
			latencyCell = fmt.Sprintf("%d / %d", agg.latencyP50, agg.latencyP90)
		}
		tokensCell := "n/a"
		if agg.totalTokensAvailable > 0 {
			tokensCell = fmt.Sprintf("%d", agg.totalTokensSum)
		}

		failuresCell := fmt.Sprintf("%d/%d (%d timeout)", agg.errors, agg.errors+scored, agg.timeouts)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d | %s |\n",
			markdownCell(m), qCell, toolSeqCell, toolArgsCell, latencyCell, tokensCell, scored, failuresCell)
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
					markdownCell(r.TraceID), markdownCell(redactErrorMessage(r.Err.Error())))
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
	out = redactPaths(out)
	return out
}

// formatManualQualityReport renders a per-model AnswerQuality comparison from
// human labels (the manual scorer). It deliberately omits latency: frozen
// labeled artifacts carry no timing, so latency is a separate measurement
// pass. Showing a zero latency column would be misleading, so it is dropped
// and the omission is stated.
func formatManualQualityReport(models []string, results []Result, cov manualReportCoverage) string {
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench — human-judged quality baseline (manual scorer)\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(&b, "AnswerQuality is the human label (`expected_answer_quality`): gold-standard, deterministic, on-box — no LLM judge, no model call, nothing leaves the box.")
	fmt.Fprintln(&b, "Latency is NOT included here — frozen artifacts carry no timing; measure latency in a separate pass.")
	fmt.Fprintf(&b, "\nCoverage: %d scored, stale: %d (excluded — label hash no longer matches an artifact), scoring errors: %d.\n\n",
		cov.Scored, cov.Stale, cov.Errored)
	fmt.Fprintln(&b, "## Quality by model (all matched labels)")
	fmt.Fprintln(&b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | n |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors
		qCell := "n/a"
		if scored > 0 {
			qCell = fmt.Sprintf("%.2f / %.2f / %.2f / %.2f / %.2f",
				agg.meanQuality, agg.qualityP25, agg.qualityP50, agg.qualityP75, agg.qualityP90)
		}
		fmt.Fprintf(&b, "| %s | %s | %d |\n", markdownCell(m), qCell, scored)
	}
	fmt.Fprintln(&b)
	return redactPaths(b.String())
}

// formatPairedReport renders the paired-label analysis: the paired-complete
// "Quality by model" headline (accepted-run evidence), the confounded
// all-matched table for comparison, the completeness worklist, the pairwise
// win/loss/tie matrix, the model-vs-baseline deltas with bootstrap CIs, and the
// two-scale resolution diagnostic. Like the manual report it omits latency
// (frozen artifacts carry no timing).
func formatPairedReport(pa pairedAnalysis) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# llm-bench — paired-label quality report\n\n")
	fmt.Fprintf(&b, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintln(&b, "AnswerQuality is the human label (`expected_answer_quality`): gold-standard, deterministic, on-box — no LLM judge, no model call.")
	fmt.Fprintln(&b, "Latency is NOT included here — frozen artifacts carry no timing; measure latency in a separate pass.")
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Provenance")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Lineup (from artifacts): %s\n", joinMarkdownCells(pa.Lineup))
	fmt.Fprintf(&b, "- Baseline: %s\n", markdownCell(pa.Baseline))
	fmt.Fprintf(&b, "- Paired-complete traces: %d of %d\n", pa.PerModelN, len(pa.AllTraces))
	if len(pa.Labelers) > 0 {
		fmt.Fprintf(&b, "- Labelers: %s\n", joinMarkdownCells(pa.Labelers))
	} else {
		fmt.Fprintln(&b, "- Labelers: (none recorded)")
	}
	fmt.Fprintf(&b, "- Bootstrap: seed=%d, n=%d\n", pa.BootstrapSeed, pa.BootstrapN)
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Quality by model (paired-complete)")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "> Headline accepted-run evidence: means over the %d trace(s) where every lineup model is labeled.\n\n", pa.PerModelN)
	fmt.Fprintln(&b, "| Model | AnswerQuality (mean) | n |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, m := range pa.Lineup {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", markdownCell(m), meanCell(pa.PerModelMean[m]), pa.PerModelN)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Quality by model (all matched labels)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "> Confounded by trace-difficulty mix (each model averaged over a different trace subset). Shown for comparison, not as accepted-run evidence.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Model | AnswerQuality (mean) | n |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, m := range pa.Lineup {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", markdownCell(m), meanCell(pa.AllMatchedMean[m]), pa.AllMatchedN[m])
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Completeness worklist")
	fmt.Fprintln(&b)
	if len(pa.Gaps) == 0 {
		fmt.Fprintln(&b, "All traces are paired-complete — no gaps.")
	} else {
		incomplete := len(pa.AllTraces) - pa.PerModelN
		fmt.Fprintf(&b, "%d gap(s) block %d trace(s) from counting. Resolve each cell, then re-run.\n\n", len(pa.Gaps), incomplete)
		fmt.Fprintln(&b, "| Trace | Model | Gap |")
		fmt.Fprintln(&b, "|---|---|---|")
		for _, g := range pa.Gaps {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", markdownCell(g.TraceID), markdownCell(g.Model), markdownCell(string(g.Reason)))
		}
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Pairwise win/loss/tie (paired-complete)")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Wins/losses/ties of model A vs model B over the %d paired-complete trace(s).\n\n", pa.PerModelN)
	fmt.Fprintln(&b, "| Model A | Model B | A wins | A losses | ties |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, rec := range pa.Pairwise {
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %d |\n", markdownCell(rec.ModelA), markdownCell(rec.ModelB), rec.Wins, rec.Losses, rec.Ties)
	}
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "## Model vs baseline (%s)\n\n", markdownCell(pa.Baseline))
	fmt.Fprintf(&b, "Mean per-trace quality delta vs baseline with a deterministic bootstrap 95%% CI (seed=%d, n=%d) and win/loss/tie.\n\n", pa.BootstrapSeed, pa.BootstrapN)
	fmt.Fprintln(&b, "| Model | mean Δ vs baseline | 95% CI | wins | losses | ties |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|")
	for _, bd := range pa.BaselineSummary {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d |\n",
			markdownCell(bd.Model), signedMeanCell(bd.MeanDelta), ciCell(bd.CILow, bd.CIHigh), bd.Wins, bd.Losses, bd.Ties)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "## Resolution diagnostic")
	fmt.Fprintln(&b)
	if pa.PerModelN > 0 {
		fmt.Fprintf(&b, "Over n=%d paired-complete trace(s): one full label flip (0↔1) moves a model's mean by %.2f (1/%d); one rubric step (0.5) moves it by %.2f (0.5/%d). Do not over-read a one-label gap.\n",
			pa.PerModelN, pa.ResolutionFull, pa.PerModelN, pa.ResolutionStep, pa.PerModelN)
	} else {
		fmt.Fprintln(&b, "No paired-complete traces — resolution is undefined. Resolve the completeness worklist first.")
	}
	fmt.Fprintln(&b)

	return redactPaths(b.String())
}

// meanCell renders a mean as 2 decimals, or "n/a" for NaN (no observations).
func meanCell(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", v)
}

// signedMeanCell renders a delta with an explicit sign so a positive vs
// negative direction vs the baseline is unambiguous; "n/a" for NaN.
func signedMeanCell(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%+.2f", v)
}

// ciCell renders a [lo, hi] confidence interval, or "n/a" when undefined.
func ciCell(lo, hi float64) string {
	if math.IsNaN(lo) || math.IsNaN(hi) {
		return "n/a"
	}
	return fmt.Sprintf("[%+.2f, %+.2f]", lo, hi)
}

// joinMarkdownCells escapes each cell and joins with ", " for inline lists.
func joinMarkdownCells(xs []string) string {
	cells := make([]string, len(xs))
	for i, x := range xs {
		cells[i] = markdownCell(x)
	}
	return strings.Join(cells, ", ")
}

// reportOptions carries metadata that formatReport embeds in report headers.
type reportOptions struct {
	Scorer               string
	JudgeModel           string
	JudgeProvider        string
	JudgeCacheHits       int64
	JudgeCacheMisses     int64
	TraceSetManifestHash string
}

// modelAggregate holds computed statistics for one model's result set.
type modelAggregate struct {
	errors               int
	timeouts             int
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
			if redactErrorMessage(r.Err.Error()) == "<error: timeout>" {
				a.timeouts++
			}
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
	if len(qVals) > 0 {
		qPs := percentiles(qVals, 0.25, 0.5, 0.75, 0.9)
		a.qualityP25 = qPs[0]
		a.qualityP50 = qPs[1]
		a.qualityP75 = qPs[2]
		a.qualityP90 = qPs[3]
	}
	if len(lVals) > 0 {
		lPs := int64Percentiles(lVals, 0.5, 0.9)
		a.latencyP50 = lPs[0]
		a.latencyP90 = lPs[1]
	}
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
