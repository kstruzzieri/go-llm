package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// recallFixturePath is the two-family test fixture.
const recallFixturePath = "testdata/recall-label/test-cases.json"

// recallFixtureCases loads the two-family test fixture.
func recallFixtureCases(t *testing.T) recallCases {
	t.Helper()
	c, err := loadRecallCases(filepath.FromSlash(recallFixturePath))
	if err != nil {
		t.Fatalf("loadRecallCases: %v", err)
	}
	return c
}

// recallFixtureFamily returns a copy of the named fixture family.
func recallFixtureFamily(t *testing.T, id string) recallFamily {
	t.Helper()
	for _, f := range recallFixtureCases(t).Families {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("no fixture family %q", id)
	return recallFamily{}
}

// recallCasesWithSplit writes the fixture to a temp file with every family set to split.
func recallCasesWithSplit(t *testing.T, split string) string {
	t.Helper()
	c := recallFixtureCases(t)
	for i := range c.Families {
		c.Families[i].Split = split
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// recallScaledCases clones the fixture families into n families spread evenly
// over tmpls templates (alternating number and identifier), ranks i%3+1 and
// S7 distractors i%2.
func recallScaledCases(t *testing.T, n, tmpls int, split string) recallCases {
	t.Helper()
	base := recallFixtureCases(t)
	c := recallCases{Schema: recallCasesSchema}
	for i := 0; i < tmpls; i++ {
		c.Templates = append(c.Templates, recallTemplate{ID: fmt.Sprintf("tpl-%d", i), ValueType: []string{"number", "identifier"}[i%2], TaskStyle: []string{"direct", "task"}[i/2%2]})
	}
	for i := 0; i < n; i++ {
		tm := c.Templates[i%tmpls]
		// the value type follows the template, so pick the matching base family
		var f recallFamily
		for _, b := range base.Families {
			if b.Value.Type == tm.ValueType {
				f = b
			}
		}
		f.ID = fmt.Sprintf("fam-%d", i)
		f.Template = tm.ID
		f.Split = split
		f.Rank = intPtr(i%3 + 1)
		f.S7Distractor = intPtr(i % 2)
		c.Families = append(c.Families, f)
	}
	return c
}

func TestRecallCasesValidate(t *testing.T) {
	tests := []struct {
		name    string
		split   string
		mutate  func(c *recallCases)
		wantErr string // empty = valid
	}{
		{"fixture is valid", "confirmatory", func(c *recallCases) {}, ""},
		{"wrong schema", "confirmatory", func(c *recallCases) { c.Schema = "other/v1" }, "schema"},
		{"unknown split argument", "confirmatory2", func(c *recallCases) {}, `split "confirmatory2" invalid`},
		{"family split invalid", "confirmatory", func(c *recallCases) { c.Families[0].Split = "x" }, `split "x" invalid`},
		{"lesson record states a different number", "confirmatory", func(c *recallCases) { c.Families[0].Lesson = "sync worker retry limit equals nine attempts" }, "lesson record does not state"},
		{"lesson record misspells the identifier", "confirmatory", func(c *recallCases) { c.Families[1].Lesson = "cache policy eviction uses evictLru setting" }, "lesson record does not state"},
		{"lesson missing from file_before", "confirmatory", func(c *recallCases) { c.Families[0].FileBefore = "package worker\n" }, "file_before does not state"},
		{"lesson present in file_s1", "confirmatory", func(c *recallCases) { c.Families[0].FileS1 = "const maxRetries = 12 // was 7\n" }, "file_s1 states"},
		{"changed missing from file_s1", "confirmatory", func(c *recallCases) { c.Families[0].FileS1 = "package worker\n" }, "file_s1 does not state the changed"},
		{"changed value belongs to another setting", "confirmatory", func(c *recallCases) {
			c.Families[0].FileS1 = "package worker\nconst maxRetries = 9\nconst historyDepth = 12\n"
		}, "file_s1: value.identifier"},
		{"lesson value belongs to another setting", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.FileBefore = "package worker\nconst maxRetries = 9\nconst depth = 7\n"
			f.FileS2 = f.FileBefore + "\n"
		}, "file_before: value.identifier"},
		{"formatting comments out setting", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.FileBefore = "package worker\n// retry limit\nconst maxRetries = 7\n"
			f.FileS2 = strings.Replace(f.FileBefore, "limit\n", "limit ", 1)
		}, "file_s2: value.identifier"},
		{"formatting changes another string literal", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.FileBefore += "const other = \"a b\"\n"
			f.FileS2 += "const other = \"a   b\"\n"
		}, "formatting changes the meaning"},
		{"file_s2 differs beyond whitespace", "confirmatory", func(c *recallCases) { c.Families[0].FileS2 += "// extra\n" }, "file_s2 differs"},
		{"file_s2 merges tokens", "confirmatory", func(c *recallCases) { c.Families[0].FileS2 = "package worker\n\nconst maxRetries =7\n" }, "file_s2 differs"},
		{"file_s2 lacks the lesson value", "confirmatory", func(c *recallCases) { c.Families[0].FileS2 = "package worker\n" }, "file_s2 does not state"},
		{"lesson stated in task", "confirmatory", func(c *recallCases) { c.Families[0].Task += " It is 7." }, "task states the lesson"},
		{"lesson stated in a distractor", "confirmatory", func(c *recallCases) { c.Families[0].Distractors[1] = "sync worker pool holds 7 threads" }, "a distractor states"},
		{"lesson stated in other_files", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["worker/doc.go"] = "// seven\n// 7\n" }, `other_files "worker/doc.go" states`},
		{"lesson stated in query", "confirmatory", func(c *recallCases) { c.Families[0].Query = "sync worker 7" }, "query states"},
		{"lesson stated in the system prompt", "confirmatory", func(c *recallCases) {
			f := &c.Families[1]
			f.Value.Lesson = "agent_memory_search"
			f.Lesson = "cache policy cache/policy.go eviction uses agent_memory_search setting"
			f.FileBefore = "const evictionMode = agent_memory_search\n"
			f.FileS2 = "const evictionMode   = agent_memory_search\n"
		}, "the system prompt states"},
		{"rank zero", "confirmatory", func(c *recallCases) { c.Families[0].Rank = intPtr(0) }, "rank 0 outside 1..3"},
		{"rank four", "confirmatory", func(c *recallCases) { c.Families[0].Rank = intPtr(4) }, "rank 4 outside 1..3"},
		{"rank missing", "confirmatory", func(c *recallCases) { c.Families[0].Rank = nil }, "rank is missing"},
		{"s7 distractor two", "confirmatory", func(c *recallCases) { c.Families[0].S7Distractor = intPtr(2) }, "s7_distractor 2 outside 0..1"},
		{"s7 distractor negative", "confirmatory", func(c *recallCases) { c.Families[0].S7Distractor = intPtr(-1) }, "s7_distractor -1 outside 0..1"},
		{"s7 distractor missing", "confirmatory", func(c *recallCases) { c.Families[0].S7Distractor = nil }, "s7_distractor is missing"},
		{"three distractors", "confirmatory", func(c *recallCases) {
			c.Families[0].Distractors = append(c.Families[0].Distractors, "sync worker extra record words")
		}, "distractors has 3 entries"},
		{"one distractor", "confirmatory", func(c *recallCases) { c.Families[0].Distractors = c.Families[0].Distractors[:1] }, "distractors has 1 entries"},
		{"number lesson 0", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.Value.Lesson, f.FileBefore = "0", "const maxRetries = 0\n"
			f.FileS2 = f.FileBefore
		}, "0 or 1"},
		{"number lesson 1", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.Value.Lesson, f.FileBefore = "1", "const maxRetries = 1\n"
			f.FileS2 = f.FileBefore
		}, "0 or 1"},
		{"number changed 0", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.Value.Changed, f.FileS1 = "0", "const maxRetries = 0\n"
		}, "0 or 1"},
		{"number changed 1", "confirmatory", func(c *recallCases) {
			f := &c.Families[0]
			f.Value.Changed, f.FileS1 = "1", "const maxRetries = 1\n"
		}, "0 or 1"},
		{"duplicate family id", "confirmatory", func(c *recallCases) { c.Families[1].ID = c.Families[0].ID }, "duplicate id"},
		{"unsafe family id", "confirmatory", func(c *recallCases) { c.Families[0].ID = "../x" }, "id must match"},
		{"unknown template", "confirmatory", func(c *recallCases) { c.Families[0].Template = "nope" }, `unknown template "nope"`},
		{"duplicate template id", "confirmatory", func(c *recallCases) { c.Templates[1].ID = c.Templates[0].ID }, "invalid or duplicate template id"},
		{"bad template value type", "confirmatory", func(c *recallCases) { c.Templates[0].ValueType = "float" }, "value_type"},
		{"value type differs from template", "confirmatory", func(c *recallCases) { c.Templates[0].ValueType = "identifier" }, "value type"},
		{"bad task style", "confirmatory", func(c *recallCases) { c.Templates[0].TaskStyle = "other" }, "task_style"},
		{"blank task", "confirmatory", func(c *recallCases) { c.Families[0].Task = " " }, "task is blank"},
		{"blank distractor", "confirmatory", func(c *recallCases) { c.Families[0].Distractors[0] = " " }, "non-blank"},
		{"empty query", "confirmatory", func(c *recallCases) { c.Families[0].Query = " " }, "query is blank"},
		{"newline in lesson record", "confirmatory", func(c *recallCases) { c.Families[0].Lesson += "\nsecond line" }, "no newline"},
		{"absolute other file", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["/etc/x"] = "x\n" }, "other_files"},
		{"escaping other file", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["../x"] = "x\n" }, "other_files"},
		{"absolute named file", "confirmatory", func(c *recallCases) { c.Families[0].NamedFile = "/worker/config.go" }, "named_file"},
		{"absolute task path", "confirmatory", func(c *recallCases) { c.Families[1].TaskPaths = []string{"/cache/policy.go"} }, "task_paths"},
		{"escaping task path", "confirmatory", func(c *recallCases) { c.Families[1].TaskPaths = []string{"../cache/policy.go"} }, "task_paths"},
		// Paths the frozen trace check (recallWorkspaceFile) refuses are refused
		// here, naming the family, instead of after the build.
		{"named file is the workspace root", "confirmatory", func(c *recallCases) { c.Families[0].NamedFile = "worker/.." }, `family num-retry: named_file: path "worker/.."`},
		{"padded task path", "confirmatory", func(c *recallCases) { c.Families[1].TaskPaths = []string{" cache/policy.go"} }, `family id-eviction: task_paths: path " cache/policy.go"`},
		{"other file is the workspace root", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["."] = "x\n" }, `family num-retry: other_files: path "."`},
		{"other file shadows named file", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["worker/config.go"] = "x\n" }, "collides with named_file"},
		{"other file differs from named file only by case", "confirmatory", func(c *recallCases) { c.Families[0].OtherFiles["Worker/Config.go"] = "x\n" }, "collides with named_file"},
		{"lesson does not name its file", "confirmatory", func(c *recallCases) { c.Families[0].Lesson = "sync worker retry limit equals 7 attempts" }, "does not name named_file"},
		{"distractor names no other file", "confirmatory", func(c *recallCases) { c.Families[0].Distractors[0] = "sync worker backoff cap equals 30 seconds" }, "distractor 0 must name a path from other_files"},
		{"distractor names the named file", "confirmatory", func(c *recallCases) {
			c.Families[0].Distractors[1] = "sync worker worker/doc.go worker/config.go logs rotate every 24 hours"
		}, "distractor 1 must name a path from other_files and not named_file"},
		{"file_s2 byte-identical to file_before", "confirmatory", func(c *recallCases) { c.Families[0].FileS2 = c.Families[0].FileBefore }, "byte-identical"},
		{"identifier absent from file_before", "confirmatory", func(c *recallCases) { c.Families[1].Value.Identifier = "evictMode" }, "value.identifier"},
		{"identifier absent from file_s1", "confirmatory", func(c *recallCases) {
			c.Families[1].FileS1 = "package cache\n\nconst mode = evictTTL\n"
		}, "value.identifier"},
		{"number family identifier absent from the files", "confirmatory", func(c *recallCases) { c.Families[0].Value.Identifier = "retryCap" }, "value.identifier"},
		{"task path not in task", "confirmatory", func(c *recallCases) { c.Families[1].TaskPaths = []string{"cache/other.go"} }, `task_paths entry "cache/other.go" does not appear in task`},
		{"two other files differ only by case", "confirmatory", func(c *recallCases) {
			c.Families[0].OtherFiles["Worker/Doc.go"] = "x\n"
		}, "collides with other_files entry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := recallFixtureCases(t)
			tc.mutate(&c)
			// the content rows are about one family; counts have their own test
			err := c.validate(tc.split, true)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestRecallCasesValidateBalance(t *testing.T) {
	tests := []struct {
		name    string
		split   string
		build   func() recallCases
		wantErr string
	}{
		{"balanced confirmatory", "confirmatory", func() recallCases { return recallScaledCases(t, 40, 4, "confirmatory") }, ""},
		{"balanced pilot", "pilot", func() recallCases { return recallScaledCases(t, 12, 4, "pilot") }, ""},
		{"confirmatory template count off", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			c.Families[0].Template, c.Families[0].Value = c.Templates[1].ID, c.Families[1].Value
			c.Families[0].FileBefore, c.Families[0].FileS1, c.Families[0].FileS2 = c.Families[1].FileBefore, c.Families[1].FileS1, c.Families[1].FileS2
			c.Families[0].Lesson, c.Families[0].Distractors = c.Families[1].Lesson, c.Families[1].Distractors
			c.Families[0].NamedFile, c.Families[0].OtherFiles = c.Families[1].NamedFile, c.Families[1].OtherFiles
			return c
		}, "template tpl-0 has 9"},
		{"confirmatory with two templates of ten families", "confirmatory", func() recallCases { return recallScaledCases(t, 20, 2, "confirmatory") }, "exactly 4 templates"},
		{"confirmatory with a repeated template pair", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			c.Templates[3].TaskStyle = "direct" // identifier/direct twice, identifier/task never
			return c
		}, "each (value_type, task_style) pair once"},
		{"confirmatory rank counts off", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			for i := range c.Families {
				c.Families[i].Rank = intPtr(1)
			}
			return c
		}, "rank 1 has"},
		{"rank counts 15/12/13 fail", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			// 14/13/13 -> 15/12/13: |3*15-40| = 5 > 3
			c.Families[1].Rank = intPtr(1)
			return c
		}, "rank"},
		{"s7 counts off", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			for i := range c.Families {
				c.Families[i].S7Distractor = intPtr(0)
			}
			return c
		}, "s7_distractor 1 has"},
		{"s7 counts within one pass", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			c.Families[1].S7Distractor = intPtr(0) // 21/19
			return c
		}, ""},
		{"pilot template count off", "pilot", func() recallCases {
			c := recallScaledCases(t, 12, 4, "pilot")
			c.Families = c.Families[:11]
			return c
		}, "has 2 pilot families, want 3"},
		{"balanced pilot with n=15 (rank 5/5/5)", "pilot", func() recallCases { return recallScaledCases(t, 15, 5, "pilot") }, "exactly 4 templates"},
		{"pilot rank counts off", "pilot", func() recallCases {
			c := recallScaledCases(t, 12, 4, "pilot")
			for i := range c.Families {
				c.Families[i].Rank = intPtr(1)
			}
			return c
		}, "rank 1 has 12 of 12 families; want 3..5"},
		{"pilot with a repeated template pair", "pilot", func() recallCases {
			c := recallScaledCases(t, 12, 4, "pilot")
			c.Templates[2].TaskStyle = c.Templates[0].TaskStyle
			return c
		}, "exactly 4 templates"},
		{"n=40 s7 counts name the derived range", "confirmatory", func() recallCases {
			c := recallScaledCases(t, 40, 4, "confirmatory")
			for i := range c.Families {
				c.Families[i].S7Distractor = intPtr(0)
			}
			return c
		}, "want 19..21"},
		{"no family in split", "pilot", func() recallCases { return recallScaledCases(t, 40, 4, "confirmatory") }, "no famil"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build().validate(tc.split, false)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// recallReadBuiltTraces reads every trace file in dir, keyed by trace ID, with the raw bytes.
func recallReadBuiltTraces(t *testing.T, dir string) (map[string]Trace, map[string]string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	traces, raws := map[string]Trace{}, map[string]string{}
	for _, p := range paths {
		tr, raw, err := readRecallFixtureTrace(p)
		if err != nil {
			t.Fatal(err)
		}
		traces[tr.ID], raws[tr.ID] = tr, string(raw)
	}
	return traces, raws
}

// readRecallFixtureTrace binds the payload ID to the filename that selects it.
func readRecallFixtureTrace(path string) (Trace, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Trace{}, nil, err
	}
	var tr Trace
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Trace{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	if filepath.Base(path) != tr.ID+".json" {
		return Trace{}, nil, fmt.Errorf("%s contains trace %q; expected filename %s.json", path, tr.ID, tr.ID)
	}
	return tr, raw, nil
}

func TestBuildRecallCorpusConfirmatory(t *testing.T) {
	out := t.TempDir()
	if _, err := buildRecallCorpusWith(context.Background(), recallCasesWithSplit(t, "confirmatory"), out, "confirmatory", recallBuildOptions{SkipBalance: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	traces, raws := recallReadBuiltTraces(t, out)
	if len(traces) != 20 {
		t.Fatalf("got %d traces, want 20", len(traces))
	}
	for _, fam := range []string{"num-retry", "id-eviction"} {
		for _, suffix := range []string{"A-0", "A-1", "A-2", "A-2-s7", "B-s1-1", "B-s1-2", "B-s2-1", "B-s2-2", "B-s2-P", "B-s2-1-repeat"} {
			id := "recall-" + fam + "-" + suffix
			tr, ok := traces[id]
			if !ok {
				t.Errorf("missing trace %s", id)
				continue
			}
			if err := validateRecallTrace(tr); err != nil {
				t.Errorf("%s: %v", id, err)
			}
			if tr.Recall.FamilyID != fam || tr.Recall.Split != "confirmatory" {
				t.Errorf("%s: recall block %+v", id, tr.Recall)
			}
		}
		for suffix, want := range map[string]int{"A-0": 0, "A-1": 0, "A-2": 1, "A-2-s7": 1, "B-s1-1": 0, "B-s1-2": 1, "B-s2-1": 0, "B-s2-2": 1, "B-s2-P": 0, "B-s2-1-repeat": 0} {
			if got := strings.Count(recallToolMessageOf(t, traces["recall-"+fam+"-"+suffix]), recallLabelSegment); got != want {
				t.Errorf("%s %s: label segment count %d, want %d", fam, suffix, got, want)
			}
		}
		if got := strings.Count(recallToolMessageOf(t, traces["recall-"+fam+"-B-s2-P"]), recallPlaceboSegment); got != 1 {
			t.Errorf("%s placebo count %d, want 1", fam, got)
		}
		// S7 labels a distractor line, A-2 the relevant one.
		s7 := recallLineWith(recallToolMessageOf(t, traces["recall-"+fam+"-A-2-s7"]), recallLabelSegment)
		a2 := recallLineWith(recallToolMessageOf(t, traces["recall-"+fam+"-A-2"]), recallLabelSegment)
		_, rel, dist, _ := recallTestLayout(t, recallFixtureFamily(t, fam))
		if !strings.Contains(a2, " · "+rel+" · ") || strings.Contains(s7, " · "+rel+" · ") || (!strings.Contains(s7, " · "+dist[0]+" · ") && !strings.Contains(s7, " · "+dist[1]+" · ")) {
			t.Errorf("%s: label lines A-2 %q, S7 %q", fam, a2, s7)
		}
		if tr := traces["recall-"+fam+"-B-s2-1-repeat"]; !tr.Recall.Repeat || tr.Recall.Arm != "1" {
			t.Errorf("%s repeat block %+v", fam, tr.Recall)
		}
		// Trace A never reads a file; Trace B reads the named file with the S1 or S2 content.
		if tool := recallToolMessageCount(traces["recall-"+fam+"-A-1"]); tool != 1 {
			t.Errorf("%s A-1: %d tool messages, want 1", fam, tool)
		}
		if tool := recallToolMessageCount(traces["recall-"+fam+"-B-s1-1"]); tool != 2 {
			t.Errorf("%s B-s1-1: %d tool messages, want 2", fam, tool)
		}
	}
	// read_file shows S1 content on S1 and S2 content on S2.
	if !strings.Contains(raws["recall-num-retry-B-s1-1"], "maxRetries = 12") || strings.Contains(raws["recall-num-retry-B-s1-1"], "maxRetries = 7") {
		t.Errorf("B-s1 read_file does not show the S1 file")
	}
	if !strings.Contains(raws["recall-num-retry-B-s2-1"], "maxRetries = 7") {
		t.Errorf("B-s2 read_file does not show the S2 file")
	}
}

// recallDistID returns the seeded ID of fam's i-th distractor record.
func recallDistID(t *testing.T, fam recallFamily, i int) string {
	t.Helper()
	_, _, d, _ := recallTestLayout(t, fam)
	return d[i]
}

// recallTestLayout is fam.layout with a layout error failing the test.
func recallTestLayout(t *testing.T, fam recallFamily) ([]recallSeedRecord, string, [2]string, []string) {
	t.Helper()
	recs, rel, dist, order, err := fam.layout()
	if err != nil {
		t.Fatal(err)
	}
	return recs, rel, dist, order
}

// recallToolMessageOf returns the first tool message of a trace (the search result).
func recallToolMessageOf(t *testing.T, tr Trace) string {
	t.Helper()
	for _, turn := range tr.Turns {
		if turn.Role == "tool" {
			return turn.Content
		}
	}
	t.Fatalf("trace %s has no tool message", tr.ID)
	return ""
}

// recallToolMessageCount counts the tool messages of a trace.
func recallToolMessageCount(tr Trace) int {
	n := 0
	for _, turn := range tr.Turns {
		if turn.Role == "tool" {
			n++
		}
	}
	return n
}

// recallLineWith returns the first line of s containing sub.
func recallLineWith(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestBuildRecallCorpusPilot(t *testing.T) {
	out := t.TempDir()
	if _, err := buildRecallCorpusWith(context.Background(), recallCasesWithSplit(t, "pilot"), out, "pilot", recallBuildOptions{SkipBalance: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	traces, _ := recallReadBuiltTraces(t, out)
	if len(traces) != 10 {
		t.Fatalf("got %d traces, want 10", len(traces))
	}
	for id, tr := range traces {
		if tr.Recall.Arm == "2" {
			t.Errorf("%s: pilot trace carries arm 2", id)
		}
		if err := validateRecallTrace(tr); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	for _, suffix := range []string{"A-0", "A-1", "B-s1-1", "B-s2-1", "B-s2-P"} {
		if _, ok := traces["recall-num-retry-"+suffix]; !ok {
			t.Errorf("missing pilot trace %s", suffix)
		}
	}
}

func TestBuildRecallCorpusSplitSelectsFamilies(t *testing.T) {
	c := recallFixtureCases(t)
	c.Families[1].Split = "pilot"
	raw, _ := json.Marshal(c)
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if _, err := buildRecallCorpusWith(context.Background(), p, out, "pilot", recallBuildOptions{SkipBalance: true}); err != nil {
		t.Fatalf("build: %v", err)
	}
	traces, _ := recallReadBuiltTraces(t, out)
	if len(traces) != 5 {
		t.Fatalf("got %d traces, want 5 (one pilot family)", len(traces))
	}
	for id := range traces {
		if !strings.HasPrefix(id, "recall-id-eviction-") {
			t.Errorf("unexpected trace %s", id)
		}
	}
}

func TestRecallBuildDeterministic(t *testing.T) {
	cases := recallCasesWithSplit(t, "confirmatory")
	var dirs [2]string
	for i := range dirs {
		dirs[i] = t.TempDir()
		if _, err := buildRecallCorpusWith(context.Background(), cases, dirs[i], "confirmatory", recallBuildOptions{SkipBalance: true}); err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
	}
	a, rawA := recallReadBuiltTraces(t, dirs[0])
	b, _ := recallReadBuiltTraces(t, dirs[1])
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("trace counts %d and %d", len(a), len(b))
	}
	tmp := os.TempDir()
	resolved, _ := filepath.EvalSymlinks(tmp)
	for id, tr := range a {
		if canonicalTraceHash(tr) != canonicalTraceHash(b[id]) {
			t.Errorf("%s: hash differs between builds", id)
		}
		for _, bad := range []string{tmp, resolved, "recall-build-"} {
			if bad != "" && strings.Contains(rawA[id], bad) {
				t.Errorf("%s: request bytes contain %q", id, bad)
			}
		}
	}
}

// recallBuiltFamilyRequests builds the confirmatory requests of a fixture family.
func recallBuiltFamilyRequests(t *testing.T, famID string) (recallFamily, map[string]provider.ChatRequest) {
	t.Helper()
	c := recallFixtureCases(t)
	var fam recallFamily
	for _, f := range c.Families {
		if f.ID == famID {
			fam = f
		}
	}
	tmpl := recallTemplate{}
	for _, tp := range c.Templates {
		if tp.ID == fam.Template {
			tmpl = tp
		}
	}
	_, reqs, err := buildRecallFamily(context.Background(), fam, tmpl, "confirmatory")
	if err != nil {
		t.Fatalf("buildRecallFamily: %v", err)
	}
	return fam, reqs
}

// recallCloneReqs deep-copies the message and tool slices so a row can edit them.
func recallCloneReqs(in map[string]provider.ChatRequest) map[string]provider.ChatRequest {
	out := make(map[string]provider.ChatRequest, len(in))
	for k, r := range in {
		r.Messages = append([]provider.ChatMessage(nil), r.Messages...)
		r.Tools = append([]provider.Tool(nil), r.Tools...)
		out[k] = r
	}
	return out
}

// recallEditFirstTool rewrites the first tool message of the request at suffix.
func recallEditFirstTool(reqs map[string]provider.ChatRequest, suffix string, f func(string) string) {
	r := reqs[suffix]
	for i, m := range r.Messages {
		if m.Role == "tool" {
			r.Messages[i].Content = f(m.Content)
			return
		}
	}
}

func TestRecallInvariantsHoldAndReject(t *testing.T) {
	fam, base := recallBuiltFamilyRequests(t, "num-retry")
	if err := checkRecallInvariants(fam, "confirmatory", base); err != nil {
		t.Fatalf("clean requests rejected: %v", err)
	}
	tests := []struct {
		name    string
		edit    func(r map[string]provider.ChatRequest)
		wantErr string
	}{
		{"arm 2 loses its label", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-2", func(s string) string { return strings.Replace(s, recallLabelSegment, "", 1) })
		}, "A-2: segment \"; files=differ\" occurs 0 times"},
		{"arm 2 label moves to a distractor", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-2", func(s string) string {
				s = strings.Replace(s, recallLabelSegment, "", 1)
				lines := strings.Split(s, "\n")
				for i, l := range lines {
					if strings.Contains(l, " · "+recallDistID(t, fam, 0)+" · ") {
						lines[i], _ = insertProvenanceSegment(l, recallLabelSegment)
					}
				}
				return strings.Join(lines, "\n")
			})
		}, "A-2: segment \"; files=differ\" is on record"},
		{"arm 2 label sits on no record line", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-2", func(s string) string {
				s = strings.Replace(s, recallLabelSegment, "", 1)
				return strings.Replace(s, "\n", recallLabelSegment+"\n", 1)
			})
		}, "A-2: segment \"; files=differ\" is on record \"\""},
		{"arm 2 differs elsewhere", func(r map[string]provider.ChatRequest) {
			q := r["A-2"]
			q.Messages[0].Content += " extra"
			r["A-2"] = q
		}, "A-2 differs from A-1"},
		{"arm 1 differs from arm 0 beyond the sentence", func(r map[string]provider.ChatRequest) {
			q := r["A-1"]
			q.Messages[0].Content += " extra"
			r["A-1"] = q
		}, "A-1 differs from A-0"},
		{"arm 1 lacks the sentence", func(r map[string]provider.ChatRequest) { r["A-1"] = r["A-0"] }, "explanation sentence"},
		{"arm 0 carries the sentence", func(r map[string]provider.ChatRequest) { r["A-0"] = r["A-1"] }, "arm 0 carries the explanation sentence"},
		{"S7 labels the relevant record", func(r map[string]provider.ChatRequest) { r["A-2-s7"] = r["A-2"] }, "A-2-s7: segment \"; files=differ\" is on record"},
		{"S7 loses its label", func(r map[string]provider.ChatRequest) { r["A-2-s7"] = r["A-1"] }, "A-2-s7: segment \"; files=differ\" occurs 0 times"},
		{"S7 labels the other distractor", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-2-s7", func(s string) string {
				s = strings.Replace(s, recallLabelSegment, "", 1)
				lines := strings.Split(s, "\n")
				for i, l := range lines {
					if strings.Contains(l, " · "+recallDistID(t, fam, 1)+" · ") {
						lines[i], _ = insertProvenanceSegment(l, recallLabelSegment)
					}
				}
				return strings.Join(lines, "\n")
			})
		}, "A-2-s7: segment \"; files=differ\" is on record"},
		{"placebo carries the label instead", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "B-s2-P", func(s string) string { return strings.Replace(s, recallPlaceboSegment, recallLabelSegment, 1) })
		}, "B-s2-P: segment \"; seq=4471\" occurs 0 times"},
		{"placebo on a distractor", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "B-s2-P", func(s string) string {
				s = strings.Replace(s, recallPlaceboSegment, "", 1)
				lines := strings.Split(s, "\n")
				for i, l := range lines {
					if strings.Contains(l, " · "+recallDistID(t, fam, 0)+" · ") {
						lines[i], _ = insertProvenanceSegment(l, recallPlaceboSegment)
					}
				}
				return strings.Join(lines, "\n")
			})
		}, "B-s2-P: segment \"; seq=4471\" is on record"},
		{"repeat differs from its twin", func(r map[string]provider.ChatRequest) {
			q := r["B-s2-1-repeat"]
			q.Messages[0].Content += " extra"
			r["B-s2-1-repeat"] = q
		}, "B-s2-1-repeat differs from B-s2-1"},
		{"record order differs from rank", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-0", func(s string) string {
				lines := strings.Split(s, "\n")
				var rec []int
				for i, l := range lines {
					if strings.Count(l, " · ") >= 2 {
						rec = append(rec, i)
					}
				}
				lines[rec[0]], lines[rec[1]] = lines[rec[1]], lines[rec[0]]
				return strings.Join(lines, "\n")
			})
		}, "search ranks by bm25 before date, so give the three records the same token count"},
		{"a trace is missing", func(r map[string]provider.ChatRequest) { delete(r, "B-s1-2") }, "was not built"},
		{"every request lists the records in the wrong order", func(r map[string]provider.ChatRequest) {
			for suffix := range r {
				recallEditFirstTool(r, suffix, func(s string) string {
					lines := strings.Split(s, "\n")
					var rec []int
					for i, l := range lines {
						if strings.Count(l, " · ") >= 2 {
							rec = append(rec, i)
						}
					}
					lines[rec[0]], lines[rec[1]] = lines[rec[1]], lines[rec[0]]
					return strings.Join(lines, "\n")
				})
			}
		}, "order"},
		{"lesson stated in the system prompt", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Messages[0].Content += " The value is 7."
			r["A-0"] = q
		}, "message 0 (system) states the lesson"},
		{"lesson stated in a distractor line", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "A-1", func(s string) string { return strings.Replace(s, "backoff cap equals 30", "backoff cap equals 7", 1) })
		}, "(tool) states the lesson"},
		{"lesson stated in B-s2-1's distractor line", func(r map[string]provider.ChatRequest) {
			recallEditFirstTool(r, "B-s2-1", func(s string) string { return strings.Replace(s, "backoff cap equals 30", "backoff cap equals 7", 1) })
		}, "B-s2-1: message 3 (tool) states the lesson"},
		{"lesson stated in B-s2-1's system message", func(r map[string]provider.ChatRequest) {
			q := r["B-s2-1"]
			q.Messages[0].Content += " The value is 7."
		}, "B-s2-1: message 0 (system) states the lesson"},
		{"lesson stated in B-s2-1's tool description", func(r map[string]provider.ChatRequest) {
			q := r["B-s2-1"]
			q.Tools[0].Function.Description += " Default is 7."
		}, "B-s2-1: tool "},
		{"lesson stated in a tool parameter description", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","properties":{"q":{"description":"at most 7 results"}}}`)
		}, "parameter schema states the lesson"},
		{"lesson stated as a number in a parameter schema bound", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","maximum":7}}}`)
		}, "parameter schema states the lesson"},
		{"lesson stated as a parameter schema key", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Parameters = json.RawMessage(`{"type":"object","properties":{"7":{"type":"string"}}}`)
		}, "parameter schema states the lesson"},
		{"lesson hidden behind a JSON escape in a parameter description", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Parameters = json.RawMessage(`{"description":"limit\n7"}`)
		}, "parameter schema states the lesson"},
		{"lesson stated in a tool name", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Name = "tool 7"
		}, "name states the lesson"},
		{"lesson stated in a tool-call name", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			for i, m := range q.Messages {
				if len(m.ToolCalls) > 0 {
					tc := m.ToolCalls[0]
					tc.Function.Name = "call 7"
					q.Messages[i].ToolCalls = []provider.ToolCall{tc}
					return
				}
			}
		}, "name states the lesson"},
		{"lesson stated in a tool message's tool name", func(r map[string]provider.ChatRequest) {
			for i, m := range r["A-0"].Messages {
				if m.Role == "tool" {
					r["A-0"].Messages[i].ToolName = "tool 7"
					return
				}
			}
		}, "(tool) tool name states the lesson"},
		{"lesson stated in a tool message's tool call id", func(r map[string]provider.ChatRequest) {
			for i, m := range r["A-0"].Messages {
				if m.Role == "tool" {
					r["A-0"].Messages[i].ToolCallID = "call 7"
					return
				}
			}
		}, "(tool) tool call id states the lesson"},
		{"explanation missing from a Trace B cell", func(r map[string]provider.ChatRequest) {
			q := r["B-s1-1"]
			q.Tools = r["A-0"].Tools
			r["B-s1-1"] = q
		}, "B-s1-1: the explanation sentence must sit on agent_memory_search"},
		{"explanation sits on another tool", func(r map[string]provider.ChatRequest) {
			q := r["A-1"]
			q.Tools = append([]provider.Tool(nil), q.Tools...)
			for i, tl := range q.Tools {
				switch tl.Function.Name {
				case "agent_memory_search":
					q.Tools[i].Function.Description = strings.TrimSuffix(tl.Function.Description, recallExplanationSentence)
				case "read_file":
					q.Tools[i].Function.Description += recallExplanationSentence
				}
			}
			r["A-1"] = q
		}, "A-1: the explanation sentence must sit on agent_memory_search"},
		{"explanation on two tools", func(r map[string]provider.ChatRequest) {
			q := r["A-1"]
			q.Tools = append([]provider.Tool(nil), q.Tools...)
			for i, tl := range q.Tools {
				if tl.Function.Name == "read_file" {
					q.Tools[i].Function.Description += recallExplanationSentence
				}
			}
			r["A-1"] = q
		}, "A-1: the explanation sentence must sit on agent_memory_search"},
		{"S2 cell differs from S1 outside the read_file result", func(r map[string]provider.ChatRequest) {
			for _, k := range []string{"B-s2-1", "B-s2-1-repeat", "B-s2-2", "B-s2-P"} {
				q := r[k]
				q.Messages[0].Content += " extra"
			}
		}, "(readonly)"},
		{"Trace B does not extend Trace A", func(r map[string]provider.ChatRequest) {
			for k, q := range r {
				if strings.HasPrefix(k, "B-") {
					q.Messages[0].Content += " extra"
				}
			}
		}, "does not extend"},
		{"lesson stated in the S1 read_file result", func(r map[string]provider.ChatRequest) {
			q := r["B-s1-1"]
			n := 0
			for i, m := range q.Messages {
				if m.Role == "tool" {
					n++
					if n == 2 {
						q.Messages[i].Content += "\nmaxRetries was 7\n"
					}
				}
			}
		}, "states the lesson"},
		{"lesson stated in a tool description", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			q.Tools[0].Function.Description += " Default is 7."
		}, "description states the lesson"},
		{"lesson stated in the scripted query", func(r map[string]provider.ChatRequest) {
			q := r["A-0"]
			for i, m := range q.Messages {
				if len(m.ToolCalls) > 0 {
					tc := m.ToolCalls[0]
					tc.Function.Arguments = json.RawMessage(`{"query":"sync worker 7"}`)
					q.Messages[i].ToolCalls = []provider.ToolCall{tc}
					return
				}
			}
		}, "arguments states the lesson"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := recallCloneReqs(base)
			tc.edit(r)
			err := checkRecallInvariants(fam, "confirmatory", r)
			if err == nil {
				t.Fatal("want an invariant error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestRecallInvariantsPilotSubset(t *testing.T) {
	fam, base := recallBuiltFamilyRequests(t, "id-eviction")
	pilot := map[string]provider.ChatRequest{}
	for _, s := range []string{"A-0", "A-1", "B-s1-1", "B-s2-1", "B-s2-P"} {
		pilot[s] = base[s]
	}
	if err := checkRecallInvariants(fam, "pilot", pilot); err != nil {
		t.Fatalf("pilot subset rejected: %v", err)
	}
}

func TestRecallCasesRejectLabelText(t *testing.T) {
	fields := []struct {
		name string
		set  func(f *recallFamily, text string)
	}{
		{"lesson", func(f *recallFamily, x string) { f.Lesson += " " + x }},
		{"distractor 0", func(f *recallFamily, x string) { f.Distractors[0] += " " + x }},
		{"distractor 1", func(f *recallFamily, x string) { f.Distractors[1] += " " + x }},
		{"task", func(f *recallFamily, x string) { f.Task += " " + x }},
		{"query", func(f *recallFamily, x string) { f.Query += " " + x }},
		{"named_file", func(f *recallFamily, x string) { f.NamedFile = "worker/" + x + ".go" }},
		{"other_files key", func(f *recallFamily, x string) { f.OtherFiles["x/"+x+".go"] = "ok\n" }},
		{"other_files x/ok.go", func(f *recallFamily, x string) { f.OtherFiles["x/ok.go"] = x + "\n" }},
		{"file_before", func(f *recallFamily, x string) { f.FileBefore += "// " + x + "\n"; f.FileS2 += "// " + x + "\n" }},
		{"file_s1", func(f *recallFamily, x string) { f.FileS1 += "// " + x + "\n" }},
		{"file_s2", func(f *recallFamily, x string) { f.FileS2 += "// " + x + "\n" }},
	}
	for _, f := range fields {
		for _, tok := range []string{"files=differ", "Files=x", "SEQ=4471", "seq=1"} {
			t.Run(f.name+"/"+tok, func(t *testing.T) {
				c := recallFixtureCases(t)
				f.set(&c.Families[0], tok)
				err := c.validate("confirmatory", true)
				want := f.name + " contains"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("error %v, want one containing %q", err, want)
				}
			})
		}
	}
}

func TestLoadRecallCasesStrict(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(recallFixturePath))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"fixture loads", string(raw), ""},
		{"trailing object", string(raw) + `{"schema":"x"}`, "data after"},
		{"trailing garbage", string(raw) + "oops", "data after"},
		{"unknown top-level key", strings.Replace(string(raw), `"templates"`, `"bogus": 1, "templates"`, 1), "unknown field"},
		{"unknown family key", strings.Replace(string(raw), `"rank": 2,`, `"rank": 2, "ranks": 2,`, 1), "unknown field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadRecallCases(p)
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
	// An omitted rank or s7_distractor decodes to nil and validate rejects it.
	for _, key := range []string{`"rank": 2,`, `"s7_distractor": 0`} {
		body := strings.Replace(string(raw), key, "", 1)
		if key == `"s7_distractor": 0` {
			body = strings.Replace(body, `"rank": 2,`, `"rank": 2`, 1)
		}
		p := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := loadRecallCases(p)
		if err != nil {
			t.Fatalf("%s omitted: %v", key, err)
		}
		if err := c.validate("confirmatory", true); err == nil || !strings.Contains(err.Error(), "is missing") {
			t.Errorf("%s omitted: validate error %v, want a missing-value error", key, err)
		}
	}
}

