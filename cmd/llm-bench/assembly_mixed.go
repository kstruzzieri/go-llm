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
	assemblyMixedPressureFraction     = 0.6
)

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
			"quality-improved: stratified-bootstrap CI low > 0; noninferior: CI low strictly > %.2f; "+
			"materially-regressed: CI high < %.2f; else inconclusive; "+
			"token and pressure numbers are descriptive only, never consulted; registered pressure fraction f=%.1f",
		assemblyMixedMinimumPairs, assemblyMixedMinimumStratumPairs,
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

// assemblyMixedPair is one complete labeled non-control legacy-mixed pair.
type assemblyMixedPair struct {
	pairID  string
	delta   float64
	stratum string
	family  string
	legacy  *AssemblyEval
	mixed   *AssemblyEval
}

// computeAssemblyMixedSection builds the legacy-mixed per-model reports from
// the already-keyed pairs. keys must be sorted (model, kind, pair) so deltas
// land in lexicographic pair-ID order per model. Exclusions accumulate in
// two pair-ID-ordered phases, not one merged order: the first (per-pair)
// loop appends missing-arm/invariant/unlabeled exclusions, then the
// second loop appends every scenario-family-crosses-strata exclusion after
// it. Invariant failures become Exclusions, never report-wide errors.
func computeAssemblyMixedSection(keys []assemblyPairKey, pairs map[assemblyPairKey]*assemblyArmSet, quality map[string]float64, seed int64, bootstrapN int) []AssemblyMixedModelReport {
	type acc struct {
		report   AssemblyMixedModelReport
		complete []assemblyMixedPair
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
			continue
		}
		if s.treat == nil {
			exclude("missing-mixed-arm")
			continue
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
		case leg.Control != mix.Control:
			exclude("control-flag-mismatch")
			continue
		}
		qL, okL := quality[s.base.ArtifactHash]
		qM, okM := quality[s.treat.ArtifactHash]
		if !okL || !okM {
			exclude("unlabeled")
			continue
		}
		if leg.Control {
			a.report.ControlPairs++
			a.report.ControlAbsDeltas = append(a.report.ControlAbsDeltas, math.Abs(qM-qL))
			continue
		}
		// Arms verified identical above (stratum/family invariants), so
		// reading them from the legacy arm is exact.
		a.complete = append(a.complete, assemblyMixedPair{
			pairID: k.pair,
			delta:  qM - qL, stratum: leg.Stratum, family: leg.ScenarioFamily,
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
		r := a.report
		r.Pairs = len(a.complete)
		if r.Pairs > 0 {
			var sum float64
			for _, p := range a.complete {
				r.Deltas = append(r.Deltas, p.delta)
				sum += p.delta
			}
			r.MeanDelta = sum / float64(r.Pairs)
			var clusters [][][]float64
			r.Strata, clusters = assemblyMixedStrata(a.complete)
			r.DeltaCILow, r.DeltaCIHigh = assemblyStratifiedClusterCI(clusters, seed, bootstrapN)
			r.ArmPressure = assemblyMixedArmPressure(a.complete)
		}
		switch {
		case r.Pairs < assemblyMixedMinimumPairs:
			r.Decision = "insufficient-corpus"
		case assemblyMixedStratumBelowFloor(r.Strata):
			r.Decision = "insufficient-stratum-balance"
		default:
			r.Decision = assemblyMixedDecision(r.DeltaCILow, r.DeltaCIHigh)
		}
		out = append(out, r)
	}
	return out
}

func assemblyMixedStratumBelowFloor(strata []AssemblyStratumReport) bool {
	for _, s := range strata {
		if s.Pairs < assemblyMixedMinimumStratumPairs {
			return true
		}
	}
	return false
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
		sr.MeanDelta = sum / float64(len(ps))
		strata = append(strata, sr)
		clusters = append(clusters, cs)
	}
	return strata, clusters
}

// assemblyStratifiedClusterCI extends bootstrapDeltaCI's mechanics (same
// seed handling, same N, same nearest-rank 2.5/97.5 percentiles) to a
// stratified cluster bootstrap: each stratum is resampled independently
// (its cluster count preserved), and the resampling unit within a stratum
// is a scenario-family cluster — all pairs sharing a family move together.
// The CI is pooled only, by pre-registration.
func assemblyStratifiedClusterCI(strata [][][]float64, seed int64, n int) (lo, hi float64) {
	totalPairs := 0
	for _, clusters := range strata {
		for _, c := range clusters {
			totalPairs += len(c)
		}
	}
	if totalPairs == 0 || n <= 0 {
		return math.NaN(), math.NaN()
	}
	rng := rand.New(rand.NewSource(seed))
	means := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		count := 0
		for _, clusters := range strata {
			k := len(clusters)
			for j := 0; j < k; j++ {
				for _, d := range clusters[rng.Intn(k)] {
					sum += d
					count++
				}
			}
		}
		means[i] = sum / float64(count)
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
			MedianShedMessages:    medianOf(msgs),
			MedianShedBytes:       medianOf(bytes),
			MedianOmittedSubjects: medianOf(omitted),
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
		return s.sum / float64(s.labeled)
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
