package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mixedArtifact builds one legacy/mixed/topline-arm artifact with valid 3c
// metadata; mut (optional) adjusts the eval before the artifact hash is
// computed, so every variant stays self-consistent.
func mixedArtifact(pair string, mode AssemblyMode, model string, mut func(*AssemblyEval)) Artifact {
	ae := &AssemblyEval{
		PairID:      pair,
		Mode:        mode,
		StateDigest: "sha256:state-" + pair,
		Budget:      4096,
		Stratum:     "shallow",
	}
	if mut != nil {
		mut(ae)
	}
	tr := Trace{
		ID:           pair + "-" + string(mode),
		Source:       "assembly-mixed-corpus",
		System:       "answer from the provided agent state",
		Turns:        []Turn{{Role: "user", Content: "state + question"}},
		Golden:       Golden{FinalAnswerCriteria: "names the value"},
		AssemblyEval: ae,
	}
	a := Artifact{TraceID: tr.ID, CandidateModel: model, Trace: tr,
		ActualFinalAnswer: "answer"}
	a.ArtifactHash = artifactHash(a)
	return a
}

// appendMixedPairs appends n complete labeled legacy/mixed pairs for model
// with pair IDs mx-<start+i> (zero-padded, so lexicographic order is build
// order). mut, when non-nil, runs on BOTH arms' evals (distinguish by
// ae.Mode) before hashing.
func appendMixedPairs(arts []Artifact, labels []Label, model string, start, n int, legacyQ, mixedQ float64, mut func(i int, ae *AssemblyEval)) ([]Artifact, []Label) {
	for i := 0; i < n; i++ {
		pair := fmt.Sprintf("mx-%03d", start+i)
		idx := i
		wrap := func(ae *AssemblyEval) {
			if mut != nil {
				mut(idx, ae)
			}
		}
		l := mixedArtifact(pair, AssemblyLegacy, model, wrap)
		m := mixedArtifact(pair, AssemblyMixed, model, wrap)
		arts = append(arts, l, m)
		labels = append(labels, labelFor(l, legacyQ), labelFor(m, mixedQ))
	}
	return arts, labels
}

func mustMixedReport(t *testing.T, arts []Artifact, labels []Label) *AssemblyReport {
	t.Helper()
	rep, err := computeAssemblyReport(arts, labels, 1, 10000, nil)
	if err != nil {
		t.Fatalf("computeAssemblyReport: %v", err)
	}
	return rep
}

func TestTraceValidatesMixedAssemblyModes(t *testing.T) {
	valid := func(mode AssemblyMode, mut func(*AssemblyEval)) Trace {
		return mixedArtifact("case-1", mode, "m", mut).Trace
	}

	for _, mode := range []AssemblyMode{AssemblyLegacy, AssemblyMixed, AssemblyTopline} {
		if err := validateTrace(valid(mode, nil)); err != nil {
			t.Fatalf("valid %s trace rejected: %v", mode, err)
		}
	}

	// Topline needs only PairID — no budget, no digest, no candidates.
	bare := valid(AssemblyTopline, func(ae *AssemblyEval) {
		ae.Budget = 0
		ae.StateDigest = ""
	})
	if err := validateTrace(bare); err != nil {
		t.Fatalf("bare topline trace rejected: %v", err)
	}

	// Unknown modes stay rejected.
	unknown := valid(AssemblyLegacy, nil)
	unknown.AssemblyEval.Mode = "outline"
	if err := validateTrace(unknown); err == nil || !strings.Contains(err.Error(), "assembly_eval") {
		t.Fatalf("unknown mode accepted or wrong error: %v", err)
	}

	// legacy/mixed require positive Budget and a StateDigest.
	for _, mode := range []AssemblyMode{AssemblyLegacy, AssemblyMixed} {
		noBudget := valid(mode, func(ae *AssemblyEval) { ae.Budget = 0 })
		if err := validateTrace(noBudget); err == nil || !strings.Contains(err.Error(), "budget") {
			t.Fatalf("%s without budget accepted or wrong error: %v", mode, err)
		}
		noDigest := valid(mode, func(ae *AssemblyEval) { ae.StateDigest = "" })
		if err := validateTrace(noDigest); err == nil || !strings.Contains(err.Error(), "state_digest") {
			t.Fatalf("%s without state_digest accepted or wrong error: %v", mode, err)
		}
	}

	// PairID stays required for every mode.
	noPair := valid(AssemblyTopline, func(ae *AssemblyEval) { ae.PairID = "" })
	if err := validateTrace(noPair); err == nil || !strings.Contains(err.Error(), "pair_id") {
		t.Fatalf("topline without pair_id accepted or wrong error: %v", err)
	}

	// 3a modes are unaffected: flat still passes with candidate metadata and
	// no budget/digest, and still fails without prompt tokens.
	if err := validateTrace(validAssemblyTrace(AssemblyFlat)); err != nil {
		t.Fatalf("flat trace regression: %v", err)
	}
	flatNoTokens := validAssemblyTrace(AssemblyFlat)
	flatNoTokens.AssemblyEval.EstimatedPromptTokens = 0
	if err := validateTrace(flatNoTokens); err == nil || !strings.Contains(err.Error(), "estimated_prompt_tokens") {
		t.Fatalf("flat without tokens accepted or wrong error: %v", err)
	}
}

