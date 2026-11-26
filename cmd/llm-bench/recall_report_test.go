package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Test fixture sizes: the endpoint every test capture records and the family
// count (the confirmatory run has 40).
const (
	recallTestBase      = "http://127.0.0.1:8090"
	recallTestFamilyNum = recallFamilyCount
)

// recallBehave scripts one model's reply to one cell of one family: its content,
// raw tool calls and prompt-token count. fam is the family's position.
type recallBehave func(model string, fam int, cell string) (string, []injectionRawCall, int)

// Scripted replies: a reply stating the lesson value, one stating no value,
// and one re-reading the named file.
var (
	recallTestRely    = func() (string, []injectionRawCall) { return "The retry limit is 5.", nil }
	recallTestNoValue = func() (string, []injectionRawCall) { return "I will look into it.", nil }
	recallTestRecheck = func() (string, []injectionRawCall) {
		return "", []injectionRawCall{{Name: "read_file", Arguments: `{"path":"retry.go"}`, ArgumentsPresent: true, DecodeOK: true}}
	}
)

// recallTestDefault is a behavior where arms 0 and 1 rely on the first rely0
// and rely1 families, arm 2 re-checks wherever arm 1 relied, S7 reacts to the label
// exactly as arm 2 does, and every Trace B reply states the current value. The label adds 6
// prompt tokens and the placebo 3 or 4 (by family parity).
func recallTestDefault(rely0, rely1 int) recallBehave {
	return func(_ string, i int, cell string) (string, []injectionRawCall, int) {
		tokens := 100
		switch cell {
		case "A-0":
			tokens = 90
		case "A-2", "A-2-s7", "B-s1-2", "B-s2-2":
			tokens = 106
		case "B-s2-P":
			tokens = 103 + i%2
		}
		var content string
		var calls []injectionRawCall
		switch cell {
		case "A-0":
			content, calls = recallTestNoValue()
			if i < rely0 {
				content, calls = recallTestRely()
			}
		case "A-1":
			content, calls = recallTestNoValue()
			if i < rely1 {
				content, calls = recallTestRely()
			}
		case "A-2", "A-2-s7":
			content, calls = recallTestNoValue()
			if i < rely1 {
				content, calls = recallTestRecheck()
			}
		case "B-s1-1", "B-s1-2":
			content = "It is 7."
		default:
			content = "The retry limit is 5."
		}
		return content, calls, tokens
	}
}

// recallTestFamilies names n families f00, f01, ...
func recallTestFamilies(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("f%02d", i)
	}
	return out
}

// recallTestTrace builds one family's trace for one recallCells cell. A Trace
// B cell also reads the named file, whose result states the current value.
func recallTestTrace(fam string, i int, c recallCell, split string) Trace {
	tr := validRecallTrace()
	tr.ID = "recall-" + fam + "-" + c.Suffix
	tr.Recall.FamilyID, tr.Recall.Template = fam, fmt.Sprintf("tpl%d", i%4)
	tr.Recall.TaskStyle = []string{"direct", "task"}[i%2]
	tr.Recall.Split, tr.Recall.Trace, tr.Recall.Arm = split, c.Trace, c.Arm
	tr.Recall.Cases, tr.Recall.Repeat = c.Cases, c.Repeat
	if c.Trace == "B" {
		tr.Turns = append(tr.Turns,
			Turn{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_2", Name: "read_file", Arguments: json.RawMessage(`{"path":"retry.go"}`)}}},
			Turn{Role: "tool", ToolCallID: "call_2", Name: "read_file", Content: "maxRetries = " + tr.Recall.currentValue()})
	}
	return tr
}

// recallTestTraces builds every cell of every family.
func recallTestTraces(n int, split string) []Trace {
	var out []Trace
	for i, fam := range recallTestFamilies(n) {
		for _, c := range recallCells(split) {
			out = append(out, recallTestTrace(fam, i, c, split))
		}
	}
	return out
}

// recallExactRegistrationFile writes a registration of exactly traces under
// split (recallRegistrationFile pads to a pilot inventory of its own test
// families), for the four registered models; mutate, if set, runs last.
func recallExactRegistrationFile(t *testing.T, traces []Trace, split string, mutate func(map[string]any)) string {
	t.Helper()
	return recallRegistrationFile(t, traces, func(r map[string]any) {
		rows := []map[string]string{}
		for _, tr := range traces {
			rows = append(rows, map[string]string{"id": tr.ID, "sha256": canonicalTraceHash(tr)})
		}
		r["split"], r["traces"] = split, rows
		if mutate != nil {
			mutate(r)
		}
	})
}

// recallTestRegistration writes a registration of exactly traces under split
// for the four registered models and returns its path and digest.
func recallTestRegistration(t *testing.T, traces []Trace, split string) (string, string) {
	t.Helper()
	path := recallExactRegistrationFile(t, traces, split, nil)
	_, digest, err := loadRecallRegistration(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, digest
}

// recallTestCapture runs the real capture path with a scripted runner and
// returns the artifacts path. fail names cells whose requests error on both
// attempts, so they leave a failed ledger row and no artifact.
func recallTestCapture(t *testing.T, traces []Trace, models []string, digest string, b recallBehave, fail func(model, traceID string) bool) string {
	t.Helper()
	return recallTestCaptureCut(t, traces, models, digest, b, fail, nil)
}

// recallInterruptRunner returns its results with the interrupt a cancelled
// run reports; the capture then skips its retry round.
type recallInterruptRunner struct{ results []Result }

// RunAll returns the scripted results and context.Canceled.
func (r *recallInterruptRunner) RunAll(context.Context, []ModelTarget, []Trace) ([]Result, error) {
	return r.results, context.Canceled
}

// recallTestCaptureCut is recallTestCapture for an interrupted capture: cut
// names the cells the cancellation cut off, which publish as unattempted
// ledger rows (failed cells keep their one attempt). A nil cut is an
// uninterrupted capture.
func recallTestCaptureCut(t *testing.T, traces []Trace, models []string, digest string, b recallBehave, fail, cut func(model, traceID string) bool) string {
	t.Helper()
	provName := openAICompatCandidateProviderName(recallTestBase)
	var first []Result
	retry := map[string][]Result{}
	var targets []ModelTarget
	temps := map[string]float64{}
	for _, m := range models {
		targets = append(targets, ModelTarget{Display: m, Provider: openAICompatTransport, Model: strings.TrimPrefix(m, "openai-compat/")})
		temps[m] = 0
		for _, tr := range traces {
			fam, _, err := parseRecallTraceID(tr.ID)
			if err != nil {
				t.Fatal(err)
			}
			var famIdx int
			if _, err := fmt.Sscanf(fam, "f%d", &famIdx); err != nil {
				t.Fatal(err)
			}
			_, cell, _ := parseRecallTraceID(tr.ID)
			if cut != nil && cut(m, tr.ID) {
				first = append(first, Result{Model: m, TraceID: tr.ID, Err: context.Canceled})
				continue
			}
			if fail != nil && fail(m, tr.ID) {
				bad := Result{Model: m, TraceID: tr.ID, Err: errors.New("dial tcp: timeout")}
				first, retry[m] = append(first, bad), append(retry[m], bad)
				continue
			}
			content, calls, tokens := b(m, famIdx, cell)
			first = append(first, Result{Model: m, TraceID: tr.ID, CandidateProvider: provName,
				Transcript: []Turn{{Role: "assistant", Content: content}}, RawToolCalls: calls,
				Score: Score{PromptEvalTokens: tokens, GenTokens: 1}})
		}
	}
	rounds := [][]Result{first}
	for _, m := range models {
		if len(retry[m]) > 0 {
			rounds = append(rounds, retry[m])
		}
	}
	out := filepath.Join(t.TempDir(), "art.jsonl")
	var runner calibrationRunner = &scriptedCaptureRunner{rounds: rounds}
	if cut != nil {
		runner = &recallInterruptRunner{results: first}
	}
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
		Clock:               func() time.Time { return time.Unix(0, 0).UTC() },
		OpenAICompatBaseURL: recallTestBase, RegistrationDigest: digest, Stdout: io.Discard,
		InjectionTemperatures: temps,
	})
	if cut != nil && (err == nil || !strings.Contains(err.Error(), "partial ledger published")) {
		t.Fatalf("interrupted capture: err = %v; want the partial-ledger interrupt", err)
	}
	if cut == nil && err != nil {
		t.Fatalf("capture: %v", err)
	}
	return out
}

