package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// lineupFromArtifacts returns the candidate-model lineup as the distinct,
// normalized, sorted set of CandidateModel values across the artifact set.
// Distinctness uses modelKey, matching the paired cell logic, so bare model
// selectors and the default bench-provider-prefixed form collapse to one model.
//
// The lineup is derived from artifacts — NOT from matched labels — so a model
// that was captured but has zero fresh labels still appears (and shows as
// all-gap in the completeness worklist) instead of vanishing.
func lineupFromArtifacts(arts []Artifact) []string {
	displayByKey := make(map[string]string, len(arts))
	for _, a := range arts {
		display := normalizeModelSelector(a.CandidateModel)
		key := modelKey(a.CandidateModel)
		if key == "" {
			continue
		}
		if current, ok := displayByKey[key]; !ok || preferLineupDisplay(display, current) {
			displayByKey[key] = display
		}
	}
	out := make([]string, 0, len(displayByKey))
	for _, display := range displayByKey {
		out = append(out, display)
	}
	sort.Strings(out)
	return out
}

func preferLineupDisplay(candidate, current string) bool {
	candidatePrefixed := strings.HasPrefix(candidate, defaultBenchProvider+"/")
	currentPrefixed := strings.HasPrefix(current, defaultBenchProvider+"/")
	if candidatePrefixed != currentPrefixed {
		return candidatePrefixed
	}
	return candidate < current
}

// bootstrapDeltaCI returns a percentile (2.5%, 97.5%) confidence interval for
// the mean of paired deltas via the bootstrap. It is deterministic: a given
// (deltas, seed, n) always yields the same interval, so callers MUST pass a
// fixed seed (the harness uses seed=1, n=10000). Each of the n resamples draws
// len(deltas) deltas with replacement and records the resample mean; the CI is
// the nearest-rank 2.5/97.5 percentiles of those means. Empty input → NaN,NaN
// so the report renders "n/a" rather than a fake zero interval.
func bootstrapDeltaCI(deltas []float64, seed int64, n int) (lo, hi float64) {
	if len(deltas) == 0 || n <= 0 {
		return math.NaN(), math.NaN()
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, n)
	k := len(deltas)
	for i := 0; i < n; i++ {
		var sum float64
		for j := 0; j < k; j++ {
			sum += deltas[rng.Intn(k)]
		}
		means[i] = sum / float64(k)
	}
	ps := percentiles(means, 0.025, 0.975)
	return ps[0], ps[1]
}

const (
	// pairedBootstrapSeed and pairedBootstrapN fix the paired bootstrap so the
	// CI is reproducible across runs and machines (spec: seed=1, n=10000).
	pairedBootstrapSeed = 1
	pairedBootstrapN    = 10000
)

// runPairedReport loads the (labels, artifacts) pair, computes the paired-label
// analysis, and renders the paired report. It loads artifacts once and keeps
// the full set so the lineup and completeness worklist see captured-but-
// unlabeled models. baseline is the optional baseline model selector (empty →
// first lineup model). filter, if non-nil, restricts arts, matched, and stale
// to the selected corpus partition's evidence before computing the analysis.
func runPairedReport(labelsPath, artifactsPath, baseline string, filter *corpusFilter) (string, error) {
	arts, err := loadArtifacts(artifactsPath)
	if err != nil {
		return "", fmt.Errorf("paired report: load artifacts: %w", err)
	}
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return "", fmt.Errorf("paired report: load labels: %w", err)
	}
	matched, stale, err := matchLabels(labels, arts)
	if err != nil {
		return "", fmt.Errorf("paired report: %w", err)
	}
	// R-C1: drop judge-validation and non-model-evidence rows (and restrict to
	// the selected partition) using the replay-path machinery. The lineup,
	// means, and gaps are then computed over the selected subset only. The
	// corpus-selection error wording below intentionally mirrors runManualReport
	// (without the "paired report:" prefix) so both offline scorers report the
	// same shared-filter failure identically.
	var corpusNote string
	if filter != nil {
		keep, data, _ := corpusEvidenceFilter(filter.Manifest, filter.Selection, tracesFromArtifacts(arts))
		if data != nil {
			// Mirrors runManualReport: only a missing evidence trace blocks; a
			// missing canary (judge-validation, non-evidence) never could have
			// entered the aggregates, so it is excluded and noted instead.
			evidence, nonEvidence := splitMissingByEvidence(filter.Manifest, data.MissingSelected)
			if len(evidence) > 0 {
				return "", fmt.Errorf("corpus selection missing %d selected evidence trace(s) from artifacts: %v", len(evidence), evidence)
			}
			corpusNote = missingCorpusNote(nonEvidence)
		}
		arts = filterArtifactsByTrace(arts, keep)
		matched = filterMatchedByTrace(matched, keep)
		stale = filterLabelsByTrace(stale, keep)
		if len(arts) == 0 {
			return "", fmt.Errorf("corpus selection matched no captured artifacts")
		}
	}
	pa, err := computePairedAnalysis(matched, stale, arts, baseline, pairedBootstrapSeed, pairedBootstrapN)
	if err != nil {
		return "", err
	}
	return formatPairedReport(pa) + corpusNote, nil
}