// TestAssemblyReportKindKeyedPairing puts the SAME PairID under both kinds in
// one artifact set: pairing must key on (kind, pair, model) and complete both
// pairs in separate sections without collision.
func TestAssemblyReportKindKeyedPairing(t *testing.T) {
	ids := []string{"c1", "c2"}
	f := assemblyArtifact("case-0", AssemblyFlat, "m", 1000, ids)
	p := assemblyArtifact("case-0", AssemblyProgressive, "m", 500, ids)
	l := mixedArtifact("case-0", AssemblyLegacy, "m", nil)
	x := mixedArtifact("case-0", AssemblyMixed, "m", nil)
	arts := []Artifact{f, p, l, x}
	labels := []Label{labelFor(f, 0.5), labelFor(p, 1.0), labelFor(l, 1.0), labelFor(x, 0.25)}

	rep := mustMixedReport(t, arts, labels)
	if len(rep.Models) != 1 || len(rep.LegacyMixedModels) != 1 {
		t.Fatalf("models=%d mixed models=%d, want 1/1", len(rep.Models), len(rep.LegacyMixedModels))
	}
	fp := rep.Models[0]
	if fp.Pairs != 1 || fp.InvalidPairs != 0 || fp.PairingGaps != 0 {
		t.Fatalf("flat-progressive pairs/invalid/gaps = %d/%d/%d, want 1/0/0",
			fp.Pairs, fp.InvalidPairs, fp.PairingGaps)
	}
	if len(fp.Deltas) != 1 || fp.Deltas[0] != 0.5 {
		t.Fatalf("flat-progressive deltas = %v, want [0.5]", fp.Deltas)
	}
	lm := rep.LegacyMixedModels[0]
	if lm.Pairs != 1 || len(lm.Exclusions) != 0 {
		t.Fatalf("legacy-mixed pairs=%d exclusions=%v, want 1 and none", lm.Pairs, lm.Exclusions)
	}
	if len(lm.Deltas) != 1 || lm.Deltas[0] != -0.75 {
		t.Fatalf("legacy-mixed deltas = %v, want [-0.75]", lm.Deltas)
	}
	if rep.LegacyMixedDecisionRule == "" {
		t.Fatal("legacy-mixed section missing its registered decision rule header")
	}
}

// TestAssemblyReportInvalidPairContract: a bad pair is excluded WITH a reason
// and the report still emits with every other pair intact.
func TestAssemblyReportInvalidPairContract(t *testing.T) {
	arts, labels := appendMixedPairs(nil, nil, "m", 0, 2, 0.5, 1.0, nil)

	// Cross-kind mode split: flat arm + mixed arm under one PairID.
	odd := assemblyArtifact("odd", AssemblyFlat, "m", 1000, []string{"c1"})
	oddMixed := mixedArtifact("odd", AssemblyMixed, "m", nil)
	arts = append(arts, odd, oddMixed)
	labels = append(labels, labelFor(odd, 0.5), labelFor(oddMixed, 0.5))

	// StateDigest mismatch across a legacy-mixed pair.
	dl := mixedArtifact("pair-digest", AssemblyLegacy, "m", nil)
	dm := mixedArtifact("pair-digest", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.StateDigest = "sha256:other"
	})
	arts = append(arts, dl, dm)
	labels = append(labels, labelFor(dl, 0.5), labelFor(dm, 0.5))

	// Budget mismatch across a legacy-mixed pair.
	bl := mixedArtifact("pair-budget", AssemblyLegacy, "m", nil)
	bm := mixedArtifact("pair-budget", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.Budget = 2048
	})
	arts = append(arts, bl, bm)
	labels = append(labels, labelFor(bl, 0.5), labelFor(bm, 0.5))

	// Candidate-ID mismatch stays an invariant for the new kind too.
	cl := mixedArtifact("pair-cand", AssemblyLegacy, "m", func(ae *AssemblyEval) {
		ae.CandidateIDs = []string{"c1"}
	})
	cm := mixedArtifact("pair-cand", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.CandidateIDs = []string{"c2"}
	})
	arts = append(arts, cl, cm)
	labels = append(labels, labelFor(cl, 0.5), labelFor(cm, 0.5))

	// Stratum mismatch across arms: stratum drives the registered per-stratum
	// floor and the CI stratification, so divergent arms must not pair.
	sl := mixedArtifact("pair-stratum", AssemblyLegacy, "m", nil)
	sm := mixedArtifact("pair-stratum", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.Stratum = "deep"
	})
	arts = append(arts, sl, sm)
	labels = append(labels, labelFor(sl, 0.5), labelFor(sm, 0.5))

	// ScenarioFamily mismatch across arms: family drives cluster structure.
	fl := mixedArtifact("pair-family", AssemblyLegacy, "m", func(ae *AssemblyEval) {
		ae.ScenarioFamily = "fam-a"
	})
	fm := mixedArtifact("pair-family", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.ScenarioFamily = "fam-b"
	})
	arts = append(arts, fl, fm)
	labels = append(labels, labelFor(fl, 0.5), labelFor(fm, 0.5))

	rep := mustMixedReport(t, arts, labels)
	lm := rep.LegacyMixedModels[0]
	if lm.Pairs != 2 {
		t.Fatalf("complete pairs = %d, want 2 (bad pairs must not pair)", lm.Pairs)
	}
	want := map[string]string{
		"odd":          "missing-legacy-arm",
		"pair-digest":  "state-digest-mismatch",
		"pair-budget":  "budget-mismatch",
		"pair-cand":    "candidate-ids-mismatch",
		"pair-stratum": "stratum-mismatch",
		"pair-family":  "scenario-family-mismatch",
	}
	got := map[string]string{}
	for _, e := range lm.Exclusions {
		if e.Kind != "legacy-mixed" {
			t.Fatalf("exclusion %v has kind %q, want legacy-mixed", e, e.Kind)
		}
		got[e.PairID] = e.Reason
	}
	for pair, reason := range want {
		if got[pair] != reason {
			t.Errorf("exclusion for %q = %q, want %q", pair, got[pair], reason)
		}
	}
	if len(lm.Exclusions) != len(want) {
		t.Fatalf("exclusions = %v, want exactly %d entries", lm.Exclusions, len(want))
	}
	// The stranded flat arm lands on the 3a side as an ordinary pairing gap.
	if rep.Models[0].PairingGaps != 1 {
		t.Fatalf("flat-progressive gaps = %d, want 1", rep.Models[0].PairingGaps)
	}
}

