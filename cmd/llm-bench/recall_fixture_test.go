package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateRecallRegistration = flag.Bool("update-recall-registration", false, "rebuild testdata/recall-label/<split> from testdata/recall-label/cases.json for every split with families and rewrite that split's registration from the rebuild")

// recallFixtureDir holds the committed #864 Stage 0 fixture: cases.json, the
// traces built from each split under <split>/, and each split's registration.
const recallFixtureDir = "testdata/recall-label"

// recallFreezeNote qualifies every regeneration remedy: once a registration is
// frozen (the confirmatory run has started), regenerating it would rewrite
// the registered measurement, so the only remedy is to revert the change.
const recallFreezeNote = " (before the freeze only; after it, revert the change)"

// recallFixtureSplits names each split's committed registration file.
var recallFixtureSplits = []struct{ split, registration string }{
	{"pilot", "registration-pilot.json"},
	{"confirmatory", "registration.json"},
}

// recallLineupDecoding is the registered decoding of every lineup model
// (recallStageModels): the #864 Global Constraints.
func recallLineupDecoding() registeredDecoding {
	temp, think := 0.0, false
	return registeredDecoding{Temperature: &temp, NumPredict: recallNumPredict, Think: &think, ThinkMode: "toggle",
		ServingStack: "llama.cpp via llama-swap 127.0.0.1:8090"}
}

