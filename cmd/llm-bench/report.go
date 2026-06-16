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
	// Per-trace detail shows every result; the combined summary aggregate,
	// when a corpus is present, includes only rows allowed as model evidence.
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	summaryResults := results
	var corpusExclusions corpusResultExclusions
	if opts.Corpus != nil {
		summaryResults, corpusExclusions = modelEvidenceResults(results, opts.Corpus)
	}
	summaryByModel := make(map[string][]Result, len(models))
	for _, r := range summaryResults {
		summaryByModel[r.Model] = append(summaryByModel[r.Model], r)
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
	emitCandidateProviders := len(opts.CandidateProviders) > 0
	emitProvenance := opts.TraceSetManifestHash != "" || emitCacheLine || emitCandidateProviders
	if emitProvenance {
		fmt.Fprintf(&b, "## Provenance\n\n")
		if opts.TraceSetManifestHash != "" {
			fmt.Fprintf(&b, "- Trace set manifest hash: `%s`\n", markdownCell(opts.TraceSetManifestHash))
		}
		if opts.Scorer == "llm-judge" && opts.JudgeProvider != "" {
			fmt.Fprintf(&b, "- Judge provider: `%s`\n", markdownCell(opts.JudgeProvider))
		}
		if emitCandidateProviders {
			var parts []string
			for _, model := range models {
				providerName := strings.TrimSpace(opts.CandidateProviders[model])
				if providerName == "" {
					continue
				}
				parts = append(parts, fmt.Sprintf("`%s: %s`", markdownCell(model), markdownCell(providerName)))
			}
			if len(parts) > 0 {
				fmt.Fprintf(&b, "- Candidate providers: %s\n", strings.Join(parts, ", "))
			}
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

	if opts.Corpus != nil {
		b.WriteString(formatCorpusComposition(opts.Corpus.Counts))
		b.WriteString(formatCorpusSelectionGaps(opts.Corpus))
	}

	fmt.Fprintf(&b, "## Results\n\n")
	if opts.Corpus != nil {
		fmt.Fprintln(&b, "> Combined across model-evidence partitions (natural + challenge) — diagnostic only; partitions are not comparable, do not cite this as a single number. See the per-partition section below.")
		if corpusExclusions.JudgeValidation > 0 {
			fmt.Fprintf(&b, ">\n> %d judge-validation result(s) excluded from this aggregate (scorer-calibration evidence, not model workload).\n", corpusExclusions.JudgeValidation)
		}
		if corpusExclusions.NonModelEvidence > 0 {
			fmt.Fprintf(&b, ">\n> %d non-model-evidence result(s) excluded from this aggregate.\n", corpusExclusions.NonModelEvidence)
		}
		if corpusExclusions.Unclassified > 0 {
			fmt.Fprintf(&b, ">\n> %d unclassified result(s) excluded from this aggregate (no corpus manifest entry).\n", corpusExclusions.Unclassified)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | Restraint (mean, diverged/eligible; tool-exposed if available) | LatencyMs (p50 / p90, successful-only) | TotalTokens | n | Failures/total (timeout) |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|---|---|---|---|\n")
	for _, m := range models {
		rs := summaryByModel[m]
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
		restraintC := restraintCell(agg.meanRestraint, agg.restraintDiverged, agg.restraintComputed,
			agg.meanToolExposedRestraint, agg.toolExposedRestraintDiverged, agg.toolExposedRestraintComputed)
		latencyCell := "n/a"
		if scored > 0 {
			latencyCell = fmt.Sprintf("%d / %d", agg.latencyP50, agg.latencyP90)
		}
		tokensCell := "n/a"
		if agg.totalTokensAvailable > 0 {
			tokensCell = fmt.Sprintf("%d", agg.totalTokensSum)
		}

		failuresCell := fmt.Sprintf("%d/%d (%d timeout)", agg.errors, agg.errors+scored, agg.timeouts)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %d | %s |\n",
			markdownCell(m), qCell, toolSeqCell, toolArgsCell, restraintC, latencyCell, tokensCell, scored, failuresCell)
	}

	if opts.Corpus != nil {
		fmt.Fprintln(&b)
		b.WriteString(formatPartitionedQuality(models, results, opts.Corpus))
	}

	if opts.ToolCallExpected != nil {
		fmt.Fprintln(&b)
		b.WriteString(formatToolUseSubset(toolUseSubsetRows(models, summaryResults, opts.ToolCallExpected)))
	}

	fmt.Fprintln(&b)
	b.WriteString(formatTokenBreakdown(models, summaryResults))

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
	fmt.Fprintln(&b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | Restraint (mean, diverged/eligible; tool-exposed if available) | n |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors
		qCell := "n/a"
		if scored > 0 {
			qCell = fmt.Sprintf("%.2f / %.2f / %.2f / %.2f / %.2f",
				agg.meanQuality, agg.qualityP25, agg.qualityP50, agg.qualityP75, agg.qualityP90)
		}
		restraintC := restraintCell(agg.meanRestraint, agg.restraintDiverged, agg.restraintComputed,
			agg.meanToolExposedRestraint, agg.toolExposedRestraintDiverged, agg.toolExposedRestraintComputed)
		fmt.Fprintf(&b, "| %s | %s | %s | %d |\n", markdownCell(m), qCell, restraintC, scored)
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

	fmt.Fprintf(&b, "## Restraint vs baseline (%s)\n\n", markdownCell(pa.Baseline))
	fmt.Fprintln(&b, "> Tool-restraint is label-free (derived from frozen artifacts), so its paired-complete sample can exceed the quality paired-complete sample. Restraint = 1.0 held / 0.0 diverged over golden-empty (restraint-eligible) traces; higher is better.")
	fmt.Fprintln(&b, ">")
	fmt.Fprintln(&b, "> Win/loss/tie below is the discordant-pair table (a win = model held while the baseline diverged).")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Quality paired-complete: %d trace(s) (label denominator)\n", pa.PerModelN)
	fmt.Fprintf(&b, "- Restraint paired-complete: %d trace(s) (artifact denominator)\n", pa.Restraint.CompleteN)
	if pa.Restraint.ToolExposedN > 0 {
		fmt.Fprintf(&b, "- Tool-exposed restraint paired-complete: %d trace(s) (companion: restraint only where tools were offered)\n", pa.Restraint.ToolExposedN)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "| Model | restraint (mean) | n |")
	fmt.Fprintln(&b, "|---|---|---|")
	for _, m := range pa.Lineup {
		fmt.Fprintf(&b, "| %s | %s | %d |\n", markdownCell(m), meanCell(pa.Restraint.PerModelMean[m]), pa.Restraint.CompleteN)
	}
	fmt.Fprintln(&b)

	fmt.Fprintln(&b, "| Model | mean restraint Δ vs baseline | 95% CI | wins | losses | ties |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|")
	for _, bd := range pa.Restraint.BaselineSummary {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %d |\n",
			markdownCell(bd.Model), signedMeanCell(bd.MeanDelta), ciCell(bd.CILow, bd.CIHigh), bd.Wins, bd.Losses, bd.Ties)
	}
	fmt.Fprintln(&b)
	if pa.Restraint.ToolExposedN > 0 {
		fmt.Fprintln(&b, "Tool-exposed restraint (companion, mean over tool-offered eligible traces):")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "| Model | tool-exposed restraint (mean) | n |")
		fmt.Fprintln(&b, "|---|---|---|")
		for _, m := range pa.Lineup {
			fmt.Fprintf(&b, "| %s | %s | %d |\n", markdownCell(m), meanCell(pa.Restraint.ToolExposedPerModelMean[m]), pa.Restraint.ToolExposedN)
		}
		fmt.Fprintln(&b)
	}
	if len(pa.Restraint.ByDifficulty) > 0 {
		fmt.Fprintln(&b, "Restraint by difficulty tier (exploratory; per-tier n is small):")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "| Difficulty | n | mean restraint Δ vs baseline | 95% CI | wins | losses | ties |")
		fmt.Fprintln(&b, "|---|---|---|---|---|---|---|")
		for _, dr := range pa.Restraint.ByDifficulty {
			for _, bd := range dr.BaselineSummary {
				fmt.Fprintf(&b, "| %s | %d | %s | %s | %d | %d | %d |\n",
					dr.Difficulty, dr.N, signedMeanCell(bd.MeanDelta), ciCell(bd.CILow, bd.CIHigh), bd.Wins, bd.Losses, bd.Ties)
			}
		}
		fmt.Fprintln(&b)
	}

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

// formatTokenBreakdown renders per-model generation vs prompt-eval token sums
// and means (and thinking when the provider isolates it), so latency drivers can
// be attributed to generation rather than total volume.
func formatTokenBreakdown(models []string, results []Result) string {
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "## Token breakdown (gen vs prompt-eval)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "> Generation (eval_count) vs prompt-eval (prompt_eval_count) tokens. Thinking tokens are shown only when the provider isolates them — Ollama folds reasoning into generation, so attribute latency to output/token volume, not thinking; the OpenAI-compat transport reports reasoning tokens when the model exposes them.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Model | Gen tokens (sum / mean, n) | Prompt-eval tokens (sum / mean, n) | Thinking tokens (sum / mean, n) | Successful rows |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d |\n",
			markdownCell(m),
			tokenSumMeanCell(agg.genTokensSum, agg.genTokensAvailable),
			tokenSumMeanCell(agg.promptEvalTokensSum, agg.promptEvalTokensAvailable),
			tokenSumMeanCell(agg.thinkingTokensSum, agg.thinkingTokensAvailable),
			scored)
	}
	fmt.Fprintln(&b)
	return b.String()
}