// recallTestRun builds a confirmatory registration and capture over the four
// registered models, then runs the report.
func recallTestRun(t *testing.T, b recallBehave, fail func(model, traceID string) bool, run int) (string, recallVerdict, error) {
	t.Helper()
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	reg, digest := recallTestRegistration(t, traces, "confirmatory")
	cap := recallTestCapture(t, traces, models, digest, b, fail)
	return runRecallReport(recallReportOptions{RegistrationPath: reg, Captures: []string{cap}, Run: run})
}

// The gate end to end over the real capture path: arm 2 re-checking on 10
// stale families passes, and arm 0 relying on only 2 of them fails.
func TestRunRecallReportPassAndFail(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rely0, rely1  int
		want          recallVerdict
		wantInReport  string
		wantNotInText string
	}{
		{"arm 2 re-checks on 10 stale families, 0 slips", 10, 10, recallPass, "Verdict: pass", ""},
		{"arm 0 relied on only 2", 2, 10, recallFail, "Verdict: fail", "Verdict: pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, v, err := recallTestRun(t, recallTestDefault(tc.rely0, tc.rely1), nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v != tc.want {
				t.Fatalf("verdict = %q, want %q\n%s", v, tc.want, text)
			}
			if !strings.Contains(text, recallDecisionRuleText) || !strings.Contains(text, tc.wantInReport) {
				t.Errorf("report lacks the rule text or %q:\n%s", tc.wantInReport, text)
			}
			if tc.wantNotInText != "" && strings.Contains(text, tc.wantNotInText) {
				t.Errorf("report holds %q", tc.wantNotInText)
			}
			for _, want := range []string{"G1 vs arm 1: B 10, C 0", "G5", "label tokens 6", "Determinism", "f = 0.25"} {
				if !strings.Contains(text, want) {
					t.Errorf("report lacks %q", want)
				}
			}
		})
	}
}

// A registered trace with no artifact (a failed capture) is a missing pair: c
// for G1, a slip for G2', never a panic and never scored as the zero kind.
func TestRecallReportMissingArtifact(t *testing.T) {
	failP := func(ids ...string) func(string, string) bool {
		return func(m, id string) bool {
			for _, want := range ids {
				if m == recallPrimaryModel && id == want {
					return true
				}
			}
			return false
		}
	}
	for _, tc := range []struct {
		name     string
		fail     func(string, string) bool
		want     recallVerdict
		wantText string
	}{
		{"one missing arm 2 where arm 1 did not rely counts as c, one missing G2 pair is one slip",
			failP("recall-f39-A-2", "recall-f00-B-s2-2"), recallPass, "arm 1 answers 39, slips 1"},
		{"two missing G2 pairs are two slips and fail", failP("recall-f00-B-s2-2", "recall-f01-B-s2-2"), recallFail, "slips 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, v, err := recallTestRun(t, recallTestDefault(10, 10), tc.fail, 1)
			if err != nil {
				t.Fatal(err)
			}
			if v != tc.want || !strings.Contains(text, tc.wantText) {
				t.Fatalf("verdict %q (want %q), text lacks %q:\n%s", v, tc.want, tc.wantText, text)
			}
		})
	}

	// A missing arm 1 S2 baseline is also a determinism original, so it is a
	// harness error (a missing repeat), not a slip the report decides on.
	if _, _, err := recallTestRun(t, recallTestDefault(10, 10), failP("recall-f00-B-s2-1"), 1); err == nil ||
		!strings.Contains(err.Error(), "determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 1 failed (no artifact): recall-f00-B-s2-1; "+recallFailedCellHint) {
		t.Errorf("failed arm 1 S2 baseline: err = %v; want the determinism refusal naming recall-f00-B-s2-1", err)
	}

	// The pair builders place a registered trace with no artifact into its
	// family and mark it Missing; the rest score from the artifacts.
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	reg, _, err := loadRecallRegistration(regPath)
	if err != nil {
		t.Fatal(err)
	}
	arts, err := loadArtifacts(recallTestCapture(t, traces, models, digest, recallTestDefault(10, 10), failP("recall-f39-A-2", "recall-f00-A-2-s7")))
	if err != nil {
		t.Fatal(err)
	}
	set, err := recallRegisteredSet(reg)
	if err != nil {
		t.Fatal(err)
	}
	ix := recallIndexOf(arts)
	g1 := evaluateRecallG1(recallCompareArm1, pairRecallA(ix, set, recallPrimaryModel, "1", "2", "S1"))
	if g1.B != 10 || g1.C != 1 || g1.Neither != 29 {
		t.Errorf("G1 vs arm 1 = %+v; want B 10, C 1 (the missing f39 pair), Neither 29", g1)
	}
	s7 := pairRecallA(ix, set, recallPrimaryModel, "1", "2", "S7")
	if len(s7) != recallTestFamilyNum || !s7[0].LabeledMissing || s7[0].Family != "f00" || s7[1].missing() {
		t.Errorf("S7 pairs = %+v; want 40 family pairs with f00 missing", s7)
	}
}

// Two changed repeats on a secondary make run 1 invalid and run 2 final. A
// determinism original or repeat with no artifact is a missing repeat: a
// harness error naming the cells and the recapture remedy, never a verdict,
// whatever the rest of the run would decide.
func TestRecallReportDeterminism(t *testing.T) {
	changeRepeats := func(model string, n int) recallBehave {
		base := recallTestDefault(10, 10)
		return func(m string, i int, cell string) (string, []injectionRawCall, int) {
			if m == model && cell == "B-s2-1-repeat" && i < n {
				return "I cannot say.", nil, 100
			}
			return base(m, i, cell)
		}
	}
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	for _, tc := range []struct {
		name    string
		changes int
		run     int
		want    recallVerdict
	}{
		{"1 change is within the allowance", 1, 1, recallPass},
		{"2 changes on a secondary, run 1", 2, 1, recallInvalid},
		{"2 changes on a secondary, run 2", 2, 2, recallInvalidFinal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := recallTestCapture(t, traces, models, digest, changeRepeats(recallTestSecondary, tc.changes), nil)
			text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Run: tc.run})
			if err != nil {
				t.Fatal(err)
			}
			if v != tc.want || !strings.Contains(text, fmt.Sprintf("%s: %d change(s) among 40 repeats", recallTestSecondary, tc.changes)) {
				t.Fatalf("verdict %q (want %q):\n%s", v, tc.want, text)
			}
		})
	}
	failOn := func(model string, ids ...string) func(string, string) bool {
		return func(m, id string) bool { return m == model && slices.Contains(ids, id) }
	}
	for _, tc := range []struct {
		name   string
		fail   func(string, string) bool
		behave recallBehave
		run    int
		lost   string // the model and the cells the error names
	}{
		{"a failed primary original", failOn(recallPrimaryModel, "recall-f00-B-s2-1"), recallTestDefault(10, 10), 1,
			recallPrimaryModel + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 1 failed (no artifact): recall-f00-B-s2-1; "},
		{"a failed secondary repeat on a run that would pass", failOn(recallTestSecondary, "recall-f05-B-s2-1-repeat"), recallTestDefault(10, 10), 2,
			recallTestSecondary + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 1 failed (no artifact): recall-f05-B-s2-1-repeat; "},
		{"failed cells on a run that would fail", failOn(recallPrimaryModel, "recall-f00-B-s2-1-repeat", "recall-f01-B-s2-1", "recall-f02-B-s2-2", "recall-f03-B-s2-2"), recallTestDefault(10, 10), 1,
			recallPrimaryModel + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 2 failed (no artifact): recall-f00-B-s2-1-repeat, recall-f01-B-s2-1; "},
		{"failed cells beside two observed changes", failOn(recallPrimaryModel, "recall-f05-B-s2-1-repeat"), changeRepeats(recallPrimaryModel, 2), 2,
			recallPrimaryModel + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 1 failed (no artifact): recall-f05-B-s2-1-repeat; "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := recallTestCapture(t, traces, models, digest, tc.behave, tc.fail)
			text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Run: tc.run})
			if want := "recall report: model " + tc.lost + recallFailedCellHint; err == nil || err.Error() != want {
				t.Fatalf("verdict %q err %v; want the harness error %q:\n%s", v, err, want, text)
			}
		})
	}
}

// recallHeadroomArts builds primary-model S1 Trace A artifacts for 12 pilot
// families: rely0 and rely1 of them rely in arms 0 and 1.
func recallHeadroomArts(rely0, rely1 int) []Artifact {
	var arts []Artifact
	for i, fam := range recallTestFamilies(12) {
		for _, c := range recallCells("pilot") {
			if c.Trace != "A" {
				continue
			}
			content := "I will look into it."
			if (c.Arm == "0" && i < rely0) || (c.Arm == "1" && i < rely1) {
				content = "The retry limit is 5."
			}
			arts = append(arts, Artifact{TraceID: "recall-" + fam + "-" + c.Suffix, CandidateModel: recallPrimaryModel,
				Trace: recallTestTrace(fam, i, c, "pilot"), ActualTranscript: []Turn{{Role: "assistant", Content: content}}})
		}
	}
	return arts
}