// recallRegistrationJSON is split's registration for the traces in dir, built
// from cases: the cases digest, the compiled rule text, scorer revision and
// primary, the lineup and its decoding, and one id and canonicalTraceHash row
// per trace in family then cell order.
func recallRegistrationJSON(t *testing.T, cases recallCases, split, dir string) []byte {
	t.Helper()
	digest, err := recallCasesDigest(cases, split)
	if err != nil {
		t.Fatal(err)
	}
	traces, _ := recallReadBuiltTraces(t, dir)
	type row struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	rows := []row{}
	for _, f := range cases.Families {
		if f.Split != split {
			continue
		}
		for _, c := range recallCells(split) {
			id := recallTraceID(f.ID, c.Suffix)
			tr, ok := traces[id]
			if !ok {
				t.Fatalf("%s holds no trace %s; regenerate with %s", dir, id, recallFixtureRegenCommand+recallFreezeNote)
			}
			rows = append(rows, row{ID: id, SHA256: canonicalTraceHash(tr)})
		}
	}
	dec := map[string]registeredDecoding{}
	for _, m := range recallStageModels {
		dec[m] = recallLineupDecoding()
	}
	reg := struct {
		Schema         string                        `json:"schema"`
		Split          string                        `json:"split"`
		CasesSHA256    string                        `json:"cases_sha256"`
		ScorerRevision string                        `json:"scorer_revision"`
		RuleText       string                        `json:"rule_text"`
		Primary        string                        `json:"primary"`
		Models         []string                      `json:"models"`
		Decoding       map[string]registeredDecoding `json:"decoding"`
		Traces         []row                         `json:"traces"`
	}{recallRegistrationSchema, split, digest, recallScorerRevision, recallDecisionRuleText, recallPrimaryModel, recallStageModels[:], dec, rows}
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

// recallLineCount is the number of lines a read of s shows.
func recallLineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// recallHalf reports whether c of n is half, rounded either way: |2c - n| <= 1.
func recallHalf(c, n int) bool { return 2*c-n >= -1 && 2*c-n <= 1 }

// recallAuthoringProblems lists how split's families break the authoring
// rulings the builder does not enforce (#864 rulings R32, R54, R56):
//   - every template holds 3 (pilot) or 10 (confirmatory) families, and each
//     rank's count per template is the closest integer to a third (|3c - n| <= 2;
//     ranks 1, 2 and 3 once each in the pilot);
//   - number values are integers 2..20 other than 3, 8 and 10, above the line
//     count of the S1 and S2 files the model may read;
//   - identifier lessons hold no "so ..." clause restating what the value means;
//   - each distractor names exactly one other file, distinct from the other
//     distractor's;
//   - s7_distractor is 1 in half of each template, task style and value type,
//     alternates within each template in the pilot, and in the confirmatory
//     split is crossed with rank (1 in half of each rank's families).
func recallAuthoringProblems(cases recallCases, split string) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	want := map[string]int{"pilot": 3, "confirmatory": 10}[split]
	tmpls := map[string]recallTemplate{}
	for _, tm := range cases.Templates {
		tmpls[tm.ID] = tm
	}
	byTemplate := map[string][]recallFamily{}
	s7 := map[string][2]int{}  // template, task style or value type -> (s7 == 1 count, families)
	s7Rank := map[int][2]int{} // rank -> (s7 == 1 count, families)
	for _, f := range cases.Families {
		if f.Split != split {
			continue
		}
		byTemplate[f.Template] = append(byTemplate[f.Template], f)
		tm := tmpls[f.Template]
		for _, k := range []string{"template " + tm.ID, tm.TaskStyle, tm.ValueType} {
			s7[k] = [2]int{s7[k][0] + f.s7(), s7[k][1] + 1}
		}
		s7Rank[f.rank()] = [2]int{s7Rank[f.rank()][0] + f.s7(), s7Rank[f.rank()][1] + 1}
		lines := max(recallLineCount(f.FileS1), recallLineCount(f.FileS2))
		if f.Value.Type == "number" {
			for _, v := range []string{f.Value.Lesson, f.Value.Changed} {
				if n, err := strconv.Atoi(v); err != nil || n < 2 || n > 20 || n == 3 || n == 8 || n == 10 || n <= lines {
					fail("family %s: number value %q is not an integer 2..20 other than 3, 8 and 10 above the file's %d lines", f.ID, v, lines)
				}
			}
		} else if strings.Contains(f.Lesson, " so ") {
			fail("family %s: identifier lesson %q restates what the value means (a \"so ...\" clause)", f.ID, f.Lesson)
		}
		var named [2]string
		for i, d := range f.Distractors {
			var hits []string
			for p := range f.OtherFiles {
				if strings.Contains(d, p) {
					hits = append(hits, p)
				}
			}
			if len(hits) != 1 {
				fail("family %s: distractor %d names other files %v, want exactly one", f.ID, i, hits)
				continue
			}
			named[i] = hits[0]
		}
		if named[0] != "" && named[0] == named[1] {
			fail("family %s: both distractors name %s, want distinct files", f.ID, named[0])
		}
	}
	if len(byTemplate) != len(cases.Templates) || len(cases.Templates) != 4 {
		fail("%s split covers %d of %d templates, want all 4", split, len(byTemplate), len(cases.Templates))
	}
	for id, fams := range byTemplate {
		if len(fams) != want {
			fail("template %s: %d %s families, want %d", id, len(fams), split, want)
		}
		ranks := map[int]int{}
		for i, f := range fams {
			ranks[f.rank()]++
			if split == "pilot" && i > 0 && f.s7() == fams[i-1].s7() {
				fail("template %s: s7_distractor does not alternate at family %s", id, f.ID)
			}
		}
		for r := 1; r <= 3; r++ {
			if d := 3*ranks[r] - len(fams); d < -2 || d > 2 {
				fail("template %s: rank %d in %d of %d families, want the closest integer to a third", id, r, ranks[r], len(fams))
			}
		}
	}
	for k, c := range s7 {
		if !recallHalf(c[0], c[1]) {
			fail("%s families: s7_distractor 1 in %d of %d, want half", k, c[0], c[1])
		}
	}
	if split == "confirmatory" {
		for r, c := range s7Rank {
			if !recallHalf(c[0], c[1]) {
				fail("rank %d families: s7_distractor 1 in %d of %d, want half (crossed with rank)", r, c[0], c[1])
			}
		}
	}
	slices.Sort(problems)
	return problems
}

