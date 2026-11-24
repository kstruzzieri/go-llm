package main

import (
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
			// Independent exact check.
			tail := new(big.Int)
			for k := b; k <= n; k++ {
				tail.Add(tail, new(big.Int).Binomial(int64(n), int64(k)))
			}
			want := new(big.Int).Mul(tail, big.NewInt(40)).Cmp(new(big.Int).Lsh(big.NewInt(1), uint(n))) < 0
			if got := recallSignTestPasses(b, n-b); got != want {
				t.Errorf("b=%d c=%d: got %v want %v", b, n-b, got, want)
			}
		}
	}
	t.Logf("closest |p-0.025| for n<=80: %g", closest)
}

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
		{"40/1 pass", mk(40, 1), 1, true, 40, 1, false, true},
		{"40/2 fail", mk(40, 2), 1, true, 40, 2, false, false},
		{"35/2 fail not inconclusive", mk(35, 2), 1, true, 35, 2, false, false},
		{"35/1 inconclusive", mk(35, 1), 1, true, 35, 1, true, false},
		{"36/1 pass", mk(36, 1), 1, true, 36, 1, false, true},
		{"continues not slip", mk(40, 0, recallBPair{Base: recallAnswers, Labeled: recallContinues}), 1, true, 41, 0, false, true},
		{"missing is slip", mk(40, 0, recallBPair{Base: recallAnswers, Labeled: recallAnswers, Missing: true}), 0, true, 40, 1, false, false},
		{"secondary 2 pass", mk(30, 2), 2, false, 30, 2, false, true},
		{"secondary 3 fail", mk(30, 3), 2, false, 30, 3, false, false},
	}
	for _, tc := range tests {
		got := evaluateRecallG2(tc.pairs, tc.maxSlips, tc.primary)
		if got.ArmOneAnswers != tc.answers || got.Slips != tc.slip || got.Inconclusive != tc.inconc || got.Pass != tc.pass {
			t.Errorf("%s: got %+v", tc.name, got)
		}
	}
}

func TestDecideRecallRun(t *testing.T) {
	good := func() recallDecisionInput {
		return recallDecisionInput{
			Run:                1,
			DeterminismChanges: map[string]int{"a": 0},
			G1:                 []recallG1Result{{Pass: true}, {Pass: true}},
			PrimaryG2:          recallG2Result{Pass: true},
			SecondaryG2:        map[string]recallG2Result{"b": {Pass: true}},
			MaxLabelTokens:     10,
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
			in.MaxLabelTokens = 99
		}, recallStopped},
		{"label 11", func(in *recallDecisionInput) { in.MaxLabelTokens = 11 }, recallFail},
		{"primary inconclusive", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{Inconclusive: true} }, recallInconclusive},
		{"fail beats inconclusive", func(in *recallDecisionInput) {
			in.PrimaryG2 = recallG2Result{Inconclusive: true}
			in.G1[0].Pass = false
		}, recallFail},
		{"primary G2 fail", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{} }, recallFail},
		{"secondary G2 fail", func(in *recallDecisionInput) { in.SecondaryG2["b"] = recallG2Result{} }, recallFail},
		{"invalid beats fail", func(in *recallDecisionInput) { in.DeterminismChanges["a"] = 2; in.MaxLabelTokens = 99 }, recallInvalid},
	}
	for _, tc := range tests {
		in := good()
		tc.mut(&in)
		if got := decideRecallRun(in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestNewcombePairedCI(t *testing.T) {
	tests := []struct {
		name       string
		a, b, c, d int
		wantHi     float64
	}{
		{"39-1-0-0", 39, 1, 0, 0, 0.129},
		{"40-0-0-0", 40, 0, 0, 0, 0.088},
	}
	for _, tc := range tests {
		lo, hi := newcombePairedCI(tc.a, tc.b, tc.c, tc.d)
		if math.Round(hi*1000)/1000 != tc.wantHi {
			t.Errorf("%s: hi=%v lo=%v want hi %v", tc.name, hi, lo, tc.wantHi)
		}
	}
	if lo, hi := newcombePairedCI(0, 0, 0, 0); lo != 0 || hi != 0 {
		t.Errorf("n=0: got %v %v", lo, hi)
	}
}

func TestRecallDecisionRuleTextNamesConstants(t *testing.T) {
	for _, want := range []string{"8", "0.025", "36", "at most 1", "3 or more", "arm 0", "10 tokens", "invalid", "inconclusive", "stopped"} {
		if !strings.Contains(recallDecisionRuleText, want) {
			t.Errorf("rule text missing %q", want)
		}
	}
}