// filterArtifactsByTrace keeps only artifacts whose trace ID is in keep.
func filterArtifactsByTrace(arts []Artifact, keep map[string]struct{}) []Artifact {
	out := make([]Artifact, 0, len(arts))
	for _, a := range arts {
		if _, ok := keep[a.TraceID]; ok {
			out = append(out, a)
		}
	}
	return out
}

// filterLabelsByTrace keeps only labels whose trace ID is in keep. Used for the
// stale set; stale labels carry their trace ID directly.
func filterLabelsByTrace(labels []Label, keep map[string]struct{}) []Label {
	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		if _, ok := keep[l.TraceID]; ok {
			out = append(out, l)
		}
	}
	return out
}

// gapReason classifies why a (trace, lineup-model) cell is not paired-complete.
type gapReason string

const (
	// gapMissingArtifact: the lineup model has no captured artifact for the
	// trace at all (nothing to label yet — re-run calibrate-capture).
	gapMissingArtifact gapReason = "missing-artifact"
	// gapMissingLabel: an artifact exists but no label references its hash
	// (the human still has to score it).
	gapMissingLabel gapReason = "missing-label"
	// gapStaleLabel: a label for the cell exists but its hash no longer
	// matches the current artifact (the artifact was re-captured — re-label).
	gapStaleLabel gapReason = "stale-label"
)

// completenessGap is one (trace, model) cell that blocks a trace from counting
// as paired-complete, with the reason the labeler needs to act on.
type completenessGap struct {
	TraceID string
	Model   string
	Reason  gapReason
}

// pairwiseRecord is one cell of the win/loss/tie matrix, stored once per
// unordered pair with ModelA before ModelB in lineup order. Counts are over the
// paired-complete trace subset: Wins = traces where A's quality exceeds B's.
type pairwiseRecord struct {
	ModelA string
	ModelB string
	Wins   int
	Losses int
	Ties   int
}

// baselineDelta is the model-vs-baseline summary over the paired-complete
// subset: the mean per-trace quality delta, its bootstrap CI, and the
// win/loss/tie tally (oriented as model-vs-baseline).
type baselineDelta struct {
	Model     string
	MeanDelta float64
	CILow     float64
	CIHigh    float64
	Wins      int
	Losses    int
	Ties      int
}

// difficultyRestraint is the paired restraint evidence for one difficulty tier,
// computed over the restraint-paired-complete subset whose traces carry that
// Golden.Difficulty. Reuses baselineDelta — no new statistics.
type difficultyRestraint struct {
	Difficulty      string
	N               int
	PerModelMean    map[string]float64
	BaselineSummary []baselineDelta
}

// restraintPairing is the label-free paired restraint evidence: it is derived
// entirely from frozen artifacts (no human labels), so its completeness can
// exceed the label-paired quality completeness. PerModelMean and BaselineSummary
// are over the restraint-paired-complete subset (every lineup model has an
// artifact for the trace AND the trace is restraint-eligible). The ToolExposed*
// fields repeat the calculation over the stricter "tools were offered" subset.
type restraintPairing struct {
	PerModelMean    map[string]float64
	CompleteN       int
	BaselineSummary []baselineDelta
	// ToolExposed* is a compact companion (means only); a paired Δ/CI for the
	// tool-exposed subset is intentionally out of scope here.
	ToolExposedPerModelMean map[string]float64
	ToolExposedN            int
	// ByDifficulty stratifies the paired restraint Δ by Golden.Difficulty over
	// the same complete subset, in canonical tier order. Exploratory at small
	// per-tier n; the headline remains the overall PerModelMean/BaselineSummary.
	ByDifficulty []difficultyRestraint
}