// writeStale writes a minimal trace file whose recall block names split ("" = no recall block).
func writeStale(t *testing.T, dir, name, split string) {
	t.Helper()
	body := `{"id":"x"}`
	if split != "" {
		id := strings.TrimSuffix(name, ".json")
		family, _, _ := parseRecallTraceID(id)
		body = fmt.Sprintf(`{"id":%q,"source":%q,"recall":{"split":%q,"family_id":%q,"trace":"A","arm":"0"}}`, id, recallTraceSource, split, family)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildRecallCorpusPrunesOnlyItsOwnSplit(t *testing.T) {
	for _, outName := range []string{"out", "out[1]"} { // glob metacharacters in the path are not a pattern
		t.Run(outName, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), outName)
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			writeStale(t, out, "recall-old-A-0.json", "confirmatory")
			writeStale(t, out, "recall-pilotfam-A-0.json", "pilot")
			writeStale(t, out, "other.json", "confirmatory")
			if err := os.WriteFile(filepath.Join(out, "recall-garbage.json"), []byte("not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := buildRecallCorpusWith(context.Background(), recallCasesWithSplit(t, "confirmatory"), out, "confirmatory", recallBuildOptions{SkipBalance: true}); err != nil {
				t.Fatal(err)
			}
			for name, wantKept := range map[string]bool{
				"recall-old-A-0.json": false, "recall-pilotfam-A-0.json": true, "other.json": true,
				"recall-garbage.json": true, "recall-num-retry-A-0.json": true,
			} {
				if _, err := os.Stat(filepath.Join(out, name)); (err == nil) != wantKept {
					t.Errorf("%s: kept=%t, want %t", name, err == nil, wantKept)
				}
			}
		})
	}
}

