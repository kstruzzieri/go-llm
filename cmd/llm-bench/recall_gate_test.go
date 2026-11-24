package main

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
)

func TestSignTestOneSidedP(t *testing.T) {
	tests := []struct {
		name string
		b, c int
		want float64
	}{
		{"8-0", 8, 0, 1.0 / 256},
		{"0-0", 0, 0, 1},
		{"13-4", 13, 4, 0.0245},
	}
	for _, tc := range tests {
		if got := signTestOneSidedP(tc.b, tc.c); math.Abs(got-tc.want) > 1e-4 {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if signTestOneSidedP(13, 4) >= recallG1Alpha {
		t.Error("(13,4) should be below 0.025")
	}
	if signTestOneSidedP(11, 3) < recallG1Alpha {
		t.Error("(11,3) should be at or above 0.025")
	}
}

// TestRecallSignTestExactMatchesFloat checks the exact integer decision
// against the float for every n <= 80 and reports the closest approach.
func TestRecallSignTestExactMatchesFloat(t *testing.T) {
	closest := math.Inf(1)
	for n := 1; n <= 80; n++ {
		for b := 0; b <= n; b++ {
			p := signTestOneSidedP(b, n-b)
			if d := math.Abs(p - recallG1Alpha); d < closest {
				closest = d
			}
			if math.Abs(p-recallG1Alpha) < 1e-12 {
				t.Logf("near-boundary case b=%d c=%d p=%v exact=%v", b, n-b, p, recallSignTestPasses(b, n-b))
			}
			if got, fl := recallSignTestPasses(b, n-b), p < recallG1Alpha; got != fl {
				t.Errorf("b=%d c=%d: exact %v float %v", b, n-b, got, fl)
			}
			// Independent exact check.
			tail := new(big.Int)
			for k := b; k <= n; k++ {
				tail.Add(tail, new(big.Int).Binomial(int64(n), int64(k)))
			}
			want := new(big.Int).Mul(tail, big.NewInt(recallG1AlphaDenom)).Cmp(new(big.Int).Lsh(big.NewInt(1), uint(n))) < 0
			if got := recallSignTestPasses(b, n-b); got != want {
				t.Errorf("b=%d c=%d: got %v want %v", b, n-b, got, want)
			}
		}
	}
	t.Logf("closest |p-0.025| for n<=80: %g", closest)
}

func TestSignTestNegativeCountsPanic(t *testing.T) {
	for _, tc := range []struct{ b, c int }{{-1, 3}, {3, -1}} {
		for name, f := range map[string]func(){
			"p":      func() { signTestOneSidedP(tc.b, tc.c) },
			"passes": func() { recallSignTestPasses(tc.b, tc.c) },
		} {
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("%s(%d,%d): no panic", name, tc.b, tc.c)
					}
				}()
				f()
			}()
		}
	}
}

// aPairs returns n identical Part A pairs with the given outcomes.
func aPairs(base, labeled recallKind, n int) []recallAPair {
	out := make([]recallAPair, n)
	for i := range out {
		out[i] = recallAPair{Base: base, Labeled: labeled}
	}
	return out
}