// tokenSumMeanCell renders "sum / mean (n=available)" for a token aggregate, or
// "n/a" when no rows reported that token kind (so an unavailable metric never
// shows a fake 0).
func tokenSumMeanCell(sum, available int) string {
	if available == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d / %d (n=%d)", sum, sum/available, available)
}

// reportOptions carries metadata that formatReport embeds in report headers.
type reportOptions struct {
	Scorer               string
	JudgeModel           string
	JudgeProvider        string
	JudgeCacheHits       int64
	JudgeCacheMisses     int64
	TraceSetManifestHash string
	CandidateProviders   map[string]string
	// Corpus, when set, makes the report corpus-aware: it adds composition
	// counts and partition-separated quality, and captions the combined
	// results table as not-comparable-across-partitions.
	Corpus *corpusReportData
	// ToolCallExpected maps trace ID → whether that trace's golden expects tool
	// calls. When set, the report adds an expected-tool-call subset section so
	// vacuous no-call rows can't masquerade as tool-use evidence.
	ToolCallExpected map[string]bool
}

// corpusReportData carries the manifest-derived view the report needs:
// composition counts, model-evidence eligibility, and corpus selection gaps.
type corpusReportData struct {
	Counts               corpusCounts
	TraceToPartition     map[string]CorpusPartition
	TraceToModelEvidence map[string]bool
	MissingSelected      []string
	UnclassifiedLoaded   []string
}

