package main

import (
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pairedTestArtifact builds a frozen Artifact for an arbitrary (trace, model)
// with a deterministic hash, so paired tests can construct multi-model corpora
// (testCalibrationArtifact hardcodes a single candidate model).
func pairedTestArtifact(traceID, model, answer string) Artifact {
	a := Artifact{
		TraceID:        traceID,
		CandidateModel: model,
		Trace: Trace{
			ID:     traceID,
			System: "system for " + traceID,
			Turns:  []Turn{{Role: "user", Content: "q for " + traceID}},
			Golden: Golden{FinalAnswerCriteria: "rubric for " + traceID},
		},
		ActualFinalAnswer: answer,
		ActualTranscript:  []Turn{{Role: "assistant", Content: answer}},
	}
	a.ArtifactHash = artifactHash(a)
	return a
}

func TestLineupFromArtifacts_SortedDedupedNormalized(t *testing.T) {
	arts := []Artifact{
		pairedTestArtifact("t1", "ollama/gemma4:31b", "a"),
		pairedTestArtifact("t1", "ollama/qwen3:8b", "a"),
		pairedTestArtifact("t2", "ollama/GEMMA4:31b", "a"), // dup of gemma after normalize
	}
	got := lineupFromArtifacts(arts)
	want := []string{"ollama/gemma4:31b", "ollama/qwen3:8b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineupFromArtifacts = %v; want %v", got, want)
	}
}

func TestComputePairedAnalysis_DedupesBareAndProviderPrefixedArtifacts(t *testing.T) {
	a1, l1 := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a2, l2 := labeledArtifact("t2", "a", "x", 0.0, "keith")
	arts := []Artifact{a1, a2}
	matched, stale := mustMatch(t, []Label{l1, l2}, arts)

	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if want := []string{"ollama/a"}; !reflect.DeepEqual(pa.Lineup, want) {
		t.Fatalf("Lineup = %v; want %v", pa.Lineup, want)
	}
	if pa.PerModelN != 2 {
		t.Fatalf("PerModelN = %d; want both labeled traces counted under one canonical model", pa.PerModelN)
	}
	if pa.AllMatchedN["ollama/a"] != 2 {
		t.Fatalf("AllMatchedN[ollama/a] = %d; want 2", pa.AllMatchedN["ollama/a"])
	}
}

// A model that appears only in artifacts with no fresh labels must still be in
// the lineup — otherwise matched-label inference would hide it completely.
func TestLineupFromArtifacts_IncludesModelWithoutLabels(t *testing.T) {
	arts := []Artifact{
		pairedTestArtifact("t1", "ollama/labeled", "a"),
		pairedTestArtifact("t1", "ollama/unlabeled", "a"),
	}
	got := lineupFromArtifacts(arts)
	want := []string{"ollama/labeled", "ollama/unlabeled"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lineupFromArtifacts = %v; want %v (unlabeled model must appear)", got, want)
	}
}

// When every paired delta is identical, every bootstrap resample has the same
// mean, so the CI collapses to that value regardless of seed.
func TestBootstrapDeltaCI_DegenerateAllEqual(t *testing.T) {
	lo, hi := bootstrapDeltaCI([]float64{0.5, 0.5, 0.5, 0.5}, 1, 10000)
	if lo != 0.5 || hi != 0.5 {
		t.Fatalf("bootstrapDeltaCI(all 0.5) = [%v, %v]; want [0.5, 0.5]", lo, hi)
	}
}

// With balanced extreme deltas {0,1}, the 2.5th-percentile resample mean lands
// in the all-zero tail and the 97.5th in the all-one tail, pinning the
// nearest-rank percentile mapping to [0.0, 1.0].
func TestBootstrapDeltaCI_PercentileMapping(t *testing.T) {
	lo, hi := bootstrapDeltaCI([]float64{0.0, 1.0}, 1, 10000)
	if lo != 0.0 || hi != 1.0 {
		t.Fatalf("bootstrapDeltaCI({0,1}) = [%v, %v]; want [0.0, 1.0]", lo, hi)
	}
}