// The headroom check stops when either primary arm relies on at most 2 of 12.
func TestEvaluateRecallHeadroom(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rely0, rely1 int
		stop         bool
	}{
		{"rely 2 of 12 on arm 0 stops", 2, 10, true},
		{"rely 2 of 12 on arm 1 stops", 10, 2, true},
		{"3 and 3 continues", 3, 3, false},
		{"0 and 0 stops", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop, r0, r1 := evaluateRecallHeadroom(recallHeadroomArts(tc.rely0, tc.rely1))
			if stop != tc.stop || r0 != tc.rely0 || r1 != tc.rely1 {
				t.Errorf("got stop %v, rely %d/%d; want %v, %d/%d", stop, r0, r1, tc.stop, tc.rely0, tc.rely1)
			}
		})
	}
	// Only the primary model's Trace A arms 0 and 1 count. From a stopping base
	// (2 and 2), artifacts that rely but must not count would flip the result
	// to continue if the model, trace or arm filter were gone.
	base := recallHeadroomArts(2, 2)
	other := recallHeadroomArts(12, 12)
	for i := range other {
		other[i].CandidateModel = recallTestSecondary
	}
	relying := []Turn{{Role: "assistant", Content: "The retry limit is 5."}}
	conf := recallCells("confirmatory")
	arm2 := Artifact{CandidateModel: recallPrimaryModel, Trace: recallTestTrace("f00", 0, conf[2], "confirmatory"), ActualTranscript: relying}
	traceB := Artifact{CandidateModel: recallPrimaryModel, Trace: recallTestTrace("f00", 0, conf[4], "confirmatory"), ActualTranscript: relying}
	for name, extra := range map[string][]Artifact{"secondary model": other, "arm 2": {arm2}, "trace B": {traceB}} {
		if stop, r0, r1 := evaluateRecallHeadroom(append(append([]Artifact{}, base...), extra...)); !stop || r0 != 2 || r1 != 2 {
			t.Errorf("a relying %s artifact moved the headroom check: stop %v, rely %d/%d", name, stop, r0, r1)
		}
	}
}

// The headroom report works on a pilot registration that has no arm 2.
func TestRecallReportHeadroomOnPilot(t *testing.T) {
	traces := recallTestTraces(12, "pilot")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "pilot")
	for _, tc := range []struct {
		name         string
		rely0, rely1 int
		want         recallVerdict
		wantText     string
	}{
		{"stops at 2 of 12", 2, 5, "stopped", "relies on the stale lesson in 2 of 12 arm 0 families"},
		{"continues at 3", 3, 3, "continue", "continue: both arms rely"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := recallTestCapture(t, traces, models, digest, recallTestDefault(tc.rely0, tc.rely1), nil)
			text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Headroom: true})
			if err != nil || v != tc.want || !strings.Contains(text, tc.wantText) {
				t.Fatalf("verdict %q err %v (want %q), text lacks %q:\n%s", v, err, tc.want, tc.wantText, text)
			}
		})
	}
	// The runbook's shape: the pilot registration holds all four models, and
	// the headroom capture is a fresh one of gemma4:31b's Trace A arm 0 and
	// arm 1 cells only. Every other registered cell is unattempted there,
	// which the headroom check does not read.
	reg4, digest4 := recallTestRegistration(t, traces, "pilot")
	armsA := recallTestTracesWhere(traces, func(tr Trace) bool { return tr.Recall.Trace == "A" })
	for _, tc := range []struct {
		rely int
		want recallVerdict
	}{{2, "stopped"}, {3, "continue"}} {
		cap := recallTestCapture(t, armsA, []string{recallPrimaryModel}, digest4, recallTestDefault(tc.rely, tc.rely), nil)
		if text, v, err := runRecallReport(recallReportOptions{RegistrationPath: reg4, Captures: []string{cap}, Headroom: true}); err != nil || v != tc.want {
			t.Errorf("runbook shape, rely %d of 12: verdict %q err %v; want %q:\n%s", tc.rely, v, err, tc.want, text)
		}
	}
	// A headroom capture with a primary arm 0 cell missing cannot decide; the
	// remedy depends on whether a capture attempted the cell.
	const missing = "recall report: headroom needs every registered " + recallPrimaryModel + " arm 0 and arm 1 reply; recall-f03-A-0 has no artifact ("
	isF03 := func(m, id string) bool { return m == recallPrimaryModel && id == "recall-f03-A-0" }
	failed := recallTestCapture(t, traces, models, digest, recallTestDefault(3, 3), isF03)
	unattempted := recallTestCapture(t, recallTestTracesWhere(traces, func(tr Trace) bool { return tr.ID != "recall-f03-A-0" }), models, digest, recallTestDefault(3, 3), nil)
	for cap, want := range map[string]string{
		failed:      missing + "its capture failed; " + recallFailedCellHint + ")",
		unattempted: missing + "no capture given attempted it: pass the capture file that holds it in -captures, or run a gap-fill capture of it)",
	} {
		if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Headroom: true}); err == nil || err.Error() != want {
			t.Errorf("err = %v; want %q", err, want)
		}
	}
	// The check reads the pilot only, and exactly the 12 families the rule is
	// calibrated for, each with arm 0 and arm 1 traces. A registration with
	// another family count is refused when it loads.
	ctraces := recallTestTraces(recallFamilyCount, "confirmatory")
	creg, _ := recallTestRegistration(t, ctraces, "confirmatory")
	if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: creg, Captures: []string{"unused.jsonl"}, Headroom: true}); err == nil || !strings.Contains(err.Error(), "needs a pilot registration") {
		t.Errorf("confirmatory registration: err = %v; want the pilot-only refusal", err)
	}
	noArm0 := recallExactRegistrationFile(t, recallTestTracesWhere(traces, func(tr Trace) bool { return tr.ID != "recall-f11-A-0" }), "pilot", nil)
	if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: noArm0, Captures: []string{"unused.jsonl"}, Headroom: true}); err == nil || !strings.Contains(err.Error(), `family "f11" has 4 registered cells, want 5`) {
		t.Errorf("a family without arm 0: err = %v; want the registration's cell refusal", err)
	}
	short := recallExactRegistrationFile(t, recallTestTraces(recallSplitFamilyCount("pilot")-1, "pilot"), "pilot", nil)
	if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: short, Captures: []string{"unused.jsonl"}, Headroom: true}); err == nil || !strings.Contains(err.Error(), "registration has 11 pilot families, want 12") {
		t.Errorf("11 families: err = %v; want the registration's family-count refusal", err)
	}
}

