package main

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func sampleCorpusManifest() Manifest {
	return Manifest{Entries: []ManifestEntry{
		{TraceID: "t1", Partition: PartitionNatural, Category: "chat", Source: "captured", AllowedAsModelEvidence: true},
		{TraceID: "t2", Partition: PartitionChallenge, Category: "subtle-bug", Source: "authored", AllowedAsModelEvidence: true},
		{TraceID: "t3", Partition: PartitionJudgeValidation, Category: "adversarial", Source: "curated", AllowedAsModelEvidence: false},
	}}
}

func TestManifest_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	m := sampleCorpusManifest()
	if err := writeManifest(path, m); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}
	got, err := loadManifest(path)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, m)
	}
}

func TestLoadManifest_RejectsUnknownPartition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	if err := writeJSONL(path, []any{
		ManifestEntry{TraceID: "t1", Partition: CorpusPartition("bogus"), Category: "chat"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatalf("loadManifest accepted an unknown partition; want error")
	}
}

func TestLoadManifest_RejectsEmptyCategory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	if err := writeJSONL(path, []any{
		ManifestEntry{TraceID: "t1", Partition: PartitionNatural, Category: ""},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatalf("loadManifest accepted an empty category; want error")
	}
}

func TestLoadManifest_RejectsDuplicateTraceID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	if err := writeJSONL(path, []any{
		ManifestEntry{TraceID: "t1", Partition: PartitionNatural, Category: "chat"},
		ManifestEntry{TraceID: "t1", Partition: PartitionChallenge, Category: "subtle-bug"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatalf("loadManifest accepted a duplicate trace ID; want error")
	}
}

func TestLoadManifest_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.jsonl")
	if err := writeJSONL(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatalf("loadManifest accepted an empty manifest; want error")
	}
}

func TestManifest_Counts(t *testing.T) {
	c := sampleCorpusManifest().Counts()
	if c.Total != 3 {
		t.Errorf("Total = %d; want 3", c.Total)
	}
	if c.ByPartition[PartitionNatural] != 1 || c.ByPartition[PartitionChallenge] != 1 || c.ByPartition[PartitionJudgeValidation] != 1 {
		t.Errorf("ByPartition = %v; want one each", c.ByPartition)
	}
	if c.ByCategory["chat"] != 1 || c.ByCategory["subtle-bug"] != 1 {
		t.Errorf("ByCategory = %v; want chat=1 subtle-bug=1", c.ByCategory)
	}
}

func TestManifest_Select(t *testing.T) {
	m := sampleCorpusManifest()
	cases := []struct {
		name string
		sel  corpusSelection
		want []string
	}{
		{"all", corpusSelection{}, []string{"t1", "t2", "t3"}},
		{"challenge only", corpusSelection{Partitions: []CorpusPartition{PartitionChallenge}}, []string{"t2"}},
		{"category", corpusSelection{Categories: []string{"subtle-bug"}}, []string{"t2"}},
		{"only model evidence", corpusSelection{OnlyModelEvidence: true}, []string{"t1", "t2"}},
		{"natural+challenge", corpusSelection{Partitions: []CorpusPartition{PartitionNatural, PartitionChallenge}}, []string{"t1", "t2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.Select(tc.sel)
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Select(%+v) = %v; want %v", tc.sel, got, tc.want)
			}
		})
	}
}

func TestFormatCorpusComposition_ShowsPartitionAndCategoryCounts(t *testing.T) {
	out := formatCorpusComposition(sampleCorpusManifest().Counts())
	for _, want := range []string{
		"## Corpus composition",
		"| natural | 1 |",
		"| challenge | 1 |",
		"| judge-validation | 1 |",
		"| **total** | 3 |",
		"| chat | 1 |",
		"| subtle-bug | 1 |",
		"| adversarial | 1 |", // judge-validation's category still appears in the composition overview
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composition missing %q:\n%s", want, out)
		}
	}
}

func TestFormatPartitionedQuality_SeparatesNaturalAndChallenge(t *testing.T) {
	results := []Result{
		{Model: "ollama/a", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
		{Model: "ollama/a", TraceID: "t2", Score: Score{AnswerQuality: 0.0}},
		{Model: "ollama/a", TraceID: "t3", Score: Score{AnswerQuality: 0.5}}, // judge-validation
	}
	traceToPartition := map[string]CorpusPartition{
		"t1": PartitionNatural, "t2": PartitionChallenge, "t3": PartitionJudgeValidation,
	}
	out := formatPartitionedQuality([]string{"ollama/a"}, results, &corpusReportData{
		TraceToPartition:     traceToPartition,
		TraceToModelEvidence: map[string]bool{"t1": true, "t2": true, "t3": false},
	})
	for _, want := range []string{
		"## Quality by model — by partition",
		"never averaged",
		"### natural",
		"### challenge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("partitioned quality missing %q:\n%s", want, out)
		}
	}
	// judge-validation must not get a model-quality section.
	if strings.Contains(out, "### judge-validation") {
		t.Errorf("judge-validation must be excluded from model quality:\n%s", out)
	}
	// natural mean = 1.00, challenge mean = 0.00; the 0.5 judge-validation row
	// must not have been folded into either average.
	natural := sectionBetween(out, "### natural", "### challenge")
	if !strings.Contains(natural, "1.00") {
		t.Errorf("natural section should show mean 1.00:\n%s", natural)
	}
}