// TestAssemblyReportRuleV2Boundaries exercises rule v2 end to end with
// uniform delta sets (a uniform set makes every bootstrap resample mean
// identical, so the CI is a point and the decision is exact). The single
// float64-unrepresentable boundary — CI low landing EXACTLY on -0.10 — is
// pinned at the decision-function level: 60 accumulated copies of
// float64(-0.10) drift to -0.0999999999999999084, so no constructed label
// set can produce that CI bound through the bootstrap.
func TestAssemblyReportRuleV2Boundaries(t *testing.T) {
	buildReport := func(legacyQ, mixedQ float64) AssemblyMixedModelReport {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, legacyQ, mixedQ, nil)
		return mustMixedReport(t, arts, labels).LegacyMixedModels[0]
	}

	// Uniform +0.5 deltas with ZERO token reduction anywhere (equal budgets,
	// zero shed, zero raw/estimated tokens): the rule must still improve —
	// token numbers are descriptive only and never consulted.
	improved := buildReport(0.5, 1.0)
	if improved.Decision != "quality-improved" {
		t.Fatalf("uniform +0.5 with zero token reduction: decision = %q, want quality-improved", improved.Decision)
	}
	if improved.DeltaCILow != 0.5 || improved.DeltaCIHigh != 0.5 {
		t.Fatalf("uniform +0.5 CI = [%v, %v], want [0.5, 0.5]", improved.DeltaCILow, improved.DeltaCIHigh)
	}

	// Uniform -0.5: upper bound < -0.10.
	if regressed := buildReport(1.0, 0.5); regressed.Decision != "materially-regressed" {
		t.Fatalf("uniform -0.5: decision = %q, want materially-regressed", regressed.Decision)
	}

	// Uniform -0.0625 (dyadic, so the CI is exactly [-0.0625, -0.0625]):
	// lower bound > -0.10 but not > 0.
	if non := buildReport(0.5625, 0.5); non.Decision != "noninferior" {
		t.Fatalf("uniform -0.0625: decision = %q, want noninferior", non.Decision)
	}

	// Strict-inequality boundaries, decision-function level.
	boundaries := []struct {
		lo, hi float64
		want   string
	}{
		{-0.10, 0.20, "inconclusive"},  // lower EXACTLY -0.10 is NOT noninferior
		{-0.10, -0.10, "inconclusive"}, // upper exactly -0.10 is NOT materially-regressed
		{0.0, 0.30, "noninferior"},     // lower exactly 0 is NOT improved
		{-0.20, 0.30, "inconclusive"},
		{-0.60, -0.15, "materially-regressed"},
		{0.01, 0.40, "quality-improved"},
	}
	for _, tc := range boundaries {
		if got := assemblyMixedDecision(tc.lo, tc.hi); got != tc.want {
			t.Errorf("assemblyMixedDecision(%v, %v) = %q, want %q", tc.lo, tc.hi, got, tc.want)
		}
	}

	// The header carries the registered rule text verbatim.
	rep := func() *AssemblyReport {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, 1, 0.5, 1.0, nil)
		return mustMixedReport(t, arts, labels)
	}()
	const wantRule = "legacy-mixed rule v2 (registered): minimum 60 complete labeled non-control pairs pooled and minimum 12 per stratum present; " +
		"quality-improved: stratified-bootstrap CI low > 0; noninferior: CI low strictly > -0.10; " +
		"materially-regressed: CI high < -0.10; else inconclusive; " +
		"token and pressure numbers are descriptive only, never consulted; registered pressure fraction f=0.6"
	if rep.LegacyMixedDecisionRule != wantRule {
		t.Fatalf("legacy-mixed rule header:\n got %q\nwant %q", rep.LegacyMixedDecisionRule, wantRule)
	}
}