func (d *corpusReportData) allowsModelEvidence(traceID string) bool {
	if d == nil || d.TraceToModelEvidence == nil {
		return true
	}
	return d.TraceToModelEvidence[traceID]
}

// formatCorpusComposition renders the manifest's partition and category counts.
func formatCorpusComposition(c corpusCounts) string {
	var b strings.Builder
	fmt.Fprintln(&b, "## Corpus composition")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Partition | Traces |")
	fmt.Fprintln(&b, "|---|---|")
	for _, p := range corpusPartitionOrder {
		fmt.Fprintf(&b, "| %s | %d |\n", p, c.ByPartition[p])
	}
	fmt.Fprintf(&b, "| **total** | %d |\n", c.Total)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Category | Traces |")
	fmt.Fprintln(&b, "|---|---|")
	for _, cat := range c.sortedCategories() {
		fmt.Fprintf(&b, "| %s | %d |\n", markdownCell(cat), c.ByCategory[cat])
	}
	fmt.Fprintln(&b)
	return b.String()
}

func formatCorpusSelectionGaps(data *corpusReportData) string {
	if data == nil || (len(data.MissingSelected) == 0 && len(data.UnclassifiedLoaded) == 0) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintln(&b, "## Corpus selection gaps")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Gap | Trace |")
	fmt.Fprintln(&b, "|---|---|")
	for _, id := range data.MissingSelected {
		fmt.Fprintf(&b, "| selected-but-missing | %s |\n", markdownCell(id))
	}
	for _, id := range data.UnclassifiedLoaded {
		fmt.Fprintf(&b, "| loaded-without-manifest | %s |\n", markdownCell(id))
	}
	fmt.Fprintln(&b)
	return b.String()
}

// formatPartitionedQuality renders per-model quality separately for the natural
// and challenge partitions. The two are never averaged into one number, and
// judge-validation traces (scorer-calibration evidence) are excluded from model
// quality entirely.
func formatPartitionedQuality(models []string, results []Result, data *corpusReportData) string {
	byPartition := make(map[CorpusPartition][]Result)
	unclassified := 0
	nonModelEvidence := 0
	for _, r := range results {
		p, ok := data.TraceToPartition[r.TraceID]
		if !ok {
			unclassified++
			continue
		}
		if p == PartitionJudgeValidation {
			continue
		}
		if !data.allowsModelEvidence(r.TraceID) {
			nonModelEvidence++
			continue
		}
		byPartition[p] = append(byPartition[p], r)
	}
	var b strings.Builder
	fmt.Fprintln(&b, "## Quality by model — by partition")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "> Natural and challenge corpora answer different questions and are never averaged into one number. Judge-validation and non-model-evidence traces are excluded from model quality.")
	fmt.Fprintln(&b)
	for _, p := range []CorpusPartition{PartitionNatural, PartitionChallenge} {
		fmt.Fprintf(&b, "### %s\n\n", p)
		rs := byPartition[p]
		if len(rs) == 0 {
			fmt.Fprintln(&b, "(no traces in this partition)")
			fmt.Fprintln(&b)
			continue
		}
		emitPartitionQualityTable(&b, models, rs)
	}
	if unclassified > 0 {
		fmt.Fprintf(&b, "_%d result(s) had no manifest partition and were omitted from the partitioned view._\n\n", unclassified)
	}
	if nonModelEvidence > 0 {
		fmt.Fprintf(&b, "_%d non-model-evidence result(s) omitted from partitioned model quality._\n\n", nonModelEvidence)
	}
	return b.String()
}