// pairedAnalysis is the computed paired-label evidence for a corpus.
type pairedAnalysis struct {
	Lineup          []string           // normalized, sorted candidate models
	Baseline        string             // declared (or default) baseline model
	AllTraces       []string           // every trace ID present in artifacts, sorted
	CompleteTraces  []string           // paired-complete trace IDs, sorted
	Gaps            []completenessGap  // worklist, sorted (trace, model)
	PerModelMean    map[string]float64 // mean AnswerQuality over CompleteTraces
	PerModelN       int                // == len(CompleteTraces)
	AllMatchedMean  map[string]float64 // mean over ALL matched labels (confounded; comparison only)
	AllMatchedN     map[string]int     // matched-label count per model
	Pairwise        []pairwiseRecord   // full matrix (i<j lineup order)
	BaselineSummary []baselineDelta    // model vs baseline (excludes baseline)
	ResolutionFull  float64            // 1/n: a full 0↔1 label flip's mean impact
	ResolutionStep  float64            // 0.5/n: one rubric-step flip's mean impact
	Labelers        []string           // distinct labelers on the complete subset
	BootstrapSeed   int64
	BootstrapN      int
	Restraint       restraintPairing // label-free paired restraint evidence
}

// cellKey identifies a (trace, candidate-model) cell using the same
// normalization the manual scorer uses, so prefix/casing variants collapse.
type cellKey struct {
	trace string
	model string
}

func newCellKey(trace, model string) cellKey {
	return cellKey{trace: normalizeModelSelector(trace), model: modelKey(model)}
}

// modelKey canonicalizes a model selector for cell matching: strip the bench
// provider prefix then lowercase/trim, so a label stored bare ("qwen3:8b")
// matches a prefixed artifact ("ollama/qwen3:8b"). Mirrors manualScorerKey.
func modelKey(model string) string {
	return normalizeModelSelector(modelSelectorWithoutBenchProvider(model))
}