func TestEvaluateRecallG1(t *testing.T) {
	build := func(b, c int, extra ...recallAPair) []recallAPair {
		p := aPairs(recallRely, recallRecheck, b)
		p = append(p, aPairs(recallRecheck, recallRely, c)...)
		return append(p, extra...)
	}
	tests := []struct {
		name      string
		pairs     []recallAPair
		b, c, nei int
		pass      bool
	}{
		{"8-0 pass", build(8, 0), 8, 0, 0, true},
		{"13-4 pass", build(13, 4), 13, 4, 0, true},
		{"10-2 pass", build(10, 2), 10, 2, 0, true},
		{"11-3 fail p", build(11, 3), 11, 3, 0, false},
		{"7-0 fail net", build(7, 0), 7, 0, 0, false},
		{"memory write neither", build(8, 0, recallAPair{Base: recallRely, Labeled: recallMemoryWrite}), 8, 0, 1, true},
		{"empty neither", build(8, 0, recallAPair{Base: recallRely, Labeled: recallEmpty}), 8, 0, 1, true},
		{"second search and no value are b", []recallAPair{{Base: recallRely, Labeled: recallSecondSearch}, {Base: recallRely, Labeled: recallNoValue}}, 2, 0, 0, false},
		{"missing is c not b", build(8, 0, recallAPair{Base: recallRely, Labeled: recallRecheck, Missing: true}), 8, 1, 0, false},
	}
	for _, tc := range tests {
		got := evaluateRecallG1("x", tc.pairs)
		if got.B != tc.b || got.C != tc.c || got.Neither != tc.nei || got.Pass != tc.pass || got.Comparison != "x" {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
}

// bPairs returns n identical Part B pairs with the given outcomes.
func bPairs(base, labeled recallBOutcome, n int) []recallBPair {
	out := make([]recallBPair, n)
	for i := range out {
		out[i] = recallBPair{Base: base, Labeled: labeled}
	}
	return out
}

func TestEvaluateRecallG2(t *testing.T) {
	mk := func(answers, slips int, extra ...recallBPair) []recallBPair {
		p := bPairs(recallAnswers, recallAnswers, answers-slips)
		p = append(p, bPairs(recallAnswers, recallDeclinesOrWrong, slips)...)
		return append(p, extra...)
	}
	tests := []struct {
		name          string
		pairs         []recallBPair
		maxSlips      int
		primary       bool
		answers, slip int
		inconc, pass  bool
	}{
		{"40/1 pass", mk(40, 1), recallPrimaryMaxSlips, true, 40, 1, false, true},
		{"40/2 fail", mk(40, 2), recallPrimaryMaxSlips, true, 40, 2, false, false},
		{"35/2 fail not inconclusive", mk(35, 2), recallPrimaryMaxSlips, true, 35, 2, false, false},
		{"35/1 inconclusive", mk(35, 1), recallPrimaryMaxSlips, true, 35, 1, true, false},
		{"36/1 pass", mk(36, 1), recallPrimaryMaxSlips, true, 36, 1, false, true},
		{"continues not slip", mk(40, 0, recallBPair{Base: recallAnswers, Labeled: recallContinues}), recallPrimaryMaxSlips, true, 41, 0, false, true},
		{"missing is slip", mk(40, 0, recallBPair{Base: recallAnswers, Labeled: recallAnswers, Missing: true}), recallPrimaryMaxSlips - 1, true, 40, 1, false, false},
		{"secondary 2 pass", mk(30, 2), recallSecondaryMaxSlips, false, 30, 2, false, true},
		{"nil primary fails", nil, recallPrimaryMaxSlips, true, 0, 0, false, false},
		{"nil secondary fails", nil, recallSecondaryMaxSlips, false, 0, 0, false, false},
		{"secondary 3 fail", mk(30, 3), recallSecondaryMaxSlips, false, 30, 3, false, false},
	}
	for _, tc := range tests {
		got := evaluateRecallG2(tc.pairs, tc.maxSlips, tc.primary)
		if got.Pairs != len(tc.pairs) || got.ArmOneAnswers != tc.answers || got.Slips != tc.slip || got.Inconclusive != tc.inconc || got.Pass != tc.pass {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
}

func TestDecideRecallRun(t *testing.T) {
	g1 := func(c string) recallG1Result { return recallG1Result{Comparison: c, B: 8, Pass: true} }
	g2 := func() recallG2Result { return recallG2Result{Pairs: 40, ArmOneAnswers: 40, Pass: true} }
	good := func() recallDecisionInput {
		return recallDecisionInput{
			Run:                1,
			DeterminismChanges: map[string]int{recallPrimaryModel: 0, "b": 0},
			G1:                 []recallG1Result{g1(recallCompareArm1), g1(recallCompareArm0)},
			PrimaryG2:          g2(),
			SecondaryG2:        map[string]recallG2Result{"b": g2()},
			LabelTokens:        map[string]int{recallPrimaryModel: recallLabelMaxTokens, "b": 1},
		}
	}
	tests := []struct {
		name string
		mut  func(*recallDecisionInput)
		want recallVerdict
	}{
		{"pass", func(*recallDecisionInput) {}, recallPass},
		{"arm0 G1 fails", func(in *recallDecisionInput) { in.G1[1].Pass = false }, recallFail},
		{"det run1", func(in *recallDecisionInput) { in.DeterminismChanges["b"] = 2 }, recallInvalid},
		{"det run2", func(in *recallDecisionInput) { in.Run = 2; in.DeterminismChanges["b"] = 2 }, recallInvalidFinal},
		{"det 1 ok", func(in *recallDecisionInput) { in.DeterminismChanges["b"] = 1 }, recallPass},
		{"stopped beats all", func(in *recallDecisionInput) {
			in.Stopped = true
			in.DeterminismChanges["b"] = 5
			in.LabelTokens["b"] = 99
		}, recallStopped},
		{"stopped with nothing else", func(in *recallDecisionInput) { *in = recallDecisionInput{Stopped: true} }, recallStopped},
		{"primary label 11", func(in *recallDecisionInput) { in.LabelTokens[recallPrimaryModel] = recallLabelMaxTokens + 1 }, recallFail},
		{"secondary label 11", func(in *recallDecisionInput) { in.LabelTokens["b"] = recallLabelMaxTokens + 1 }, recallFail},
		{"primary inconclusive", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{Pairs: 40, Inconclusive: true} }, recallInconclusive},
		{"fail beats inconclusive", func(in *recallDecisionInput) {
			in.PrimaryG2 = recallG2Result{Pairs: 40, Inconclusive: true}
			in.G1[0].Pass = false
		}, recallFail},
		{"primary G2 fail", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{Pairs: 40, Slips: 2} }, recallFail},
		{"secondary G2 fail", func(in *recallDecisionInput) { in.SecondaryG2["b"] = recallG2Result{Pairs: 40, Slips: 3} }, recallFail},
		{"invalid beats fail", func(in *recallDecisionInput) {
			in.DeterminismChanges[recallPrimaryModel] = 2
			in.LabelTokens["b"] = 99
		}, recallInvalid},
	}
	for _, tc := range tests {
		in := good()
		tc.mut(&in)
		if got := decideRecallRun(in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRecallDecisionInputValidate(t *testing.T) {
	good := func() recallDecisionInput {
		return recallDecisionInput{
			Run:                1,
			DeterminismChanges: map[string]int{recallPrimaryModel: 0, "b": 0},
			G1: []recallG1Result{
				{Comparison: recallCompareArm1, B: 8, Pass: true},
				{Comparison: recallCompareArm0, B: 8, Pass: true},
			},
			PrimaryG2:   recallG2Result{Pairs: 40, Pass: true},
			SecondaryG2: map[string]recallG2Result{"b": {Pairs: 40, Pass: true}},
			LabelTokens: map[string]int{recallPrimaryModel: 1, "b": 1},
		}
	}
	tests := []struct {
		name string
		mut  func(*recallDecisionInput)
		ok   bool
	}{
		{"valid", func(*recallDecisionInput) {}, true},
		{"no G1", func(in *recallDecisionInput) { in.G1 = nil }, false},
		{"one G1", func(in *recallDecisionInput) { in.G1 = in.G1[:1] }, false},
		{"arm 1 twice", func(in *recallDecisionInput) { in.G1[1].Comparison = recallCompareArm1 }, false},
		{"G1 with zero pairs", func(in *recallDecisionInput) { in.G1[0] = recallG1Result{Comparison: recallCompareArm1} }, false},
		{"run 0", func(in *recallDecisionInput) { in.Run = 0 }, false},
		{"run 3", func(in *recallDecisionInput) { in.Run = recallMaxRun + 1 }, false},
		{"zero-value primary G2", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{} }, false},
		{"zero-pair secondary G2", func(in *recallDecisionInput) { in.SecondaryG2["b"] = recallG2Result{} }, false},
		{"secondary holds primary", func(in *recallDecisionInput) {
			in.SecondaryG2[recallPrimaryModel] = recallG2Result{Pairs: 40, Pass: true}
		}, false},
		{"determinism missing primary", func(in *recallDecisionInput) { delete(in.DeterminismChanges, recallPrimaryModel) }, false},
		{"determinism missing secondary", func(in *recallDecisionInput) { delete(in.DeterminismChanges, "b") }, false},
		{"determinism extra model", func(in *recallDecisionInput) { in.DeterminismChanges["c"] = 0 }, false},
		{"labels missing model", func(in *recallDecisionInput) { delete(in.LabelTokens, "b") }, false},
		{"labels extra model", func(in *recallDecisionInput) { in.LabelTokens["c"] = 1 }, false},
		{"labels zero value", func(in *recallDecisionInput) { in.LabelTokens["b"] = 0 }, false},
	}
	for _, tc := range tests {
		in := good()
		tc.mut(&in)
		if err := in.validate(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
		want := recallFail
		if tc.ok {
			want = recallPass
		}
		if got := decideRecallRun(in); got != want {
			t.Errorf("%s: decideRecallRun = %v, want %v", tc.name, got, want)
		}
	}
}

func TestNewcombePairedCI(t *testing.T) {
	tests := []struct {
		name                string
		a, b, c, d          int
		wantLo, wantHi, tol float64
	}{
		{"39-1-0-0", 39, 1, 0, 0, -0.065, 0.129, 5e-4},
		{"40-0-0-0", 40, 0, 0, 0, -0.088, 0.088, 5e-4},
		{"newcombe-1998", 36, 12, 2, 0, 0.0569, 0.3404, 5e-5},
	}
	for _, tc := range tests {
		lo, hi := newcombePairedCI(tc.a, tc.b, tc.c, tc.d)
		if math.Abs(lo-tc.wantLo) > tc.tol || math.Abs(hi-tc.wantHi) > tc.tol {
			t.Errorf("%s: want lo %v hi %v, got lo %v hi %v (tol %g)", tc.name, tc.wantLo, tc.wantHi, lo, hi, tc.tol)
		}
	}
	for _, n := range []int{5, 13, 19, 20} {
		lo, hi := newcombePairedCI(n, 0, 0, n)
		if math.IsNaN(lo) || math.IsNaN(hi) || math.Abs(lo) > 1e-8 || math.Abs(hi) > 1e-8 {
			t.Errorf("(%d,0,0,%d): want lo 0 hi 0 (tol 1e-8), got lo %v hi %v", n, n, lo, hi)
		}
	}
	if lo, hi := newcombePairedCI(0, 0, 0, 0); lo != 0 || hi != 0 {
		t.Errorf("n=0: got %v %v", lo, hi)
	}
}

func TestRecallDecisionRuleTextNamesConstants(t *testing.T) {
	for _, want := range []string{
		fmt.Sprintf("at least %d", recallG1MinNet),
		fmt.Sprintf("p < %g", recallG1Alpha),
		fmt.Sprintf("at most %d slip", recallPrimaryMaxSlips),
		fmt.Sprintf("fewer than %d of 40", recallBaselineMin),
		fmt.Sprintf("%d or more slips", recallSecondaryMaxSlips+1),
		fmt.Sprintf("%d or more changed outcomes", recallDeterminismMaxChanges+1),
		fmt.Sprintf("at most %d prompt tokens", recallLabelMaxTokens),
		fmt.Sprintf("at most %d of the 12", recallHeadroomMaxRely),
		"re-checked, searched again, or gave no value",
		"plus every missing pair; every pair in neither B nor C",
		"counts for neither side",
		"not counted among arm 1's answers",
		"harness error",
		"never a gate outcome",
		string(recallInvalidFinal),
		fmt.Sprintf("a run number other than 1 or %d", recallMaxRun),
		"a run that was not stopped",
		"a missing or repeated comparison",
		"the primary model listed as another model",
		"arm 0", "arm 1", "G2'", "Only pass leads to Stage 1",
	} {
		if !strings.Contains(recallDecisionRuleText, want) {
			t.Errorf("rule text missing %q", want)
		}
	}
}
