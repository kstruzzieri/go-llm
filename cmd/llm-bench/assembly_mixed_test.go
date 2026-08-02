package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// mixedArtifact builds one legacy/mixed/topline-arm artifact with valid 3c
// metadata; mut (optional) adjusts the eval before the artifact hash is
// computed, so every variant stays self-consistent. Defaults: a registered
// stratum and a unique per-pair scenario family (a singleton cluster), so a
// default pair passes every registered report gate.
func mixedArtifact(pair string, mode AssemblyMode, model string, mut func(*AssemblyEval)) Artifact {
	ae := &AssemblyEval{
		PairID:         pair,
		Mode:           mode,
		StateDigest:    "sha256:state-" + pair,
		Budget:         4096,
		Stratum:        "conversation_only",
		ScenarioFamily: "fam-" + pair,
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
// order). Strata round-robin over the registered set by absolute index
// (start+i), so 60 pairs from start 0 land exactly 12 in each registered
// stratum; families stay the mixedArtifact per-pair singleton default. mut,
// when non-nil, runs on BOTH arms' evals (distinguish by ae.Mode) after the
// defaults and before hashing.
func appendMixedPairs(arts []Artifact, labels []Label, model string, start, n int, legacyQ, mixedQ float64, mut func(i int, ae *AssemblyEval)) ([]Artifact, []Label) {
	for i := 0; i < n; i++ {
		pair := fmt.Sprintf("mx-%03d", start+i)
		idx := i
		stratum := assemblyMixedRegisteredStrata[(start+i)%len(assemblyMixedRegisteredStrata)]
		wrap := func(ae *AssemblyEval) {
			ae.Stratum = stratum
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
	rep, err := computeAssemblyReport(arts, labels, 1, 10000, nil, assemblyReportExtras{})
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

	// TwinGroup mismatch across arms: twin drives the registered
	// permutation grouping, so it pairs like stratum and family.
	wl := mixedArtifact("pair-twin", AssemblyLegacy, "m", func(ae *AssemblyEval) {
		ae.TwinGroup = "tg-a"
	})
	wm := mixedArtifact("pair-twin", AssemblyMixed, "m", func(ae *AssemblyEval) {
		ae.TwinGroup = "tg-b"
	})
	arts = append(arts, wl, wm)
	labels = append(labels, labelFor(wl, 0.5), labelFor(wm, 0.5))

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
		"pair-twin":    "twin-group-mismatch",
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
		"minimum 6 scenario-family clusters per stratum and maximum cluster size 3; " +
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
	build := func(clustered bool) ([]Artifact, []Label) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, 58, 0.5, 1.0, nil)
		// Two strongly-negative twin pairs in conversation_only (the
		// mixedArtifact default stratum). clustered shares one family so
		// they resample as a unit; otherwise each keeps its unique default
		// family (the naive per-pair shape).
		for _, pair := range []string{"tw-000", "tw-001"} {
			mut := func(ae *AssemblyEval) {
				if clustered {
					ae.ScenarioFamily = "twin-fam"
				}
			}
			l := mixedArtifact(pair, AssemblyLegacy, "m", mut)
			x := mixedArtifact(pair, AssemblyMixed, "m", mut)
			arts = append(arts, l, x)
			labels = append(labels, labelFor(l, 1.0), labelFor(x, 0.0))
		}
		return arts, labels
	}

	artsA, labelsA := build(true)
	first := mustMixedReport(t, artsA, labelsA).LegacyMixedModels[0]
	second := mustMixedReport(t, artsA, labelsA).LegacyMixedModels[0]
	if first.DeltaCILow != second.DeltaCILow || first.DeltaCIHigh != second.DeltaCIHigh {
		t.Fatalf("bootstrap not deterministic: [%v, %v] vs [%v, %v]",
			first.DeltaCILow, first.DeltaCIHigh, second.DeltaCILow, second.DeltaCIHigh)
	}

	// Splitting the twin family (every pair its own cluster) must change the
	// CI, and DIRECTIONALLY: the two -1.0 twins moving together fatten the
	// lower tail, so the clustered lower bound sits strictly below the naive
	// per-pair resampling's lower bound.
	artsB, labelsB := build(false)
	naive := mustMixedReport(t, artsB, labelsB).LegacyMixedModels[0]
	if !(first.DeltaCILow < naive.DeltaCILow) {
		t.Fatalf("clustered CI low %v not below naive CI low %v; twin clusters are not moving together",
			first.DeltaCILow, naive.DeltaCILow)
	}

	// Per-stratum entries: descriptives only. 58 base pairs round-robin the
	// five registered strata (12/12/12/11/11); the twins land in
	// conversation_only, so it holds 12 base wins plus the 2 twin losses.
	if len(first.Strata) != len(assemblyMixedRegisteredStrata) {
		t.Fatalf("strata = %v, want all %d registered strata", first.Strata, len(assemblyMixedRegisteredStrata))
	}
	var s AssemblyStratumReport
	for _, entry := range first.Strata {
		if entry.Stratum == "conversation_only" {
			s = entry
		}
	}
	if s.Stratum != "conversation_only" || s.Pairs != 14 || s.Wins != 12 || s.Losses != 2 || s.Ties != 0 {
		t.Fatalf("stratum entry = %+v, want conversation_only/14/12/2/0", s)
	}
	if s.MeanDelta != 4.0/14 { // (12*0.5 - 2*1.0) / 14
		t.Fatalf("stratum mean delta = %v, want %v", s.MeanDelta, 4.0/14)
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
	exclusionsOf := func(lm AssemblyMixedModelReport) map[string]string {
		got := map[string]string{}
		for _, e := range lm.Exclusions {
			got[e.PairID] = e.Reason
		}
		return got
	}

	t.Run("insufficient corpus below pooled floor", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs-1, 0.5, 1.0, nil)
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs-1 || lm.Decision != "insufficient-corpus" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-corpus",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs-1)
		}
	})

	t.Run("stratum floor", func(t *testing.T) {
		// 60 pooled, but one registered stratum holds only 11: move pair
		// mx-000 out of conversation_only into memory_only (11 vs 13).
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, func(i int, ae *AssemblyEval) {
			if i == 0 {
				ae.Stratum = "memory_only"
			}
		})
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs || lm.Decision != "insufficient-stratum-balance" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-stratum-balance",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs)
		}
	})

	t.Run("absent registered stratum trips the stratum floor", func(t *testing.T) {
		// 60 pooled and every PRESENT stratum holds >= 12, but
		// chain_retention is entirely absent (its pairs folded into
		// conversation_only). The registered set demands all five.
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, func(_ int, ae *AssemblyEval) {
			if ae.Stratum == "chain_retention" {
				ae.Stratum = "conversation_only"
			}
		})
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs || lm.Decision != "insufficient-stratum-balance" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-stratum-balance for an absent stratum",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs)
		}
	})

	t.Run("unregistered stratum is excluded, never pooled", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		il := mixedArtifact("pair-invented", AssemblyLegacy, "m", func(ae *AssemblyEval) { ae.Stratum = "shallow" })
		im := mixedArtifact("pair-invented", AssemblyMixed, "m", func(ae *AssemblyEval) { ae.Stratum = "shallow" })
		arts = append(arts, il, im)
		labels = append(labels, labelFor(il, 0.5), labelFor(im, 1.0))

		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d (invented-stratum pair must not pool)", lm.Pairs, assemblyMixedMinimumPairs)
		}
		if got := exclusionsOf(lm); got["pair-invented"] != "unregistered-stratum" {
			t.Fatalf("exclusions = %v, want pair-invented => unregistered-stratum", lm.Exclusions)
		}
		if lm.Decision != "quality-improved" {
			t.Fatalf("decision = %q, want quality-improved after the exclusion", lm.Decision)
		}
	})

	t.Run("missing scenario family is excluded", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		nl := mixedArtifact("pair-nofam", AssemblyLegacy, "m", func(ae *AssemblyEval) { ae.ScenarioFamily = "" })
		nm := mixedArtifact("pair-nofam", AssemblyMixed, "m", func(ae *AssemblyEval) { ae.ScenarioFamily = "" })
		arts = append(arts, nl, nm)
		labels = append(labels, labelFor(nl, 0.5), labelFor(nm, 1.0))

		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d (family-less pair must not pool)", lm.Pairs, assemblyMixedMinimumPairs)
		}
		if got := exclusionsOf(lm); got["pair-nofam"] != "missing-scenario-family" {
			t.Fatalf("exclusions = %v, want pair-nofam => missing-scenario-family", lm.Exclusions)
		}
		if lm.Decision != "quality-improved" {
			t.Fatalf("decision = %q, want quality-improved after the exclusion", lm.Decision)
		}
	})

	t.Run("one unlabeled built pair gates the whole verdict", func(t *testing.T) {
		// 61 built non-control pairs, 60 labeled: completeness is EVERY
		// eligible pair labeled, so one unlabeled pair means
		// incomplete-labeling regardless of the floors being met.
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		ul := mixedArtifact("pair-nolabel", AssemblyLegacy, "m", nil)
		um := mixedArtifact("pair-nolabel", AssemblyMixed, "m", nil)
		arts = append(arts, ul, um)
		labels = append(labels, labelFor(ul, 0.5)) // mixed arm never labeled

		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d", lm.Pairs, assemblyMixedMinimumPairs)
		}
		if got := exclusionsOf(lm); got["pair-nolabel"] != "unlabeled" {
			t.Fatalf("exclusions = %v, want pair-nolabel => unlabeled", lm.Exclusions)
		}
		if lm.Decision != "incomplete-labeling" {
			t.Fatalf("decision = %q, want incomplete-labeling (an eligible pair was never labeled)", lm.Decision)
		}
	})

	t.Run("structural exclusions never trip incomplete-labeling", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		// Missing-arm pair, its lone arm UNLABELED: a capture gap, not a
		// labeling gap.
		arts = append(arts, mixedArtifact("pair-lone", AssemblyLegacy, "m", nil))
		// Digest mismatch, unlabeled on one arm: invariant failure wins.
		dl := mixedArtifact("pair-digest", AssemblyLegacy, "m", nil)
		dm := mixedArtifact("pair-digest", AssemblyMixed, "m", func(ae *AssemblyEval) {
			ae.StateDigest = "sha256:other"
		})
		arts = append(arts, dl, dm)
		labels = append(labels, labelFor(dl, 0.5))
		// Unlabeled CONTROL pair: controls are outside the verdict, so an
		// unlabeled control never gates it.
		cl := mixedArtifact("pair-ctl", AssemblyLegacy, "m", func(ae *AssemblyEval) { ae.Control = true })
		cm := mixedArtifact("pair-ctl", AssemblyMixed, "m", func(ae *AssemblyEval) { ae.Control = true })
		arts = append(arts, cl, cm)
		labels = append(labels, labelFor(cl, 1.0))

		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d", lm.Pairs, assemblyMixedMinimumPairs)
		}
		got := exclusionsOf(lm)
		if got["pair-lone"] != "missing-mixed-arm" || got["pair-digest"] != "state-digest-mismatch" || got["pair-ctl"] != "unlabeled" {
			t.Fatalf("exclusions = %v, want missing-mixed-arm + state-digest-mismatch + control unlabeled", lm.Exclusions)
		}
		if lm.Decision != "quality-improved" {
			t.Fatalf("decision = %q, want quality-improved (structural gaps consume slack, they do not gate labeling)", lm.Decision)
		}
	})

	t.Run("cluster diversity floor", func(t *testing.T) {
		// Fold conversation_only's 12 pairs into 5 families (3/3/3/2/1):
		// floors all pass, but 5 distinct clusters < the registered 6.
		fam := []int{0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 4}
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, func(i int, ae *AssemblyEval) {
			if ae.Stratum == "conversation_only" {
				ae.ScenarioFamily = fmt.Sprintf("cfam-%d", fam[i/5])
			}
		})
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs || lm.Decision != "insufficient-cluster-diversity" {
			t.Fatalf("pairs=%d decision=%q, want %d/insufficient-cluster-diversity",
				lm.Pairs, lm.Decision, assemblyMixedMinimumPairs)
		}
	})

	t.Run("oversized cluster is excluded loudly", func(t *testing.T) {
		arts, labels := appendMixedPairs(nil, nil, "m", 0, assemblyMixedMinimumPairs, 0.5, 1.0, nil)
		// Four extra pairs sharing one family in one stratum: one past the
		// registered cap of 3, so ALL four pairs leave, listed.
		arts, labels = appendMixedPairs(arts, labels, "m", assemblyMixedMinimumPairs, 4, 0.5, 1.0, func(_ int, ae *AssemblyEval) {
			ae.Stratum = "conversation_only"
			ae.ScenarioFamily = "big-fam"
		})
		lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
		if lm.Pairs != assemblyMixedMinimumPairs {
			t.Fatalf("pairs = %d, want %d (oversized cluster must not pool)", lm.Pairs, assemblyMixedMinimumPairs)
		}
		got := exclusionsOf(lm)
		for i := 0; i < 4; i++ {
			pair := fmt.Sprintf("mx-%03d", assemblyMixedMinimumPairs+i)
			if got[pair] != "oversized-cluster" {
				t.Errorf("exclusion for %q = %q, want oversized-cluster", pair, got[pair])
			}
		}
		if len(lm.Exclusions) != 4 {
			t.Fatalf("exclusions = %v, want exactly the four big-fam pairs", lm.Exclusions)
		}
		if lm.Decision != "quality-improved" {
			t.Fatalf("decision = %q, want quality-improved after the exclusion", lm.Decision)
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
			{"top-000", "conversation_only", 0.75, true},
			{"top-001", "memory_only", 0.25, true},
			{"top-002", "conversation_only", 0, false},
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
	conv, mem := tl.ByStratum[0], tl.ByStratum[1]
	if conv.Stratum != "conversation_only" || conv.Count != 2 || conv.Labeled != 1 || conv.MeanQuality != 0.75 {
		t.Fatalf("conversation_only topline stratum = %+v", conv)
	}
	if mem.Stratum != "memory_only" || mem.Count != 1 || mem.Labeled != 1 || mem.MeanQuality != 0.25 {
		t.Fatalf("memory_only topline stratum = %+v", mem)
	}
}

// TestAssemblyReportScenarioFamilyCrossesStrata: a non-empty family under
// more than one stratum cannot cluster-within-stratum; every pair it touches
// is excluded with a structural reason instead of silently splitting.
func TestAssemblyReportScenarioFamilyCrossesStrata(t *testing.T) {
	arts, labels := appendMixedPairs(nil, nil, "m", 0, 58, 0.5, 1.0, nil)
	for i, stratum := range []string{"conversation_only", "memory_only"} {
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
	// The strata section is rebuilt from the filtered set: the 58 base
	// pairs remain (12/12/12/11/11 round-robin) and the excluded xfam
	// deltas are gone from the pooled evidence.
	total := 0
	for _, s := range lm.Strata {
		total += s.Pairs
	}
	if len(lm.Strata) != len(assemblyMixedRegisteredStrata) || total != 58 {
		t.Fatalf("strata = %+v, want the five registered strata holding 58 pairs total", lm.Strata)
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
// (FNV-1a-64("pair-alpha|c|fc") = 18218199419608068020, even => legacy is A),
// pair-gamma (15644729119194098327, odd => mixed is A), and pair-delta;
// pair-beta is left unlabeled (report-excluded) and pair-ctl is a
// negative-control pair. Every pair keeps its unique default scenario family,
// so each labeled pair is its own independence group.
func fcReportFixture() (arts []Artifact, labels []Label) {
	for _, pair := range []string{"pair-alpha", "pair-beta", "pair-delta", "pair-gamma"} {
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

// fcMixedWinPref returns the preference value ("a" or "b") that scores a
// MIXED win for pair under the registered parity.
func fcMixedWinPref(pair, model string) string {
	if fcSideIsLegacyA(pair, model) {
		return "b"
	}
	return "a"
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
		rep, err := computeAssemblyReport(arts, labels, 1, 200, prefs, assemblyReportExtras{})
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
			if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}, assemblyReportExtras{}); err == nil {
				t.Errorf("%s accepted; want loud error", name)
			}
		}
	})

	t.Run("duplicate row is a loud error", func(t *testing.T) {
		row := fcRowFor(arts, "pair-alpha", "a")
		if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row, row}, assemblyReportExtras{}); err == nil {
			t.Fatalf("duplicate preference row accepted; want loud error")
		}
	})

	t.Run("invalid preference value is a loud error", func(t *testing.T) {
		row := fcRowFor(arts, "pair-alpha", "a")
		row.Preference = "x"
		if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}, assemblyReportExtras{}); err == nil {
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

	t.Run("a/b hashes must bind to the pair's exact arms", func(t *testing.T) {
		// Swapped hashes: the right arm SET but the wrong a/b order for the
		// registered parity.
		swapped := fcRowFor(arts, "pair-alpha", "a")
		swapped.ArtifactHashA, swapped.ArtifactHashB = swapped.ArtifactHashB, swapped.ArtifactHashA
		// Foreign hashes: valid artifacts, but another pair's arms.
		foreign := fcRowFor(arts, "pair-alpha", "a")
		other := fcRowFor(arts, "pair-gamma", "a")
		foreign.ArtifactHashA, foreign.ArtifactHashB = other.ArtifactHashA, other.ArtifactHashB
		for name, row := range map[string]FCPreference{"swapped hashes": swapped, "foreign hashes": foreign} {
			_, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{row}, assemblyReportExtras{})
			if err == nil || !strings.Contains(err.Error(), "pair-alpha") {
				t.Errorf("%s: want loud error naming pair-alpha, got %v", name, err)
			}
		}
		// Binding applies to EXCLUDED pairs too whenever both arms exist:
		// pair-beta is report-excluded (unlabeled) but fully built, so a
		// swapped-hash row on it is corruption, not a skip.
		exSwapped := fcRowFor(arts, "pair-beta", "a")
		exSwapped.ArtifactHashA, exSwapped.ArtifactHashB = exSwapped.ArtifactHashB, exSwapped.ArtifactHashA
		if _, err := computeAssemblyReport(arts, labels, 1, 200, []FCPreference{exSwapped}, assemblyReportExtras{}); err == nil || !strings.Contains(err.Error(), "pair-beta") {
			t.Errorf("swapped hashes on excluded pair: want loud error naming pair-beta, got %v", err)
		}
	})

	t.Run("cluster permutation p pins to 1 for a single group", func(t *testing.T) {
		// One group with aggregate +1: every flip yields |sum| = 1 >= 1, so
		// count = B and p = (B+1)/(B+1) = 1 exactly.
		fc := fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "a")}))
		if fc.PClusterPermutation != 1 {
			t.Fatalf("p_cluster_permutation = %v, want exactly 1 (single group)", fc.PClusterPermutation)
		}
	})

	t.Run("cluster permutation p pins to 1 for balanced signs", func(t *testing.T) {
		// Aggregates +1 and -1: observed |sum| = 0, every flip satisfies
		// |sum| >= 0, so count = B and p = 1 exactly.
		fc := fcOf(report([]FCPreference{fcRowFor(arts, "pair-alpha", "a"), fcRowFor(arts, "pair-gamma", "a")}))
		if fc.PClusterPermutation != 1 {
			t.Fatalf("p_cluster_permutation = %v, want exactly 1 (balanced signs)", fc.PClusterPermutation)
		}
	})

	t.Run("cluster permutation p for three singleton wins", func(t *testing.T) {
		rows := []FCPreference{
			fcRowFor(arts, "pair-alpha", fcMixedWinPref("pair-alpha", "c")),
			fcRowFor(arts, "pair-delta", fcMixedWinPref("pair-delta", "c")),
			fcRowFor(arts, "pair-gamma", fcMixedWinPref("pair-gamma", "c")),
		}
		rep := report(rows)
		fc := fcOf(rep)
		if fc.MixedWins != 3 || fc.LegacyWins != 0 || fc.NNonTie != 3 {
			t.Fatalf("fc = %+v; want three mixed wins", fc)
		}
		// Three singleton groups, aggregates (+1,+1,+1), observed |sum| = 3.
		// A permutation reaches |sum| >= 3 only when all three flips agree:
		// 2 of the 8 equally likely patterns, so the exact expectation is
		// 2/8 = 0.25 and p ~= (0.25*B+1)/(B+1). The permutation test shares
		// the report's seed/B (this harness passes 1/200; production passes
		// pairedBootstrapSeed/pairedBootstrapN), so the pinned literal is the
		// deterministic seed-1 draw at B = 200: count = 48, p = 49/201.
		if want := 49.0 / 201; fc.PClusterPermutation != want {
			t.Fatalf("p_cluster_permutation = %v, want %v (seed-1 draw, expectation 0.25)", fc.PClusterPermutation, want)
		}
		// Deterministic across runs.
		if again := fcOf(report(rows)); again.PClusterPermutation != fc.PClusterPermutation {
			t.Fatalf("p_cluster_permutation not deterministic: %v vs %v", fc.PClusterPermutation, again.PClusterPermutation)
		}
		// Decision stays the fixture's own (pair-beta is unlabeled): the
		// permutation p must never feed the primary rule.
		if d := rep.LegacyMixedModels[0].Decision; d != "incomplete-labeling" {
			t.Fatalf("decision = %q, want incomplete-labeling regardless of the permutation p", d)
		}
	})

	t.Run("twin groups merge families in the permutation test", func(t *testing.T) {
		// Three labeled pairs, two sharing twin_group tg-1: two independence
		// groups with aggregates (+2, +1), observed |sum| = 3. |±2 ±1| >= 3
		// only when the two flips agree: exact expectation 2/4 = 0.5 — twice
		// the unmerged three-group fraction, so merging observably weakens
		// the evidence. Pinned literal: the deterministic seed-1 draw at the
		// harness's B = 200 is count = 91, p = 92/201.
		var twinArts []Artifact
		var twinLabels []Label
		for _, pair := range []string{"tw-a", "tw-b", "tw-c"} {
			mut := func(ae *AssemblyEval) {
				if pair != "tw-c" {
					ae.TwinGroup = "tg-1"
				}
			}
			l := mixedArtifact(pair, AssemblyLegacy, "c", mut)
			m := mixedArtifact(pair, AssemblyMixed, "c", mut)
			twinArts = append(twinArts, l, m)
			twinLabels = append(twinLabels, labelFor(l, 0), labelFor(m, 1))
		}
		var rows []FCPreference
		for _, pair := range []string{"tw-a", "tw-b", "tw-c"} {
			rows = append(rows, fcRowFor(twinArts, pair, fcMixedWinPref(pair, "c")))
		}
		rep, err := computeAssemblyReport(twinArts, twinLabels, 1, 200, rows, assemblyReportExtras{})
		if err != nil {
			t.Fatalf("computeAssemblyReport: %v", err)
		}
		fc := rep.LegacyMixedModels[0].ForcedChoice
		if fc == nil || fc.MixedWins != 3 {
			t.Fatalf("fc = %+v; want three mixed wins", fc)
		}
		if want := 92.0 / 201; fc.PClusterPermutation != want {
			t.Fatalf("p_cluster_permutation = %v, want %v (twin-merged, expectation 0.5)", fc.PClusterPermutation, want)
		}
	})
}