// computePairedAnalysis turns matched labels (+ stale labels + the full
// artifact set) into paired-label evidence. The lineup is derived from the
// artifacts (not the matched labels) so a captured-but-unlabeled model still
// appears. Quality comes directly from Label.ExpectedAnswerQuality — never from
// ManualScorer.Score, whose tool-metric computation can error for reasons
// unrelated to the human quality verdict. seed/bootstrapN drive the
// deterministic CI (the harness uses seed=1, n=10000).
func computePairedAnalysis(matched []matchedLabel, stale []Label, arts []Artifact, baseline string, seed int64, bootstrapN int) (pairedAnalysis, error) {
	if len(arts) == 0 {
		return pairedAnalysis{}, fmt.Errorf("paired analysis: no artifacts")
	}
	lineup := lineupFromArtifacts(arts)
	if len(lineup) == 0 {
		return pairedAnalysis{}, fmt.Errorf("paired analysis: no candidate models found in artifacts (all CandidateModel values are blank)")
	}

	// Resolve baseline against the lineup (matching on the stripped key so both
	// "ollama/b" and bare "b" resolve to the canonical lineup form). Empty
	// defaults to the first (sorted) lineup model and is reported.
	var resolvedBaseline string
	if strings.TrimSpace(baseline) == "" {
		resolvedBaseline = lineup[0]
	} else {
		want := modelKey(baseline)
		for _, m := range lineup {
			if modelKey(m) == want {
				resolvedBaseline = m
				break
			}
		}
		if resolvedBaseline == "" {
			return pairedAnalysis{}, fmt.Errorf("paired analysis: baseline %q is not in the lineup %v", baseline, lineup)
		}
	}

	// Index artifacts by cell; reject duplicate (trace, model) cells (mirrors
	// runManualReport — two artifacts for one cell is an ambiguity).
	artCell := make(map[cellKey]struct{}, len(arts))
	traceDisplay := make(map[string]string)
	for _, a := range arts {
		k := newCellKey(a.TraceID, a.CandidateModel)
		if _, ok := artCell[k]; ok {
			return pairedAnalysis{}, fmt.Errorf("paired analysis: duplicate artifacts for (trace %q, model %q)", a.TraceID, a.CandidateModel)
		}
		artCell[k] = struct{}{}
		tk := normalizeModelSelector(a.TraceID)
		if _, ok := traceDisplay[tk]; !ok {
			traceDisplay[tk] = a.TraceID
		}
	}

	// Quality per labeled cell (from the human label), and labeler per cell.
	quality := make(map[cellKey]float64, len(matched))
	labelerByCell := make(map[cellKey]string, len(matched))
	for _, m := range matched {
		k := newCellKey(m.Artifact.TraceID, m.Artifact.CandidateModel)
		quality[k] = m.Label.ExpectedAnswerQuality
		if lb := strings.TrimSpace(m.Label.Labeler); lb != "" {
			labelerByCell[k] = lb
		}
	}

	// Cells that have a stale label (artifact present but label hash
	// mismatched). Attribution needs the label to carry trace/model:
	// -blind-ingest stamps both on every label it writes, so only
	// hand-authored hash-only labels (discouraged per CALIBRATION.md) lose the
	// stale-label gap reason and fall back to missing-label.
	staleCell := make(map[cellKey]struct{}, len(stale))
	for _, l := range stale {
		staleCell[newCellKey(l.TraceID, l.CandidateModel)] = struct{}{}
	}

	// Sorted distinct trace keys.
	traceKeys := make([]string, 0, len(traceDisplay))
	for tk := range traceDisplay {
		traceKeys = append(traceKeys, tk)
	}
	sort.Strings(traceKeys)

	pa := pairedAnalysis{
		Lineup:         lineup,
		Baseline:       resolvedBaseline,
		PerModelMean:   make(map[string]float64, len(lineup)),
		AllMatchedMean: make(map[string]float64, len(lineup)),
		AllMatchedN:    make(map[string]int, len(lineup)),
		BootstrapSeed:  seed,
		BootstrapN:     bootstrapN,
	}

	// All-matched per-model means (confounded; for the comparison table). Bucket
	// each matched label under its lineup display form, using the human label
	// quality directly (never ManualScorer.Score).
	keyToDisplay := make(map[string]string, len(lineup))
	for _, m := range lineup {
		keyToDisplay[modelKey(m)] = m
	}
	allMatched := make(map[string][]float64, len(lineup))
	for _, m := range matched {
		disp := keyToDisplay[modelKey(m.Artifact.CandidateModel)]
		allMatched[disp] = append(allMatched[disp], m.Label.ExpectedAnswerQuality)
	}
	for _, m := range lineup {
		pa.AllMatchedMean[m] = mean(allMatched[m])
		pa.AllMatchedN[m] = len(allMatched[m])
	}

	// Walk every (trace, model) cell: record gaps, and collect the per-model
	// quality vectors over the paired-complete traces.
	var completeKeys []string
	perModelComplete := make(map[string][]float64, len(lineup))
	labelerSet := make(map[string]struct{})
	for _, tk := range traceKeys {
		display := traceDisplay[tk]
		pa.AllTraces = append(pa.AllTraces, display)
		complete := true
		for _, model := range lineup {
			k := cellKey{trace: tk, model: modelKey(model)}
			if _, labeled := quality[k]; labeled {
				continue
			}
			complete = false
			reason := gapMissingArtifact
			if _, hasArt := artCell[k]; hasArt {
				reason = gapMissingLabel
				if _, isStale := staleCell[k]; isStale {
					reason = gapStaleLabel
				}
			}
			pa.Gaps = append(pa.Gaps, completenessGap{TraceID: display, Model: model, Reason: reason})
		}
		if !complete {
			continue
		}
		completeKeys = append(completeKeys, tk)
		pa.CompleteTraces = append(pa.CompleteTraces, display)
		for _, model := range lineup {
			k := cellKey{trace: tk, model: modelKey(model)}
			perModelComplete[model] = append(perModelComplete[model], quality[k])
			if lb, ok := labelerByCell[k]; ok {
				labelerSet[lb] = struct{}{}
			}
		}
	}

	pa.PerModelN = len(completeKeys)
	for _, model := range lineup {
		pa.PerModelMean[model] = mean(perModelComplete[model])
	}
	for lb := range labelerSet {
		pa.Labelers = append(pa.Labelers, lb)
	}
	sort.Strings(pa.Labelers)

	// Pairwise matrix (i<j) over the complete subset.
	for i := 0; i < len(lineup); i++ {
		for j := i + 1; j < len(lineup); j++ {
			a, b := lineup[i], lineup[j]
			rec := pairwiseRecord{ModelA: a, ModelB: b}
			va, vb := perModelComplete[a], perModelComplete[b]
			for idx := range completeKeys {
				switch {
				case va[idx] > vb[idx]:
					rec.Wins++
				case va[idx] < vb[idx]:
					rec.Losses++
				default:
					rec.Ties++
				}
			}
			pa.Pairwise = append(pa.Pairwise, rec)
		}
	}

	// Baseline-focused summary: every non-baseline model vs the baseline.
	base := perModelComplete[resolvedBaseline]
	for _, model := range lineup {
		if model == resolvedBaseline {
			continue
		}
		vm := perModelComplete[model]
		deltas := make([]float64, len(completeKeys))
		bd := baselineDelta{Model: model}
		for idx := range completeKeys {
			d := vm[idx] - base[idx]
			deltas[idx] = d
			switch {
			case d > 0:
				bd.Wins++
			case d < 0:
				bd.Losses++
			default:
				bd.Ties++
			}
		}
		bd.MeanDelta = mean(deltas)
		bd.CILow, bd.CIHigh = bootstrapDeltaCI(deltas, seed, bootstrapN)
		pa.BaselineSummary = append(pa.BaselineSummary, bd)
	}

	if pa.PerModelN > 0 {
		pa.ResolutionFull = 1.0 / float64(pa.PerModelN)
		pa.ResolutionStep = 0.5 / float64(pa.PerModelN)
	} else {
		pa.ResolutionFull = math.NaN()
		pa.ResolutionStep = math.NaN()
	}
	pa.Restraint = computeRestraintPairing(arts, lineup, resolvedBaseline, seed, bootstrapN)
	return pa, nil
}