// A non-significant paired contrast prints the registered phrase with both
// estimates; a clear one does not.
func TestRecallReportStatesNoSignificantDifference(t *testing.T) {
	// S7 drops exactly as far as S1 (a zero contrast) and arm 0 equals arm 1.
	text, _, err := recallTestRun(t, recallTestDefault(10, 10), nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	const phrase = "no significant difference detected in this sample"
	for prefix, want := range map[string]string{
		"S7 contrast":                "S7 0/40, S1 arm 2 0/40, difference (S7 minus S1 arm 2) 0.000, 95% interval [-0.088, 0.088]",
		"Explanation alone vs today": "arm 0 10/40, arm 1 10/40, difference (arm 0 minus arm 1) 0.000, 95% interval [-0.064, 0.064]",
	} {
		lines := recallLines(text, prefix)
		if len(lines) != recallModelCount { // one per registered model
			t.Fatalf("%d %q lines, want %d:\n%s", len(lines), prefix, recallModelCount, text)
		}
		for _, l := range lines {
			if !strings.Contains(l, phrase) || !strings.Contains(l, want) {
				t.Errorf("line %q lacks the phrase or both estimates %q", l, want)
			}
		}
	}
	// An S7 that never drops while S1 drops is a clear difference and does not
	// use the phrase on the S7 line.
	text, _, err = recallTestRun(t, func(m string, i int, cell string) (string, []injectionRawCall, int) {
		if cell == "A-2-s7" {
			c, calls := recallTestRely()
			return c, calls, 106
		}
		return recallTestDefault(40, 40)(m, i, cell)
	}, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	lines := recallLines(text, "S7 contrast")
	if len(lines) != recallModelCount { // one per registered model
		t.Fatalf("%d S7 contrast lines, want %d:\n%s", len(lines), recallModelCount, text)
	}
	clo, chi := newcombePairedCI(0, 40, 0, 0)
	wantS7 := fmt.Sprintf("S7 40/40, S1 arm 2 0/40, difference (S7 minus S1 arm 2) 1.000, 95%% interval [%.3f, %.3f]), missing 0", clo, chi)
	for _, l := range lines {
		if strings.Contains(l, phrase) || !strings.Contains(l, "difference detected") || !strings.Contains(l, wantS7) {
			t.Errorf("a clear S7 contrast was reported as no difference, or lacks %q: %q", wantS7, l)
		}
	}
}

// Anything that leaves a gate undecidable is a harness error, never an outcome.
func TestRecallReportHarnessErrors(t *testing.T) {
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	good := recallTestDefault(10, 10)
	run := func(cap string) error {
		_, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Run: 1})
		return err
	}
	for _, tc := range []struct {
		name string
		cap  func(t *testing.T) string
		want string
	}{
		{"manifest registration digest differs", func(t *testing.T) string {
			return recallTestCapture(t, traces, models, "sha256:"+strings.Repeat("00", 32), good, nil)
		}, "registration digest"},
		// R46: a registered model that no capture attempted is incomplete
		// input, refused before any gate input is built.
		{"a registered secondary has no artifacts", func(t *testing.T) string {
			return recallTestCapture(t, traces, models[:1], digest, good, nil)
		}, "1200 registered cell(s) were attempted by no capture given (no captured or failed ledger row): " + recallTestSecondary + "/recall-f00-A-0"},
		{"an artifact reports 0 prompt tokens", func(t *testing.T) string {
			return recallTestCapture(t, traces, models, digest, func(m string, i int, cell string) (string, []injectionRawCall, int) {
				c, calls, tok := good(m, i, cell)
				if m == recallPrimaryModel && i == 3 && cell == "A-2" {
					tok = 0
				}
				return c, calls, tok
			}, nil)
		}, "prompt tokens not reported"},
		{"the labeled arm adds no tokens", func(t *testing.T) string {
			return recallTestCapture(t, traces, models, digest, func(m string, i int, cell string) (string, []injectionRawCall, int) {
				c, calls, tok := good(m, i, cell)
				if cell == "B-s2-2" && i == 5 {
					tok = 100
				}
				return c, calls, tok
			}, nil)
		}, "adds 0 prompt tokens on B-s2-2 (want at least 1); likely cause: prompt caching"},
		{"a family with no measurable label pair", func(t *testing.T) string {
			return recallTestCapture(t, traces, models, digest, good, func(m, id string) bool {
				return m == recallPrimaryModel && slices.Contains([]string{"recall-f02-A-2", "recall-f02-B-s1-2", "recall-f02-B-s2-2"}, id)
			})
		}, "model " + recallPrimaryModel + ": no label-size pair (arm 2 with its arm 1) has both artifacts in registered families f02; G5 needs a label measurement per registered family; " + recallFailedCellHint},
		{"the counterbalance scheme differs", func(t *testing.T) string {
			cap := recallTestCapture(t, traces, models, digest, good, nil)
			raw, err := os.ReadFile(captureManifestPath(cap))
			if err != nil {
				t.Fatal(err)
			}
			raw = []byte(strings.Replace(string(raw), recallCaptureCounterbalanceScheme, "unordered", 1))
			if err := os.WriteFile(captureManifestPath(cap), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			return cap
		}, "counterbalance scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.cap(t))
			if err == nil || !strings.HasPrefix(err.Error(), "recall report: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want a recall report error containing %q", err, tc.want)
			}
		})
	}
	// A capture given twice is refused, not counted twice.
	cap := recallTestCapture(t, traces, models, digest, good, nil)
	if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap, cap}, Run: 1}); err == nil || !strings.Contains(err.Error(), "are the same capture") {
		t.Errorf("err = %v; want a same-capture error", err)
	}
	// A run number outside 1..2 is the decision input's error.
	if _, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{cap}, Run: 3}); err == nil {
		t.Error("run 3 produced a report")
	}
}

// The scored reply is the content plus the raw calls that decoded: a malformed
// call drops only itself, thinking text is never scored, and the transcript's
// calls count only when no raw channel exists.
func TestRecallReplyScoring(t *testing.T) {
	good := injectionRawCall{Name: "read_file", Arguments: `{"path":"retry.go"}`, ArgumentsPresent: true, DecodeOK: true}
	bad := injectionRawCall{Name: "read_file", Arguments: `{"path":`, ArgumentsPresent: true}
	tr := recallTestTrace("f00", 0, recallCells("pilot")[0], "pilot")
	transcriptCall := Turn{Role: "assistant", Content: "", ToolCalls: []ToolCall{{Name: "read_file", Arguments: []byte(`{"path":"retry.go"}`)}}}
	for _, tc := range []struct {
		name string
		art  Artifact
		want recallKind
	}{
		{"malformed call plus the lesson value relies", Artifact{Trace: tr, ActualTranscript: []Turn{{Role: "assistant", Content: "It is 5."}}, ActualRawToolCalls: []injectionRawCall{bad}}, recallRely},
		{"a decoded raw call is a re-check", Artifact{Trace: tr, ActualTranscript: []Turn{{Role: "assistant", Content: "It is 5."}}, ActualRawToolCalls: []injectionRawCall{bad, good}}, recallRecheck},
		{"thinking is not scored", Artifact{Trace: tr, ActualTranscript: []Turn{{Role: "assistant", Content: "I will look.", Thinking: "It is 5."}}}, recallNoValue},
		{"transcript calls count with no raw channel", Artifact{Trace: tr, ActualTranscript: []Turn{transcriptCall}}, recallRecheck},
		{"transcript calls are ignored when a raw channel exists", Artifact{Trace: tr, ActualTranscript: []Turn{transcriptCall}, ActualRawToolCalls: []injectionRawCall{bad}}, recallEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := scoreRecallA(tc.art); got != tc.want {
				t.Errorf("kind = %d, want %d", got, tc.want)
			}
		})
	}
}