// TestAssemblyReportStratifiedBootstrap pins seed determinism, cluster
// integrity (scenario-family clusters resample as a unit), and the
// pooled-CI-only contract for per-stratum entries.
func TestAssemblyReportStratifiedBootstrap(t *testing.T) {
	build := func(family string) ([]Artifact, []Label) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, 58, 0.5, 1.0, nil)
		// Two strongly-negative twin pairs; family "" means each pair is its
		// own cluster (the naive/unclustered shape).
		for _, pair := range []string{"tw-000", "tw-001"} {
			mut := func(ae *AssemblyEval) { ae.ScenarioFamily = family }
			l := mixedArtifact(pair, AssemblyLegacy, "m", mut)
			x := mixedArtifact(pair, AssemblyMixed, "m", mut)
			arts = append(arts, l, x)
			labels = append(labels, labelFor(l, 1.0), labelFor(x, 0.0))
		}
		return arts, labels
	}

	artsA, labelsA := build("twin-fam")
	first := mustMixedReport(t, artsA, labelsA).LegacyMixedModels[0]
	second := mustMixedReport(t, artsA, labelsA).LegacyMixedModels[0]
	if first.DeltaCILow != second.DeltaCILow || first.DeltaCIHigh != second.DeltaCIHigh {
		t.Fatalf("bootstrap not deterministic: [%v, %v] vs [%v, %v]",
			first.DeltaCILow, first.DeltaCIHigh, second.DeltaCILow, second.DeltaCIHigh)
	}

	// Removing the family IDs (every pair its own cluster) must change the
	// CI, and DIRECTIONALLY: the two -1.0 twins moving together fatten the
	// lower tail, so the clustered lower bound sits strictly below the naive
	// per-pair resampling's lower bound.
	artsB, labelsB := build("")
	naive := mustMixedReport(t, artsB, labelsB).LegacyMixedModels[0]
	if !(first.DeltaCILow < naive.DeltaCILow) {
		t.Fatalf("clustered CI low %v not below naive CI low %v; twin clusters are not moving together",
			first.DeltaCILow, naive.DeltaCILow)
	}

	// Per-stratum entries: descriptives only.
	if len(first.Strata) != 1 {
		t.Fatalf("strata = %v, want exactly one", first.Strata)
	}
	s := first.Strata[0]
	if s.Stratum != "shallow" || s.Pairs != 60 || s.Wins != 58 || s.Losses != 2 || s.Ties != 0 {
		t.Fatalf("stratum entry = %+v, want shallow/60/58/2/0", s)
	}
	if s.MeanDelta != 0.45 { // (58*0.5 - 2*1.0) / 60
		t.Fatalf("stratum mean delta = %v, want 0.45", s.MeanDelta)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{
		"stratum": true, "pairs": true, "mean_delta": true,
		"wins": true, "losses": true, "ties": true,
	}
	for k := range keys {
		if !wantKeys[k] {
			t.Errorf("unexpected per-stratum key %q", k)
		}
		for _, banned := range []string{"ci", "low", "high", "lower", "upper"} {
			if strings.Contains(k, banned) {
				t.Errorf("per-stratum key %q leaks a CI field (%q); pooled CI only", k, banned)
			}
		}
	}
	if len(keys) != len(wantKeys) {
		t.Fatalf("per-stratum keys = %v, want exactly %v", keys, wantKeys)
	}
}

