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

const recallTestSecondary = "openai-compat/qwen3-coder-next:latest"

func completeRecallDecisionInput() recallDecisionInput {
	in := recallDecisionInput{
		Run:                1,
		RegisteredModels:   []string{recallPrimaryModel, recallTestSecondary, "openai-compat/qwen3.6:35b-a3b", "openai-compat/qwen3.8:27b"},
		SecondaryG2:        make(map[string]recallG2Result),
		DeterminismChanges: make(map[string]map[string]bool),
		LabelTokens:        make(map[string]map[string]int),
	}
	var a []recallAPair
	var b []recallBPair
	for i := range recallFamilyCount {
		family := fmt.Sprintf("f%d", i)
		in.RegisteredFamilies = append(in.RegisteredFamilies, family)
		labeled := recallRely
		if i < recallG1MinNet {
			labeled = recallRecheck
		}
		a = append(a, recallAPair{Family: family, Base: recallRely, Labeled: labeled})
		b = append(b, recallBPair{Family: family, Base: recallAnswers, Labeled: recallAnswers})
	}
	in.G1 = []recallG1Result{evaluateRecallG1(recallCompareArm1, a), evaluateRecallG1(recallCompareArm0, a)}
	in.PrimaryG2 = evaluateRecallG2(b, recallPrimaryMaxSlips, true)
	for _, model := range in.RegisteredModels {
		if model != recallPrimaryModel {
			in.SecondaryG2[model] = evaluateRecallG2(b, recallSecondaryMaxSlips, false)
		}
		in.DeterminismChanges[model] = make(map[string]bool)
		in.LabelTokens[model] = make(map[string]int)
		for _, family := range in.RegisteredFamilies {
			in.DeterminismChanges[model][family] = false
			in.LabelTokens[model][family] = recallLabelMaxTokens
		}
	}
	return in
}

