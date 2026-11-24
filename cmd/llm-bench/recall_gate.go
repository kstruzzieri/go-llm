package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Pre-registered #864 Stage 0 gate constants. Every comparison against them
// is on integer counts; no float rate is compared with a threshold.
const (
	// recallPrimaryModel is the model G1 and the primary G2 are decided on.
	recallPrimaryModel = "openai-compat/gemma4:31b"
	// recallG1MinNet is the minimum B-C for G1.
	recallG1MinNet = 8
	// recallG1AlphaDenom is 1/alpha: the one-sided sign-test level is 1/40.
	recallG1AlphaDenom = 40
	// recallG1Alpha is the sign-test level for display; the decision uses
	// exact integer arithmetic on recallG1AlphaDenom.
	recallG1Alpha = 1.0 / recallG1AlphaDenom
	// recallPrimaryMaxSlips is the most slips the primary model may have.
	recallPrimaryMaxSlips = 1
	// recallSecondaryMaxSlips is the most slips another local model may have.
	recallSecondaryMaxSlips = 2
	// recallBaselineMin is the fewest arm-1 answers the primary G2 needs.
	recallBaselineMin = 36
	// recallDeterminismMaxChanges is the most changed repeat outcomes allowed.
	recallDeterminismMaxChanges = 1
	// recallHeadroomMaxRely is the headroom precondition's rely ceiling.
	recallHeadroomMaxRely = 2
	// recallLabelMaxTokens is the label's token budget (G5).
	recallLabelMaxTokens = 10
)

// recallMaxRun is the last run number: one rerun after a recorded serving fix.
const recallMaxRun = 2

const (
	// recallCompareArm1 labels the G1 comparison against arm 1 (the
	// like-for-like baseline).
	recallCompareArm1 = "arm 1"
	// recallCompareArm0 labels the G1 comparison against arm 0 (today's
	// behavior).
	recallCompareArm0 = "arm 0"
)

// recallDecisionRuleText is the plain-English decision rule, rendered as the
// report header and written into #864. It holds literal numbers (it must stay
// a const); TestRecallDecisionRuleTextNamesConstants builds each phrase from
// the constants so the text cannot drift from them.
const recallDecisionRuleText = `recall label decision rule (registered):
G1 (benefit): on gemma4:31b the labeled arm must beat arm 1 AND arm 0, each decided separately. For each comparison, B is every family where the comparison arm repeated the stale lesson and the labeled arm instead re-checked, searched again, or gave no value; C is every family where the comparison arm did not repeat the stale lesson and the labeled arm did, plus every missing pair; every pair in neither B nor C (for example a memory write, another tool call, another value, or an empty reply) counts for neither side. Pass needs B minus C of at least 8, and an exact one-sided sign test on the B + C discordant pairs with p < 0.025.
G2' (safety): a slip is a question the baseline answered and the labeled reply declined or got wrong; a missing pair counts as a slip and is not counted among arm 1's answers. 2 or more slips on gemma4:31b fails whatever its baseline; with at most 1 slip, the result is inconclusive when its arm 1 answers fewer than 36 of 40. Each other local model blocks on 3 or more slips.
G5: the label costs at most 10 prompt tokens per labeled record, on each model.
Determinism: 2 or more changed outcomes among any one model's 40 repeats makes the run invalid; one rerun after a recorded serving fix decides, and a second invalid result is final.
Stopped: the run stops before the confirmatory capture if gemma4:31b's arm 0 or arm 1 relies on the stale lesson in at most 2 of the 12 pilot S1 families.
The report refuses to decide on incomplete input (a missing comparison or one with no pairs, a model without G2 pairs, a determinism count or a measured label size, a run number other than 1 or 2): that is a harness error, fixed and the report re-run on the same captures; it is never a gate outcome.
Order: stopped, then invalid (invalid-final on run 2), then fail (any gate failing), then inconclusive, then pass. Only pass leads to Stage 1; fail, inconclusive, stopped and a second invalid are final.
Rates and intervals are descriptive only and never compared with a threshold.`

// recallAPair is one question's Part A outcome in a baseline and a labeled arm.
type recallAPair struct {
	Family        string
	Base, Labeled recallKind
	Missing       bool
}

// recallBPair is one question's Part B outcome in a baseline and a labeled arm.
type recallBPair struct {
	Family        string
	Base, Labeled recallBOutcome
	Missing       bool
}

// recallSignTail returns tail = sum_{k=b..n} C(n,k) and n = b+c exactly.
func recallSignTail(b, c int) (tail *big.Int, n int) {
	if b < 0 || c < 0 {
		panic(fmt.Sprintf("recall: negative sign-test counts b=%d c=%d", b, c))
	}
	n = b + c
	tail = new(big.Int)
	for k := b; k <= n; k++ {
		tail.Add(tail, new(big.Int).Binomial(int64(n), int64(k)))
	}
	return tail, n
}