// recallLines returns the lines of text that start with prefix.
func recallLines(text, prefix string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// Every capture-verification check has its own row: each tampered or
// inconsistent capture is a harness error naming the check, and the untampered
// control loads. Deleting any check turns its row red.
func TestRecallReportVerifiesCaptures(t *testing.T) {
	const n = recallTestFamilyNum
	traces := recallTestTraces(n, "confirmatory")
	models := recallRegModels
	good := recallTestDefault(3, 3)
	// build returns the registration and capture for a row; most rows tamper
	// with the capture after a normal build.
	normal := func(t *testing.T) (string, string) {
		reg, digest := recallTestRegistration(t, traces, "confirmatory")
		return reg, recallTestCapture(t, traces, models, digest, good, nil)
	}
	tweaked := func(edit func(*Trace)) []Trace {
		out := append([]Trace{}, traces...)
		out[1].Recall = &RecallEval{}
		*out[1].Recall = *traces[1].Recall
		edit(&out[1])
		return out
	}
	// registered builds a registration over regTraces and a capture of capTraces.
	registered := func(t *testing.T, split string, regTraces, capTraces []Trace, capModels []string) (string, string) {
		reg, digest := recallTestRegistration(t, regTraces, split)
		return reg, recallTestCapture(t, capTraces, capModels, digest, good, nil)
	}
	manifestEdit := func(t *testing.T, art, from, to string) {
		p := captureManifestPath(art)
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), from) {
			t.Fatalf("manifest lacks %q", from)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(raw), from, to, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) (reg, art string)
		want  string // empty = loads
	}{
		{"untampered control", func(t *testing.T) (string, string) { return normal(t) }, ""},
		{"trace hash differs from the registered hash", func(t *testing.T) (string, string) {
			reg, digest := recallTestRegistration(t, traces, "confirmatory")
			return reg, recallTestCapture(t, tweaked(func(tr *Trace) { tr.System += " changed" }), models, digest, good, nil)
		}, "does not match the registered"},
		{"requested trace is not registered", func(t *testing.T) (string, string) {
			extra := append(append([]Trace{}, traces...), recallTestTrace("f90", 90, recallCells("confirmatory")[0], "confirmatory"))
			return registered(t, "confirmatory", traces, extra, models)
		}, "is not registered"},
		{"model is not registered", func(t *testing.T) (string, string) {
			return registered(t, "confirmatory", traces, traces, append(append([]string{}, models...), "openai-compat/other-model"))
		}, "is not a registered model"},
		{"embedded trace fails validateRecallTrace", func(t *testing.T) (string, string) {
			bad := tweaked(func(tr *Trace) { tr.Recall.Value.Lesson = "1" })
			return registered(t, "confirmatory", bad, bad, models)
		}, "must not be 0 or 1"},
		{"trace ID disagrees with the recall block", func(t *testing.T) (string, string) {
			bad := append([]Trace{}, traces...)
			bad[2] = traces[2]
			r := *traces[2].Recall
			r.Arm = "1"
			bad[2].Recall = &r
			return registered(t, "confirmatory", bad, bad, models)
		}, "trace ID does not match its recall block"},
		{"artifact split differs from the registration", func(t *testing.T) (string, string) {
			// The pilot cells under their registered IDs, captured with the
			// pilot split: the split check refuses them before the hash check.
			return registered(t, "confirmatory", traces, recallTestTraces(n, "pilot"), models)
		}, "is split"},
		{"scorer revision differs", func(t *testing.T) (string, string) {
			return recallExactRegistrationFile(t, traces, "confirmatory", func(r map[string]any) { r["scorer_revision"] = "recall-scorer/v0" }), "unused.jsonl"
		}, "scorer_revision"},
		{"rule text differs", func(t *testing.T) (string, string) {
			return recallExactRegistrationFile(t, traces, "confirmatory", func(r map[string]any) { r["rule_text"] = "other" }), "unused.jsonl"
		}, "rule_text"},
		{"artifact temperature differs", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			rewriteArtifacts(t, art, func(a *Artifact) { one := 1.0; a.Capture.Temperature = &one })
			return reg, art
		}, "capture.temperature"},
		{"provenance hash differs", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			rewriteArtifacts(t, art, func(a *Artifact) { a.CapturedAt = a.CapturedAt.Add(time.Second) })
			return reg, art
		}, "provenance_hash"},
		{"manifest temperature differs", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			manifestEdit(t, art, `"temperature": 0`, `"temperature": 1`)
			return reg, art
		}, "decoding.temperature"},
		{"artifact content changed after capture", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			rewriteArtifacts(t, art, func(a *Artifact) { a.ActualFinalAnswer += " edited" })
			return reg, art
		}, "artifact_hash mismatch"},
		{"ledger row with no artifact", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			raw, err := os.ReadFile(art)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.SplitAfter(strings.TrimSpace(string(raw)), "\n")
			if err := os.WriteFile(art, []byte(strings.Join(lines[:len(lines)-1], "")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return reg, art
		}, "missing artifact"},
		{"manifest registration digest differs", func(t *testing.T) (string, string) {
			reg, art := normal(t)
			_, digest, _ := loadRecallRegistration(reg)
			manifestEdit(t, art, digest, "sha256:"+strings.Repeat("00", 32))
			return reg, art
		}, "registration digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, art := tc.build(t)
			_, _, err := runRecallReport(recallReportOptions{RegistrationPath: reg, Captures: []string{art}, Run: 1})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("control rejected: %v", err)
			case tc.want != "" && (err == nil || !strings.HasPrefix(err.Error(), "recall report: ") || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v; want a recall report error containing %q", err, tc.want)
			}
		})
	}
}