// TestRecallRegistrationsLoad pins each committed recall registration to the
// fixture: for every split, the split's cases must keep the authoring rulings,
// the registration must load, be exactly what the split's traces register
// (cases digest, rule text, scorer revision, lineup decoding, trace hashes)
// and bind the committed traces for every registered model, and the trace
// directory must hold exactly the registered traces. A split with no families
// and no registration is skipped. With -update-recall-registration it first
// rebuilds each split's traces (pruning stale ones) and rewrites its
// registration, unless the authoring checks fail.
//
// Only a run without -race also rebuilds every trace from cases.json and
// requires the committed traces to equal that build byte for byte (the check
// that catches a builder, prompt or tool-text change). Under -race, which is
// all CI runs, the rebuild (about 150 s for a confirmatory split) is skipped:
// the registration is checked against the committed trace files themselves
// (canonicalTraceHash of each file) and the directory's file set, so CI
// catches a registration and corpus that disagree but not a corpus that has
// drifted from what cases.json now builds. Run the test without -race before
// a recall fixture PR.
func TestRecallRegistrationsLoad(t *testing.T) {
	t.Parallel()
	cases, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range recallFixtureSplits {
		t.Run(sp.split, func(t *testing.T) {
			t.Parallel()
			testRecallFixtureSplit(t, cases, sp.split, sp.registration)
		})
	}
}