// TestAssemblyStratifiedClusterCIFixedWeights pins the registered
// fixed-stratum-weight scheme with a structural property the old pooled
// scheme violates. Stratum A: 12 pairs of delta +1 in 6 clusters of size 2.
// Stratum B: 12 pairs of delta -1 in UNEVEN clusters (3,3,2,2,1,1). Under
// fixed weights every replicate is exactly
// (12/24)*(+1) + (12/24)*(-1) = 0.5 - 0.5 = 0: each stratum's ratio-of-sums
// mean over identical deltas is exactly +/-1 whatever clusters are drawn,
// and the weights never move — so BOTH CI endpoints equal 0 exactly. The old
// pooled replicate (sum of resampled deltas / resampled count) depends on how
// many stratum-B pairs a resample happens to draw (anywhere from 6 to 18), so
// its replicates scatter around +/- a nonzero value and its endpoints
// separate. Equal-to-zero endpoints are therefore a scheme-distinguishing
// pin, not a tautology.
func TestAssemblyStratifiedClusterCIFixedWeights(t *testing.T) {
	cluster := func(size int, d float64) []float64 {
		c := make([]float64, size)
		for i := range c {
			c[i] = d
		}
		return c
	}
	strataA := [][]float64{cluster(2, 1), cluster(2, 1), cluster(2, 1), cluster(2, 1), cluster(2, 1), cluster(2, 1)}
	strataB := [][]float64{cluster(3, -1), cluster(3, -1), cluster(2, -1), cluster(2, -1), cluster(1, -1), cluster(1, -1)}
	lo, hi := assemblyStratifiedClusterCI([][][]float64{strataA, strataB}, 1, 5000)
	if lo != 0 || hi != 0 {
		t.Fatalf("fixed-weight CI = [%v, %v], want exactly [0, 0]", lo, hi)
	}
	lo2, hi2 := assemblyStratifiedClusterCI([][][]float64{strataA, strataB}, 1, 5000)
	if lo2 != lo || hi2 != hi {
		t.Fatalf("CI not deterministic: [%v, %v] vs [%v, %v]", lo, hi, lo2, hi2)
	}
}