// computeRestraintPairing builds paired restraint evidence directly from
// artifacts, reusing cellKey/modelKey/mean/bootstrapDeltaCI — no new statistics.
// A trace is restraint-paired-complete when every lineup model has an artifact
// for it AND restraintSignals reports computed==true for each (eligibility is a
// property of the trace's golden, so it holds uniformly; the per-cell flag
// defines the restraint-eligibility subset and also excludes malformed/empty
// artifacts as a side effect). The tool-exposed subset additionally requires
// every cell to have had tools offered.
// Callers must pass at most one artifact per (trace, model) cell;
// computePairedAnalysis enforces this before calling, so duplicates are not
// re-checked here.
// Unlike the manual/calibration path (which hard-errors on missing trace
// context via calibrationTraceFromArtifact), this path keeps the quality report
// available and excludes invalid trace context from restraint pairing so
// malformed artifacts cannot inflate restraint N.
func computeRestraintPairing(arts []Artifact, lineup []string, baseline string, seed int64, bootstrapN int) restraintPairing {
	type cellRestraint struct {
		value       float64
		computed    bool
		toolExposed bool
	}
	byCell := make(map[cellKey]cellRestraint, len(arts))
	traceDisplay := make(map[string]string)
	traceDifficulty := make(map[string]string)
	for _, a := range arts {
		value, computed := 0.0, false
		if restraintArtifactTraceContextValid(a) {
			value, computed = restraintSignals(a.Trace, a.ActualTranscript)
		}
		byCell[newCellKey(a.TraceID, a.CandidateModel)] = cellRestraint{
			value:       value,
			computed:    computed,
			toolExposed: computed && len(a.Trace.Tools) > 0,
		}
		tk := normalizeModelSelector(a.TraceID)
		if _, ok := traceDisplay[tk]; !ok {
			traceDisplay[tk] = a.TraceID
		}
		if _, ok := traceDifficulty[tk]; !ok {
			traceDifficulty[tk] = a.Trace.Golden.Difficulty
		}
	}

	traceKeys := make([]string, 0, len(traceDisplay))
	for tk := range traceDisplay {
		traceKeys = append(traceKeys, tk)
	}
	sort.Strings(traceKeys)

	var completeKeys []string
	perModel := make(map[string][]float64, len(lineup))
	var teCompleteKeys []string
	perModelTE := make(map[string][]float64, len(lineup))
	for _, tk := range traceKeys {
		complete, teComplete := true, true
		for _, model := range lineup {
			cr, ok := byCell[cellKey{trace: tk, model: modelKey(model)}]
			if !ok || !cr.computed {
				complete = false
			}
			if !ok || !cr.toolExposed {
				teComplete = false
			}
		}
		if complete {
			completeKeys = append(completeKeys, tk)
			for _, model := range lineup {
				cr := byCell[cellKey{trace: tk, model: modelKey(model)}]
				perModel[model] = append(perModel[model], cr.value)
			}
		}
		if teComplete {
			teCompleteKeys = append(teCompleteKeys, tk)
			for _, model := range lineup {
				cr := byCell[cellKey{trace: tk, model: modelKey(model)}]
				perModelTE[model] = append(perModelTE[model], cr.value)
			}
		}
	}

	rp := restraintPairing{
		PerModelMean:            make(map[string]float64, len(lineup)),
		CompleteN:               len(completeKeys),
		ToolExposedPerModelMean: make(map[string]float64, len(lineup)),
		ToolExposedN:            len(teCompleteKeys),
	}
	for _, model := range lineup {
		rp.PerModelMean[model] = mean(perModel[model])
		rp.ToolExposedPerModelMean[model] = mean(perModelTE[model])
	}

	base := perModel[baseline]
	for _, model := range lineup {
		if model == baseline {
			continue
		}
		vm := perModel[model]
		deltas := make([]float64, len(completeKeys))
		bd := baselineDelta{Model: model}
		for idx := range completeKeys {
			d := vm[idx] - base[idx]
			deltas[idx] = d
			switch {
			case d > 0:
				bd.Wins++
			case d < 0:
				bd.Losses++
			default:
				bd.Ties++
			}
		}
		bd.MeanDelta = mean(deltas)
		bd.CILow, bd.CIHigh = bootstrapDeltaCI(deltas, seed, bootstrapN)
		rp.BaselineSummary = append(rp.BaselineSummary, bd)
	}
	rp.ByDifficulty = restraintByDifficulty(completeKeys, traceDifficulty, perModel, lineup, baseline, seed, bootstrapN)
	return rp
}