// Determinism is the actual spec requirement: the same seed must reproduce the
// same CI exactly, and lo<=hi must always hold.
func TestBootstrapDeltaCI_DeterministicForSeed(t *testing.T) {
	deltas := []float64{0.0, 0.5, 1.0, 0.5, 0.0, 1.0, 0.5}
	lo1, hi1 := bootstrapDeltaCI(deltas, 1, 10000)
	lo2, hi2 := bootstrapDeltaCI(deltas, 1, 10000)
	if lo1 != lo2 || hi1 != hi2 {
		t.Fatalf("non-deterministic for seed=1: [%v,%v] vs [%v,%v]", lo1, hi1, lo2, hi2)
	}
	if lo1 > hi1 {
		t.Fatalf("lo>hi: [%v, %v]", lo1, hi1)
	}
}

func TestBootstrapDeltaCI_EmptyIsNaN(t *testing.T) {
	lo, hi := bootstrapDeltaCI(nil, 1, 10000)
	if !math.IsNaN(lo) || !math.IsNaN(hi) {
		t.Fatalf("bootstrapDeltaCI(nil) = [%v, %v]; want [NaN, NaN]", lo, hi)
	}
}

func TestMatchLabels_PartitionsMatchedAndStale(t *testing.T) {
	a1 := pairedTestArtifact("t1", "ollama/m", "ans1")
	labels := []Label{
		{TraceID: "t1", CandidateModel: "ollama/m", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		{TraceID: "t2", CandidateModel: "ollama/m", ArtifactHash: "sha256:gone", ExpectedAnswerQuality: 0.5},
	}
	matched, stale, err := matchLabels(labels, []Artifact{a1})
	if err != nil {
		t.Fatalf("matchLabels: %v", err)
	}
	if len(matched) != 1 || matched[0].Artifact.ArtifactHash != a1.ArtifactHash {
		t.Fatalf("matched = %+v; want 1 row for a1", matched)
	}
	if len(stale) != 1 || stale[0].TraceID != "t2" {
		t.Fatalf("stale = %+v; want 1 row for t2", stale)
	}
}

func TestMatchLabels_RejectsDuplicateMatchedHash(t *testing.T) {
	a1 := pairedTestArtifact("t1", "ollama/m", "ans1")
	labels := []Label{
		{TraceID: "t1", CandidateModel: "ollama/m", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		{TraceID: "t1", CandidateModel: "ollama/m", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 0.5},
	}
	if _, _, err := matchLabels(labels, []Artifact{a1}); err == nil {
		t.Fatalf("matchLabels accepted two labels for the same artifact hash; want error")
	}
}

func mustMatch(t *testing.T, labels []Label, arts []Artifact) ([]matchedLabel, []Label) {
	t.Helper()
	m, s, err := matchLabels(labels, arts)
	if err != nil {
		t.Fatalf("matchLabels: %v", err)
	}
	return m, s
}

func hasGap(gaps []completenessGap, trace, model string, reason gapReason) bool {
	for _, g := range gaps {
		if g.TraceID == trace && g.Model == model && g.Reason == reason {
			return true
		}
	}
	return false
}

// labeledArtifact builds an artifact and a matching label in one shot.
func labeledArtifact(traceID, model, answer string, q float64, labeler string) (Artifact, Label) {
	a := pairedTestArtifact(traceID, model, answer)
	l := Label{
		TraceID: traceID, CandidateModel: model, ArtifactHash: a.ArtifactHash,
		ExpectedAnswerQuality: q, Labeler: labeler,
	}
	return a, l
}

// TestComputePairedAnalysis_GapReasonsAndCompleteness exercises all three gap
// reasons (missing-artifact, missing-label, stale-label) and confirms a trace
// counts as paired-complete only when every lineup cell is labeled.
func TestComputePairedAnalysis_GapReasonsAndCompleteness(t *testing.T) {
	// t1: both A and B labeled -> complete.
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	// t2: A labeled, B has an artifact but NO label -> missing-label.
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.0, "keith")
	a2B := pairedTestArtifact("t2", "ollama/b", "x")
	// t3: A labeled, B has NO artifact at all -> missing-artifact.
	a3A, l3A := labeledArtifact("t3", "ollama/a", "x", 1.0, "keith")
	// t4: A labeled, B has an artifact + a STALE label (wrong hash) -> stale-label.
	a4A, l4A := labeledArtifact("t4", "ollama/a", "x", 1.0, "keith")
	a4B := pairedTestArtifact("t4", "ollama/b", "x")
	l4Bstale := Label{TraceID: "t4", CandidateModel: "ollama/b", ArtifactHash: "sha256:stale", ExpectedAnswerQuality: 0.5}

	arts := []Artifact{a1A, a1B, a2A, a2B, a3A, a4A, a4B}
	labels := []Label{l1A, l1B, l2A, l3A, l4A, l4Bstale}
	matched, stale := mustMatch(t, labels, arts)

	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}

	if want := []string{"ollama/a", "ollama/b"}; !reflect.DeepEqual(pa.Lineup, want) {
		t.Fatalf("Lineup = %v; want %v", pa.Lineup, want)
	}
	if want := []string{"t1"}; !reflect.DeepEqual(pa.CompleteTraces, want) {
		t.Fatalf("CompleteTraces = %v; want %v", pa.CompleteTraces, want)
	}
	if !hasGap(pa.Gaps, "t2", "ollama/b", gapMissingLabel) {
		t.Errorf("missing gap {t2, ollama/b, missing-label}; gaps=%+v", pa.Gaps)
	}
	if !hasGap(pa.Gaps, "t3", "ollama/b", gapMissingArtifact) {
		t.Errorf("missing gap {t3, ollama/b, missing-artifact}; gaps=%+v", pa.Gaps)
	}
	if !hasGap(pa.Gaps, "t4", "ollama/b", gapStaleLabel) {
		t.Errorf("missing gap {t4, ollama/b, stale-label}; gaps=%+v", pa.Gaps)
	}
}

