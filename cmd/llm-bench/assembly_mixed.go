package main

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"sort"
)

// Slice 3c (#331) legacy-mixed assembly kind: pairing invariants, the
// pre-registered decision rule v2, and the stratified cluster bootstrap.
// Also owns the topline descriptive section (computeAssemblyTopline) and the
// medianOf helper. The flat-progressive kind's rule and header live in
// assembly.go, untouched.

// Assembly pair kinds. Each mode maps to a kind individually, so the pair
// key (kind, PairID, model) is computable per artifact and the SAME PairID
// may appear under both kinds in one artifacts file without collision.
const (
	assemblyKindFlatProgressive = "flat-progressive"
	assemblyKindLegacyMixed     = "legacy-mixed"
)

// assemblyModeKind maps a paired assembly mode to its pair kind. Topline is
// unpaired and has no kind.
func assemblyModeKind(m AssemblyMode) string {
	switch m {
	case AssemblyFlat, AssemblyProgressive:
		return assemblyKindFlatProgressive
	case AssemblyLegacy, AssemblyMixed:
		return assemblyKindLegacyMixed
	default:
		return ""
	}
}

// assemblyPairKey is the report-time pairing key. kind keeps the two
// experiments (3a flat-progressive, 3c legacy-mixed) from colliding when a
// PairID is reused across them.
type assemblyPairKey struct{ kind, pair, model string }

// assemblyArmSet holds one pair's two arms: base is the reference rendering
// (flat or legacy), treat the treatment rendering (progressive or mixed).
type assemblyArmSet struct{ base, treat *Artifact }

// Decision-rule v2 constants, pre-registered (#331 slice 3c). Thresholds
// 0 / -0.10, floors 60 pooled / 12 per stratum; f=0.6 is the registered
// pressure fraction the builder targets (descriptive here, never consulted
// by the rule).
const (
	assemblyMixedNonInferiorityMargin = -0.10
	assemblyMixedMinimumPairs         = 60
	assemblyMixedMinimumStratumPairs  = 12
	// Aliases the governing constant (assembly_mixed_fixture.go) so the
	// rendered rule header cannot drift from the budget the builder applies.
	assemblyMixedPressureFraction = mixedBudgetFraction
)

// REGISTERED cluster-independence constants (#331 slice 3c): every registered
// stratum must hold at least assemblyMixedMinClustersPerStratum DISTINCT
// scenario-family clusters among the complete pairs (fewer trips the
// insufficient-cluster-diversity gate, like insufficient-corpus), and no
// cluster may hold more than assemblyMixedMaxClusterSize complete pairs
// (every pair of an oversized cluster is excluded loudly with reason
// "oversized-cluster", never silently down-weighted).
const (
	assemblyMixedMinClustersPerStratum = 6
	assemblyMixedMaxClusterSize        = 3
)

// assemblyMixedRegisteredStrata is the REGISTERED closed stratum set of the
// legacy-mixed experiment, in registration order. Single source of truth:
// the report gate (unregistered-stratum exclusion + the absent-stratum arm of
// insufficient-stratum-balance) and the fixture validator's vocabulary
// (mixedStrata, assembly_mixed_fixture.go) both read it, so the two cannot
// drift.
var assemblyMixedRegisteredStrata = []string{
	"conversation_only", "memory_only", "cross_domain_join",
	"stale_vs_fresh", "chain_retention",
}

// assemblyMixedStratumSet is the membership view of
// assemblyMixedRegisteredStrata.
var assemblyMixedStratumSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(assemblyMixedRegisteredStrata))
	for _, s := range assemblyMixedRegisteredStrata {
		m[s] = struct{}{}
	}
	return m
}()

// assemblyMixedDecision applies rule v2 to the pooled stratified-bootstrap
// CI on the paired delta (mixed - legacy). Strict inequalities throughout:
// a lower bound landing exactly on the margin is NOT noninferior, and a
// lower bound of exactly zero is NOT an improvement. Token and pressure
// numbers are deliberately absent from the signature — they are descriptive
// only and never consulted.
func assemblyMixedDecision(ciLo, ciHi float64) string {
	switch {
	case ciLo > 0:
		return "quality-improved"
	case ciHi < assemblyMixedNonInferiorityMargin:
		return "materially-regressed"
	case ciLo > assemblyMixedNonInferiorityMargin:
		return "noninferior"
	default:
		return "inconclusive"
	}
}

// assemblyMixedDecisionRuleText renders the registered rule v2 header from
// the pre-registered constants (single source of truth).
func assemblyMixedDecisionRuleText() string {
	return fmt.Sprintf(
		"legacy-mixed rule v2 (registered): minimum %d complete labeled non-control pairs pooled and minimum %d per stratum present; "+
			"minimum %d scenario-family clusters per stratum and maximum cluster size %d; "+
			"quality-improved: stratified-bootstrap CI low > 0; noninferior: CI low strictly > %.2f; "+
			"materially-regressed: CI high < %.2f; else inconclusive; "+
			"token and pressure numbers are descriptive only, never consulted; registered pressure fraction f=%.1f",
		assemblyMixedMinimumPairs, assemblyMixedMinimumStratumPairs,
		assemblyMixedMinClustersPerStratum, assemblyMixedMaxClusterSize,
		assemblyMixedNonInferiorityMargin, assemblyMixedNonInferiorityMargin,
		assemblyMixedPressureFraction)
}