func TestBuildRecallCorpusFailureLeavesOutDirUntouched(t *testing.T) {
	c := recallFixtureCases(t)
	for i := range c.Families {
		c.Families[i].Split = "confirmatory"
	}
	c.Families[1].Task += " It is evictLRU." // validate rejects it
	raw, _ := json.Marshal(c)
	cases := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(cases, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStale(t, out, "recall-old-A-0.json", "confirmatory")
	if _, err := buildRecallCorpusWith(context.Background(), cases, out, "confirmatory", recallBuildOptions{SkipBalance: true}); err == nil {
		t.Fatal("want a validate error")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 || entries[0].Name() != "recall-old-A-0.json" {
		t.Errorf("failed build changed the output dir: %v", entries)
	}
	missing := filepath.Join(t.TempDir(), "never")
	if _, err := buildRecallCorpusWith(context.Background(), cases, missing, "confirmatory", recallBuildOptions{SkipBalance: true}); err == nil {
		t.Fatal("want a validate error")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("failed build created the output dir")
	}
}

func TestRecallSeededIDs(t *testing.T) {
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, fam := range recallFixtureCases(t).Families {
		_, rel, dist, order := recallTestLayout(t, fam)
		ids := []string{rel, dist[0], dist[1]}
		for _, id := range ids {
			if !hex32.MatchString(id) {
				t.Errorf("%s: id %q is not 32 lowercase hex digits", fam.ID, id)
			}
			if strings.Contains(id, fam.ID) || strings.Contains(id, "rel") {
				t.Errorf("%s: id %q carries the family ID or a role", fam.ID, id)
			}
			if statesValue(id, fam.Value, fam.Value.Lesson) {
				t.Errorf("%s: id %q states the lesson value", fam.ID, id)
			}
		}
		if ids[0] == ids[1] || ids[0] == ids[2] || ids[1] == ids[2] || len(order) != 3 {
			t.Errorf("%s: ids %v are not distinct", fam.ID, ids)
		}
		_, rel2, dist2, _ := recallTestLayout(t, fam)
		if rel2 != rel || dist2 != dist {
			t.Errorf("%s: ids are not deterministic", fam.ID)
		}
	}
}

// An ID that begins with the digits of a number lesson states it; recordID
// moves on to the next candidate.
func TestRecallRecordIDSkipsAnIDThatStatesTheValue(t *testing.T) {
	found := false
	for i := 0; i < 400 && !found; i++ {
		fam := recallFixtureFamily(t, "num-retry")
		fam.ID = fmt.Sprintf("probe-%d", i)
		first := recallSeededID(fam.ID, "relevant", 0)
		if !statesValue(first, fam.Value, fam.Value.Lesson) {
			continue
		}
		found = true
		want := ""
		for k := 1; want == ""; k++ {
			if id := recallSeededID(fam.ID, "relevant", k); id[0] >= 'a' && !statesValue(id, fam.Value, fam.Value.Lesson) {
				want = id
			}
		}
		if got, err := fam.recordID("relevant"); err != nil || got != want {
			t.Errorf("recordID = %q, %v; want the first letter-led candidate that does not state the value, %q", got, err, want)
		}
	}
	if !found {
		t.Fatal("no probe family produced an ID stating the lesson value")
	}
}

func TestParseRecallTraceIDMatchesBuiltTraces(t *testing.T) {
	for _, split := range []string{"pilot", "confirmatory"} {
		t.Run(split, func(t *testing.T) {
			out := t.TempDir()
			if _, err := buildRecallCorpusWith(context.Background(), recallCasesWithSplit(t, split), out, split, recallBuildOptions{SkipBalance: true}); err != nil {
				t.Fatalf("build: %v", err)
			}
			traces, _ := recallReadBuiltTraces(t, out)
			if len(traces) == 0 {
				t.Fatal("no traces built")
			}
			byCell := map[string]recallCell{}
			for _, c := range recallCells(split) {
				byCell[c.Suffix] = c
			}
			for id, tr := range traces {
				fam, cell, err := parseRecallTraceID(id)
				if err != nil {
					t.Errorf("%s: %v", id, err)
					continue
				}
				c, ok := byCell[cell]
				r := tr.Recall
				if !ok || fam != r.FamilyID || c.Trace != r.Trace || c.Arm != r.Arm || c.Repeat != r.Repeat || !slices.Equal(c.Cases, r.Cases) {
					t.Errorf("%s parses to family %q cell %q (%+v), recall block %+v", id, fam, cell, c, r)
				}
				if key := recallCellKey(tr); key != cell {
					t.Errorf("%s: recallCellKey %q, parsed cell %q", id, key, cell)
				}
			}
		})
	}
	for _, bad := range []string{"other-fam-A-0", "recall-fam-A-9", "recall--A-0", "recall-A-0"} {
		if _, _, err := parseRecallTraceID(bad); err == nil {
			t.Errorf("parseRecallTraceID(%q) accepted", bad)
		}
	}
	// Family IDs may hold dashes and cell-like words; the longest suffix wins.
	if fam, cell, err := parseRecallTraceID("recall-a-B-s2-1-repeat"); err != nil || fam != "a" || cell != "B-s2-1-repeat" {
		t.Errorf("dashed parse = %q %q %v", fam, cell, err)
	}
	if fam, cell, err := parseRecallTraceID("recall-x-A-2-s7"); err != nil || fam != "x" || cell != "A-2-s7" {
		t.Errorf("A-2-s7 parse = %q %q %v", fam, cell, err)
	}
}

func TestRecallCasesDigest(t *testing.T) {
	base := recallFixtureCases(t)
	for i := range base.Families {
		base.Families[i].Split = "confirmatory"
	}
	want, err := recallCasesDigest(base, "confirmatory")
	if err != nil || !recallDigestPattern.MatchString(want) {
		t.Fatalf("digest %q err %v", want, err)
	}
	appended := recallFixtureCases(t)
	for i := range appended.Families {
		appended.Families[i].Split = "confirmatory"
	}
	extra := recallFixtureCases(t).Families[0]
	extra.ID, extra.Split = "later-pilot", "pilot"
	appended.Families = append(appended.Families, extra)
	if got, _ := recallCasesDigest(appended, "confirmatory"); got != want {
		t.Errorf("appending a pilot family changed the confirmatory digest: %s vs %s", got, want)
	}
	changed := base
	changed.Families = append([]recallFamily(nil), base.Families...)
	changed.Families[1].Task += " Be brief."
	if got, _ := recallCasesDigest(changed, "confirmatory"); got == want {
		t.Error("changing a confirmatory family left the digest unchanged")
	}
	if _, err := recallCasesDigest(base, "pilot"); err == nil {
		t.Error("digest of a split with no families accepted")
	}
}

func TestBuildRecallCorpusReturnsCasesDigest(t *testing.T) {
	cases := recallCasesWithSplit(t, "confirmatory")
	loaded, err := loadRecallCases(cases)
	if err != nil {
		t.Fatal(err)
	}
	want, err := recallCasesDigest(loaded, "confirmatory")
	if err != nil {
		t.Fatal(err)
	}
	got, err := buildRecallCorpusWith(context.Background(), cases, t.TempDir(), "confirmatory", recallBuildOptions{SkipBalance: true})
	if err != nil || got != want {
		t.Errorf("digest %q err %v, want %q", got, err, want)
	}
}

// A workspace that reaches Trace A (here the S2 file text) fails the build.
func TestRecallBuildRejectsWorkspaceLeakIntoTraceA(t *testing.T) {
	c := recallFixtureCases(t)
	fam, tmpl := c.Families[0], c.Templates[0]
	leaky := func(ctx context.Context, f recallFamily, cell recallCell) (provider.ChatRequest, error) {
		req, err := captureRecallCell(ctx, f, cell)
		if cell.Suffix == "A-1" && cell.S2 {
			req.Messages = append([]provider.ChatMessage(nil), req.Messages...)
			req.Messages[0].Content += " " + f.FileS2
		}
		return req, err
	}
	if _, _, err := buildRecallFamilyWith(context.Background(), fam, tmpl, "confirmatory", captureRecallCell); err != nil {
		t.Fatalf("clean build: %v", err)
	}
	_, _, err := buildRecallFamilyWith(context.Background(), fam, tmpl, "confirmatory", leaky)
	if err == nil || !strings.Contains(err.Error(), "workspace leaked into Trace A") {
		t.Fatalf("error %v, want a workspace-leak error", err)
	}
}

func TestRecallStringsScansNumbers(t *testing.T) {
	var got []string
	recallStrings(map[string]any{"maximum": float64(7), "flag": true, "n": 2.5, "k": []any{float64(12)}}, &got)
	for _, want := range []string{"7", "2.5", "12", "maximum"} {
		if !slices.Contains(got, want) {
			t.Errorf("strings %v lack %q", got, want)
		}
	}
	if slices.Contains(got, "true") {
		t.Errorf("a boolean was scanned: %v", got)
	}
}

// Every seeded ID starts with a letter, so no number token can be read from it
// (a digit-led ID would state a number lesson, and a quoted ID would read as a
// different value to the scorer).
func TestRecallSeededIDsHoldNoNumberToken(t *testing.T) {
	shape := regexp.MustCompile(`^[a-f][0-9a-f]{31}$`)
	c := recallFixtureCases(t)
	for _, split := range []string{"pilot", "confirmatory"} {
		for i := 0; i < 60; i++ {
			for _, base := range c.Families {
				fam := base
				fam.ID, fam.Split = fmt.Sprintf("%s-%s-%d", base.ID, split, i), split
				_, rel, dist, _ := recallTestLayout(t, fam)
				for _, id := range []string{rel, dist[0], dist[1]} {
					if !shape.MatchString(id) {
						t.Fatalf("%s: id %q is not [a-f][0-9a-f]{31}", fam.ID, id)
					}
					for n := 2; n <= 20; n++ {
						v := RecallValue{Type: "number"}
						if statesValue(id, v, fmt.Sprint(n)) {
							t.Fatalf("%s: id %q states the number %d", fam.ID, id, n)
						}
					}
				}
			}
		}
	}
}

func TestRecallYAMLFixtureBindings(t *testing.T) {
	f := recallFamily{NamedFile: "worker.yaml", Value: RecallValue{Identifier: "retries", Type: "number", Lesson: "7", Changed: "12"},
		FileBefore: "worker:\n  retries: 7\n  depth: 9\n",
		FileS1:     "worker:\n  retries: 12\n  depth: 9\n",
		FileS2:     "worker:\n    retries: 7\n    depth: 9\n",
	}
	if err := f.validateSource(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ s1, s2 string }{
		{"worker:\n  retries: 9\n  depth: 12\n", f.FileS2},
		{"worker:\n  retries: 12\nother:\n  retries: 12\n", f.FileS2},
		{"other:\n  retries: 12\n", f.FileS2},
		{f.FileS1, "worker:\n  retries: 7\ndepth: 9\n"},
	} {
		bad := f
		bad.FileS1, bad.FileS2 = tc.s1, tc.s2
		if err := bad.validateSource(); err == nil {
			t.Errorf("accepted invalid source: S1 %q S2 %q", tc.s1, tc.s2)
		}
	}
}

func TestBuildRecallCorpusProtectsExistingFiles(t *testing.T) {
	const name = "recall-num-retry-A-0.json"
	for _, kind := range []string{"input", "hardlinked input", "other split", "foreign", "symlink", "lookalike"} {
		t.Run(kind, func(t *testing.T) {
			out := t.TempDir()
			p := filepath.Join(out, name)
			cases := recallCasesWithSplit(t, "pilot")
			var err error
			switch kind {
			case "input":
				raw, readErr := os.ReadFile(cases)
				if readErr != nil {
					t.Fatal(readErr)
				}
				cases = p
				err = os.WriteFile(p, raw, 0600)
			case "hardlinked input":
				err = os.Link(cases, p)
			case "other split":
				writeStale(t, out, name, "confirmatory")
			case "foreign":
				err = os.WriteFile(p, []byte("unrelated"), 0600)
			case "symlink":
				if err := os.Symlink(cases, p); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			case "lookalike":
				err = os.WriteFile(p, []byte(`{"id":"other","recall":{"split":"pilot"}}`), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildRecallCorpusWith(t.Context(), cases, out, "pilot", recallBuildOptions{SkipBalance: true}); err == nil {
				t.Fatal("expected output collision")
			}
			got, err := os.ReadFile(p)
			if err != nil || string(got) != string(original) {
				t.Fatalf("colliding file changed: %v", err)
			}
			entries, err := os.ReadDir(out)
			if err != nil || len(entries) != 1 {
				t.Fatalf("partial publication: %v, %v", entries, err)
			}
		})
	}
}

// The record-ID search is capped: a predicate nothing satisfies exhausts it
// with a wrapped error naming the family and role, and a satisfiable one
// returns the first accepted candidate.
func TestSearchRecallRecordIDCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tries   int
		accept  func(string) bool
		wantErr string
		wantK   int
	}{
		{"first candidate accepted", 5, func(string) bool { return true }, "", 0},
		{"third candidate accepted", 5, func() func(string) bool {
			n := 0
			return func(string) bool { n++; return n == 3 }
		}(), "", 2},
		{"nothing accepted exhausts the cap", 7, func(string) bool { return false }, "recall build: family fam-x: no acceptable d0 record ID in 7 candidates", 0},
		{"zero tries", 0, func(string) bool { return true }, "no acceptable d0 record ID in 0 candidates", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := searchRecallRecordID("fam-x", "d0", tc.tries, tc.accept)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
					t.Fatalf("got %q, %v; want an error containing %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != recallSeededID("fam-x", "d0", tc.wantK) {
				t.Fatalf("got %q, %v; want candidate %d", got, err, tc.wantK)
			}
		})
	}
}