// TestComputePairedAnalysis_PairedMeansDifferFromUnpaired pins that the paired-
// complete mean is computed only over the intersection, so it differs from the
// confounded all-matched-labels mean.
func TestComputePairedAnalysis_PairedMeansDifferFromUnpaired(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.0, "keith") // only A labeled on t2
	arts := []Artifact{a1A, a1B, a2A}
	labels := []Label{l1A, l1B, l2A}
	matched, stale := mustMatch(t, labels, arts)

	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if pa.PerModelN != 1 {
		t.Fatalf("PerModelN = %d; want 1 (only t1 is complete)", pa.PerModelN)
	}
	// Paired-complete mean for A is 1.0 (t1 only), NOT the confounded 0.5 it
	// would be averaged over both matched labels.
	if pa.PerModelMean["ollama/a"] != 1.0 {
		t.Errorf("paired mean A = %v; want 1.0 (unpaired would be 0.5)", pa.PerModelMean["ollama/a"])
	}
	if pa.PerModelMean["ollama/b"] != 0.0 {
		t.Errorf("paired mean B = %v; want 0.0", pa.PerModelMean["ollama/b"])
	}
}

// TestComputePairedAnalysis_PairwiseAndBaseline checks the win/loss/tie matrix
// (including ties) and that the baseline-focused summary agrees with the matrix.
func TestComputePairedAnalysis_PairwiseAndBaseline(t *testing.T) {
	// Two complete traces; A beats B once, ties once.
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.5, "keith")
	a2B, l2B := labeledArtifact("t2", "ollama/b", "x", 0.5, "keith")
	arts := []Artifact{a1A, a1B, a2A, a2B}
	labels := []Label{l1A, l1B, l2A, l2B}
	matched, stale := mustMatch(t, labels, arts)

	pa, err := computePairedAnalysis(matched, stale, arts, "ollama/b", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if len(pa.Pairwise) != 1 {
		t.Fatalf("Pairwise = %+v; want 1 record", pa.Pairwise)
	}
	rec := pa.Pairwise[0]
	// Records are stored A<B in lineup order: A=ollama/a, B=ollama/b.
	if rec.ModelA != "ollama/a" || rec.ModelB != "ollama/b" || rec.Wins != 1 || rec.Losses != 0 || rec.Ties != 1 {
		t.Fatalf("Pairwise[0] = %+v; want a-vs-b 1/0/1", rec)
	}
	// Baseline is B; the summary row for A must show A beating B once, tying once.
	if pa.Baseline != "ollama/b" {
		t.Fatalf("Baseline = %q; want ollama/b", pa.Baseline)
	}
	if len(pa.BaselineSummary) != 1 {
		t.Fatalf("BaselineSummary = %+v; want 1 row (A vs baseline B)", pa.BaselineSummary)
	}
	bd := pa.BaselineSummary[0]
	if bd.Model != "ollama/a" || bd.Wins != 1 || bd.Losses != 0 || bd.Ties != 1 {
		t.Fatalf("BaselineSummary[0] = %+v; want A 1/0/1 vs baseline", bd)
	}
	// MeanDelta = mean(A-B) over [1-0, 0.5-0.5] = 0.5.
	if bd.MeanDelta != 0.5 {
		t.Errorf("MeanDelta = %v; want 0.5", bd.MeanDelta)
	}
}