// AssemblyExclusion records one pair a kind excluded from pairing, with the
// invariant it failed. Exclusions never abort the report; duplicate arms
// remain report-wide errors, matching 3a.
type AssemblyExclusion struct {
	PairID string `json:"pair_id"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// AssemblyStratumReport is one stratum's descriptives over the complete
// non-control pairs. It deliberately carries NO CI fields: the
// pre-registration allows a pooled CI only.
type AssemblyStratumReport struct {
	Stratum   string  `json:"stratum"`
	Pairs     int     `json:"pairs"`
	MeanDelta float64 `json:"mean_delta"`
	Wins      int     `json:"wins"`
	Losses    int     `json:"losses"`
	Ties      int     `json:"ties"`
}

// AssemblyArmPressure is one arm's aggregate pressure descriptives over the
// complete non-control pairs. Per-arm only by design: no field anywhere in
// the report diffs pressure or shed-cause data ACROSS arms.
type AssemblyArmPressure struct {
	Mode                  string         `json:"mode"`
	MedianShedMessages    float64        `json:"median_shed_messages"`
	MedianShedBytes       float64        `json:"median_shed_bytes"`
	MedianOmittedSubjects float64        `json:"median_omitted_subjects"`
	PressureLevels        map[string]int `json:"pressure_levels"`
}

// AssemblyMixedModelReport is one candidate model's legacy-mixed evidence:
// the pooled verdict (rule v2), per-stratum descriptives, control-pair noise
// descriptives, per-arm pressure descriptives, and the exclusion worklist.
type AssemblyMixedModelReport struct {
	CandidateModel   string                  `json:"candidate_model"`
	Pairs            int                     `json:"pairs"` // complete labeled non-control
	ControlPairs     int                     `json:"control_pairs"`
	MeanDelta        float64                 `json:"mean_delta"`
	DeltaCILow       float64                 `json:"delta_ci_low"`
	DeltaCIHigh      float64                 `json:"delta_ci_high"`
	Decision         string                  `json:"decision"`
	Deltas           []float64               `json:"deltas"` // lexicographic pair-ID order
	Strata           []AssemblyStratumReport `json:"strata,omitempty"`
	ControlAbsDeltas []float64               `json:"control_abs_deltas,omitempty"` // |delta| per control pair, pair-ID order
	ArmPressure      []AssemblyArmPressure   `json:"arm_pressure,omitempty"`
	Exclusions       []AssemblyExclusion     `json:"exclusions,omitempty"`
	// ClusterDiagnostics is the descriptive cluster-independence summary and
	// leave-one-group-out sensitivity band. Descriptive ONLY: the decision
	// rule never consults it. Nil (absent from JSON) when no complete pairs
	// exist; 3a sections never carry it.
	ClusterDiagnostics *AssemblyClusterDiagnostics `json:"cluster_diagnostics,omitempty"`
	// ForcedChoice is the registered SECONDARY analysis over the -fc-ingest
	// preference sidecar (-fc-preferences). Present only when the flag is set;
	// nil (and absent from JSON) otherwise — 3a sections never carry it. It
	// never feeds Decision.
	ForcedChoice *AssemblyForcedChoice `json:"forced_choice,omitempty"`
}

// AssemblyForcedChoice is the forced-choice secondary analysis for one
// legacy-mixed model section: preference rows resolved to arms through the
// registered side assignment (fcSideIsLegacyA) with each row's a/b hashes
// bound to its pair's exact arms. SkippedExcluded counts preference rows
// whose pair did not enter the primary analysis (report-excluded for any
// reason, or a negative-control pair) — they never enter either test.
type AssemblyForcedChoice struct {
	MixedWins  int `json:"mixed_wins"`
	LegacyWins int `json:"legacy_wins"`
	Ties       int `json:"ties"`
	NNonTie    int `json:"n_nontie"`
	// PTwoSided is the two-sided binomial sign test over the non-tie
	// preferences (p = 0.5 null). DESCRIPTIVE only: it treats every non-tie
	// preference as independent, which the clustered corpus design does not
	// guarantee. The registered secondary p-value is PClusterPermutation.
	PTwoSided float64 `json:"p_two_sided"`
	// PClusterPermutation is the REGISTERED secondary p-value: the cluster
	// sign-flip permutation test (fcClusterPermutationP), whose flip unit is
	// the independence group, honoring the corpus's cluster structure. Never
	// consulted by the primary Decision.
	PClusterPermutation float64 `json:"p_cluster_permutation"`
	SkippedExcluded     int     `json:"skipped_excluded"`
	// ArmGuessAccuracy is the descriptive blinding audit (#331 W3): how many
	// rows carried a non-blank arm_guess and how many of those guessed the
	// side the sealed sidemap assigns to the MIXED arm. Present only when the
	// report ran with -fc-sidemap; NEVER consulted by any decision or test
	// (mutation-checked).
	ArmGuessAccuracy *AssemblyArmGuessAccuracy `json:"arm_guess_accuracy,omitempty"`
}

// AssemblyArmGuessAccuracy is the descriptive arm-guess tally.
type AssemblyArmGuessAccuracy struct {
	NGuessed int `json:"n_guessed"`
	NCorrect int `json:"n_correct"`
}

// signTestTwoSidedP is the two-sided binomial sign test against p = 0.5 over
// the n non-tie preferences:
// p = min(1, 2 * P(X >= max(wins, n-wins))), X ~ Bin(n, 0.5). Binomial
// coefficients are built iteratively (C(n,i+1) = C(n,i)*(n-i)/(i+1)) with
// stdlib float64 sums — NOT exact arithmetic: coefficients round once n
// exceeds 56 and every term carries float64 rounding, but the accumulated
// relative error stays orders of magnitude below any reporting threshold for
// n up to a few hundred, far past a mixed-corpus pair count. n == 0 is
// pinned to p = 1.0 (no non-tie preferences carry no evidence against the
// null) rather than omitting the field.
func signTestTwoSidedP(wins, n int) float64 {
	if n == 0 {
		return 1
	}
	k := wins
	if n-wins > k {
		k = n - wins
	}
	coef, tail := 1.0, 0.0
	for i := 0; i <= n; i++ {
		if i >= k {
			tail += coef
		}
		coef = coef * float64(n-i) / float64(i+1)
	}
	if p := 2 * tail / math.Exp2(float64(n)); p < 1 {
		return p
	}
	return 1
}

// fcGroupSign is one non-tie forced-choice sign with the pair metadata the
// cluster permutation test groups by.
type fcGroupSign struct {
	pairID, fam, twin string
	sign              float64 // +1 mixed win, -1 legacy win
}

// attachAssemblyForcedChoice attaches the forced-choice secondary analyses to
// every legacy-mixed model section. Each preference row's a/b sides resolve
// to arms through resolve (nil defaults to fcParityResolver, the pre-sidemap
// default; a sidemap-backed resolver errors loudly on a missing pair), and
// every row whose pair has BOTH arms present — excluded pairs included —
// must carry EXACTLY that pair's legacy/mixed arm hashes in the a/b order
// the resolver dictates; a mismatch is a loud error naming the pair. Only
// rows on missing-arm pairs fall back to presence-only hash validation
// (there is no second arm to bind against). Rows on pairs the primary
// analysis excluded (any Exclusions reason) or on negative-control pairs are
// skipped and counted in SkippedExcluded. Unknown model/pair/hash, a
// duplicate row, or an invalid preference value is a loud error. seed and
// bootstrapN drive the cluster permutation test and are the SAME values the
// caller's CI uses, so a sensitivity rerun with a varied seed moves both the
// CI and the permutation p together. armGuess (set iff the report ran with a
// sidemap) additionally tallies the DESCRIPTIVE arm-guess blinding audit
// over every row carrying a guess — resolved through the same resolver,
// consulted by no decision or test. Runs strictly after every Decision is
// final and never writes one.
func attachAssemblyForcedChoice(models []AssemblyMixedModelReport, prefs []FCPreference, pairs map[assemblyPairKey]*assemblyArmSet, artHashes map[string]struct{}, seed int64, bootstrapN int, resolve fcSideResolver, armGuess bool) error {
	if resolve == nil {
		resolve = fcParityResolver
	}
	byModel := make(map[string]*AssemblyMixedModelReport, len(models))
	excluded := make(map[string]map[string]struct{}, len(models))
	for i := range models {
		m := &models[i]
		m.ForcedChoice = &AssemblyForcedChoice{PTwoSided: 1, PClusterPermutation: 1}
		if armGuess {
			m.ForcedChoice.ArmGuessAccuracy = &AssemblyArmGuessAccuracy{}
		}
		byModel[m.CandidateModel] = m
		ex := make(map[string]struct{}, len(m.Exclusions))
		for _, e := range m.Exclusions {
			ex[e.PairID] = struct{}{}
		}
		excluded[m.CandidateModel] = ex
	}
	seen := map[assemblyPairKey]struct{}{}
	signs := map[string][]fcGroupSign{}
	for i, row := range prefs {
		m, ok := byModel[row.CandidateModel]
		if !ok {
			return fmt.Errorf("fc-preference row %d: unknown candidate model %q", i, row.CandidateModel)
		}
		for _, h := range []string{row.ArtifactHashA, row.ArtifactHashB} {
			if _, ok := artHashes[h]; !ok {
				return fmt.Errorf("fc-preference row %d (pair %q model %q): artifact hash %q not in -artifacts", i, row.PairID, row.CandidateModel, h)
			}
		}
		k := assemblyPairKey{assemblyKindLegacyMixed, row.PairID, row.CandidateModel}
		s, ok := pairs[k]
		if !ok {
			return fmt.Errorf("fc-preference row %d: unknown pair %q for model %q", i, row.PairID, row.CandidateModel)
		}
		if _, dup := seen[k]; dup {
			return fmt.Errorf("fc-preference row %d: duplicate preference for pair %q model %q", i, row.PairID, row.CandidateModel)
		}
		seen[k] = struct{}{}
		fc := m.ForcedChoice
		legacyIsA, err := resolve(row.PairID, row.CandidateModel)
		if err != nil {
			return fmt.Errorf("fc-preference row %d: %w", i, err)
		}
		if acc := fc.ArmGuessAccuracy; acc != nil && row.ArmGuess != "" {
			// Descriptive blinding audit only: a guess is "correct" when the
			// guessed SIDE is the mixed arm under the sealed assignment.
			acc.NGuessed++
			if (row.ArmGuess == "a") != legacyIsA {
				acc.NCorrect++
			}
		}
		// Bind the row's a/b hashes to the pair's EXACT arms under the
		// registered side assignment whenever BOTH arms exist — excluded
		// pairs included. Only a missing-arm pair has nothing to bind
		// against, leaving the presence check above as its whole validation.
		if s.base != nil && s.treat != nil {
			wantA, wantB := s.base.ArtifactHash, s.treat.ArtifactHash
			if !legacyIsA {
				wantA, wantB = wantB, wantA
			}
			if row.ArtifactHashA != wantA || row.ArtifactHashB != wantB {
				return fmt.Errorf("fc-preference row %d (pair %q model %q): artifact_hash_a/b %q/%q do not match the pair's legacy/mixed arms under the registered side assignment (want %q/%q)",
					i, row.PairID, row.CandidateModel, row.ArtifactHashA, row.ArtifactHashB, wantA, wantB)
			}
		}
		if _, ex := excluded[row.CandidateModel][row.PairID]; ex {
			fc.SkippedExcluded++
			continue
		}
		if s.base != nil && s.base.Trace.AssemblyEval.Control {
			fc.SkippedExcluded++ // negative controls never enter the verdict, so never the tests
			continue
		}
		switch row.Preference {
		case "tie":
			fc.Ties++
		case "a", "b":
			mixedWin := (row.Preference == "a") != legacyIsA
			sign := -1.0
			if mixedWin {
				fc.MixedWins++
				sign = 1
			} else {
				fc.LegacyWins++
			}
			ae := s.base.Trace.AssemblyEval
			signs[row.CandidateModel] = append(signs[row.CandidateModel], fcGroupSign{
				pairID: row.PairID, fam: ae.ScenarioFamily, twin: ae.TwinGroup, sign: sign,
			})
		default:
			return fmt.Errorf("fc-preference row %d (pair %q): invalid preference %q (want a, b, or tie)", i, row.PairID, row.Preference)
		}
	}
	for i := range models {
		fc := models[i].ForcedChoice
		fc.NNonTie = fc.MixedWins + fc.LegacyWins
		fc.PTwoSided = canonicalStat(signTestTwoSidedP(fc.MixedWins, fc.NNonTie))
		fc.PClusterPermutation = canonicalStat(fcClusterPermutationP(signs[models[i].CandidateModel], seed, bootstrapN))
	}
	return nil
}

// fcClusterPermutationP is the REGISTERED forced-choice secondary test: a
// cluster sign-flip permutation test over the non-tie preferences. Signs
// (+1 mixed win, -1 legacy win) are aggregated per independence group
// (group = scenario_family, with twin_group merging where set); each of the
// b seeded permutations flips every group's aggregate sign independently
// with p = 0.5; the two-sided p is the add-one-smoothed fraction
// (count+1)/(b+1) of permutations whose |statistic| >= the observed
// |statistic|. The statistic is the mean non-tie sign; the comparison uses
// the sign SUMS, which is equivalent (both sides divide by the same fixed
// n). Entries are sorted by pair ID before grouping so the group order — and
// therefore the seeded flip stream — never depends on sidecar row order.
// Zero non-tie preferences pin p to 1.0, matching signTestTwoSidedP.
func fcClusterPermutationP(entries []fcGroupSign, seed int64, b int) float64 {
	if len(entries) == 0 || b <= 0 {
		return 1
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].pairID < entries[j].pairID })
	fams := make([]string, len(entries))
	twins := make([]string, len(entries))
	for i, e := range entries {
		fams[i], twins[i] = e.fam, e.twin
	}
	groups := independenceGroups(fams, twins)
	agg := make([]float64, len(groups))
	var obs float64
	for gi, g := range groups {
		for _, i := range g {
			agg[gi] += entries[i].sign
		}
		obs += agg[gi]
	}
	obs = math.Abs(obs)
	rng := rand.New(rand.NewSource(seed))
	count := 0
	for i := 0; i < b; i++ {
		var sum float64
		for _, a := range agg {
			if rng.Intn(2) == 1 {
				sum -= a
			} else {
				sum += a
			}
		}
		if math.Abs(sum) >= obs {
			count++
		}
	}
	return float64(count+1) / float64(b+1)
}

// independenceGroups partitions items into independence groups for the
// cluster diagnostics and the forced-choice permutation test: items sharing
// a scenario family form one group, and every non-empty twin_group merges
// the families it touches into a single group (a twin group may span strata,
// so this is the one place cluster boundaries merge). fams and twins are
// parallel; a blank family isolates its item. Groups come back in
// first-item order, so deterministic input order yields deterministic
// groups.
func independenceGroups(fams, twins []string) [][]int {
	parent := map[string]string{}
	var find func(string) string
	find = func(x string) string {
		p, ok := parent[x]
		if !ok || p == x {
			parent[x] = x
			return x
		}
		root := find(p)
		parent[x] = root
		return root
	}
	keys := make([]string, len(fams))
	for i := range fams {
		key := "fam\x00" + fams[i]
		if fams[i] == "" {
			key = fmt.Sprintf("solo\x00%d", i)
		}
		keys[i] = key
		find(key)
		if twins[i] != "" {
			ra, rb := find(key), find("twin\x00"+twins[i])
			if ra != rb {
				parent[ra] = rb
			}
		}
	}
	idx := map[string]int{}
	var groups [][]int
	for i, key := range keys {
		root := find(key)
		g, ok := idx[root]
		if !ok {
			g = len(groups)
			idx[root] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}

// AssemblyToplineStratum is one stratum's slice of the topline ceiling.
type AssemblyToplineStratum struct {
	Stratum     string  `json:"stratum"`
	Count       int     `json:"count"`
	Labeled     int     `json:"labeled"`
	MeanQuality float64 `json:"mean_quality"`
}

// AssemblyToplineReport is the per-model descriptive ceiling from unpaired
// topline (full-State) artifacts. Never counts toward completeness.
type AssemblyToplineReport struct {
	CandidateModel string                   `json:"candidate_model"`
	Count          int                      `json:"count"`
	Labeled        int                      `json:"labeled"`
	MeanQuality    float64                  `json:"mean_quality"` // mean labeled AnswerQuality; 0 when Labeled == 0
	ByStratum      []AssemblyToplineStratum `json:"by_stratum,omitempty"`
}

// AssemblyClusterDiagnostics is the descriptive cluster-independence summary
// for one legacy-mixed model section. Descriptive ONLY — the decision rule
// never consults any field here (mutation-checked).
type AssemblyClusterDiagnostics struct {
	ClustersPerStratum map[string]int           `json:"clusters_per_stratum"`
	MaxClusterSize     int                      `json:"max_cluster_size"`
	IndependenceGroups int                      `json:"independence_groups"`
	LeaveOneOut        AssemblyLeaveOneGroupOut `json:"leave_one_out"`
}

// AssemblyLeaveOneGroupOut is the leave-one-group-out sensitivity band: the
// pooled CI lower bound recomputed (same seed, same B) with each independence
// group's pairs removed in turn; MinLow/MaxLow are the extremes over the
// removals.
type AssemblyLeaveOneGroupOut struct {
	MinLow float64 `json:"min_low"`
	MaxLow float64 `json:"max_low"`
}

// assemblyMixedPair is one complete labeled non-control legacy-mixed pair.
// twin drives independence-group merging (permutation grouping is
// registered, so twin is a pair invariant like stratum and family); arms are
// verified identical at pairing time, so reading it from the legacy arm is
// exact.
type assemblyMixedPair struct {
	pairID  string
	delta   float64
	stratum string
	family  string
	twin    string
	legacy  *AssemblyEval
	mixed   *AssemblyEval
}

// captureVerification is the report-side view of a capture run manifest
// (-capture-manifest, #331 W3): which artifact hashes the manifest lists
// with usage_present true. Nil means no manifest was supplied.
// expectedPairs (v2 manifests only; nil for v1) lists every legacy-mixed
// pair key the capture ledger expected, so a pair whose arms ALL failed at
// capture — invisible to artifact-driven pair discovery — is synthesized
// into the registered missing-arm exclusions instead of vanishing.
type captureVerification struct {
	usagePresent          map[string]bool // artifact_hash -> usage_present
	expectedPairs         []assemblyPairKey
	expectedArtifacts     map[string]captureExpectedArtifact // nil=v1; nonnil=v2, including all-failed
	v2Manifest            *captureManifest
	legacyV1ModelIdentity bool
}

type captureExpectedArtifact struct {
	traceID, model, pairID, arm string
}

// capturePairTemperaturesEqual reports whether both arms carry capture
// provenance with an explicit temperature equal to the REGISTERED
// assemblyCaptureTemperature — not merely equal to each other: provenance is
// the one field artifactHash does not seal, so a pair captured consistently
// at the wrong temperature must be excluded too. A missing provenance or
// temperature fails: the manifest-verified workflow cannot vouch for a pair
// whose decoding conditions it cannot compare.
func capturePairTemperaturesEqual(base, treat *Artifact) bool {
	if base.Capture == nil || treat.Capture == nil ||
		base.Capture.Temperature == nil || treat.Capture.Temperature == nil {
		return false
	}
	return *base.Capture.Temperature == assemblyCaptureTemperature &&
		*treat.Capture.Temperature == assemblyCaptureTemperature
}

// computeAssemblyMixedSection builds the legacy-mixed per-model reports from
// the already-keyed pairs. keys must be sorted (model, kind, pair) so deltas
// land in lexicographic pair-ID order per model. Exclusions accumulate in
// pair-ID-ordered phases, not one merged order: the per-pair loop appends
// missing-arm / capture-verification / invariant / unregistered-stratum /
// missing-scenario-family / unlabeled exclusions, then the per-model loop
// appends every scenario-family-crosses-strata exclusion, then every
// oversized-cluster exclusion. Invariant failures become Exclusions, never
// report-wide errors. verify, when non-nil (-capture-manifest), excludes any
// complete pair whose arms are absent from the manifest or lack reported
// usage ("unverified-capture") or whose arms disagree on capture temperature
// ("temperature-mismatch"). Decision gates run in registered order:
// incomplete-labeling (any complete-built non-control pair missing a label
// on either arm), the pooled floor, the per-stratum floor over the
// REGISTERED strata (an absent registered stratum trips it too), then the
// cluster-diversity floor.
func computeAssemblyMixedSection(keys []assemblyPairKey, pairs map[assemblyPairKey]*assemblyArmSet, quality map[string]float64, seed int64, bootstrapN int, verify *captureVerification) []AssemblyMixedModelReport {
	type acc struct {
		report    AssemblyMixedModelReport
		complete  []assemblyMixedPair
		unlabeled int // complete-built non-control pairs missing a label on either arm
	}
	models := map[string]*acc{}
	var order []string
	for _, k := range keys {
		if k.kind != assemblyKindLegacyMixed {
			continue
		}
		a, ok := models[k.model]
		if !ok {
			a = &acc{report: AssemblyMixedModelReport{CandidateModel: k.model}}
			models[k.model] = a
			order = append(order, k.model)
		}
		exclude := func(reason string) {
			a.report.Exclusions = append(a.report.Exclusions, AssemblyExclusion{
				PairID: k.pair, Kind: assemblyKindLegacyMixed, Reason: reason,
			})
		}
		s := pairs[k]
		if s.base == nil {
			exclude("missing-legacy-arm")
			if s.treat == nil {
				exclude("missing-mixed-arm")
			}
			continue
		}
		if s.treat == nil {
			exclude("missing-mixed-arm")
			continue
		}
		if verify != nil {
			// Manifest verification (#331 W3): both arms must be listed with
			// usage_present, and their capture temperatures must agree.
			if !verify.usagePresent[s.base.ArtifactHash] || !verify.usagePresent[s.treat.ArtifactHash] {
				exclude("unverified-capture")
				continue
			}
			if !capturePairTemperaturesEqual(s.base, s.treat) {
				exclude("temperature-mismatch")
				continue
			}
		}
		leg, mix := s.base.Trace.AssemblyEval, s.treat.Trace.AssemblyEval
		switch {
		case !reflect.DeepEqual(leg.CandidateIDs, mix.CandidateIDs):
			exclude("candidate-ids-mismatch")
			continue
		case leg.StateDigest != mix.StateDigest:
			exclude("state-digest-mismatch")
			continue
		case leg.Budget != mix.Budget:
			exclude("budget-mismatch")
			continue
		case leg.Stratum != mix.Stratum:
			exclude("stratum-mismatch")
			continue
		case leg.ScenarioFamily != mix.ScenarioFamily:
			exclude("scenario-family-mismatch")
			continue
		case leg.TwinGroup != mix.TwinGroup:
			exclude("twin-group-mismatch")
			continue
		case leg.Control != mix.Control:
			exclude("control-flag-mismatch")
			continue
		}
		if !leg.Control {
			// Registered strata only: a stratum outside the registered set is
			// excluded loudly, never silently pooled. The independence unit is
			// mandatory: a pair without a scenario family cannot cluster.
			if _, ok := assemblyMixedStratumSet[leg.Stratum]; !ok {
				exclude("unregistered-stratum")
				continue
			}
			if leg.ScenarioFamily == "" {
				exclude("missing-scenario-family")
				continue
			}
		}
		qL, okL := quality[s.base.ArtifactHash]
		qM, okM := quality[s.treat.ArtifactHash]
		if !okL || !okM {
			if !leg.Control {
				a.unlabeled++ // trips the incomplete-labeling gate below
			}
			exclude("unlabeled")
			continue
		}
		if leg.Control {
			a.report.ControlPairs++
			a.report.ControlAbsDeltas = append(a.report.ControlAbsDeltas, canonicalStat(math.Abs(qM-qL)))
			continue
		}
		// Arms verified identical above (stratum/family invariants), so
		// reading them from the legacy arm is exact. Keep this raw: the
		// decision must not use a rounded presentation value.
		a.complete = append(a.complete, assemblyMixedPair{
			pairID: k.pair,
			delta:  qM - qL, stratum: leg.Stratum, family: leg.ScenarioFamily,
			twin:   leg.TwinGroup,
			legacy: leg, mixed: mix,
		})
	}

	var out []AssemblyMixedModelReport
	for _, model := range order {
		a := models[model]
		// A non-empty scenario family spanning more than one stratum breaks
		// the cluster-within-stratum bootstrap model (its pairs could no
		// longer move together); exclude every pair such a family touches.
		famStratum := map[string]string{}
		crossed := map[string]bool{}
		for _, p := range a.complete {
			if p.family == "" {
				continue
			}
			if s, ok := famStratum[p.family]; ok {
				if s != p.stratum {
					crossed[p.family] = true
				}
				continue
			}
			famStratum[p.family] = p.stratum
		}
		if len(crossed) > 0 {
			kept := make([]assemblyMixedPair, 0, len(a.complete))
			for _, p := range a.complete {
				if crossed[p.family] {
					a.report.Exclusions = append(a.report.Exclusions, AssemblyExclusion{
						PairID: p.pairID, Kind: assemblyKindLegacyMixed,
						Reason: "scenario-family-crosses-strata",
					})
					continue
				}
				kept = append(kept, p)
			}
			a.complete = kept
		}
		// A cluster above the registered size cap would dominate its
		// stratum's resampling; exclude every pair it holds, loudly.
		famCount := map[string]int{}
		for _, p := range a.complete {
			famCount[p.family]++
		}
		oversized := false
		for _, n := range famCount {
			if n > assemblyMixedMaxClusterSize {
				oversized = true
				break
			}
		}
		if oversized {
			kept := make([]assemblyMixedPair, 0, len(a.complete))
			for _, p := range a.complete {
				if famCount[p.family] > assemblyMixedMaxClusterSize {
					a.report.Exclusions = append(a.report.Exclusions, AssemblyExclusion{
						PairID: p.pairID, Kind: assemblyKindLegacyMixed,
						Reason: "oversized-cluster",
					})
					continue
				}
				kept = append(kept, p)
			}
			a.complete = kept
		}
		r := a.report
		r.Pairs = len(a.complete)
		var clusters [][][]float64
		var ciLo, ciHi float64
		if r.Pairs > 0 {
			var sum float64
			for _, p := range a.complete {
				r.Deltas = append(r.Deltas, canonicalStat(p.delta))
				sum += p.delta
			}
			r.MeanDelta = canonicalStat(sum / float64(r.Pairs))
			r.Strata, clusters = assemblyMixedStrata(a.complete)
			ciLo, ciHi = assemblyStratifiedClusterCI(clusters, seed, bootstrapN)
			r.DeltaCILow, r.DeltaCIHigh = canonicalStat(ciLo), canonicalStat(ciHi)
			r.ArmPressure = assemblyMixedArmPressure(a.complete)
			r.ClusterDiagnostics = assemblyMixedClusterDiagnostics(
				a.complete, r.Strata, clusters, ciLo, seed, bootstrapN)
		}
		switch {
		case a.unlabeled > 0:
			r.Decision = "incomplete-labeling"
		case r.Pairs < assemblyMixedMinimumPairs:
			r.Decision = "insufficient-corpus"
		case assemblyMixedStratumBelowFloor(r.Strata):
			r.Decision = "insufficient-stratum-balance"
		case assemblyMixedClusterDiversityBelowFloor(clusters):
			r.Decision = "insufficient-cluster-diversity"
		default:
			r.Decision = assemblyMixedDecision(ciLo, ciHi)
		}
		out = append(out, r)
	}
	return out
}

// assemblyMixedStratumBelowFloor checks the registered per-stratum floor over
// the REGISTERED stratum set: a registered stratum entirely absent from the
// complete pairs counts as zero and trips the floor exactly like a present
// stratum below it. Unregistered strata cannot appear in strata (their pairs
// were excluded upstream).
func assemblyMixedStratumBelowFloor(strata []AssemblyStratumReport) bool {
	counts := make(map[string]int, len(strata))
	for _, s := range strata {
		counts[s.Stratum] = s.Pairs
	}
	for _, name := range assemblyMixedRegisteredStrata {
		if counts[name] < assemblyMixedMinimumStratumPairs {
			return true
		}
	}
	return false
}

// assemblyMixedClusterDiversityBelowFloor checks the registered
// cluster-diversity floor: every stratum among the complete pairs must hold
// at least assemblyMixedMinClustersPerStratum distinct scenario-family
// clusters. Callers gate on the stratum floor first, so every registered
// stratum is present when this runs.
func assemblyMixedClusterDiversityBelowFloor(clusters [][][]float64) bool {
	for _, cs := range clusters {
		if len(cs) < assemblyMixedMinClustersPerStratum {
			return true
		}
	}
	return false
}

// assemblyMixedClusterDiagnostics computes the descriptive
// cluster-independence summary and the leave-one-group-out sensitivity band
// for one model's complete pairs. Descriptive ONLY — the decision rule never
// consults it. strata and clusters are the aligned assemblyMixedStrata
// outputs over complete; pooledLow is the full-set CI lower bound, used as
// the degenerate band when a single group covers every pair (removing it
// would leave nothing to estimate).
func assemblyMixedClusterDiagnostics(complete []assemblyMixedPair, strata []AssemblyStratumReport, clusters [][][]float64, pooledLow float64, seed int64, bootstrapN int) *AssemblyClusterDiagnostics {
	diag := &AssemblyClusterDiagnostics{ClustersPerStratum: make(map[string]int, len(strata))}
	for i, sr := range strata {
		diag.ClustersPerStratum[sr.Stratum] = len(clusters[i])
		for _, c := range clusters[i] {
			if len(c) > diag.MaxClusterSize {
				diag.MaxClusterSize = len(c)
			}
		}
	}
	fams := make([]string, len(complete))
	twins := make([]string, len(complete))
	for i, p := range complete {
		fams[i], twins[i] = p.family, p.twin
	}
	groups := independenceGroups(fams, twins)
	diag.IndependenceGroups = len(groups)
	lows := make([]float64, 0, len(groups))
	for _, g := range groups {
		drop := make(map[int]bool, len(g))
		for _, i := range g {
			drop[i] = true
		}
		remaining := make([]assemblyMixedPair, 0, len(complete)-len(g))
		for i, p := range complete {
			if !drop[i] {
				remaining = append(remaining, p)
			}
		}
		if len(remaining) == 0 {
			continue
		}
		_, cs := assemblyMixedStrata(remaining)
		lo, _ := assemblyStratifiedClusterCI(cs, seed, bootstrapN)
		lows = append(lows, lo)
	}
	if len(lows) == 0 {
		diag.LeaveOneOut = AssemblyLeaveOneGroupOut{MinLow: canonicalStat(pooledLow), MaxLow: canonicalStat(pooledLow)}
		return diag
	}
	band := AssemblyLeaveOneGroupOut{MinLow: lows[0], MaxLow: lows[0]}
	for _, lo := range lows[1:] {
		band.MinLow = math.Min(band.MinLow, lo)
		band.MaxLow = math.Max(band.MaxLow, lo)
	}
	diag.LeaveOneOut = AssemblyLeaveOneGroupOut{
		MinLow: canonicalStat(band.MinLow),
		MaxLow: canonicalStat(band.MaxLow),
	}
	return diag
}

// assemblyMixedStrata groups complete pairs by stratum (sorted) and returns
// both the per-stratum descriptives and the bootstrap cluster structure:
// per stratum, scenario-family clusters in first-seen (pair-ID) order, a
// family-less pair forming its own cluster.
func assemblyMixedStrata(complete []assemblyMixedPair) ([]AssemblyStratumReport, [][][]float64) {
	byStratum := map[string][]assemblyMixedPair{}
	var names []string
	for _, p := range complete {
		if _, ok := byStratum[p.stratum]; !ok {
			names = append(names, p.stratum)
		}
		byStratum[p.stratum] = append(byStratum[p.stratum], p)
	}
	sort.Strings(names)
	strata := make([]AssemblyStratumReport, 0, len(names))
	clusters := make([][][]float64, 0, len(names))
	for _, name := range names {
		ps := byStratum[name]
		sr := AssemblyStratumReport{Stratum: name, Pairs: len(ps)}
		famIdx := map[string]int{}
		var cs [][]float64
		var sum float64
		for _, p := range ps {
			sum += p.delta
			switch {
			case p.delta > 0:
				sr.Wins++
			case p.delta < 0:
				sr.Losses++
			default:
				sr.Ties++
			}
			if p.family == "" {
				cs = append(cs, []float64{p.delta})
				continue
			}
			idx, ok := famIdx[p.family]
			if !ok {
				idx = len(cs)
				famIdx[p.family] = idx
				cs = append(cs, nil)
			}
			cs[idx] = append(cs[idx], p.delta)
		}
		sr.MeanDelta = canonicalStat(sum / float64(len(ps)))
		strata = append(strata, sr)
		clusters = append(clusters, cs)
	}
	return strata, clusters
}

// assemblyStratifiedClusterCI extends bootstrapDeltaCI's mechanics (same
// seed handling, same N, same nearest-rank 2.5/97.5 percentiles) to a
// FIXED-STRATUM-WEIGHT cluster bootstrap. REGISTERED estimand and scheme:
// the point estimand is the unweighted mean delta over all complete
// non-control pairs; each replicate resamples every stratum independently
// (k_s of its k_s scenario-family clusters with replacement — all pairs
// sharing a family move together), takes the stratum's ratio-of-sums mean
// over the resampled pairs, and combines the strata as
// sum_s (n_s/N) * mean_s, where n_s is the ORIGINAL complete-pair count of
// stratum s and N = sum_s n_s. The stratum weights are FIXED across
// replicates, so replicate noise reflects within-stratum cluster resampling
// only — never accidental stratum re-weighting when uneven cluster sizes
// make a resample land more pairs in one stratum. The CI is pooled only, by
// pre-registration.
func assemblyStratifiedClusterCI(strata [][][]float64, seed int64, n int) (lo, hi float64) {
	totalPairs := 0
	weights := make([]float64, len(strata))
	for si, clusters := range strata {
		for _, c := range clusters {
			weights[si] += float64(len(c))
			totalPairs += len(c)
		}
	}
	if totalPairs == 0 || n <= 0 {
		return math.NaN(), math.NaN()
	}
	for si := range weights {
		weights[si] /= float64(totalPairs)
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, n)
	for i := 0; i < n; i++ {
		var rep float64
		for si, clusters := range strata {
			k := len(clusters)
			if k == 0 {
				continue // zero-weight stratum: nothing to draw
			}
			var sum float64
			count := 0
			for j := 0; j < k; j++ {
				for _, d := range clusters[rng.Intn(k)] {
					sum += d
					count++
				}
			}
			rep += float64(weights[si] * (sum / float64(count)))
		}
		means[i] = rep
	}
	ps := percentiles(means, 0.025, 0.975)
	return ps[0], ps[1]
}

// assemblyMixedArmPressure computes per-arm aggregate pressure descriptives
// over the complete non-control pairs. Callers guard len(complete) > 0.
func assemblyMixedArmPressure(complete []assemblyMixedPair) []AssemblyArmPressure {
	arm := func(mode AssemblyMode, pick func(p assemblyMixedPair) *AssemblyEval) AssemblyArmPressure {
		msgs := make([]float64, len(complete))
		bytes := make([]float64, len(complete))
		omitted := make([]float64, len(complete))
		levels := map[string]int{}
		for i, p := range complete {
			ae := pick(p)
			msgs[i] = float64(ae.ShedMessages)
			bytes[i] = float64(ae.ShedBytes)
			omitted[i] = float64(ae.OmittedSubjects)
			if ae.PressureLevel != "" {
				levels[ae.PressureLevel]++
			}
		}
		return AssemblyArmPressure{
			Mode:                  string(mode),
			MedianShedMessages:    canonicalStat(medianOf(msgs)),
			MedianShedBytes:       canonicalStat(medianOf(bytes)),
			MedianOmittedSubjects: canonicalStat(medianOf(omitted)),
			PressureLevels:        levels,
		}
	}
	return []AssemblyArmPressure{
		arm(AssemblyLegacy, func(p assemblyMixedPair) *AssemblyEval { return p.legacy }),
		arm(AssemblyMixed, func(p assemblyMixedPair) *AssemblyEval { return p.mixed }),
	}
}

// computeAssemblyTopline aggregates unpaired topline artifacts into the
// per-model descriptive ceiling section, grouped by stratum.
func computeAssemblyTopline(arts []*Artifact, quality map[string]float64) []AssemblyToplineReport {
	type sacc struct {
		count, labeled int
		sum            float64
	}
	type macc struct {
		total     sacc
		byStratum map[string]*sacc
		names     []string
	}
	models := map[string]*macc{}
	var order []string
	for _, a := range arts {
		key := modelKey(a.CandidateModel)
		m, ok := models[key]
		if !ok {
			m = &macc{byStratum: map[string]*sacc{}}
			models[key] = m
			order = append(order, key)
		}
		stratum := a.Trace.AssemblyEval.Stratum
		s, ok := m.byStratum[stratum]
		if !ok {
			s = &sacc{}
			m.byStratum[stratum] = s
			m.names = append(m.names, stratum)
		}
		m.total.count++
		s.count++
		if q, ok := quality[a.ArtifactHash]; ok {
			m.total.labeled++
			m.total.sum += q
			s.labeled++
			s.sum += q
		}
	}
	sort.Strings(order)
	meanQ := func(s sacc) float64 {
		if s.labeled == 0 {
			return 0
		}
		return canonicalStat(s.sum / float64(s.labeled))
	}
	var out []AssemblyToplineReport
	for _, model := range order {
		m := models[model]
		sort.Strings(m.names)
		rep := AssemblyToplineReport{
			CandidateModel: model,
			Count:          m.total.count,
			Labeled:        m.total.labeled,
			MeanQuality:    meanQ(m.total),
		}
		for _, name := range m.names {
			s := m.byStratum[name]
			rep.ByStratum = append(rep.ByStratum, AssemblyToplineStratum{
				Stratum: name, Count: s.count, Labeled: s.labeled, MeanQuality: meanQ(*s),
			})
		}
		out = append(out, rep)
	}
	return out
}

// medianOf returns the midpoint median (average of the two central order
// statistics for even n), matching the 3a token-reduction median. Callers
// guard len(xs) > 0.
func medianOf(xs []float64) float64 {
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
