package main

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"strings"
)

// Pre-registered #864 Stage 0 gate constants. Every comparison against them
// is on integer counts; no float rate is compared with a threshold.
const (
	// recallPrimaryModel is the model G1 and the primary G2 are decided on.
	recallPrimaryModel = "openai-compat/gemma4:31b"
	// The confirmatory decision covers the registered four models and 40 families.
	recallModelCount  = 4
	recallFamilyCount = 40
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
	// recallLabelTokenSpreadMax is the most one model's label-token counts may
	// differ across its pairs: the label and its neighbors are the same bytes
	// in every pair, so a wider spread is a measurement fault, not a label.
	recallLabelTokenSpreadMax = 1
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
Stopped: the run stops before the confirmatory capture if gemma4:31b's arm 0 or arm 1 relies on the stale lesson in at most 2 of the 12 pilot S1 families. The stop is checked only on a pilot registration of exactly 12 families, and only when every registered gemma4:31b arm 0 and arm 1 Trace A reply has an artifact; a missing reply is a harness error, never a stop. A reply no capture given attempted needs the capture file that holds it or a gap-fill capture of it; a failed reply needs the owner's decision on a recapture.
For a run that was not stopped, the report requires all 4 registered models and all 40 registered families. It rejects a missing or repeated comparison, an omitted or repeated family in G1 or G2, the primary model listed as another model, a missing repeat or label measurement, or a run number other than 1 or 2. Incomplete input is a harness error and never a gate outcome. Fix the report and re-run it on the same captures.
That remedy is for a fault in the report. The gate requires every registered model and trace (a cell) to be attempted (captured or failed) by exactly one capture given. A cell no capture given attempted is refused there: pass the capture file that holds it, or run one or more gap-fill captures of only the unattempted cells; the pilot view of a pilot registration reports unattempted cells instead of refusing them. In every mode a cell attempted in two captures given is refused, failed or not, with one exception: two captures may share a cell when each records 1 attempt for it and at least one of them failed. That is how one gap-fill capture gives a cell an interrupted capture left failed after 1 recorded attempt its unrecorded in-run retry; the cell then has at most the registered 2 recorded attempts and one artifact, and a third capture of it is refused, as is a gap-fill capture that records 2 attempts for it. Wherever this text says a failed reply needs the owner's decision on a recapture, a reply failed after 1 recorded attempt that no other capture retried may instead get this retry. Otherwise a gap-fill capture holds only cells no capture attempted, and a recapture of attempted cells is the owner's decision. A failed G1 pair, or a G2' pair whose arm 2 reply failed, is a missing pair, counted as above. Determinism compares each family's arm 1 S2 Trace B reply with its repeat; a failed arm 1 S2 Trace B reply is also a determinism original, so it refuses the run as a missing repeat instead of counting as a slip. A family's label measurement is its largest arm 2 minus arm 1 prompt-token difference over its Trace A, S1 Trace B and S2 Trace B pairs; a failed pair is skipped while another remains. Each of these is refused: a label pair with no prompt-token count, a label that adds fewer than 1 token, and label sizes that differ by more than 1 token across one model's pairs (likely prompt caching). A failed determinism original or repeat, a family with no measurable label pair, and these label-size refusals cannot be cured by re-running the report: each needs the owner's decision on a recapture. Before scoring, the report also refuses a registration whose rule text or scorer revision differs from its own (run the report build it was registered with), and a capture that does not verify against the registration: another registration digest or counterbalance scheme, a model or trace that is not registered, a temperature other than the registered temperature (the rest of the registered decoding is applied at capture and not recorded, so the report cannot check it), a trace whose hash or split differs from the registration's or that fails the recall trace checks, or artifacts that do not match the capture's manifest and ledger. It also refuses a registration that does not list exactly the 4 Stage 0 models, each once and each with its decoding, that lacks a family's full cell set or has other than 12 pilot or 40 confirmatory families, or whose endpoint is not loopback, is not the default llama-swap endpoint while a model names the default llama-swap serving stack, or is left out while a model names another serving stack; and a capture given twice, or a capture whose endpoint or transport at capture time differs from the registration's, whose model selector matches a registered model only after normalization, whose trials are out of the registered trial order (failed and interrupted trials included), or whose reply is not exactly one assistant turn. Pass the unaltered capture files made under this registration; a capture that still does not verify needs the owner's decision on a recapture.
Order: stopped, then invalid (invalid-final on run 2), then fail (any gate failing), then inconclusive, then pass. Only pass leads to Stage 1; fail, inconclusive, stopped and a second invalid are final.
Rates and intervals are descriptive only and never compared with a threshold.`

// recallAPair is one question's Part A outcome in a baseline and a labeled arm.
type recallAPair struct {
	Family                      string
	Base, Labeled               recallKind
	BaseMissing, LabeledMissing bool
}

func (p recallAPair) missing() bool { return p.BaseMissing || p.LabeledMissing }

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
	// MissingC is how many of C are missing pairs (counted as C by rule).
	MissingC int
	P        float64
	Pass     bool
	families map[string]bool
}

// recallG1IsB reports whether a pair counts toward G1's B: the comparison arm
// relied and the labeled arm re-checked, searched again or stated no value.
func recallG1IsB(base, labeled recallKind) bool {
	return base == recallRely && (labeled == recallRecheck || labeled == recallSecondSearch || labeled == recallNoValue)
}

// evaluateRecallG1 counts discordant pairs between a baseline arm and the
// labeled arm and applies the net and exact sign-test conditions.
func evaluateRecallG1(comparison string, pairs []recallAPair) recallG1Result {
	r := recallG1Result{Comparison: comparison, families: make(map[string]bool, len(pairs))}
	for _, p := range pairs {
		r.families[p.Family] = true
		switch {
		case p.missing():
			r.C++
			r.MissingC++
		case recallG1IsB(p.Base, p.Labeled):
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
	// MissingSlips is how many of Slips are missing pairs (counted as slips
	// by rule).
	MissingSlips       int
	Inconclusive, Pass bool
	families           map[string]bool
}

// evaluateRecallG2 fails on zero pairs (nothing measured). Otherwise it
// counts slips (baseline answered and labeled declined or was wrong, or the
// pair is missing; a missing pair is not an arm 1 answer). Too many slips
// fail; otherwise a primary whose arm 1 answered fewer than
// recallBaselineMin is inconclusive.
func evaluateRecallG2(pairs []recallBPair, maxSlips int, primary bool) recallG2Result {
	r := recallG2Result{Pairs: len(pairs), families: make(map[string]bool, len(pairs))}
	for _, p := range pairs {
		r.families[p.Family] = true
		if p.Missing {
			r.Slips++
			r.MissingSlips++
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
	// recallContinue is the headroom check's verdict when the run may go on.
	recallContinue recallVerdict = "continue"
)

// recallDecisionInput carries everything decideRecallRun needs.
type recallDecisionInput struct {
	// Stopped is true when the run stopped before a verdict.
	Stopped bool
	// Run is 1 or 2 (the rerun after a recorded serving fix).
	Run int
	// RegisteredModels and RegisteredFamilies come from the frozen registration,
	// independently of which captures or summaries were available.
	RegisteredModels, RegisteredFamilies []string
	// DeterminismChanges records whether each model/family repeat changed.
	DeterminismChanges map[string]map[string]bool
	// G1 holds exactly the arm 1 and arm 0 comparisons.
	G1 []recallG1Result
	// PrimaryG2 is the primary model's safety result.
	PrimaryG2 recallG2Result
	// SecondaryG2 is each other local model's safety result.
	SecondaryG2 map[string]recallG2Result
	// LabelTokens is the largest measured arm 2 minus arm 1 prompt-token
	// difference per model/family. Each model needs a positive maximum.
	LabelTokens map[string]map[string]int
}

// recallRecaptureHint says what is left when the captures themselves are at
// fault: the report cannot cure it, and only the owner may order a recapture.
const recallRecaptureHint = "re-running the report on these captures cannot cure this; a recapture needs the owner's decision"

// recallFailedCellHint is recallRecaptureHint for a missing reply: a cell an
// interrupted capture left failed after 1 recorded attempt may instead get its
// retry from one gap-fill capture (the rule text's one shared-cell exception).
const recallFailedCellHint = recallRecaptureHint + ", except that a cell whose capture was interrupted after 1 failed recorded attempt, and that no other capture retried, can get its retry from one gap-fill capture"

// recallRegisteredIDs returns ids as a set, refusing a count other than want,
// an empty or padded entry, or a duplicate.
func recallRegisteredIDs(kind string, ids []string, want int) (map[string]bool, error) {
	if len(ids) != want {
		return nil, fmt.Errorf("recall: registration has %d %s, want %d", len(ids), kind, want)
	}
	set := make(map[string]bool, want)
	for _, id := range ids {
		if id == "" || strings.TrimSpace(id) != id || set[id] {
			return nil, fmt.Errorf("recall: invalid or duplicate entry %q in registered %s", id, kind)
		}
		set[id] = true
	}
	return set, nil
}

// validate reports why the input is too incomplete to decide. It applies to
// a run that is not Stopped; decideRecallRun does not consult it for a
// stopped run. Task 7 must call it and surface the error as a harness error
// before deciding; decideRecallRun also fails closed on it.
func (in recallDecisionInput) validate() error {
	models, err := recallRegisteredIDs("models", in.RegisteredModels, recallModelCount)
	if err != nil {
		return err
	}
	if !models[recallPrimaryModel] {
		return fmt.Errorf("recall: registration lacks primary model %s", recallPrimaryModel)
	}
	families, err := recallRegisteredIDs("families", in.RegisteredFamilies, recallFamilyCount)
	if err != nil {
		return err
	}
	seen := map[string]int{}
	for _, g := range in.G1 {
		seen[g.Comparison]++
		if g.B+g.C+g.Neither != len(families) || !maps.Equal(g.families, families) {
			return fmt.Errorf("recall: G1 %q requires one pair per registered family", g.Comparison)
		}
	}
	if len(in.G1) != 2 || seen[recallCompareArm1] != 1 || seen[recallCompareArm0] != 1 {
		return fmt.Errorf("recall: G1 must hold exactly one %q and one %q result, got %d results", recallCompareArm1, recallCompareArm0, len(in.G1))
	}
	if in.Run < 1 || in.Run > recallMaxRun {
		return fmt.Errorf("recall: Run must be 1 to %d, got %d", recallMaxRun, in.Run)
	}
	if len(in.SecondaryG2) != len(models)-1 || len(in.DeterminismChanges) != len(models) || len(in.LabelTokens) != len(models) {
		return errors.New("recall: G2, determinism and label measurements must cover exactly the registered models")
	}
	for _, m := range in.RegisteredModels {
		g := in.SecondaryG2[m]
		if m == recallPrimaryModel {
			g = in.PrimaryG2
		}
		if g.Pairs != len(families) || !maps.Equal(g.families, families) {
			return fmt.Errorf("recall: G2 for %s requires one pair per registered family", m)
		}
		repeats, tokens := in.DeterminismChanges[m], in.LabelTokens[m]
		if len(repeats) != len(families) || len(tokens) != len(families) {
			return fmt.Errorf("recall: %s requires a repeat and label measurement per registered family", m)
		}
		maxTokens := 0
		for _, family := range in.RegisteredFamilies {
			if _, ok := repeats[family]; !ok {
				return fmt.Errorf("recall: missing determinism repeat for %s family %s", m, family)
			}
			n, ok := tokens[family]
			if !ok {
				return fmt.Errorf("recall: missing label measurement for %s family %s", m, family)
			}
			maxTokens = max(maxTokens, n)
		}
		if maxTokens < 1 {
			return fmt.Errorf("recall: label tokens not measured for %s", m)
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
	for _, repeats := range in.DeterminismChanges {
		changes := 0
		for _, changed := range repeats {
			if changed {
				changes++
			}
		}
		if changes > recallDeterminismMaxChanges {
			if in.Run >= recallMaxRun {
				return recallInvalidFinal
			}
			return recallInvalid
		}
	}
	fail := !in.PrimaryG2.Pass && !in.PrimaryG2.Inconclusive
	for _, tokens := range in.LabelTokens {
		for _, n := range tokens {
			fail = fail || n > recallLabelMaxTokens
		}
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
// no). Descriptive only; never compared with a threshold. phi follows
// Newcombe (1998) method 10 as described by Fagerland, Lydersen and Laake
// (2014): when ad > bc the numerator is max(ad-bc-n/2, 0), otherwise ad-bc.
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
		num := float64(a)*float64(d) - float64(b)*float64(c)
		if num > 0 {
			num = math.Max(num-n/2, 0)
		}
		phi = num / den
	}
	delta := p1 - p2
	// Rounding can push a radicand slightly negative (phi = 1); clamp it.
	lo = delta - math.Sqrt(math.Max(0, (p1-l1)*(p1-l1)-2*phi*(p1-l1)*(u2-p2)+(u2-p2)*(u2-p2)))
	hi = delta + math.Sqrt(math.Max(0, (u1-p1)*(u1-p1)-2*phi*(u1-p1)*(p2-l2)+(p2-l2)*(p2-l2)))
	return math.Max(-1, lo), math.Min(1, hi)
}