// TestAssemblyReportClusterDiagnostics pins the descriptive
// cluster-independence section: per-stratum cluster counts, max cluster
// size, twin-merged independence-group count, and the leave-one-group-out
// band — including the exact upper end when removing the only negative
// group leaves a uniform +0.5 corpus (every replicate exactly 0.5).
func TestAssemblyReportClusterDiagnostics(t *testing.T) {
	// 58 pairs of +0.5 (round-robin strata, singleton families), then 2
	// pairs of -1.0 sharing twin_group tg-x across two strata: 60 pairs,
	// 12 per stratum, 60 singleton clusters, 59 independence groups.
	arts, labels := appendMixedPairs(nil, nil, "m", 0, 58, 0.5, 1.0, nil)
	arts, labels = appendMixedPairs(arts, labels, "m", 58, 2, 1.0, 0.0, func(_ int, ae *AssemblyEval) {
		ae.TwinGroup = "tg-x"
	})
	lm := mustMixedReport(t, arts, labels).LegacyMixedModels[0]
	if lm.Pairs != 60 || lm.Decision != "quality-improved" {
		t.Fatalf("pairs=%d decision=%q, want 60/quality-improved", lm.Pairs, lm.Decision)
	}
	diag := lm.ClusterDiagnostics
	if diag == nil {
		t.Fatal("cluster_diagnostics missing")
	}
	if len(diag.ClustersPerStratum) != len(assemblyMixedRegisteredStrata) {
		t.Fatalf("clusters_per_stratum = %v, want all five registered strata", diag.ClustersPerStratum)
	}
	for _, s := range assemblyMixedRegisteredStrata {
		if diag.ClustersPerStratum[s] != 12 {
			t.Errorf("clusters_per_stratum[%s] = %d, want 12", s, diag.ClustersPerStratum[s])
		}
	}
	if diag.MaxClusterSize != 1 {
		t.Fatalf("max_cluster_size = %d, want 1 (singleton families)", diag.MaxClusterSize)
	}
	if diag.IndependenceGroups != 59 {
		t.Fatalf("independence_groups = %d, want 59 (tg-x merges two singleton clusters)", diag.IndependenceGroups)
	}
	// Removing the tg-x group leaves 58 uniform +0.5 deltas: every replicate
	// is exactly 0.5, so the band's upper end is exactly 0.5. Every other
	// removal keeps the two -1.0 pairs, so its lower bound sits strictly
	// below.
	if diag.LeaveOneOut.MaxLow != 0.5 {
		t.Fatalf("leave_one_out.max_low = %v, want exactly 0.5", diag.LeaveOneOut.MaxLow)
	}
	if !(diag.LeaveOneOut.MinLow < diag.LeaveOneOut.MaxLow) {
		t.Fatalf("leave_one_out band [%v, %v] not ordered min < max", diag.LeaveOneOut.MinLow, diag.LeaveOneOut.MaxLow)
	}

	// No complete pairs => no diagnostics section at all.
	ul := mixedArtifact("pair-empty", AssemblyLegacy, "m", nil)
	um := mixedArtifact("pair-empty", AssemblyMixed, "m", nil)
	empty := mustMixedReport(t, []Artifact{ul, um}, []Label{labelFor(ul, 0.5)}).LegacyMixedModels[0]
	if empty.Pairs != 0 || empty.ClusterDiagnostics != nil {
		t.Fatalf("pairs=%d diagnostics=%+v, want 0 pairs and no diagnostics", empty.Pairs, empty.ClusterDiagnostics)
	}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "cluster_diagnostics") {
		t.Fatalf("cluster_diagnostics key present with zero complete pairs:\n%s", raw)
	}
}