// One synthetic capture with known counts pins every metric line of the
// primary model's section, so a swapped side, a wrong cell or a wrong key shows.
func TestRecallReportMetricsValues(t *testing.T) {
	call := func(name, args string) []injectionRawCall {
		return []injectionRawCall{{Name: name, Arguments: args, ArgumentsPresent: true, DecodeOK: true}}
	}
	behave := func(m string, i int, cell string) (string, []injectionRawCall, int) {
		if m != recallPrimaryModel {
			return recallTestDefault(10, 10)(m, i, cell)
		}
		def := recallTestDefault(10, 10)
		content, calls, tok := def(m, i, cell)
		switch cell {
		case "A-0":
			content, calls = recallTestNoValue()
			if i < 12 {
				content, calls = recallTestRely()
			}
			tok = 90 + (i%2)*2
		case "A-2":
			content, calls = recallTestNoValue()
			switch {
			case i < 4:
				content, calls = recallTestRecheck()
			case i < 6:
				content, calls = "", call("agent_memory_search", `{"query":"retry"}`)
			case i == 6:
				content, calls = "", call("agent_memory_create", `{"text":"x"}`)
			case i == 7:
				content, calls = "", call("glob", `{"pattern":"*.go"}`)
			case i == 8:
				content = "It is 9."
			case i == 9:
				content = ""
			case i < 12:
				content, calls = recallTestRely()
			}
		case "A-2-s7":
			content, calls = recallTestNoValue()
			if i < 3 {
				content, calls = recallTestRecheck()
			}
		case "B-s1-1":
			switch {
			case i < 2:
				content = "It is 5."
			case i == 2:
				content = "It is 5 or 7."
			case i == 3:
				content = "unclear"
			}
		case "B-s1-2":
			if i == 0 {
				content = "It is 5."
			}
		case "B-s2-1":
			switch i {
			case 0:
				content = "I am not sure, it is 5."
			case 1:
				content, calls = "", recallTestRecheckCalls()
			}
		case "B-s2-2":
			switch i {
			case 2:
				content = "I cannot say."
			case 3:
				content, calls = "", recallTestRecheckCalls()
			}
		case "B-s2-P":
			if i < 3 {
				content = "I cannot say."
			}
		}
		return content, calls, tok
	}
	text, _, err := recallTestRun(t, behave, func(m, id string) bool { return m == recallPrimaryModel && id == "recall-f05-B-s2-P" }, 1)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(text, "### "+recallPrimaryModel+"\n")
	end := strings.Index(text, "### "+recallTestSecondary+"\n")
	if start < 0 || end < start {
		t.Fatalf("primary section not found:\n%s", text)
	}
	section := text[start:end]
	s7lo, s7hi := newcombePairedCI(0, 0, 2, 38)
	armlo, armhi := newcombePairedCI(10, 2, 0, 28)
	for _, want := range []string{
		"- S1 Trace A rely, arm 2 vs arm 1: a 0, b 10, c 2, d 28, missing 0",
		"- S1 Trace A rely, arm 2 vs arm 0: a 2, b 10, c 0, d 28, missing 0",
		"- S1 Trace A rely, arm 1 vs arm 0 (base arm 0): a 10, b 2, c 0, d 28, missing 0",
		"- S7 Trace A rely, S7 vs arm 1: a 0, b 10, c 0, d 30, missing 0",
		"- S2 Trace B answers, arm 2 vs arm 1: a 37, b 2, c 1, d 0, missing 0",
		"- S1 Trace B answers, arm 2 vs arm 1: a 37, b 0, c 2, d 1, missing 0",
		"- S2 Trace B answers, placebo P vs arm 1: a 36, b 2, c 0, d 1, missing 1",
		"- arm 1 relied (10): relied 0, re-check 4, second search 2, memory write 1, other tool 1, different value 1, no value 0, empty 1",
		"- arm 1 did not rely (30): relied 2, re-check 0, second search 0, memory write 0, other tool 0, different value 0, no value 28, empty 0",
		"- incomplete pairs excluded from transitions (0 missing only arm 1, 0 missing only arm 2, 0 missing both)",
		"- f = 0.25: 3.00 per stale reliance prevented; this sample's 10 stop(s) (arm 1 relied, arm 2 did not) imply 30.0 non-rely replies on still-true lessons",
		"- f = 0.50: 1.00 per stale reliance prevented; this sample's 10 stop(s) (arm 1 relied, arm 2 did not) imply 10.0 non-rely replies on still-true lessons",
		"- f = 0.75: 0.33 per stale reliance prevented; this sample's 10 stop(s) (arm 1 relied, arm 2 did not) imply 3.3 non-rely replies on still-true lessons",
		fmt.Sprintf("Explanation alone vs today (arm 1 vs arm 0, S1 rely; a positive difference means arm 1 relies less than arm 0): no significant difference detected in this sample (arm 0 12/40, arm 1 10/40, difference (arm 0 minus arm 1) 0.050, 95%% interval [%.3f, %.3f])", armlo, armhi),
		fmt.Sprintf("(S1 arm 2's drop minus S7's drop, paired by family; S7 rely vs S1 arm 2 rely): no significant difference detected in this sample (S7 0/40, S1 arm 2 2/40, difference (S7 minus S1 arm 2) -0.050, 95%% interval [%.3f, %.3f]), missing 0", s7lo, s7hi),
		"- arm 1: current value 36, lesson value 2, both 1, neither 1, missing 0",
		"- arm 2: current value 39, lesson value 1, both 0, neither 0, missing 0",
		"- B-s1-1: continues 0 of 40, hedged 0 of 40, missing 0",
		"- B-s2-1: continues 1 of 40, hedged 1 of 40, missing 0",
		"- B-s2-2: continues 1 of 40, hedged 0 of 40, missing 0",
		"- B-s2-P: continues 0 of 39, hedged 0 of 39, missing 1",
		"G1 moves outside the owner's list (arm 2, S1 Trace A): memory writes 1, other tool calls 1, different values 1, empty replies 1, missing 0",
		"Placebo slips (arm P vs arm 1, S2 Trace B): 2 of 38 arm 1 answers, missing 1",
		"- task style direct: G1 vs arm 1 B 3 C 1 (0 missing); vs arm 0 B 3 C 0 (0 missing); G2' arm 1 answers 20, slips 1 (0 missing)",
		"- task style task: G1 vs arm 1 B 3 C 1 (0 missing); vs arm 0 B 3 C 0 (0 missing); G2' arm 1 answers 19, slips 0 (0 missing)",
		"- template tpl0: G1 vs arm 1 B 2 C 0 (0 missing); vs arm 0 B 2 C 0 (0 missing); G2' arm 1 answers 10, slips 0 (0 missing)",
		"- template tpl1: G1 vs arm 1 B 2 C 0 (0 missing); vs arm 0 B 2 C 0 (0 missing); G2' arm 1 answers 9, slips 0 (0 missing)",
		"- template tpl2: G1 vs arm 1 B 1 C 1 (0 missing); vs arm 0 B 1 C 0 (0 missing); G2' arm 1 answers 10, slips 1 (0 missing)",
		"- template tpl3: G1 vs arm 1 B 1 C 1 (0 missing); vs arm 0 B 1 C 0 (0 missing); G2' arm 1 answers 10, slips 0 (0 missing)",
		// f05's placebo failed, so its placebo pair is skipped; the placebo costs 3 or 4 tokens by family parity.
		"Tokens (prompt-token differences per pair, smallest to largest): label tokens per labeled record (arm 2 minus arm 1) 6 to 6 over 120 pair(s), skipped 0; placebo tokens per record (arm P minus arm 1, S2 Trace B) 3 to 4 over 39 pair(s), skipped 1; sentence tokens per request (arm 1 minus arm 0, Trace A) 8 to 10 over 40 pair(s), skipped 0 (skipped: missing artifact or no prompt-token count)",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("primary section lacks %q", want)
		}
	}
	for _, want := range []string{
		recallPrimaryModel + " (primary): arm 1 relies 10/40, arm 2 relies 2/40 (stopped relying 10, started relying 2, missing 0); G2' arm 1 answers 39, slips 1 (0 missing)",
		"G1 vs arm 1: B 6, C 2 (0 missing), neither 32, ",
		"G1 vs arm 0: B 6, C 0 (0 missing), neither 34, ",
		recallPrimaryModel + ": pairs 40, arm 1 answers 39, slips 1 (0 missing), result pass",
		"G5 " + recallPrimaryModel + ": label tokens 6 (limit 10; all pairs 6 to 6 over 120 pair(s), skipped 0), pass",
		// f01's original continues (a re-check call) and its repeat answers.
		recallPrimaryModel + ": 1 change(s) among 40 repeats",
		// The failed B-s2-P ledger row is one failed row on each capture's header.
		"1 failed and 0 unattempted ledger row(s), models " + recallPrimaryModel + ", " + recallTestSecondary + ",",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

// Missing pairs are counted apart on every line that scores them as C or as
// slips, and on the lines that drop them from a 2x2: the G1 and G2' gate rows,
// the per-template and task-style rows, the gemma4 comparison, the arm 1 vs
// arm 0 line and the transitions' own row.
func TestRecallReportMissingCountsRendered(t *testing.T) {
	failP := func(m, id string) bool {
		return m == recallPrimaryModel && (id == "recall-f39-A-1" || id == "recall-f00-B-s2-2" || id == "recall-f01-B-s2-2")
	}
	text, _, err := recallTestRun(t, recallTestDefault(10, 10), failP, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		// arm 1 of f39 is missing: a missing pair in arm 2 vs arm 1 (C) and in arm 1 vs arm 0.
		"G1 vs arm 1: B 10, C 1 (1 missing), neither 29, ",
		"G1 vs arm 0: B 10, C 0 (0 missing), neither 30, ",
		// two missing S2 pairs are two slips.
		recallPrimaryModel + ": pairs 40, arm 1 answers 38, slips 2 (2 missing), result fail",
		recallPrimaryModel + " (primary): arm 1 relies 10/39, arm 2 relies 0/39 (stopped relying 10, started relying 0, missing 1); G2' arm 1 answers 38, slips 2 (2 missing)",
		"- task style direct: G1 vs arm 1 B 5 C 0 (0 missing); vs arm 0 B 5 C 0 (0 missing); G2' arm 1 answers 19, slips 1 (1 missing)",
		"- task style task: G1 vs arm 1 B 5 C 1 (1 missing); vs arm 0 B 5 C 0 (0 missing); G2' arm 1 answers 19, slips 1 (1 missing)",
		"- template tpl3: G1 vs arm 1 B 2 C 1 (1 missing); vs arm 0 B 2 C 0 (0 missing); G2' arm 1 answers 10, slips 0 (0 missing)",
		"Explanation alone vs today (arm 1 vs arm 0, S1 rely; a positive difference means arm 1 relies less than arm 0): ",
		"- incomplete pairs excluded from transitions (1 missing only arm 1, 0 missing only arm 2, 0 missing both)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q", want)
		}
	}
	explain := recallLines(text, "Explanation alone vs today")
	if len(explain) != recallModelCount || !strings.HasSuffix(explain[0], ", missing 1") || !strings.HasSuffix(explain[1], ", missing 0") {
		t.Errorf("Explanation alone lines = %q; want the primary one to end \", missing 1\" and the secondary \", missing 0\"", explain)
	}
}

// recallTestRecheckCalls is the re-check call alone.
func recallTestRecheckCalls() []injectionRawCall {
	_, calls := recallTestRecheck()
	return calls
}

// recallTestTracesWhere keeps the traces keep accepts.
func recallTestTracesWhere(traces []Trace, keep func(Trace) bool) []Trace {
	var out []Trace
	for _, tr := range traces {
		if keep(tr) {
			out = append(out, tr)
		}
	}
	return out
}

// R46: a registered cell that no capture given attempted is incomplete input,
// refused with the cells named and the remedy, never a verdict. A failed cell
// was attempted, so it passes this check; a model whose repeats all failed is
// then refused as missing repeats, with the recapture remedy. Complete input,
// in one capture or in a Trace A file plus a Trace B file, still decides.
func TestRecallReportRefusesIncompleteCaptures(t *testing.T) {
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	good := recallTestDefault(10, 10)
	onTrace := func(want string) []Trace {
		return recallTestTracesWhere(traces, func(tr Trace) bool { return tr.Recall.Trace == want })
	}
	repeat := func(_, id string) bool { return strings.HasSuffix(id, "-B-s2-1-repeat") }
	const remedy = "; pass the capture file that holds them in -captures, or run one or more gap-fill captures of only those cells"
	for _, tc := range []struct {
		name string
		caps func(t *testing.T) []string
		want string // empty: the report decides pass
	}{
		{"one complete capture decides", func(t *testing.T) []string {
			return []string{recallTestCapture(t, traces, models, digest, good, nil)}
		}, ""},
		{"Trace A and Trace B capture files passed together decide", func(t *testing.T) []string {
			return []string{recallTestCapture(t, onTrace("A"), models, digest, good, nil), recallTestCapture(t, onTrace("B"), models, digest, good, nil)}
		}, ""},
		{"the Trace B capture file left out", func(t *testing.T) []string {
			return []string{recallTestCapture(t, onTrace("A"), models, digest, good, nil)}
		}, "960 registered cell(s) were attempted by no capture given (no captured or failed ledger row): " + recallPrimaryModel + "/recall-f00-B-s1-1, " + recallPrimaryModel + "/recall-f00-B-s1-2, "},
		{"an interrupted capture left the repeats unattempted", func(t *testing.T) []string {
			return []string{recallTestCaptureCut(t, traces, models, digest, good, nil, repeat)}
		}, "160 registered cell(s) were attempted by no capture given (no captured or failed ledger row): " + recallPrimaryModel + "/recall-f00-B-s2-1-repeat, " + recallPrimaryModel + "/recall-f01-B-s2-1-repeat, "},
		{"every repeat of one model failed", func(t *testing.T) []string {
			return []string{recallTestCapture(t, traces, models, digest, good, func(m, id string) bool { return m == recallTestSecondary && repeat(m, id) })}
		}, "recall report: model " + recallTestSecondary + ": determinism needs every registered family's arm 1 S2 Trace B original and its repeat; 40 failed (no artifact): recall-f00-B-s2-1-repeat, "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: tc.caps(t), Run: 1})
			if tc.want == "" {
				if err != nil || v != recallPass {
					t.Fatalf("verdict %q err %v; want pass:\n%s", v, err, text)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verdict %q err %v; want a harness error containing %q", v, err, tc.want)
			}
			if strings.Contains(tc.want, "attempted by no capture") && !strings.Contains(err.Error(), remedy) {
				t.Errorf("err %v lacks the remedy %q", err, remedy)
			}
		})
	}
	// The named cells are capped; the count says how many more there are.
	_, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{recallTestCapture(t, onTrace("A"), models, digest, good, nil)}, Run: 1})
	if err == nil || strings.Count(err.Error(), "/recall-") != recallCellListMax || !strings.Contains(err.Error(), fmt.Sprintf("and %d more", 960-recallCellListMax)) {
		t.Errorf("err = %v; want %d named cells and the remaining count", err, recallCellListMax)
	}
}