// emitPartitionQualityTable writes one per-model quality+failures table for a
// single partition's results.
func emitPartitionQualityTable(b *strings.Builder, models []string, results []Result) {
	byModel := make(map[string][]Result, len(models))
	for _, r := range results {
		byModel[r.Model] = append(byModel[r.Model], r)
	}
	fmt.Fprintln(b, "| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | n | Failures/total (timeout) |")
	fmt.Fprintln(b, "|---|---|---|---|")
	for _, m := range models {
		rs := byModel[m]
		agg := aggregate(rs)
		scored := len(rs) - agg.errors
		qCell := "n/a"
		if scored > 0 {
			qCell = fmt.Sprintf("%.2f / %.2f / %.2f / %.2f / %.2f",
				agg.meanQuality, agg.qualityP25, agg.qualityP50, agg.qualityP75, agg.qualityP90)
		}
		failuresCell := fmt.Sprintf("%d/%d (%d timeout)", agg.errors, agg.errors+scored, agg.timeouts)
		fmt.Fprintf(b, "| %s | %s | %d | %s |\n", markdownCell(m), qCell, scored, failuresCell)
	}
	fmt.Fprintln(b)
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

	genTokensSum              int
	genTokensAvailable        int
	promptEvalTokensSum       int
	promptEvalTokensAvailable int
	thinkingTokensSum         int
	thinkingTokensAvailable   int

	restraintSum                 float64 // Σ Restraint over eligible (RestraintComputed) rows
	restraintComputed            int     // # eligible rows (primary denominator)
	restraintDiverged            int     // # eligible rows with Restraint == 0
	meanRestraint                float64 // restraintSum / restraintComputed (0 when none)
	toolExposedRestraintSum      float64 // Σ over ToolExposedRestraintComputed rows
	toolExposedRestraintComputed int     // # tool-exposed eligible rows (companion denominator)
	toolExposedRestraintDiverged int     // # tool-exposed eligible rows with ToolExposedRestraint == 0
	meanToolExposedRestraint     float64
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
		if r.Score.GenTokens > 0 {
			a.genTokensSum += r.Score.GenTokens
			a.genTokensAvailable++
		}
		if r.Score.PromptEvalTokens > 0 {
			a.promptEvalTokensSum += r.Score.PromptEvalTokens
			a.promptEvalTokensAvailable++
		}
		// Thinking uses the explicit Computed flag, NOT a >0 guard like gen and
		// prompt-eval. Those use >0 only as a proxy for "reported" (a real
		// generation always has >0 tokens, so 0 means missing). Thinking has a
		// genuine availability signal, so a computed 0 is a REAL measurement
		// (reasoning ran/was off and produced nothing) that must render as 0/0,
		// letting a reader tell "measured zero reasoning" from "provider does
		// not isolate reasoning" (n/a).
		if r.Score.ThinkingTokensComputed {
			a.thinkingTokensSum += r.Score.ThinkingTokens
			a.thinkingTokensAvailable++
		}
		if r.Score.RestraintComputed {
			a.restraintSum += r.Score.Restraint
			a.restraintComputed++
			if r.Score.Restraint == 0 {
				a.restraintDiverged++
			}
		}
		if r.Score.ToolExposedRestraintComputed {
			a.toolExposedRestraintSum += r.Score.ToolExposedRestraint
			a.toolExposedRestraintComputed++
			if r.Score.ToolExposedRestraint == 0 {
				a.toolExposedRestraintDiverged++
			}
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
	if a.restraintComputed > 0 {
		a.meanRestraint = a.restraintSum / float64(a.restraintComputed)
	}
	if a.toolExposedRestraintComputed > 0 {
		a.meanToolExposedRestraint = a.toolExposedRestraintSum / float64(a.toolExposedRestraintComputed)
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

// restraintCell renders mean restraint with its diverged/eligible counts, or
// "n/a" when no traces were restraint-eligible (RestraintComputed == 0 for the
// model), mirroring metricCell's not-computed sentinel.
func restraintCell(mean float64, diverged, eligible int, toolMean float64, toolDiverged, toolEligible int) string {
	if eligible == 0 {
		return "n/a"
	}
	if toolEligible > 0 {
		return fmt.Sprintf("%.2f (%d/%d diverged; tool-exposed %.2f, %d/%d diverged)",
			mean, diverged, eligible, toolMean, toolDiverged, toolEligible)
	}
	return fmt.Sprintf("%.2f (%d/%d diverged)", mean, diverged, eligible)
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