func TestAssemblyReportCompleteness(t *testing.T) {
	t.Run("insufficient corpus below pooled floor", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs-1, 0.5, 1.0, nil)
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs-1 || lm.Decision != "insufficient-corpus" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-corpus",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs-1)
		}
	})

	t.Run("stratum floor", func(t *testing.T) {
		// 60 pooled, but the "deep" stratum holds only 11.
		arts, labels := appendMixedPairs(nil, nil, "m", 0, 49, 0.5, 1.0, nil)
		arts, labels = appendMixedPairs(arts, labels, "m", 49, 11, 0.5, 1.0, func(_ int, ae *AssemblyEval) {
			ae.Stratum = "deep"
		})
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs || lm.Decision != "insufficient-stratum-balance" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-stratum-balance",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs)
		}
	})

	t.Run("unlabeled and invalid pairs are excluded with reasons", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		// Unlabeled: both arms captured, mixed arm never labeled.
		ul := mixedArtifact("pair-nolabel", AssemblyLegacy, "m", nil)
		um := mixedArtifact("pair-nolabel", AssemblyMixed, "m", nil)
		arts = append(arts, ul, um)
		labels = append(labels, labelFor(ul, 0.5))
		// Invalidated: digest mismatch.
		dl := mixedArtifact("pair-digest", AssemblyLegacy, "m", nil)
		dm := mixedArtifact("pair-digest", AssemblyMixed, "m", func(ae *AssemblyEval) {
			ae.StateDigest = "sha256:other"
		})
		arts = append(arts, dl, dm)
		labels = append(labels, labelFor(dl, 0.5), labelFor(dm, 0.5))

		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d", lm.Pairs, assemblyMixedMinimumPairs)
		}
		got := map[string]string{}
		for _, e := range lm.Exclusions {
			got[e.PairID] = e.Reason
		}
		if got["pair-nolabel"] != "unlabeled" || got["pair-digest"] != "state-digest-mismatch" {
			t.Fatalf("exclusions = %v, want unlabeled + state-digest-mismatch entries", lm.Exclusions)
		}
		if lm.Decision != "quality-improved" {
			t.Fatalf("decision = %q, want quality-improved despite exclusions", lm.Decision)
		}
	})
}

// TestAssemblyReportAuxModes: topline artifacts are descriptive-only (never
// paired, never counted toward completeness) and control pairs are excluded
// from the verdict CI (flipping their labels cannot move the CI or status).
func TestAssemblyReportAuxModes(t *testing.T) {
	build := func(controlLegacyQ, controlMixedQ float64) ([]Artifact, []Label) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		for _, pair := range []string{"ct-000", "ct-001"} {
			mut := func(ae *AssemblyEval) { ae.Control = true }
			l := mixedArtifact(pair, AssemblyLegacy, "m", mut)
			x := mixedArtifact(pair, AssemblyMixed, "m", mut)
			arts = append(arts, l, x)
			labels = append(labels, labelFor(l, controlLegacyQ), labelFor(x, controlMixedQ))
		}
		top := []struct {
			pair    string
			stratum string
			quality float64
			labeled bool
		}{
			{"top-000", "shallow", 0.75, true},
			{"top-001", "deep", 0.25, true},
			{"top-002", "shallow", 0, false},
		}
		for _, tc := range top {
			a := mixedArtifact(tc.pair, AssemblyTopline, "m", func(ae *AssemblyEval) {
				ae.Stratum = tc.stratum
			})
			arts = append(arts, a)
			if tc.labeled {
				labels = append(labels, labelFor(a, tc.quality))
			}
		}
		return arts, labels
	}

	arts, labels := build(0.25, 1.0)
	rep := mustMixedReport(t, arts, labels)
	lm := rep.LegacyMixedModels[0]
	if lm.Pairs != assemblyMixedMinimumPairs {
		t.Fatalf("pairs = %d, want %d (topline and control pairs must not count)", lm.Pairs, assemblyMixedMinimumPairs)
	}
	if len(lm.Exclusions) != 0 {
		t.Fatalf("exclusions = %v, want none (topline/control are not exclusions)", lm.Exclusions)
	}
	if lm.ControlPairs != 2 {
		t.Fatalf("control pairs = %d, want 2", lm.ControlPairs)
	}
	if len(lm.ControlAbsDeltas) != 2 || lm.ControlAbsDeltas[0] != 0.75 || lm.ControlAbsDeltas[1] != 0.75 {
		t.Fatalf("control |deltas| = %v, want [0.75 0.75]", lm.ControlAbsDeltas)
	}
	if lm.Decision != "quality-improved" {
		t.Fatalf("decision = %q, want quality-improved", lm.Decision)
	}

	// Flip the control labels: verdict CI and status must be identical.
	artsFlip, labelsFlip := build(1.0, 0.25)
	flip := mustMixedReport(t, artsFlip, labelsFlip).LegacyMixedModels[0]
	if flip.DeltaCILow != lm.DeltaCILow || flip.DeltaCIHigh != lm.DeltaCIHigh || flip.Decision != lm.Decision {
		t.Fatalf("flipping control labels moved the verdict: [%v, %v] %q vs [%v, %v] %q",
			lm.DeltaCILow, lm.DeltaCIHigh, lm.Decision, flip.DeltaCILow, flip.DeltaCIHigh, flip.Decision)
	}

	// Topline descriptive section.
	if len(rep.Topline) != 1 {
		t.Fatalf("topline sections = %d, want 1", len(rep.Topline))
	}
	tl := rep.Topline[0]
	if tl.CandidateModel != "m" || tl.Count != 3 || tl.Labeled != 2 || tl.MeanQuality != 0.5 {
		t.Fatalf("topline = %+v, want m/3/2/0.5", tl)
	}
	if len(tl.ByStratum) != 2 {
		t.Fatalf("topline strata = %v, want 2", tl.ByStratum)
	}
	deep, shallow := tl.ByStratum[0], tl.ByStratum[1]
	if deep.Stratum != "deep" || deep.Count != 1 || deep.Labeled != 1 || deep.MeanQuality != 0.25 {
		t.Fatalf("deep topline stratum = %+v", deep)
	}
	if shallow.Stratum != "shallow" || shallow.Count != 2 || shallow.Labeled != 1 || shallow.MeanQuality != 0.75 {
		t.Fatalf("shallow topline stratum = %+v", shallow)
	}
}