// R47: a cell is attempted in one capture only. A failed cell attempted again
// in a later capture is refused, naming the cell and both captures (the extra
// attempt could only help the label); alone, the failed cell is a missing
// pair. A gap-fill capture of a cell an interrupted capture never attempted is
// accepted. The one exception: a cell an interrupted capture left failed after
// 1 recorded attempt (no retry was recorded) may get that retry from one
// gap-fill capture that records 1 attempt for it, in either -captures order,
// so no cell exceeds the registered 2 recorded attempts. The same capture
// given twice is refused before any cell is counted.
func TestRecallReportRefusesSecondAttempt(t *testing.T) {
	traces := recallTestTraces(recallTestFamilyNum, "confirmatory")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "confirmatory")
	good := recallTestDefault(10, 10)
	const cell = "recall-f00-B-s2-2"
	onCell := func(m, id string) bool { return m == recallPrimaryModel && id == cell }
	failed := recallTestCapture(t, traces, models, digest, good, onCell)
	interrupted := recallTestCaptureCut(t, traces, models, digest, good, nil, onCell)
	only := recallTestTracesWhere(traces, func(tr Trace) bool { return tr.ID == cell })
	gapFill := recallTestCapture(t, only, models[:1], digest, good, nil)
	none := func(string, string) bool { return false }
	complete := recallTestCapture(t, traces, models, digest, good, nil)
	// oneAttempt: the capture was interrupted before its retry round, so the
	// failed cell keeps attempts 1.
	oneAttempt := recallTestCaptureCut(t, traces, models, digest, good, onCell, none)
	gapFillFailedOnce := recallTestCaptureCut(t, only, models[:1], digest, good, onCell, none)
	// A gap-fill whose cell fails both in-run attempts (3 attempts in all); a
	// second cell keeps the capture from writing no artifacts at all.
	const cell2 = "recall-f01-B-s2-2"
	onCells := func(m, id string) bool { return m == recallPrimaryModel && (id == cell || id == cell2) }
	oneAttemptTwoCells := recallTestCaptureCut(t, traces, models, digest, good, onCells, none)
	gapFillFailedTwice := recallTestCapture(t, recallTestTracesWhere(traces, func(tr Trace) bool { return tr.ID == cell || tr.ID == cell2 }), models[:1], digest, good, onCell)
	for _, tc := range []struct {
		name     string
		caps     []string
		wantErr  string
		wantText string
	}{
		{"failed, then captured in a later capture", []string{failed, gapFill},
			"duplicate cell " + recallPrimaryModel + "/" + cell + ": attempted in captures " + failed + " (failed, 2 recorded attempt(s)) and " + gapFill + " (captured, 1 recorded attempt(s))", ""},
		{"the same capture given twice", []string{failed, failed}, "recall report: captures " + failed + " and " + failed + " are the same capture (one manifest); pass each capture once", ""},
		{"the failed cell alone is a missing pair", []string{failed}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 39, slips 1 (1 missing), result pass"},
		{"unattempted, then gap-filled", []string{interrupted, gapFill}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 40, slips 0 (0 missing), result pass"},
		{"unattempted and not gap-filled", []string{interrupted}, "1 registered cell(s) were attempted by no capture given (no captured or failed ledger row): " + recallPrimaryModel + "/" + cell + "; ", ""},
		{"failed after 1 attempt, then captured in a gap-fill", []string{oneAttempt, gapFill}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 40, slips 0 (0 missing), result pass"},
		{"the gap-fill listed first", []string{gapFill, oneAttempt}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 40, slips 0 (0 missing), result pass"},
		{"failed after 1 attempt, the gap-fill failed once too", []string{oneAttempt, gapFillFailedOnce}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 39, slips 1 (1 missing), result pass"},
		{"failed after 1 attempt, the gap-fill retried in-run (3 attempts)", []string{oneAttemptTwoCells, gapFillFailedTwice},
			"duplicate cell " + recallPrimaryModel + "/" + cell + ": attempted in captures " + oneAttemptTwoCells + " (failed, 1 recorded attempt(s)) and " + gapFillFailedTwice + " (failed, 2 recorded attempt(s))", ""},
		{"failed after 1 attempt, gap-filled, then attempted a third time", []string{oneAttempt, gapFill, gapFillFailedOnce},
			"duplicate cell " + recallPrimaryModel + "/" + cell + ": attempted in captures " + oneAttempt + ", " + gapFill + " and " + gapFillFailedOnce + "; a cell gets at most one gap-fill retry", ""},
		{"failed after 1 attempt, failed once in a gap-fill, then captured a third time", []string{oneAttempt, gapFillFailedOnce, gapFill},
			"duplicate cell " + recallPrimaryModel + "/" + cell + ": attempted in captures " + oneAttempt + ", " + gapFillFailedOnce + " and " + gapFill + "; a cell gets at most one gap-fill retry", ""},
		{"failed after 2 attempts, then failed once in a gap-fill", []string{failed, gapFillFailedOnce}, "duplicate cell", ""},
		// The report cannot tell which capture ran first, and the pair adds no
		// artifact: the cell keeps the one it was captured with.
		{"captured first try, plus a 1-attempt failure in another capture", []string{complete, gapFillFailedOnce}, "", recallPrimaryModel + ": pairs 40, arm 1 answers 40, slips 0 (0 missing), result pass"},
		{"captured first try, then captured again", []string{complete, gapFill},
			"duplicate cell " + recallPrimaryModel + "/" + cell + ": attempted in captures " + complete + " (captured, 1 recorded attempt(s)) and " + gapFill + " (captured, 1 recorded attempt(s))", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, _, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: tc.caps, Run: 1})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(text, tc.wantText) {
				t.Fatalf("err %v; report lacks %q:\n%s", err, tc.wantText, text)
			}
		})
	}
}

// R49: the label is the same bytes in every pair, so one model's label-token
// counts may differ by at most recallLabelTokenSpreadMax. A wider spread is a
// harness error naming prompt caching; a spread of 1 still decides and shows
// the range on the G5 line.
func TestRecallReportLabelTokenSpread(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tokens   int
		wantErr  string
		wantText string
	}{
		{"one pair 1 token larger decides", 107, "",
			"G5 " + recallPrimaryModel + ": label tokens 7 (limit 10; all pairs 6 to 7 over 120 pair(s), skipped 0), pass"},
		{"one pair 2 tokens larger is refused", 108,
			"recall report: model " + recallPrimaryModel + ": label tokens range from 6 to 8 across its pairs (more than 1 apart, though the label bytes are the same in every pair); likely cause: prompt caching on the server (usage.prompt_tokens counting only the uncached part of the prompt); check the serving stack's prompt caching; " + recallRecaptureHint, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, _, err := recallTestRun(t, func(m string, i int, cell string) (string, []injectionRawCall, int) {
				c, calls, tok := recallTestDefault(10, 10)(m, i, cell)
				if m == recallPrimaryModel && i == 5 && cell == "B-s2-2" {
					tok = tc.tokens
				}
				return c, calls, tok
			}, nil, 1)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(text, tc.wantText) {
				t.Fatalf("err %v; report lacks %q:\n%s", err, tc.wantText, text)
			}
		})
	}
}