// TestComputePairedAnalysis_ResolutionTwoScales pins the resolution diagnostic
// at both the full-flip (1/n) and rubric-step (0.5/n) scales.
func TestComputePairedAnalysis_ResolutionTwoScales(t *testing.T) {
	var arts []Artifact
	var labels []Label
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		aA, lA := labeledArtifact(id, "ollama/a", "x", 1.0, "keith")
		aB, lB := labeledArtifact(id, "ollama/b", "x", 1.0, "keith")
		arts = append(arts, aA, aB)
		labels = append(labels, lA, lB)
	}
	matched, stale := mustMatch(t, labels, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if pa.PerModelN != 4 {
		t.Fatalf("PerModelN = %d; want 4", pa.PerModelN)
	}
	if pa.ResolutionFull != 0.25 {
		t.Errorf("ResolutionFull = %v; want 0.25 (1/4)", pa.ResolutionFull)
	}
	if pa.ResolutionStep != 0.125 {
		t.Errorf("ResolutionStep = %v; want 0.125 (0.5/4)", pa.ResolutionStep)
	}
}

// TestComputePairedAnalysis_LabelersSortedDeduped pins provenance: distinct
// labelers from the complete-trace evidence, sorted and deduped.
func TestComputePairedAnalysis_LabelersSortedDeduped(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "zoe")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 1.0, "amy")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 1.0, "amy")
	a2B, l2B := labeledArtifact("t2", "ollama/b", "x", 1.0, "zoe")
	arts := []Artifact{a1A, a1B, a2A, a2B}
	labels := []Label{l1A, l1B, l2A, l2B}
	matched, stale := mustMatch(t, labels, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if want := []string{"amy", "zoe"}; !reflect.DeepEqual(pa.Labelers, want) {
		t.Fatalf("Labelers = %v; want %v (sorted, deduped)", pa.Labelers, want)
	}
}