func testRecallFixtureSplit(t *testing.T, cases recallCases, split, registration string) {
	casesPath := filepath.Join(recallFixtureDir, "cases.json")
	committedDir := filepath.Join(recallFixtureDir, split)
	regPath := filepath.Join(recallFixtureDir, registration)
	has := slices.ContainsFunc(cases.Families, func(f recallFamily) bool { return f.Split == split })
	_, statErr := os.Stat(regPath)
	switch exists := statErr == nil; {
	case !has && exists:
		t.Fatalf("%s exists but cases.json has no %s families; remove it or add the families and run %s", regPath, split, recallFixtureRegenCommand+recallFreezeNote)
	case !has:
		t.Skipf("cases.json has no %s families and %s does not exist: nothing is registered", split, regPath)
	case !exists && !*updateRecallRegistration:
		t.Fatalf("cases.json has %s families but %s does not exist; generate it with %s", split, regPath, recallFixtureRegenCommand+recallFreezeNote)
	}
	for _, p := range recallAuthoringProblems(cases, split) {
		t.Errorf("%s; fix cases.json, then run %s", p, recallFixtureRegenCommand+recallFreezeNote)
	}
	if *updateRecallRegistration && t.Failed() {
		t.Fatal("the cases break the authoring rules above; not regenerating: fix cases.json first")
	}
	digest, err := recallCasesDigest(cases, split)
	if err != nil {
		t.Fatal(err)
	}

	rebuild := !raceEnabled || *updateRecallRegistration
	built := committedDir // under -race: register the committed traces themselves
	if rebuild {
		built = t.TempDir()
		if got, err := buildRecallCorpus(context.Background(), casesPath, built, split); err != nil {
			t.Fatal(err)
		} else if got != digest {
			t.Fatalf("build digest %s, want %s", got, digest)
		}
	}
	want := recallRegistrationJSON(t, cases, split, built)
	if *updateRecallRegistration {
		if _, err := buildRecallCorpus(context.Background(), casesPath, committedDir, split); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(regPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if rebuild {
		diff, err := corpusDirDiff(built, committedDir)
		if err != nil {
			t.Fatalf("%v; regenerate with %s", err, recallFixtureRegenCommand+recallFreezeNote)
		}
		if diff != "" {
			t.Fatalf("%s: %s; regenerate with %s", committedDir, diff, recallFixtureRegenCommand+recallFreezeNote)
		}
	} else {
		names, err := corpusDirNames(committedDir)
		if err != nil {
			t.Fatal(err)
		}
		wantNames := map[string]struct{}{}
		for _, f := range cases.Families {
			if f.Split == split {
				for _, c := range recallCells(split) {
					wantNames[recallTraceID(f.ID, c.Suffix)+".json"] = struct{}{}
				}
			}
		}
		if !reflect.DeepEqual(names, wantNames) {
			t.Fatalf("%s holds %d files, want exactly the %d registered traces; regenerate with %s", committedDir, len(names), len(wantNames), recallFixtureRegenCommand+recallFreezeNote)
		}
	}
	reg, _, err := loadRecallRegistration(regPath)
	if err != nil {
		t.Fatalf("%v; regenerate with %s", err, recallFixtureRegenCommand+recallFreezeNote)
	}
	if reg.Split != split || reg.CasesSHA256 != digest {
		t.Fatalf("registration split %q cases_sha256 %s, want %s %s; regenerate with %s", reg.Split, reg.CasesSHA256, split, digest, recallFixtureRegenCommand+recallFreezeNote)
	}
	for _, m := range reg.Models {
		if d := reg.Decoding[m]; !reflect.DeepEqual(d, recallLineupDecoding()) {
			got, _ := json.Marshal(d)
			lineup, _ := json.Marshal(recallLineupDecoding())
			t.Fatalf("%s: registered decoding %s, want the lineup decoding %s; regenerate with %s", m, got, lineup, recallFixtureRegenCommand+recallFreezeNote)
		}
	}
	got, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the registration of the split's traces (cases digest, trace hashes or layout); regenerate with %s", regPath, recallFixtureRegenCommand+recallFreezeNote)
	}

	// The registration binds the committed traces for every registered model.
	var paths []string
	for _, row := range reg.Traces {
		paths = append(paths, filepath.Join(committedDir, row.ID+".json"))
	}
	traces, err := loadTraces(paths)
	if err != nil {
		t.Fatalf("%v; regenerate with %s", err, recallFixtureRegenCommand+recallFreezeNote)
	}
	targets, err := parseModelTargets(strings.Join(reg.Models, ","))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.bindCapture(traces, targets); err != nil {
		t.Fatal(err)
	}
}

// recallAuthoringConfirmatory derives a 40-family confirmatory split from the
// committed pilot that keeps every authoring rule and the builder's split
// balance: each template's three pilot families repeat to ten, the j-th with
// rank (j + template index)%3 + 1 (so no rank takes four families in every
// template) and s7 j%2. Under -update-recall-registration it builds and
// registers cleanly (checked by hand once; 400 traces).
func recallAuthoringConfirmatory(t *testing.T, pilot recallCases) recallCases {
	t.Helper()
	out := recallCases{Schema: pilot.Schema, Templates: pilot.Templates}
	for ti, tm := range pilot.Templates {
		var src []recallFamily
		for _, f := range pilot.Families {
			if f.Template == tm.ID {
				src = append(src, f)
			}
		}
		for j := 0; j < 10; j++ {
			f := src[j%len(src)]
			rank, s7 := (j+ti)%3+1, j%2
			f.ID, f.Split, f.Rank, f.S7Distractor = fmt.Sprintf("%s-c%02d", tm.ID, j), "confirmatory", &rank, &s7
			out.Families = append(out.Families, f)
		}
	}
	return out
}

// Each authoring rule names its breach, and the committed pilot and a
// confirmatory split built to the rules break none.
func TestRecallAuthoringRules(t *testing.T) {
	pilot, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := recallAuthoringProblems(pilot, "pilot"); len(got) != 0 {
		t.Fatalf("committed pilot: %v", got)
	}
	conf := recallAuthoringConfirmatory(t, pilot)
	if got := recallAuthoringProblems(conf, "confirmatory"); len(got) != 0 {
		t.Fatalf("rule-keeping confirmatory split: %v", got)
	}
	if err := conf.validate("confirmatory", false); err != nil {
		t.Fatalf("rule-keeping confirmatory split fails the builder's validation: %v", err)
	}
	family := func(c recallCases, id string) *recallFamily {
		for i := range c.Families {
			if c.Families[i].ID == id {
				return &c.Families[i]
			}
		}
		t.Fatalf("no family %s", id)
		return nil
	}
	clone := func(c recallCases) recallCases {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var out recallCases
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	skewS7 := func(c recallCases) recallCases {
		// s7 = 1 on each template's rank 1 families, then rank 2, until
		// five; then one template trades a rank 1 one for a rank 3 one, so
		// every template, task style and value type stays at half and
		// every rank keeps both values, but rank 1 has 13 of 14.
		ones := map[string]int{}
		for _, r := range []int{1, 2, 3} {
			for i := range c.Families {
				if f := &c.Families[i]; f.rank() == r {
					*f.S7Distractor = 0
					if ones[f.Template] < 5 {
						*f.S7Distractor = 1
						ones[f.Template]++
					}
				}
			}
		}
		*family(c, "num-direct-c00").S7Distractor = 0
		*family(c, "num-direct-c02").S7Distractor = 1
		return c
	}
	for _, tc := range []struct {
		name, split string
		edit        func(c recallCases) recallCases
		want        string
	}{
		{"value 3", "pilot", func(c recallCases) recallCases { family(c, "p01").Value.Lesson = "3"; return c }, `family p01: number value "3"`},
		{"value 8", "pilot", func(c recallCases) recallCases { family(c, "p01").Value.Changed = "8"; return c }, `family p01: number value "8"`},
		{"value 10", "pilot", func(c recallCases) recallCases { family(c, "p02").Value.Changed = "10"; return c }, `family p02: number value "10"`},
		{"value 21", "pilot", func(c recallCases) recallCases { family(c, "p02").Value.Changed = "21"; return c }, `family p02: number value "21"`},
		{"value at the line count", "pilot", func(c recallCases) recallCases { family(c, "p09").Value.Lesson = "4"; return c }, `family p09: number value "4" is not an integer 2..20 other than 3, 8 and 10 above the file's 4 lines`},
		{"identifier lesson meaning clause", "pilot", func(c recallCases) recallCases {
			family(c, "p03").Lesson += " so the oldest go first"
			return c
		}, "family p03: identifier lesson"},
		{"distractor names no other file", "pilot", func(c recallCases) recallCases {
			family(c, "p01").Distractors[0] = "a lesson about nothing"
			return c
		}, "family p01: distractor 0 names other files []"},
		{"distractors name one file", "pilot", func(c recallCases) recallCases {
			f := family(c, "p01")
			f.Distractors[1] = strings.ReplaceAll(f.Distractors[1], "ingest/encode.go", "ingest/sink.go")
			return c
		}, "family p01: both distractors name ingest/sink.go"},
		{"pilot s7 does not alternate", "pilot", func(c recallCases) recallCases { *family(c, "p05").S7Distractor = 0; return c }, "template num-direct: s7_distractor does not alternate at family p05"},
		{"pilot ranks repeat", "pilot", func(c recallCases) recallCases { *family(c, "p05").Rank = 1; return c }, "template num-direct: rank 1 in 2 of 3 families"},
		{"confirmatory template short", "confirmatory", func(c recallCases) recallCases { c.Families = c.Families[1:]; return c }, "template num-direct: 9 confirmatory families, want 10"},
		{"confirmatory ranks unbalanced", "confirmatory", func(c recallCases) recallCases {
			for _, id := range []string{"num-direct-c01", "num-direct-c04"} {
				*family(c, id).Rank = 1
			}
			return c
		}, "template num-direct: rank 1 in 6 of 10 families"},
		{"confirmatory s7 unbalanced in a template", "confirmatory", func(c recallCases) recallCases {
			*family(c, "id-task-c00").S7Distractor = 1
			*family(c, "id-task-c02").S7Distractor = 1
			return c
		}, "template id-task families: s7_distractor 1 in 7 of 10"},
		{"confirmatory s7 tracks rank", "confirmatory", func(c recallCases) recallCases {
			for i := range c.Families {
				*c.Families[i].S7Distractor = map[bool]int{true: 1, false: 0}[c.Families[i].rank() == 2]
			}
			return c
		}, "rank 2 families: s7_distractor 1 in 13 of 13"},
		{"confirmatory s7 skewed within rank", "confirmatory", skewS7, "rank 1 families: s7_distractor 1 in 13 of 14"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := pilot
			if tc.split == "confirmatory" {
				base = conf
			}
			got := recallAuthoringProblems(tc.edit(clone(base)), tc.split)
			if !slices.ContainsFunc(got, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("problems %q lack %q", got, tc.want)
			}
		})
	}
	// The skewed split breaks the per-rank rule and nothing else.
	for _, p := range recallAuthoringProblems(skewS7(clone(conf)), "confirmatory") {
		if !strings.HasPrefix(p, "rank ") {
			t.Errorf("skewed s7 split: unexpected problem %q", p)
		}
	}
}

// The seeded records' day numbers lie outside 2..20, so a reply quoting a
// record's date never states a fixture value.
func TestRecallRecordDaysStateNoValue(t *testing.T) {
	cases, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range cases.Families {
		recs, _, _, _, err := f.layout()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range recs {
			if d := r.CreatedAt.Day(); d >= 2 && d <= 20 {
				t.Errorf("family %s: record %s is dated day %d, inside the 2..20 a value takes", f.ID, r.ID, d)
			}
		}
	}
}
