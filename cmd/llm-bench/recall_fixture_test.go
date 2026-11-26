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

type recallFixtureSplit struct{ split, registration string }

// recallFixtureSplits names each split's committed registration file.
var recallFixtureSplits = []recallFixtureSplit{
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

// recallRegistrationJSON is split's registration for the supplied traces, built
// from cases: the cases digest, the compiled rule text, scorer revision and
// primary, the lineup and its decoding, and one id and canonicalTraceHash row
// per trace in family then cell order.
func recallRegistrationJSON(t *testing.T, cases recallCases, split string, traces map[string]Trace) []byte {
	t.Helper()
	digest, err := recallCasesDigest(cases, split)
	if err != nil {
		t.Fatal(err)
	}
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
				t.Fatalf("%s split has no trace %s; regenerate with %s", split, id, recallFixtureRegenCommand+recallFreezeNote)
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
		if len(f.Distractors) != len(named) {
			fail("family %s: distractors has %d entries, want exactly %d", f.ID, len(f.Distractors), len(named))
			continue
		}
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

// recallFixtureActive reports whether a split has inputs to check. A
// registration without families is stale, even during regeneration.
func recallFixtureActive(cases recallCases, split, regPath string, update bool) (bool, error) {
	has := slices.ContainsFunc(cases.Families, func(f recallFamily) bool { return f.Split == split })
	_, err := os.Stat(regPath)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if !has && err == nil {
		return false, fmt.Errorf("%s exists but cases.json has no %s families; remove it or add the families and run %s", regPath, split, recallFixtureRegenCommand+recallFreezeNote)
	}
	if has && err != nil && !update {
		return false, fmt.Errorf("cases.json has %s families but %s does not exist; generate it with %s", split, regPath, recallFixtureRegenCommand+recallFreezeNote)
	}
	return has, nil
}

// regenerateRecallFixtures builds every active split in scratch space. Only
// after all authoring checks and builds pass can the registrations, traces
// and removals be published together under the corpus builder's locks.
func regenerateRecallFixtures(t *testing.T, root string, cases recallCases) (retErr error) {
	t.Helper()
	casesPath := filepath.Join(root, "cases.json")
	var activeSplits []recallFixtureSplit
	for _, sp := range recallFixtureSplits {
		regPath := filepath.Join(root, sp.registration)
		active, err := recallFixtureActive(cases, sp.split, regPath, true)
		if err != nil {
			return err
		}
		if !active {
			continue
		}
		if problems := recallAuthoringProblems(cases, sp.split); len(problems) > 0 {
			return fmt.Errorf("fix cases.json before regenerating: %s", strings.Join(problems, "\n"))
		}
		activeSplits = append(activeSplits, sp)
	}
	bySplit := make(map[string][]filePublication)
	var files []filePublication
	for _, sp := range activeSplits {
		wantDigest, err := recallCasesDigest(cases, sp.split)
		if err != nil {
			return err
		}
		built := t.TempDir()
		if digest, err := buildRecallCorpus(t.Context(), casesPath, built, sp.split); err != nil {
			return err
		} else if digest != wantDigest {
			return fmt.Errorf("%s cases changed while regenerating: built %s, expected %s", sp.split, digest, wantDigest)
		}
		traces, raws := recallReadBuiltTraces(t, built)
		reg := recallRegistrationJSON(t, cases, sp.split, traces)
		files = append(files, filePublication{target: filepath.Join(root, sp.registration), data: reg, mode: 0o644})
		for id, raw := range raws {
			file := filePublication{target: filepath.Join(root, sp.split, id+".json"), data: []byte(raw), mode: 0o644}
			bySplit[sp.split] = append(bySplit[sp.split], file)
			files = append(files, file)
		}
	}
	var removals []string
	for _, sp := range activeSplits {
		dir := filepath.Join(root, sp.split)
		_, statErr := os.Lstat(dir)
		created := os.IsNotExist(statErr)
		unlock, err := lockRecallOutputDir(dir)
		if err != nil {
			return err
		}
		if created {
			defer func(d string) {
				if retErr != nil {
					_ = os.Remove(d)
				}
			}(dir)
		}
		defer unlock()
		stale, err := recallOutputRemovals(casesPath, dir, sp.split, bySplit[sp.split])
		if err != nil {
			return err
		}
		removals = append(removals, stale...)
	}
	out, err := publishFileSet(files, removals)
	for _, warning := range out.cleanupWarnings {
		t.Logf("recall fixture cleanup: %v", warning)
	}
	return err
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
// that catches a builder, prompt or tool-text change). Under -race the rebuild
// (about 150 s for a confirmatory split) is skipped:
// the registration is checked against the committed trace files themselves
// (canonicalTraceHash of each file) and the directory's file set, so CI
// catches a registration and corpus that disagree but not a corpus that has
// drifted from what cases.json now builds. Run the test without -race before
// a recall fixture PR.
func TestRecallRegistrationsLoad(t *testing.T) {
	if !*updateRecallRegistration {
		t.Parallel()
	}
	cases, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if *updateRecallRegistration {
		if err := regenerateRecallFixtures(t, recallFixtureDir, cases); err != nil {
			t.Fatal(err)
		}
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
	active, err := recallFixtureActive(cases, split, regPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		t.Skipf("cases.json has no %s families and %s does not exist: nothing is registered", split, regPath)
	}
	for _, p := range recallAuthoringProblems(cases, split) {
		t.Errorf("%s; fix cases.json, then run %s", p, recallFixtureRegenCommand+recallFreezeNote)
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
	builtTraces, _ := recallReadBuiltTraces(t, built)
	want := recallRegistrationJSON(t, cases, split, builtTraces)

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
		{"third distractor", "pilot", func(c recallCases) recallCases {
			f := family(c, "p01")
			f.Distractors = append(f.Distractors, f.Distractors[0])
			return c
		}, "family p01: distractors has 3 entries, want exactly 2"},
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

// Swapping two valid trace payloads must fail even without a full rebuild.
func TestRecallFixtureTraceFilename(t *testing.T) {
	dir := t.TempDir()
	ids := []string{"recall-p01-B-s1-1", "recall-p01-B-s2-1"}
	for i, id := range ids {
		other := ids[1-i]
		raw, err := os.ReadFile(filepath.Join(recallFixtureDir, "pilot", other+".json"))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, id+".json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readRecallFixtureTrace(path); err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), other) {
			t.Errorf("swapped trace %s: got %v, want an error identifying both traces", id, err)
		}
	}
}

// An unfinished confirmatory split must leave even a stale pilot untouched.
func TestRecallFixtureRegenerationPreflightsAllSplits(t *testing.T) {
	cases, err := loadRecallCases(filepath.Join(recallFixtureDir, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	cases.Families = slices.DeleteFunc(cases.Families, func(f recallFamily) bool { return f.Split != "pilot" })
	conf := recallAuthoringConfirmatory(t, cases)
	cases.Families = append(cases.Families, conf.Families[0])
	raw, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cases.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(dir, "registration-pilot.json")
	prior := []byte("previous pilot registration")
	if err := os.WriteFile(regPath, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := regenerateRecallFixtures(t, dir, cases); err == nil || !strings.Contains(err.Error(), "confirmatory") {
		t.Fatalf("regenerate with incomplete confirmatory split: %v", err)
	}
	got, err := os.ReadFile(regPath)
	if err != nil || !bytes.Equal(got, prior) {
		t.Fatalf("pilot registration after failed regeneration: got %q, err %v", got, err)
	}
	for _, name := range []string{"pilot", "confirmatory", "registration.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s after failed regeneration: got %v, want absent", name, err)
		}
	}

	// A lock failure on a second active split must remove any newly created
	// directory for the first split rather than leaving an empty directory behind.
	t.Run("removes newly created split dir on later lock failure", func(t *testing.T) {
		full := recallAuthoringConfirmatory(t, cases)
		full.Families = append(slices.DeleteFunc(slices.Clone(cases.Families), func(f recallFamily) bool { return f.Split != "pilot" }), full.Families...)
		fullRaw, err := json.Marshal(full)
		if err != nil {
			t.Fatal(err)
		}
		d := t.TempDir()
		if err := os.WriteFile(filepath.Join(d, "cases.json"), fullRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(d, "confirmatory", ".recall-build.lock"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := regenerateRecallFixtures(t, d, full); err == nil || !strings.Contains(err.Error(), ".recall-build.lock") {
			t.Fatalf("regenerate with locked confirmatory dir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(d, "pilot")); !os.IsNotExist(err) {
			t.Errorf("pilot dir after failed regeneration: got %v, want absent", err)
		}
	})
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