// A model with artifacts but zero labels stays in the lineup and produces a
// missing-label gap on every trace (never silently dropped).
func TestComputePairedAnalysis_FullyUnlabeledModelIsAllGap(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1C := pairedTestArtifact("t1", "ollama/c", "x") // never labeled
	arts := []Artifact{a1A, a1C}
	labels := []Label{l1A}
	matched, stale := mustMatch(t, labels, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if !contains(pa.Lineup, "ollama/c") {
		t.Fatalf("Lineup %v must include the unlabeled model ollama/c", pa.Lineup)
	}
	if !hasGap(pa.Gaps, "t1", "ollama/c", gapMissingLabel) {
		t.Fatalf("expected missing-label gap for the unlabeled model; gaps=%+v", pa.Gaps)
	}
	if len(pa.CompleteTraces) != 0 {
		t.Fatalf("CompleteTraces = %v; want none (c unlabeled)", pa.CompleteTraces)
	}
}

func TestComputePairedAnalysis_BaselineNotInLineupErrors(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	matched, stale := mustMatch(t, []Label{l1A}, []Artifact{a1A})
	if _, err := computePairedAnalysis(matched, stale, []Artifact{a1A}, "ollama/nope", 1, 10000); err == nil {
		t.Fatalf("computePairedAnalysis accepted a baseline not in the lineup; want error")
	}
}

// A baseline passed bare ("a") must resolve to the prefixed lineup form
// ("ollama/a") — labels are stored bare, artifacts carry the bench prefix.
func TestComputePairedAnalysis_BaselineBareResolvesToLineup(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	arts := []Artifact{a1A, a1B}
	matched, stale := mustMatch(t, []Label{l1A, l1B}, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "a", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if pa.Baseline != "ollama/a" {
		t.Fatalf("Baseline = %q; want ollama/a (bare 'a' must resolve to prefixed form)", pa.Baseline)
	}
}

// All artifacts having a blank CandidateModel yields an empty lineup; that must
// be a clean error, not a lineup[0] panic (symmetric with the no-artifacts guard).
func TestComputePairedAnalysis_EmptyLineupErrors(t *testing.T) {
	blank := pairedTestArtifact("t1", "", "x")
	if _, err := computePairedAnalysis(nil, nil, []Artifact{blank}, "", 1, 10000); err == nil {
		t.Fatalf("computePairedAnalysis accepted artifacts with no candidate models; want error")
	}
}

func TestComputePairedAnalysis_DuplicateArtifactCellErrors(t *testing.T) {
	a1 := pairedTestArtifact("t1", "ollama/a", "ans A")
	a2 := pairedTestArtifact("t1", "ollama/a", "ans B") // same (trace,model), different content/hash
	if _, err := computePairedAnalysis(nil, nil, []Artifact{a1, a2}, "", 1, 10000); err == nil {
		t.Fatalf("computePairedAnalysis accepted duplicate (trace,model) artifacts; want error")
	}
}

// Zero paired-complete traces must render cleanly: empty CompleteTraces, NaN
// resolution, and a still-populated gap worklist.
func TestComputePairedAnalysis_ZeroCompleteRendersClean(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B := pairedTestArtifact("t1", "ollama/b", "x") // unlabeled -> t1 incomplete
	arts := []Artifact{a1A, a1B}
	matched, stale := mustMatch(t, []Label{l1A}, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if len(pa.CompleteTraces) != 0 {
		t.Fatalf("CompleteTraces = %v; want empty", pa.CompleteTraces)
	}
	if !math.IsNaN(pa.ResolutionFull) || !math.IsNaN(pa.ResolutionStep) {
		t.Fatalf("resolution = [%v, %v]; want NaN when n=0", pa.ResolutionFull, pa.ResolutionStep)
	}
	if len(pa.Gaps) == 0 {
		t.Fatalf("expected a non-empty gap worklist even with zero complete traces")
	}
}

// TestComputePairedAnalysis_AllMatchedMeans pins the confounded all-matched
// per-model means (used for the "all matched labels" comparison table), which
// differ from the paired-complete means.
func TestComputePairedAnalysis_AllMatchedMeans(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.0, "keith")
	arts := []Artifact{a1A, a1B, a2A}
	matched, stale := mustMatch(t, []Label{l1A, l1B, l2A}, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	if pa.AllMatchedMean["ollama/a"] != 0.5 || pa.AllMatchedN["ollama/a"] != 2 {
		t.Errorf("all-matched A = %v (n=%d); want 0.5 (n=2)", pa.AllMatchedMean["ollama/a"], pa.AllMatchedN["ollama/a"])
	}
	if pa.AllMatchedMean["ollama/b"] != 0.0 || pa.AllMatchedN["ollama/b"] != 1 {
		t.Errorf("all-matched B = %v (n=%d); want 0.0 (n=1)", pa.AllMatchedMean["ollama/b"], pa.AllMatchedN["ollama/b"])
	}
}

// TestFormatPairedReport_AllSections renders a partially-complete corpus and
// pins every required section: paired-complete headline, all-matched table,
// completeness worklist, pairwise matrix, baseline summary with CI, and the
// two-scale resolution diagnostic.
func TestFormatPairedReport_AllSections(t *testing.T) {
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.0, "keith")
	a2B := pairedTestArtifact("t2", "ollama/b", "x") // unlabeled -> gap
	arts := []Artifact{a1A, a1B, a2A, a2B}
	matched, stale := mustMatch(t, []Label{l1A, l1B, l2A}, arts)
	pa, err := computePairedAnalysis(matched, stale, arts, "", 1, 10000)
	if err != nil {
		t.Fatalf("computePairedAnalysis: %v", err)
	}
	out := formatPairedReport(pa)
	for _, want := range []string{
		"Quality by model (paired-complete)",
		"Quality by model (all matched labels)",
		"Completeness worklist",
		"missing-label",
		"| t2 | ollama/b | missing-label |",
		"Pairwise win/loss/tie",
		"| ollama/a | ollama/b | 1 | 0 | 0 |",
		"Model vs baseline (ollama/a)",
		"95% CI",
		"Resolution diagnostic",
		"1/1",   // full-flip scale, n=1
		"0.5/1", // rubric-step scale, n=1
		"Labelers: keith",
		"Bootstrap: seed=1, n=10000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("paired report missing %q:\n%s", want, out)
		}
	}
	// Headline paired-complete mean for A is 1.00; the all-matched mean is 0.50.
	if !strings.Contains(out, "| ollama/a | 1.00 | 1 |") {
		t.Errorf("paired report missing paired-complete A row (1.00, n=1):\n%s", out)
	}
	if !strings.Contains(out, "| ollama/a | 0.50 | 2 |") {
		t.Errorf("paired report missing all-matched A row (0.50, n=2):\n%s", out)
	}
}

func TestRunPairedReport_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")

	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	a1B, l1B := labeledArtifact("t1", "ollama/b", "x", 0.0, "keith")
	a2A, l2A := labeledArtifact("t2", "ollama/a", "x", 0.0, "keith")
	a2B := pairedTestArtifact("t2", "ollama/b", "x") // unlabeled -> gap
	if err := writeJSONL(artsPath, []any{a1A, a1B, a2A, a2B}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labelsPath, []any{l1A, l1B, l2A}); err != nil {
		t.Fatal(err)
	}

	out, err := runPairedReport(labelsPath, artsPath, "", nil)
	if err != nil {
		t.Fatalf("runPairedReport: %v", err)
	}
	for _, want := range []string{
		"Quality by model (paired-complete)",
		"Completeness worklist",
		"| t2 | ollama/b | missing-label |",
		"Bootstrap: seed=1, n=10000",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("runPairedReport output missing %q:\n%s", want, out)
		}
	}
}