// TestAssemblyReportScenarioFamilyCrossesStrata: a non-empty family under
// more than one stratum cannot cluster-within-stratum; every pair it touches
// is excluded with a structural reason instead of silently splitting.
func TestAssemblyReportScenarioFamilyCrossesStrata(t *testing.T) {
	arts, labels := appendMixedPairs(nil, nil, "m", 0, 58, 0.5, 1.0, nil)
	for i, stratum := range []string{"shallow", "deep"} {
		pair := fmt.Sprintf("xf-%03d", i)
		mut := func(ae *AssemblyEval) {
			ae.Stratum = stratum
			ae.ScenarioFamily = "xfam"
		}
		l := mixedArtifact(pair, AssemblyLegacy, "m", mut)
		x := mixedArtifact(pair, AssemblyMixed, "m", mut)
		arts = append(arts, l, x)
		labels = append(labels, labelFor(l, 0.5), labelFor(x, 1.0))
	}

	lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
	if lm.Pairs != 58 {
		t.Fatalf("pairs = %d, want 58 (cross-strata family pairs must be excluded)", lm.Pairs)
	}
	got := map[string]string{}
	for _, e := range lm.Exclusions {
		got[e.PairID] = e.Reason
	}
	for _, pair := range []string{"xf-000", "xf-001"} {
		if got[pair] != "scenario-family-crosses-strata" {
			t.Errorf("exclusion for %q = %q, want scenario-family-crosses-strata", pair, got[pair])
		}
	}
	if len(lm.Exclusions) != 2 {
		t.Fatalf("exclusions = %v, want exactly the two xfam pairs", lm.Exclusions)
	}
	// The strata section is rebuilt from the filtered set: only "shallow"
	// remains, and the excluded deltas are gone from the pooled evidence.
	if len(lm.Strata) != 1 || lm.Strata[0].Stratum != "shallow" || lm.Strata[0].Pairs != 58 {
		t.Fatalf("strata = %+v, want shallow/58 only", lm.Strata)
	}
}

func TestMedianOf(t *testing.T) {
	cases := []struct {
		name string
		xs   []float64
		want float64
	}{
		{"even count averages the middle pair", []float64{1, 2, 3, 4}, 2.5},
		{"odd count takes the middle", []float64{1, 2, 3}, 2},
		{"unsorted input", []float64{3, 1, 4, 2}, 2.5},
		{"single value", []float64{7}, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := medianOf(tc.xs); got != tc.want {
				t.Fatalf("medianOf(%v) = %v, want %v", tc.xs, got, tc.want)
			}
		})
	}
}