func TestFormatReport_WithCorpusEmitsCompositionAndPartitioned(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
		{Model: "m", TraceID: "t2", Score: Score{AnswerQuality: 0.0}},
	}
	counts := corpusCounts{
		ByPartition: map[CorpusPartition]int{PartitionNatural: 1, PartitionChallenge: 1},
		ByCategory:  map[string]int{"chat": 1, "subtle-bug": 1},
		Total:       2,
	}
	out := formatReport([]string{"m"}, results, reportOptions{
		Scorer: "manual",
		Corpus: &corpusReportData{
			Counts:           counts,
			TraceToPartition: map[string]CorpusPartition{"t1": PartitionNatural, "t2": PartitionChallenge},
		},
	})
	for _, want := range []string{
		"## Corpus composition",
		"## Quality by model — by partition",
		"partitions are not comparable", // caveat on the combined table
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report with corpus missing %q:\n%s", want, out)
		}
	}
}

// sectionBetween returns the substring of s starting at the first occurrence of
// start up to (but excluding) the first subsequent occurrence of end. If end is
// absent it returns to the end of s.
func sectionBetween(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	if j := strings.Index(rest[len(start):], end); j >= 0 {
		return rest[:len(start)+j]
	}
	return rest
}

func TestBuildCorpusRun_FiltersToSelectionIntersection(t *testing.T) {
	m := sampleCorpusManifest()                           // t1 natural, t2 challenge, t3 judge-validation
	loaded := []Trace{{ID: "t1"}, {ID: "t2"}, {ID: "t4"}} // t3 missing; t4 not in manifest
	run, data, missing := buildCorpusRun(m, corpusSelection{}, loaded)

	var runIDs []string
	for _, tr := range run {
		runIDs = append(runIDs, tr.ID)
	}
	sort.Strings(runIDs)
	if !reflect.DeepEqual(runIDs, []string{"t1", "t2"}) {
		t.Fatalf("run IDs = %v; want [t1 t2] (intersection of selected and loaded)", runIDs)
	}
	if !reflect.DeepEqual(missing, []string{"t3"}) {
		t.Fatalf("missing = %v; want [t3] (selected but not loaded)", missing)
	}
	if data.Counts.Total != 2 {
		t.Errorf("Counts.Total = %d; want 2 (only the run subset)", data.Counts.Total)
	}
	if data.TraceToPartition["t1"] != PartitionNatural || data.TraceToPartition["t2"] != PartitionChallenge {
		t.Errorf("TraceToPartition = %v; want t1=natural t2=challenge", data.TraceToPartition)
	}
}

func TestBuildCorpusRun_RespectsPartitionSelection(t *testing.T) {
	m := sampleCorpusManifest()
	loaded := []Trace{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}
	run, _, _ := buildCorpusRun(m, corpusSelection{Partitions: []CorpusPartition{PartitionChallenge}}, loaded)
	if len(run) != 1 || run[0].ID != "t2" {
		t.Fatalf("run = %v; want only the challenge trace t2", run)
	}
}

func TestParseCorpusPartitions(t *testing.T) {
	if got, err := parseCorpusPartitions(""); err != nil || got != nil {
		t.Fatalf("parseCorpusPartitions(\"\") = (%v, %v); want (nil, nil)", got, err)
	}
	got, err := parseCorpusPartitions("natural, challenge ")
	if err != nil {
		t.Fatalf("parseCorpusPartitions: %v", err)
	}
	if !reflect.DeepEqual(got, []CorpusPartition{PartitionNatural, PartitionChallenge}) {
		t.Fatalf("got %v; want [natural challenge]", got)
	}
	if _, err := parseCorpusPartitions("bogus"); err == nil {
		t.Fatalf("parseCorpusPartitions(bogus) accepted unknown partition; want error")
	}
}