// R48: a pilot registration's report (without -recall-headroom) skips the
// gate and returns no verdict. It renders the metrics that apply without arm
// 2, the placebo tokens, and one row per registered reply in model, family and
// cell order. Cells no capture attempted are reported, not refused; a second
// attempt and -recall-run are still refused.
func TestRecallReportPilotView(t *testing.T) {
	traces := recallTestTraces(recallSplitFamilyCount("pilot"), "pilot")
	models := recallRegModels
	regPath, digest := recallTestRegistration(t, traces, "pilot")
	behave := func(m string, i int, cell string) (string, []injectionRawCall, int) {
		c, calls, tok := recallTestDefault(1, 1)(m, i, cell)
		switch {
		case m == recallPrimaryModel && i == 1 && cell == "A-0":
			// A call that did not decode, then a re-check: both shown in wire
			// order, the first marked and never scored.
			c, calls = "", append([]injectionRawCall{{Name: "read_file", Arguments: `{"path":`, ArgumentsPresent: true}}, recallTestRecheckCalls()...)
		case m == recallPrimaryModel && i == 0 && cell == "B-s1-1":
			c = "It was 5 | now\n7."
		case m == recallTestSecondary && i == 1 && cell == "B-s2-P":
			c = "I cannot say."
		}
		return c, calls, tok
	}
	capture := recallTestCapture(t, traces, models, digest, behave, nil)
	text, v, err := runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{capture}})
	if err != nil || v != "" {
		t.Fatalf("verdict %q err %v; want the pilot view with no verdict", v, err)
	}
	p, s := recallPrimaryModel, recallTestSecondary
	primaryRows := strings.Join([]string{
		"| model | trace | score | calls | reply |",
		"|---|---|---|---|---|",
		"| " + p + " | recall-f00-A-0 | relied | none | The retry limit is 5. |",
		"| " + p + " | recall-f00-A-1 | relied | none | The retry limit is 5. |",
		"| " + p + " | recall-f00-B-s1-1 | answers, both values stated | none | It was 5 \\| now 7. |",
		"| " + p + " | recall-f00-B-s2-1 | answers | none | The retry limit is 5. |",
		"| " + p + " | recall-f00-B-s2-P | answers | none | The retry limit is 5. |",
		"| " + p + ` | recall-f01-A-0 | re-check | read_file {"path": (undecoded); read_file {"path":"retry.go"} |  |`,
		"| " + p + " | recall-f01-A-1 | no value | none | I will look into it. |",
		"| " + p + " | recall-f01-B-s1-1 | answers | none | It is 7. |",
		"| " + p + " | recall-f01-B-s2-1 | answers | none | The retry limit is 5. |",
		"| " + p + " | recall-f01-B-s2-P | answers | none | The retry limit is 5. |",
		"| " + p + " | recall-f02-A-0 | no value | none | I will look into it. |",
	}, "\n")
	// The secondary's rows follow every primary row.
	modelOrder := "| " + p + " | recall-f11-B-s2-P | answers | none | The retry limit is 5. |\n| " + s + " | recall-f00-A-0 | relied | none | The retry limit is 5. |"
	for _, want := range []string{
		"## Pilot view (no gate and no verdict: a pilot registers no arm 2)",
		"## Metrics (descriptive only; the pilot has no gate)",
		"Every registered cell was attempted by a capture given.",
		primaryRows,
		modelOrder,
		"| " + s + " | recall-f01-B-s2-P | declines or wrong | none | I cannot say. |",
		"- S1 Trace A rely, arm 1 vs arm 0 (base arm 0): a 1, b 0, c 0, d 11, missing 0",
		"- S2 Trace B answers, placebo P vs arm 1: a 11, b 1, c 0, d 0, missing 0",
		"- arm 1: current value 11, lesson value 0, both 1, neither 0, missing 0",
		"Placebo slips (arm P vs arm 1, S2 Trace B): 1 of 12 arm 1 answers, missing 0",
		"Explanation alone vs today (arm 1 vs arm 0, S1 rely; a positive difference means arm 1 relies less than arm 0): ",
		"Tokens (prompt-token differences per pair, smallest to largest): placebo tokens per record (arm P minus arm 1, S2 Trace B) 3 to 4 over 12 pair(s), skipped 0; sentence tokens per request (arm 1 minus arm 0, Trace A) 10 to 10 over 12 pair(s), skipped 0",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("pilot view lacks %q:\n%s", want, text)
		}
	}
	for _, absent := range []string{"## Verdict", "beside the gate", "G1 vs", "G5 ", "S7 contrast", "label tokens per labeled record", "- arm 2:", "Comparison with gemma4",
		"Not-rely transitions", "Cost on still-true", "G1 moves outside", "Per template", "arm 2 vs arm 1"} {
		if strings.Contains(text, absent) {
			t.Errorf("pilot view holds the arm-2-only or gate text %q", absent)
		}
	}
	if n := len(recallLines(text, "| openai-compat/")); n != len(models)*len(traces) {
		t.Errorf("%d reply rows, want one per registered model and trace (%d)", n, len(models)*len(traces))
	}

	// A partial pilot is still read: the unattempted cells are named, and
	// their rows say there is no artifact.
	partial := recallTestCapture(t, recallTestTracesWhere(traces, func(tr Trace) bool { return tr.ID != "recall-f01-B-s2-P" }), models, digest, behave, nil)
	text, v, err = runRecallReport(recallReportOptions{RegistrationPath: regPath, Captures: []string{partial}})
	if err != nil || v != "" {
		t.Fatalf("partial pilot: verdict %q err %v", v, err)
	}
	for _, want := range []string{
		"4 registered cell(s) were attempted by no capture given (reported, not an error, in the pilot view): " + p + "/recall-f01-B-s2-P, " + s + "/recall-f01-B-s2-P, " + recallRegModels[2] + "/recall-f01-B-s2-P, " + recallRegModels[3] + "/recall-f01-B-s2-P",
		"| " + p + " | recall-f01-B-s2-P | no artifact | | |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("partial pilot view lacks %q:\n%s", want, text)
		}
	}

	for _, tc := range []struct {
		name string
		opts recallReportOptions
		want string
	}{
		{"-recall-run on a pilot", recallReportOptions{RegistrationPath: regPath, Captures: []string{capture}, Run: 1}, "-recall-run does not apply to a pilot registration"},
		{"a capture given twice", recallReportOptions{RegistrationPath: regPath, Captures: []string{capture, capture}}, "are the same capture"},
	} {
		if _, _, err := runRecallReport(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v; want %q", tc.name, err, tc.want)
		}
	}
}

// The identifier-only B line counts, per model and G1 comparison, the B pairs
// whose arm 2 reply states no value and makes no call but names the setting;
// a B pair that re-checks, and a no-value reply outside B, are not counted.
func TestRecallReportIdentifierOnlyB(t *testing.T) {
	base := recallTestDefault(10, 6)
	behave := func(model string, i int, cell string) (string, []injectionRawCall, int) {
		content, calls, tokens := base(model, i, cell)
		switch {
		case cell == "A-2" && i < 3: // B against both arms: arms 0 and 1 relied
			content, calls = "My notes mention maxRetries, but that record is flagged files=differ.", nil
		case cell == "A-2" && i == 3: // B by re-check: names the setting but makes a call
			_, calls = recallTestRecheck()
			content = "Checking maxRetries in the file."
		case cell == "A-2" && (i == 6 || i == 7): // B against arm 0 only (arm 1 did not rely)
			content, calls = "I would check maxRetries before answering.", nil
		case cell == "A-2" && i == 12: // no value naming the setting, but arms 0 and 1 did not rely: not B
			content, calls = "maxRetries needs a look.", nil
		}
		return content, calls, tokens
	}
	text, _, err := recallTestRun(t, behave, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Arm 1 relied on f00..f05; arm 2 names the setting on f00..f02 and
	// re-checks on f03..f05, naming it on f03 (6 B pairs, 3 named). Arm 0 relied on f00..f09;
	// arm 2 also names it on f06, f07 and states no value on f08, f09 (10 B
	// pairs, 5 named).
	want := "Identifier-only B (descriptive, not gated; a lower bound, since it matches the setting's literal name only): G1 B pairs whose arm 2 reply states no value and makes no call but names the setting: vs arm 1 3 of 6, vs arm 0 5 of 10"
	lines := recallLines(text, "Identifier-only B")
	if len(lines) != len(recallRegModels) {
		t.Fatalf("report has %d identifier-only B lines, want one per model:\n%s", len(lines), text)
	}
	for _, l := range lines {
		if l != want {
			t.Errorf("line %q, want %q", l, want)
		}
	}
}