func TestDecideRecallRun(t *testing.T) {
	twoChanges := func(in *recallDecisionInput, model string) {
		in.DeterminismChanges[model]["f0"] = true
		in.DeterminismChanges[model]["f1"] = true
	}
	inconclusive := func(in *recallDecisionInput) {
		in.PrimaryG2.Pass, in.PrimaryG2.Inconclusive = false, true
	}
	tests := []struct {
		name string
		mut  func(*recallDecisionInput)
		want recallVerdict
	}{
		{"pass", func(*recallDecisionInput) {}, recallPass},
		{"missing captures count against the label", func(in *recallDecisionInput) {
			var pairs []recallAPair
			for i, family := range in.RegisteredFamilies {
				pairs = append(pairs, recallAPair{Family: family, Base: recallRely, Labeled: recallRecheck, Missing: i >= 8})
			}
			in.G1[0] = evaluateRecallG1(recallCompareArm1, pairs)
		}, recallFail},
		{"arm0 G1 fails", func(in *recallDecisionInput) { in.G1[1].Pass = false }, recallFail},
		{"det run1", func(in *recallDecisionInput) { twoChanges(in, recallTestSecondary) }, recallInvalid},
		{"det run2", func(in *recallDecisionInput) { in.Run = 2; twoChanges(in, recallTestSecondary) }, recallInvalidFinal},
		{"det 1 per model ok", func(in *recallDecisionInput) {
			in.DeterminismChanges[recallPrimaryModel]["f0"] = true
			in.DeterminismChanges[recallTestSecondary]["f1"] = true
		}, recallPass},
		{"stopped beats all", func(in *recallDecisionInput) {
			in.Stopped = true
			twoChanges(in, recallTestSecondary)
			in.LabelTokens[recallTestSecondary]["f0"] = 99
		}, recallStopped},
		{"stopped with nothing else", func(in *recallDecisionInput) { *in = recallDecisionInput{Stopped: true} }, recallStopped},
		{"primary label 11", func(in *recallDecisionInput) { in.LabelTokens[recallPrimaryModel]["f0"] = recallLabelMaxTokens + 1 }, recallFail},
		{"secondary label 11", func(in *recallDecisionInput) { in.LabelTokens[recallTestSecondary]["f0"] = recallLabelMaxTokens + 1 }, recallFail},
		{"primary inconclusive", inconclusive, recallInconclusive},
		{"fail beats inconclusive", func(in *recallDecisionInput) {
			inconclusive(in)
			in.G1[0].Pass = false
		}, recallFail},
		{"primary G2 fail", func(in *recallDecisionInput) { in.PrimaryG2.Pass = false }, recallFail},
		{"secondary G2 fail", func(in *recallDecisionInput) {
			g := in.SecondaryG2[recallTestSecondary]
			g.Pass = false
			in.SecondaryG2[recallTestSecondary] = g
		}, recallFail},
		{"invalid beats fail", func(in *recallDecisionInput) {
			twoChanges(in, recallPrimaryModel)
			in.LabelTokens[recallTestSecondary]["f0"] = 99
		}, recallInvalid},
	}
	for _, tc := range tests {
		in := completeRecallDecisionInput()
		tc.mut(&in)
		if !in.Stopped {
			if err := in.validate(); err != nil {
				t.Fatalf("%s: unexpected incomplete input: %v", tc.name, err)
			}
		}
		if got := decideRecallRun(in); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRecallDecisionInputValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*recallDecisionInput)
		ok   bool
	}{
		{"valid", func(*recallDecisionInput) {}, true},
		{"missing registration", func(in *recallDecisionInput) { in.RegisteredModels = nil }, false},
		{"wrong primary", func(in *recallDecisionInput) { in.RegisteredModels[0] = "other" }, false},
		{"duplicate registered model", func(in *recallDecisionInput) { in.RegisteredModels[2] = recallTestSecondary }, false},
		{"incomplete registered families", func(in *recallDecisionInput) { in.RegisteredFamilies = in.RegisteredFamilies[:8] }, false},
		{"duplicate registered family", func(in *recallDecisionInput) { in.RegisteredFamilies[1] = "f0" }, false},
		{"all secondary measurements omitted", func(in *recallDecisionInput) {
			for model := range in.SecondaryG2 {
				delete(in.SecondaryG2, model)
				delete(in.DeterminismChanges, model)
				delete(in.LabelTokens, model)
			}
		}, false},
		{"unregistered model substituted in every result", func(in *recallDecisionInput) {
			in.SecondaryG2["other"] = in.SecondaryG2[recallTestSecondary]
			in.DeterminismChanges["other"] = in.DeterminismChanges[recallTestSecondary]
			in.LabelTokens["other"] = in.LabelTokens[recallTestSecondary]
			delete(in.SecondaryG2, recallTestSecondary)
			delete(in.DeterminismChanges, recallTestSecondary)
			delete(in.LabelTokens, recallTestSecondary)
		}, false},
		{"no G1", func(in *recallDecisionInput) { in.G1 = nil }, false},
		{"one G1", func(in *recallDecisionInput) { in.G1 = in.G1[:1] }, false},
		{"arm 1 twice", func(in *recallDecisionInput) { in.G1[1].Comparison = recallCompareArm1 }, false},
		{"G1 with zero pairs", func(in *recallDecisionInput) { in.G1[0] = recallG1Result{Comparison: recallCompareArm1} }, false},
		{"only eight favorable families", func(in *recallDecisionInput) {
			var pairs []recallAPair
			for _, family := range in.RegisteredFamilies[:8] {
				pairs = append(pairs, recallAPair{Family: family, Base: recallRely, Labeled: recallRecheck})
			}
			in.G1[0] = evaluateRecallG1(recallCompareArm1, pairs)
		}, false},
		{"repeated G1 family", func(in *recallDecisionInput) {
			pairs := aPairs(recallRely, recallRecheck, recallFamilyCount)
			for i := range pairs {
				pairs[i].Family = in.RegisteredFamilies[i]
			}
			pairs[1].Family = pairs[0].Family
			in.G1[1] = evaluateRecallG1(recallCompareArm0, pairs)
		}, false},
		{"unknown G2 family", func(in *recallDecisionInput) {
			g := in.SecondaryG2[recallTestSecondary]
			delete(g.families, "f0")
			g.families["other"] = true
		}, false},
		{"run 0", func(in *recallDecisionInput) { in.Run = 0 }, false},
		{"run 3", func(in *recallDecisionInput) { in.Run = recallMaxRun + 1 }, false},
		{"zero-value primary G2", func(in *recallDecisionInput) { in.PrimaryG2 = recallG2Result{} }, false},
		{"zero-pair secondary G2", func(in *recallDecisionInput) { in.SecondaryG2[recallTestSecondary] = recallG2Result{} }, false},
		{"secondary holds primary", func(in *recallDecisionInput) {
			in.SecondaryG2[recallPrimaryModel] = recallG2Result{Pairs: 40, Pass: true}
		}, false},
		{"determinism missing primary", func(in *recallDecisionInput) { delete(in.DeterminismChanges, recallPrimaryModel) }, false},
		{"determinism missing secondary", func(in *recallDecisionInput) { delete(in.DeterminismChanges, recallTestSecondary) }, false},
		{"determinism extra model", func(in *recallDecisionInput) { in.DeterminismChanges["other"] = nil }, false},
		{"determinism missing repeat", func(in *recallDecisionInput) { delete(in.DeterminismChanges[recallTestSecondary], "f0") }, false},
		{"wrong repeat family", func(in *recallDecisionInput) {
			delete(in.DeterminismChanges[recallTestSecondary], "f0")
			in.DeterminismChanges[recallTestSecondary]["other"] = false
		}, false},
		{"labels missing model", func(in *recallDecisionInput) { delete(in.LabelTokens, recallTestSecondary) }, false},
		{"labels extra model", func(in *recallDecisionInput) { in.LabelTokens["other"] = nil }, false},
		{"labels missing family", func(in *recallDecisionInput) { delete(in.LabelTokens[recallTestSecondary], "f0") }, false},
		{"wrong label family", func(in *recallDecisionInput) {
			delete(in.LabelTokens[recallTestSecondary], "f0")
			in.LabelTokens[recallTestSecondary]["other"] = 1
		}, false},
		{"labels zero maximum", func(in *recallDecisionInput) {
			for f := range in.LabelTokens[recallTestSecondary] {
				in.LabelTokens[recallTestSecondary][f] = 0
			}
		}, false},
	}
	for _, tc := range tests {
		in := completeRecallDecisionInput()
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
		{"13-0-0-13", 13, 0, 0, 13, -0.0704, 0.0704, 5e-4},
		{"5-0-0-5", 5, 0, 0, 5, -0.1666, 0.1666, 5e-4},
		{"38-1-0-1", 38, 1, 0, 1, -0.0730, 0.1350, 5e-4},
		{"30-5-2-3", 30, 5, 2, 3, -0.0672, 0.2194, 5e-4},
	}
	for _, tc := range tests {
		lo, hi := newcombePairedCI(tc.a, tc.b, tc.c, tc.d)
		if math.Abs(lo-tc.wantLo) > tc.tol || math.Abs(hi-tc.wantHi) > tc.tol {
			t.Errorf("%s: want lo %v hi %v, got lo %v hi %v (tol %g)", tc.name, tc.wantLo, tc.wantHi, lo, hi, tc.tol)
		}
	}
	for _, tc := range [][4]int{{5, 0, 0, 5}, {13, 0, 0, 13}, {20, 0, 0, 20}, {1, 0, 0, 0}, {0, 0, 0, 1}, {0, 5, 0, 0}, {0, 0, 5, 0}, {7, 3, 3, 7}} {
		lo, hi := newcombePairedCI(tc[0], tc[1], tc[2], tc[3])
		if math.IsNaN(lo) || math.IsNaN(hi) || lo < -1 || hi > 1 || lo > hi {
			t.Errorf("%v: bad interval %v %v", tc, lo, hi)
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
		fmt.Sprintf("all %d registered models and all %d registered families", recallModelCount, recallFamilyCount),
		"a missing or repeated comparison",
		"the primary model listed as another model",
		// The capture-completeness, duplicate-attempt, determinism, label-size
		// and capture-verification refusals and the headroom stop's
		// completeness rule, each with its remedy.
		fmt.Sprintf("only on a pilot registration of exactly %d families", recallSplitFamilyCount("pilot")),
		"every registered gemma4:31b arm 0 and arm 1 Trace A reply has an artifact; a missing reply is a harness error, never a stop",
		"A reply no capture given attempted needs the capture file that holds it or a gap-fill capture of it; a failed reply needs the owner's decision on a recapture",
		"That remedy is for a fault in the report.",
		"The gate requires every registered model and trace (a cell) to be attempted (captured or failed) by exactly one capture given",
		"A cell no capture given attempted is refused there: pass the capture file that holds it, or run one or more gap-fill captures of only the unattempted cells",
		"the pilot view of a pilot registration reports unattempted cells instead of refusing them",
		"In every mode a cell attempted in two captures given is refused, failed or not and whatever its attempt count: a gap-fill capture holds only cells no capture attempted, and a recapture of attempted cells is the owner's decision",
		"A failed G1 pair, or a G2' pair whose arm 2 reply failed, is a missing pair, counted as above",
		"Determinism compares each family's arm 1 S2 Trace B reply with its repeat; a failed arm 1 S2 Trace B reply is also a determinism original, so it refuses the run as a missing repeat instead of counting as a slip",
		"largest arm 2 minus arm 1 prompt-token difference over its Trace A, S1 Trace B and S2 Trace B pairs; a failed pair is skipped while another remains",
		"Each of these is refused: a label pair with no prompt-token count, a label that adds fewer than 1 token, and label sizes that",
		fmt.Sprintf("differ by more than %d token across one model's pairs (likely prompt caching)", recallLabelTokenSpreadMax),
		"A failed determinism original or repeat, a family with no measurable label pair, and these label-size refusals cannot be cured by re-running the report: each needs the owner's decision on a recapture",
		"the report also refuses a registration whose rule text or scorer revision differs from its own (run the report build it was registered with)",
		"a capture that does not verify against the registration: another registration digest or counterbalance scheme, a model or trace that is not registered, a temperature other than the registered temperature (the rest of the registered decoding is applied at capture and not recorded, so the report cannot check it), a trace whose hash or split differs from the registration's or that fails the recall trace checks, or artifacts that do not match the capture's manifest and ledger",
		"Pass the unaltered capture files made under this registration; a capture that still does not verify needs the owner's decision on a recapture",
		"arm 0", "arm 1", "G2'", "Only pass leads to Stage 1",
		strings.TrimPrefix(recallPrimaryModel, "openai-compat/"),
	} {
		if !strings.Contains(recallDecisionRuleText, want) {
			t.Errorf("rule text missing %q", want)
		}
	}
}