func TestRunPairedReport_BaselineNotInLineupErrors(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
	a1A, l1A := labeledArtifact("t1", "ollama/a", "x", 1.0, "keith")
	if err := writeJSONL(artsPath, []any{a1A}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labelsPath, []any{l1A}); err != nil {
		t.Fatal(err)
	}
	if _, err := runPairedReport(labelsPath, artsPath, "ollama/ghost", nil); err == nil {
		t.Fatalf("runPairedReport accepted a baseline not in the lineup; want error")
	}
}

func TestRunPairedReport_FilterRestrictsToChallengePartition(t *testing.T) {
	dir := t.TempDir()
	artsPath := filepath.Join(dir, "artifacts.jsonl")
	labelsPath := filepath.Join(dir, "labels.jsonl")
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
	filter := &corpusFilter{
		Manifest: Manifest{Entries: []ManifestEntry{
			{TraceID: "nat1", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
			{TraceID: "r2c-fabrication-01", Partition: PartitionChallenge, Category: "fabrication", AllowedAsModelEvidence: true},
		}},
		Selection: corpusSelection{Partitions: []CorpusPartition{PartitionChallenge}},
	}
	out, err := runPairedReport(labelsPath, artsPath, "", filter)
	if err != nil {
		t.Fatalf("runPairedReport with challenge filter: %v", err)
	}
	if !strings.Contains(out, "Paired-complete traces: 1 of 1") {
		t.Fatalf("paired report did not restrict to exactly the challenge trace:\n%s", out)
	}
	if strings.Contains(out, "nat1") {
		t.Fatalf("natural trace leaked into the challenge-only paired report:\n%s", out)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