// recallSignTestPasses reports p < 1/recallG1AlphaDenom exactly:
// recallG1AlphaDenom x tail < 2^n. It panics on negative counts.
func recallSignTestPasses(b, c int) bool {
	tail, n := recallSignTail(b, c)
	tail.Mul(tail, big.NewInt(recallG1AlphaDenom))
	return tail.Cmp(new(big.Int).Lsh(big.NewInt(1), uint(n))) < 0
}

// signTestOneSidedP returns P(X >= b), X ~ Binomial(b+c, 1/2), for display.
// It is 1 when b+c is 0 and panics on negative counts. The gate decision
// uses recallSignTestPasses.
func signTestOneSidedP(b, c int) float64 {
	tail, n := recallSignTail(b, c)
	f, _ := new(big.Rat).SetFrac(tail, new(big.Int).Lsh(big.NewInt(1), uint(n))).Float64()
	return f
}

// recallG1Result is one G1 comparison's counts and verdict.
type recallG1Result struct {
	Comparison    string
	B, C, Neither int
	P             float64
	Pass          bool
}

// evaluateRecallG1 counts discordant pairs between a baseline arm and the
// labeled arm and applies the net and exact sign-test conditions.
func evaluateRecallG1(comparison string, pairs []recallAPair) recallG1Result {
	r := recallG1Result{Comparison: comparison}
	for _, p := range pairs {
		switch {
		case p.Missing:
			r.C++
		case p.Base == recallRely && (p.Labeled == recallRecheck || p.Labeled == recallSecondSearch || p.Labeled == recallNoValue):
			r.B++
		case p.Base != recallRely && p.Labeled == recallRely:
			r.C++
		default:
			r.Neither++
		}
	}
	r.P = signTestOneSidedP(r.B, r.C)
	r.Pass = r.B-r.C >= recallG1MinNet && recallSignTestPasses(r.B, r.C)
	return r
}

// recallG2Result is one model's safety count and verdict.
type recallG2Result struct {
	Pairs, ArmOneAnswers, Slips int
	Inconclusive, Pass          bool
}

// evaluateRecallG2 fails on zero pairs (nothing measured). Otherwise it
// counts slips (baseline answered and labeled declined or was wrong, or the
// pair is missing; a missing pair is not an arm 1 answer). Too many slips
// fail; otherwise a primary whose arm 1 answered fewer than
// recallBaselineMin is inconclusive.
func evaluateRecallG2(pairs []recallBPair, maxSlips int, primary bool) recallG2Result {
	r := recallG2Result{Pairs: len(pairs)}
	for _, p := range pairs {
		if p.Missing {
			r.Slips++
			continue
		}
		if p.Base == recallAnswers {
			r.ArmOneAnswers++
			if p.Labeled == recallDeclinesOrWrong {
				r.Slips++
			}
		}
	}
	switch {
	case len(pairs) == 0: // nothing measured: fail closed
	case r.Slips > maxSlips:
	case primary && r.ArmOneAnswers < recallBaselineMin:
		r.Inconclusive = true
	default:
		r.Pass = true
	}
	return r
}

// recallVerdict is the outcome of one Stage 0 run.
type recallVerdict string

const (
	// recallPass means every gate held.
	recallPass recallVerdict = "pass"
	// recallFail means a gate failed.
	recallFail recallVerdict = "fail"
	// recallInconclusive means the primary baseline was too weak to judge G2.
	recallInconclusive recallVerdict = "inconclusive"
	// recallInvalid means determinism broke on run 1; a rerun is allowed.
	recallInvalid recallVerdict = "invalid"
	// recallInvalidFinal means determinism broke on run 2.
	recallInvalidFinal recallVerdict = "invalid-final"
	// recallStopped means the run was stopped before a verdict.
	recallStopped recallVerdict = "stopped"
)

// recallDecisionInput carries everything decideRecallRun needs.
type recallDecisionInput struct {
	// Stopped is true when the run stopped before a verdict.
	Stopped bool
	// Run is 1 or 2 (the rerun after a recorded serving fix).
	Run int
	// DeterminismChanges is keyed by model display name; its key set must be
	// the primary model plus every key of SecondaryG2.
	DeterminismChanges map[string]int
	// G1 holds exactly the arm 1 and arm 0 comparisons.
	G1 []recallG1Result
	// PrimaryG2 is the primary model's safety result.
	PrimaryG2 recallG2Result
	// SecondaryG2 is each other local model's safety result.
	SecondaryG2 map[string]recallG2Result
	// LabelTokens is keyed like DeterminismChanges: per model, the largest
	// arm 2 minus arm 1 prompt-token difference over that model's labeled
	// pairs. Each value must be at least 1 (0 means not measured).
	LabelTokens map[string]int
}

// recallKeysMatch reports whether got holds exactly the primary model plus
// the keys of secondary.
func recallKeysMatch(got map[string]int, secondary map[string]recallG2Result) bool {
	if _, ok := got[recallPrimaryModel]; !ok || len(got) != len(secondary)+1 {
		return false
	}
	for m := range secondary {
		if _, ok := got[m]; !ok {
			return false
		}
	}
	return true
}