// Judge-validation results must NOT fold into the combined model-quality
// aggregate (they are scorer-calibration evidence, never model workload). The
// combined table mean must reflect only natural+challenge.
func TestFormatReport_ExcludesJudgeValidationFromCombinedAggregate(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0}}, // natural
		{Model: "m", TraceID: "t2", Score: Score{AnswerQuality: 0.0}}, // challenge
		{Model: "m", TraceID: "t3", Score: Score{AnswerQuality: 0.0}}, // judge-validation
	}
	out := formatReport([]string{"m"}, results, reportOptions{
		Scorer: "manual",
		Corpus: &corpusReportData{
			Counts: corpusCounts{
				ByPartition: map[CorpusPartition]int{PartitionNatural: 1, PartitionChallenge: 1, PartitionJudgeValidation: 1},
				ByCategory:  map[string]int{"chat": 3},
				Total:       3,
			},
			TraceToPartition: map[string]CorpusPartition{
				"t1": PartitionNatural, "t2": PartitionChallenge, "t3": PartitionJudgeValidation,
			},
		},
	})
	// Mean over natural+challenge only = (1.0+0.0)/2 = 0.50.
	if !strings.Contains(out, "| m | 0.50 / 0.00 / 0.00 / 1.00 / 1.00 |") {
		t.Errorf("combined aggregate should exclude judge-validation (mean 0.50, not 0.33):\n%s", out)
	}
	// The 3-value mean (0.33) would appear only if judge-validation leaked in.
	if strings.Contains(out, "| m | 0.33") {
		t.Errorf("judge-validation leaked into the combined aggregate (mean 0.33):\n%s", out)
	}
	if !strings.Contains(out, "judge-validation result(s) excluded") {
		t.Errorf("report should note the judge-validation exclusion:\n%s", out)
	}
}

func TestFormatReport_ExcludesNonEvidenceNaturalChallengeFromModelQuality(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0}}, // natural evidence
		{Model: "m", TraceID: "t2", Score: Score{AnswerQuality: 0.0}}, // natural, but not model evidence
	}
	out := formatReport([]string{"m"}, results, reportOptions{
		Scorer: "manual",
		Corpus: &corpusReportData{
			Counts: corpusCounts{
				ByPartition: map[CorpusPartition]int{PartitionNatural: 2},
				ByCategory:  map[string]int{"chat": 2},
				Total:       2,
			},
			TraceToPartition:     map[string]CorpusPartition{"t1": PartitionNatural, "t2": PartitionNatural},
			TraceToModelEvidence: map[string]bool{"t1": true, "t2": false},
		},
	})
	resultsSection := sectionBetween(out, "## Results", "## Quality by model")
	if !strings.Contains(resultsSection, "| m | 1.00 / 1.00 / 1.00 / 1.00 / 1.00 |") {
		t.Errorf("combined aggregate should include only allowed model evidence:\n%s", resultsSection)
	}
	if strings.Contains(resultsSection, "| m | 0.50") {
		t.Errorf("non-evidence natural/challenge row leaked into model quality:\n%s", resultsSection)
	}
	if !strings.Contains(out, "non-model-evidence result(s) excluded") {
		t.Errorf("report should note non-model-evidence exclusions:\n%s", out)
	}
	natural := sectionBetween(out, "### natural", "### challenge")
	if !strings.Contains(natural, "| m | 1.00 / 1.00 / 1.00 / 1.00 / 1.00 | 1 |") {
		t.Errorf("natural partition quality should include only t1 evidence row:\n%s", natural)
	}
}

func TestBuildCorpusRun_ReportDataSurfacesMissingAndUnclassified(t *testing.T) {
	m := sampleCorpusManifest()                           // t1 natural, t2 challenge, t3 judge-validation
	loaded := []Trace{{ID: "t1"}, {ID: "t2"}, {ID: "t4"}} // t3 missing; t4 not in manifest
	run, data, missing := buildCorpusRun(m, corpusSelection{}, loaded)

	if len(run) != 2 {
		t.Fatalf("run length = %d; want 2 selected loaded traces", len(run))
	}
	if !reflect.DeepEqual(missing, []string{"t3"}) {
		t.Fatalf("missing = %v; want [t3]", missing)
	}
	if !reflect.DeepEqual(data.MissingSelected, []string{"t3"}) {
		t.Fatalf("data.MissingSelected = %v; want [t3]", data.MissingSelected)
	}
	if !reflect.DeepEqual(data.UnclassifiedLoaded, []string{"t4"}) {
		t.Fatalf("data.UnclassifiedLoaded = %v; want [t4]", data.UnclassifiedLoaded)
	}

	out := formatReport([]string{"m"}, []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
		{Model: "m", TraceID: "t2", Score: Score{AnswerQuality: 0.0}},
	}, reportOptions{Scorer: "manual", Corpus: data})
	for _, want := range []string{
		"## Corpus selection gaps",
		"| selected-but-missing | t3 |",
		"| loaded-without-manifest | t4 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}

func TestSplitCommaList(t *testing.T) {
	if got := splitCommaList(""); got != nil {
		t.Fatalf("splitCommaList(\"\") = %v; want nil", got)
	}
	got := splitCommaList(" subtle-bug , tool-use ,")
	if !reflect.DeepEqual(got, []string{"subtle-bug", "tool-use"}) {
		t.Fatalf("splitCommaList = %v; want [subtle-bug tool-use] (trimmed, empties dropped)", got)
	}
}