// TestAssemblyReportNoCrossArmCauseDiff: pressure/shed data is reported
// per-arm ONLY. The marshaled model report must carry the exact allowed key
// set inside arm_pressure and no pressure-flavored key anywhere else — a
// cross-arm cause-diff field has nowhere to hide.
func TestAssemblyReportNoCrossArmCauseDiff(t *testing.T) {
	arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, func(_ int, ae *AssemblyEval) {
		if ae.Mode == AssemblyLegacy {
			ae.ShedMessages, ae.ShedBytes, ae.PressureLevel = 10, 2000, "high"
		} else {
			ae.ShedMessages, ae.ShedBytes, ae.OmittedSubjects, ae.PressureLevel = 4, 800, 2, "low"
		}
		ae.RawStateTokens = 9000
	})
	lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]

	if len(lm.ArmPressure) != 2 {
		t.Fatalf("arm pressure entries = %v, want legacy + mixed", lm.ArmPressure)
	}
	leg, mix := lm.ArmPressure[0], lm.ArmPressure[1]
	if leg.Mode != "legacy" || leg.MedianShedMessages != 10 || leg.MedianShedBytes != 2000 ||
		leg.MedianOmittedSubjects != 0 || leg.PressureLevels["high"] != assemblyMixedMinimumPairs {
		t.Fatalf("legacy arm pressure = %+v", leg)
	}
	if mix.Mode != "mixed" || mix.MedianShedMessages != 4 || mix.MedianShedBytes != 800 ||
		mix.MedianOmittedSubjects != 2 || mix.PressureLevels["low"] != assemblyMixedMinimumPairs {
		t.Fatalf("mixed arm pressure = %+v", mix)
	}

	raw, err := json.Marshal(lm)
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}

	// Exact allowed key set for each pressure entry.
	var armEntries []map[string]json.RawMessage
	if err := json.Unmarshal(report["arm_pressure"], &armEntries); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"mode": true, "median_shed_messages": true, "median_shed_bytes": true,
		"median_omitted_subjects": true, "pressure_levels": true,
	}
	for _, entry := range armEntries {
		if len(entry) != len(allowed) {
			t.Fatalf("arm pressure keys = %v, want exactly %v", entry, allowed)
		}
		for k := range entry {
			if !allowed[k] {
				t.Errorf("arm pressure key %q not in the allowed set", k)
			}
		}
	}

	// No pressure-flavored key may exist anywhere OUTSIDE arm_pressure.
	delete(report, "arm_pressure")
	rest, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			for k, child := range node {
				for _, banned := range []string{"shed", "omitted", "pressure", "cause", "diff"} {
					if strings.Contains(k, banned) {
						t.Errorf("key %q outside arm_pressure matches cross-arm cause-diff pattern %q", k, banned)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	var tree any
	if err := json.Unmarshal(rest, &tree); err != nil {
		t.Fatal(err)
	}
	walk(tree)
}

// --- forced-choice sign test (registered secondary analysis, #331 3c) ---

// TestSignTestTwoSidedP pins the exact two-sided binomial sign test.
// Hand check for (wins=8, n=10): upper-tail extreme k = max(8, 2) = 8;
// tail = C(10,8)+C(10,9)+C(10,10) = 45+10+1 = 56; p = 2*56/2^10 =
// 112/1024 = 0.109375 (every term exactly representable in float64).
func TestSignTestTwoSidedP(t *testing.T) {
	cases := []struct {
		wins, n int
		want    float64
	}{
		{8, 10, 0.109375},
		{2, 10, 0.109375}, // symmetric: k = max(wins, n-wins)
		{5, 5, 0.0625},    // 2*C(5,5)/2^5 = 2/32
		{2, 4, 1},         // 2*(C(4,2)+C(4,3)+C(4,4))/16 = 22/16, clamped
		{1, 1, 1},         // 2*1/2
		{0, 0, 1},         // pinned convention: no non-tie preferences => 1.0
	}
	for _, tc := range cases {
		if got := signTestTwoSidedP(tc.wins, tc.n); got != tc.want {
			t.Errorf("signTestTwoSidedP(%d, %d) = %v; want %v", tc.wins, tc.n, got, tc.want)
		}
	}
}

// fcReportFixture: labeled legacy/mixed pairs for model "c" on pair-alpha
// (FNV-1a-64("pair-alpha|c|fc") = 18218199419608068020, even => legacy is A)
// and pair-gamma (15644729119194098327, odd => mixed is A); pair-beta is left
// unlabeled (report-excluded) and pair-ctl is a negative-control pair.
func fcReportFixture() (arts []Artifact, labels []Label) {
	for _, pair := range []string{"pair-alpha", "pair-beta", "pair-gamma"} {
		l := mixedArtifact(pair, AssemblyLegacy, "c", nil)
		m := mixedArtifact(pair, AssemblyMixed, "c", nil)
		arts = append(arts, l, m)
		if pair != "pair-beta" {
			labels = append(labels, labelFor(l, 0), labelFor(m, 1))
		}
	}
	ctl := func(ae *AssemblyEval) { ae.Control = true }
	l := mixedArtifact("pair-ctl", AssemblyLegacy, "c", ctl)
	m := mixedArtifact("pair-ctl", AssemblyMixed, "c", ctl)
	arts = append(arts, l, m)
	labels = append(labels, labelFor(l, 1), labelFor(m, 1))
	return arts, labels
}

// fcRowFor builds a valid preference row for one fixture pair, with hashA/B
// ordered by the registered parity exactly as -fc-ingest would emit them.
func fcRowFor(arts []Artifact, pair, preference string) FCPreference {
	var legacy, mixed string
	for _, a := range arts {
		if a.Trace.AssemblyEval.PairID != pair {
			continue
		}
		if a.Trace.AssemblyEval.Mode == AssemblyLegacy {
			legacy = a.ArtifactHash
		} else {
			mixed = a.ArtifactHash
		}
	}
	hashA, hashB := legacy, mixed
	if !fcSideIsLegacyA(pair, "c") {
		hashA, hashB = mixed, legacy
	}
	return FCPreference{PairID: pair, CandidateModel: "c",
		ArtifactHashA: hashA, ArtifactHashB: hashB, Preference: preference, Labeler: "t"}
}

func TestAssemblyForcedChoiceSection(t *testing.T) {
	arts, labels := fcReportFixture()
	report := func(prefs []FCPreference) *AssemblyReport {
		t.Helper()
		rep, err := computeAssemblyReport(arts, labels, 1, 200, prefs)
		if err != nil {
			t.Fatalf("computeAssemblyReport: %v", err)
		}
		return rep
	}
	fcOf := func(rep *AssemblyReport) *AssemblyForcedChoice {
		t.Helper()
		if len(rep.LegacyMixedModels) != 1 {
			t.Fatalf("legacy-mixed models = %d; want 1", len(rep.LegacyMixedModels))
		}
		fc := rep.LegacyMixedModels[0].ForcedChoice
		if fc == nil {
			t.Fatalf("forced_choice section missing with -fc-preferences set")
		}
		return fc
	}

	t.Run("a and b resolve through the registered parity", func(t *testing.T) {
		// pair-alpha: legacy is A, so "a" is a LEGACY win; pair-gamma: mixed
		// is A, so "a" is a MIXED win.
		fc := fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "a"), fcRowFor(arts, "pair-gamma", "a")}))
		if fc.LegacyWins != 1 || fc.MixedWins != 1 || fc.Ties != 0 || fc.NNonTie != 2 || fc.SkippedExcluded != 0 {
			t.Fatalf("fc = %+v; want legacy 1 / mixed 1 / ties 0 / n_nontie 2", fc)
		}
		if fc.PTwoSided != 1 { // k = max(1,1) = 1 of n=2: 2*(C(2,1)+C(2,2))/4 = 1.5, clamped
			t.Fatalf("p = %v; want 1.0", fc.PTwoSided)
		}
		// "b" flips through the SAME parity: pair-alpha "b" is a MIXED win.
		fc = fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "b")}))
		if fc.MixedWins != 1 || fc.LegacyWins != 0 {
			t.Fatalf("fc = %+v; want pair-alpha b => mixed win", fc)
		}
	})

	t.Run("ties never enter the sign test", func(t *testing.T) {
		fc := fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "tie"), fcRowFor(arts, "pair-gamma", "a")}))
		if fc.Ties != 1 || fc.NNonTie != 1 || fc.MixedWins != 1 {
			t.Fatalf("fc = %+v; want 1 tie excluded from n_nontie", fc)
		}
	})

	t.Run("n_nontie zero pins p to 1.0", func(t *testing.T) {
		fc := fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "tie")}))
		if fc.NNonTie != 0 || fc.PTwoSided != 1 {
			t.Fatalf("fc = %+v; want n_nontie 0 with p pinned to 1.0", fc)
		}
	})

	t.Run("excluded and control pairs are skipped and counted", func(t *testing.T) {
		fc := fcOf(report([]FCPreference{
			fcRowFor(arts, "pair-beta", "a"), // unlabeled => report-excluded
			fcRowFor(arts, "pair-ctl", "a"),  // negative control
			fcRowFor(arts, "pair-alpha", "a"),
		}))
		if fc.SkippedExcluded != 2 || fc.NNonTie != 1 || fc.LegacyWins != 1 || fc.MixedWins != 0 {
			t.Fatalf("fc = %+v; want 2 skipped (unlabeled + control), only pair-alpha counted", fc)
		}
	})

	t.Run("flag unset leaves no forced_choice key anywhere", func(t *testing.T) {
		raw, err := json.Marshal(report(nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "forced_choice") {
			t.Fatalf("forced_choice key present without -fc-preferences:\n%s", raw)
		}
	})

	t.Run("empty sidecar with flag set still emits the section", func(t *testing.T) {
		fc := fcOf(report([]FCPreference{}))
		if fc.NNonTie != 0 || fc.PTwoSided != 1 || fc.SkippedExcluded != 0 {
			t.Fatalf("fc = %+v; want a zero-count section with p 1.0", fc)
		}
	})

	t.Run("unknown pair, model, or hash are loud errors", func(t *testing.T) {
		unknownPair := fcRowFor(arts, "pair-alpha", "a")
		unknownPair.PairID = "pair-nope" // hashes stay valid; pair lookup must fail
		unknownModel := fcRowFor(arts, "pair-alpha", "a")
		unknownModel.CandidateModel = "z"
		unknownHash := fcRowFor(arts, "pair-alpha", "a")
		unknownHash.ArtifactHashA = "sha256:nope"
		for name, row := range map[string]FCPreference{
			"unknown pair": unknownPair, "unknown model": unknownModel, "unknown hash": unknownHash,
		} {
			if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}); err == nil {
				t.Errorf("%s accepted; want loud error", name)
			}
		}
	})

	t.Run("duplicate row is a loud error", func(t *testing.T) {
		row := fcRowFor(arts, "pair-alpha", "a")
		if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row, row}); err == nil {
			t.Fatalf("duplicate preference row accepted; want loud error")
		}
	})

	t.Run("invalid preference value is a loud error", func(t *testing.T) {
		row := fcRowFor(arts, "pair-alpha", "a")
		row.Preference = "x"
		if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}); err == nil {
			t.Fatalf("preference %q accepted; want loud error", row.Preference)
		}
	})

	t.Run("primary decision is independent of forced-choice data", func(t *testing.T) {
		without := report(nil)
		with := report([]FCPreference{fcRowFor(arts, "pair-alpha", "a"), fcRowFor(arts, "pair-gamma", "a")})
		for i := range without.LegacyMixedModels {
			w, g := without.LegacyMixedModels[i].Decision, with.LegacyMixedModels[i].Decision
			if w != g {
				t.Fatalf("decision changed when forced-choice data was supplied: %q -> %q", w, g)
			}
		}
	})
}