// validate reports why the input is too incomplete to decide. It applies to
// a run that is not Stopped; decideRecallRun does not consult it for a
// stopped run. Task 7 must call it and surface the error as a harness error
// before deciding; decideRecallRun also fails closed on it.
func (in recallDecisionInput) validate() error {
	seen := map[string]int{}
	for _, g := range in.G1 {
		seen[g.Comparison]++
		if g.B+g.C+g.Neither < 1 {
			return fmt.Errorf("recall: G1 %q has no pairs", g.Comparison)
		}
	}
	if len(in.G1) != 2 || seen[recallCompareArm1] != 1 || seen[recallCompareArm0] != 1 {
		return fmt.Errorf("recall: G1 must hold exactly one %q and one %q result, got %d results", recallCompareArm1, recallCompareArm0, len(in.G1))
	}
	if in.Run < 1 || in.Run > recallMaxRun {
		return fmt.Errorf("recall: Run must be 1 to %d, got %d", recallMaxRun, in.Run)
	}
	if _, ok := in.SecondaryG2[recallPrimaryModel]; ok {
		return fmt.Errorf("recall: SecondaryG2 must not hold the primary model %s", recallPrimaryModel)
	}
	if !recallKeysMatch(in.DeterminismChanges, in.SecondaryG2) {
		return errors.New("recall: DeterminismChanges models must be the primary plus SecondaryG2's models")
	}
	if !recallKeysMatch(in.LabelTokens, in.SecondaryG2) {
		return errors.New("recall: LabelTokens models must be the primary plus SecondaryG2's models")
	}
	for m, n := range in.LabelTokens {
		if n < 1 {
			return fmt.Errorf("recall: label tokens not measured for %s", m)
		}
	}
	if in.PrimaryG2.Pairs < 1 {
		return errors.New("recall: primary G2 has no pairs")
	}
	for m, g := range in.SecondaryG2 {
		if g.Pairs < 1 {
			return fmt.Errorf("recall: G2 for %s has no pairs", m)
		}
	}
	return nil
}

// decideRecallRun applies the registered order: stopped, determinism, fail,
// inconclusive, pass. Fail beats inconclusive. A stopped run is decided
// before validation. Task 7 calls validate first and returns its error as a
// harness error, so the recallFail returned here on a validate error is
// defensive only.
func decideRecallRun(in recallDecisionInput) recallVerdict {
	if in.Stopped {
		return recallStopped
	}
	if in.validate() != nil {
		return recallFail
	}
	for _, n := range in.DeterminismChanges {
		if n > recallDeterminismMaxChanges {
			if in.Run >= recallMaxRun {
				return recallInvalidFinal
			}
			return recallInvalid
		}
	}
	fail := !in.PrimaryG2.Pass && !in.PrimaryG2.Inconclusive
	for _, n := range in.LabelTokens {
		fail = fail || n > recallLabelMaxTokens
	}
	for _, g := range in.G1 {
		fail = fail || !g.Pass
	}
	for _, g := range in.SecondaryG2 {
		fail = fail || !g.Pass
	}
	switch {
	case fail:
		return recallFail
	case in.PrimaryG2.Inconclusive:
		return recallInconclusive
	}
	return recallPass
}

// recallWilson returns the Wilson score interval for x of n at z.
func recallWilson(x, n, z float64) (lo, hi float64) {
	p := x / n
	den := 1 + z*z/n
	ctr := (p + z*z/(2*n)) / den
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / den
	return ctr - half, ctr + half
}

// newcombePairedCI is Newcombe (1998) method 10 for the difference of paired
// proportions p1-p2 from the 2x2 cells (a both yes, b yes/no, c no/yes, d both
// no). Descriptive only; never compared with a threshold.
func newcombePairedCI(a, b, c, d int) (lo, hi float64) {
	n := float64(a + b + c + d)
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p1, p2 := float64(a+b)/n, float64(a+c)/n
	l1, u1 := recallWilson(float64(a+b), n, z)
	l2, u2 := recallWilson(float64(a+c), n, z)
	phi := 0.0
	if den := math.Sqrt(float64(a+b) * float64(c+d) * float64(a+c) * float64(b+d)); den != 0 {
		phi = (float64(a)*float64(d) - float64(b)*float64(c)) / den
	}
	delta := p1 - p2
	// Rounding can push a radicand slightly negative (phi = 1); clamp it.
	lo = delta - math.Sqrt(math.Max(0, (p1-l1)*(p1-l1)-2*phi*(p1-l1)*(u2-p2)+(u2-p2)*(u2-p2)))
	hi = delta + math.Sqrt(math.Max(0, (u1-p1)*(u1-p1)-2*phi*(u1-p1)*(p2-l2)+(p2-l2)*(p2-l2)))
	return math.Max(-1, lo), math.Min(1, hi)
}
