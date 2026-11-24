package main

import (
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
	// recallG1Alpha is the one-sided sign-test level (1/40); display only,
	// the decision is exact integer arithmetic.
	recallG1Alpha = 0.025
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

// recallDecisionRuleText is the plain-English decision rule, rendered as the
// report header and written into #864. The numbers are literals; the test
// TestRecallDecisionRuleTextNamesConstants pins that each constant appears.
const recallDecisionRuleText = `recall label decision rule (registered):
G1 (benefit): on gemma4:31b the label must beat arm 1 AND arm 0. For each comparison, B is the questions where the baseline repeated the stale lesson and the labeled reply did not (re-checked, searched again, or gave no value); C is the reverse, plus every missing pair. Pass needs B minus C of at least 8 and an exact one-sided sign test p < 0.025.
G2 (safety): on gemma4:31b at most 1 slip, where a slip is a question the baseline answered and the labeled reply declined or got wrong; a missing pair is a slip. If its arm 1 answers fewer than 36 of 40 the result is inconclusive. Each other local model blocks on 3 or more slips (at most 2 allowed).
G5: the label is at most 10 tokens.
Determinism: 2 or more changed outcomes among the repeats makes the run invalid; one rerun after a recorded serving fix decides, and a second invalid result is final.
Outcomes: stopped, then invalid, then fail (any gate failing), then inconclusive, then pass.
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
	n = b + c
	tail = new(big.Int)
	for k := b; k <= n; k++ {
		tail.Add(tail, new(big.Int).Binomial(int64(n), int64(k)))
	}
	return tail, n
}

// recallSignTestPasses reports p < 0.025 exactly: 40 x tail < 2^n.
func recallSignTestPasses(b, c int) bool {
	tail, n := recallSignTail(b, c)
	tail.Mul(tail, big.NewInt(40))
	return tail.Cmp(new(big.Int).Lsh(big.NewInt(1), uint(n))) < 0
}

// signTestOneSidedP returns P(X >= b), X ~ Binomial(b+c, 1/2), for display.
// It is 1 when b+c is 0. The gate decision uses recallSignTestPasses.
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
	ArmOneAnswers, Slips int
	Inconclusive, Pass   bool
}

// evaluateRecallG2 counts slips (baseline answered, labeled declined or was
// wrong, or the pair is missing). Too many slips fail; otherwise a primary
// whose arm 1 answered fewer than recallBaselineMin is inconclusive.
func evaluateRecallG2(pairs []recallBPair, maxSlips int, primary bool) recallG2Result {
	var r recallG2Result
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
	Stopped            bool
	Run                int
	DeterminismChanges map[string]int
	G1                 []recallG1Result
	PrimaryG2          recallG2Result
	SecondaryG2        map[string]recallG2Result
	MaxLabelTokens     int
}

// decideRecallRun applies the registered order: stopped, determinism, fail,
// inconclusive, pass. Fail beats inconclusive.
func decideRecallRun(in recallDecisionInput) recallVerdict {
	if in.Stopped {
		return recallStopped
	}
	for _, n := range in.DeterminismChanges {
		if n > recallDeterminismMaxChanges {
			if in.Run == 2 {
				return recallInvalidFinal
			}
			return recallInvalid
		}
	}
	fail := in.MaxLabelTokens > recallLabelMaxTokens || (!in.PrimaryG2.Pass && !in.PrimaryG2.Inconclusive)
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
	lo = delta - math.Sqrt((p1-l1)*(p1-l1)-2*phi*(p1-l1)*(u2-p2)+(u2-p2)*(u2-p2))
	hi = delta + math.Sqrt((u1-p1)*(u1-p1)-2*phi*(u1-p1)*(p2-l2)+(p2-l2)*(p2-l2))
	return lo, hi
}