// restraintByDifficulty buckets the complete subset by trace difficulty and
// computes a per-tier paired Δ reusing mean/bootstrapDeltaCI. completeKeys and
// perModel[model] are index-aligned (same order). Tiers render in canonical
// order; an empty difficulty is bucketed under "unspecified".
func restraintByDifficulty(completeKeys []string, traceDifficulty map[string]string, perModel map[string][]float64, lineup []string, baseline string, seed int64, bootstrapN int) []difficultyRestraint {
	order := []string{"obvious", "tempting", "adversarial", "unspecified"}
	idxByTier := map[string][]int{}
	for i, tk := range completeKeys {
		tier := traceDifficulty[tk]
		if tier == "" {
			tier = "unspecified"
		}
		idxByTier[tier] = append(idxByTier[tier], i)
	}
	var out []difficultyRestraint
	for _, tier := range order {
		idxs := idxByTier[tier]
		if len(idxs) == 0 {
			continue
		}
		dr := difficultyRestraint{Difficulty: tier, N: len(idxs), PerModelMean: map[string]float64{}}
		for _, model := range lineup {
			vals := make([]float64, len(idxs))
			for j, i := range idxs {
				vals[j] = perModel[model][i]
			}
			dr.PerModelMean[model] = mean(vals)
		}
		for _, model := range lineup {
			if model == baseline {
				continue
			}
			deltas := make([]float64, len(idxs))
			bd := baselineDelta{Model: model}
			for j, i := range idxs {
				d := perModel[model][i] - perModel[baseline][i]
				deltas[j] = d
				switch {
				case d > 0:
					bd.Wins++
				case d < 0:
					bd.Losses++
				default:
					bd.Ties++
				}
			}
			bd.MeanDelta = mean(deltas)
			bd.CILow, bd.CIHigh = bootstrapDeltaCI(deltas, seed, bootstrapN)
			dr.BaselineSummary = append(dr.BaselineSummary, bd)
		}
		out = append(out, dr)
	}
	return out
}

// restraintArtifactTraceContextValid reports whether an artifact carries enough
// embedded trace context to be trusted for restraint pairing. It requires a
// matching trace ID AND a golden final-answer rubric (criteria or substring),
// mirroring calibrationTraceFromArtifact's rubric check. The rubric guard is
// load-bearing: a truncated artifact like Trace{ID: TraceID} with a zero-value
// Golden has no ToolCalls, so without it restraintSignals would treat the
// rubric-less trace as golden-empty/restraint-eligible/held and inflate the
// paired restraint denominator. A real restraint-eligible trace always has a
// rubric even though its Golden.ToolCalls is empty.
func restraintArtifactTraceContextValid(a Artifact) bool {
	if strings.TrimSpace(a.Trace.ID) == "" || a.Trace.ID != a.TraceID {
		return false
	}
	return strings.TrimSpace(a.Trace.Golden.FinalAnswerCriteria) != "" ||
		strings.TrimSpace(a.Trace.Golden.FinalAnswerSubstring) != ""
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}
