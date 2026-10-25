package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/internal/secretscan/secrettest"
)

// The #517 Secrets evidence report (spec §5.4, §5.6): docs/security/
// secrets-evidence.md is rendered from the case table (secrettest.Evaluate),
// the committed census results and the workflow table run under G and G+S,
// and a regeneration gate compares it with the committed file.

var updateSecretsEvidence = flag.Bool("update-secrets-evidence", false, "rewrite docs/security/secrets-evidence.md from the case table, the committed census results and the workflow table")

var secretsEvidencePath = filepath.Join("..", "..", "docs", "security", "secrets-evidence.md")

// censusResultsDoc mirrors cmd/llm-bench's censusResults (census_results.go)
// with the same JSON tags; a main package cannot be imported. Strata is not in
// the results file: loadCensusDoc reads it from the manifest the results bind
// through manifest_sha256, for the file counts and classes.
type censusResultsDoc struct {
	Schema         string               `json:"schema"`
	ManifestSHA256 string               `json:"manifest_sha256"`
	FullSHA256     string               `json:"full_sha256"`
	Aggregates     []censusAggregateDoc `json:"aggregates"`
	Fired          []censusFiredDoc     `json:"fired"`
	Strata         []censusStratumDoc   `json:"-"`
}

// censusAggregateDoc mirrors censusAggregate: one arm on one carrier of one
// stratum. See docs/llm/census/README.md for each count.
type censusAggregateDoc struct {
	Stratum            string `json:"stratum"`
	Carrier            string `json:"carrier"`
	Arm                string `json:"arm"`
	Observations       int    `json:"observations"`
	Files              int    `json:"files"`
	Trailer            int    `json:"trailer"`
	Blocked            int    `json:"blocked"`
	CallBlocked        int    `json:"call_blocked"`
	Suppressed         int    `json:"suppressed"`
	TrailerBytes       int    `json:"trailer_bytes"`
	AdversarialFixture int    `json:"adversarial_fixture"`
	Refused            int    `json:"refused"`
	Unobserved         int    `json:"unobserved"`
}

// censusFiredDoc mirrors censusObservation: one observation with a finding.
type censusFiredDoc struct {
	Stratum  string             `json:"stratum"`
	Path     string             `json:"path"`
	Carrier  string             `json:"carrier"`
	Origin   string             `json:"origin"`
	Bytes    int                `json:"bytes"`
	Refused  string             `json:"refused,omitempty"`
	Findings []censusFindingDoc `json:"findings"`
}

// censusFindingDoc mirrors censusFinding.
type censusFindingDoc struct {
	Interceptor string `json:"interceptor"`
	Rule        string `json:"rule"`
	Verdict     string `json:"verdict"`
	Target      string `json:"target"`
	Group       int    `json:"group"`
	Alt         int    `json:"alt"`
}

// censusStratumDoc is one manifest stratum: its registration, its file count,
// and the paths classed adversarial-fixture (BC5).
type censusStratumDoc struct {
	Name, Source, Commit, License string
	Files                         int
	Adversarial                   map[string]bool
}

// secretsRowRecord is one workflow row run under both arms (ruling R3).
type secretsRowRecord struct {
	Row   secretsRow
	G, GS secretsRowResult
}

// decodeStrict decodes exactly one JSON value, rejecting unknown fields, so
// a schema change in the census output cannot be dropped silently here.
func decodeStrict(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

// loadCensusDoc reads the committed census results and the manifest they were
// computed from; a manifest that no longer hashes to manifest_sha256 means the
// results are stale, and the report refuses to mix the two.
func loadCensusDoc(t *testing.T) censusResultsDoc {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "llm", "census")
	raw, err := os.ReadFile(filepath.Join(dir, "census-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc censusResultsDoc
	if err := decodeStrict(raw, &doc); err != nil {
		t.Fatalf("census-results.json: %v", err)
	}
	if doc.Schema != "llm-bench.census-results/v1" {
		t.Fatalf("census-results.json schema %q, want llm-bench.census-results/v1", doc.Schema)
	}
	mraw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(mraw); hex.EncodeToString(sum[:]) != doc.ManifestSHA256 {
		t.Fatal("census-results.json was computed from another manifest.json; rerun the census (docs/llm/census/README.md)")
	}
	var m struct {
		Schema string `json:"schema"`
		Strata []struct {
			Name    string `json:"name"`
			Source  string `json:"source"`
			Commit  string `json:"commit"`
			License string `json:"license"`
			Files   []struct {
				Path   string `json:"path"`
				SHA256 string `json:"sha256"`
				Class  string `json:"class,omitempty"`
			} `json:"files"`
		} `json:"strata"`
	}
	if err := decodeStrict(mraw, &m); err != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	for _, s := range m.Strata {
		st := censusStratumDoc{Name: s.Name, Source: s.Source, Commit: s.Commit, License: s.License, Files: len(s.Files), Adversarial: map[string]bool{}}
		for _, f := range s.Files {
			if f.Class == "adversarial-fixture" {
				st.Adversarial[f.Path] = true
			}
		}
		doc.Strata = append(doc.Strata, st)
	}
	return doc
}

type secretsEvidenceInputs struct {
	acc    []secrettest.Outcome
	census censusResultsDoc
	rows   []secretsRowRecord
}

// gatherSecretsEvidence evaluates the case table, loads the census and runs
// every workflow row under G and G+S. A row that cannot run on this host
// (rows 6 and 8 need the exec runner) skips the caller: the report is only
// regenerated where every row runs.
func gatherSecretsEvidence(t *testing.T) secretsEvidenceInputs {
	t.Helper()
	cases, err := secrettest.Cases()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := secrettest.Evaluate(t.Context(), cases)
	if err != nil {
		t.Fatal(err)
	}
	in := secretsEvidenceInputs{acc: acc, census: loadCensusDoc(t)}
	for _, run := range secretsRowRuns(t, nil) {
		for arm, name := range secretsArmNames {
			switch {
			case run.skipped[arm]:
				t.Skipf("workflow row %s under %s did not run on this host; the evidence report regenerates only where every row runs", run.row.ID, name)
			case !run.ran[arm]:
				t.Fatalf("workflow row %s under %s failed", run.row.ID, name)
			}
		}
		in.rows = append(in.rows, secretsRowRecord{Row: run.row, G: run.res[0], GS: run.res[1]})
	}
	return in
}

var secretsEvidenceMemo struct {
	sync.Mutex
	in *secretsEvidenceInputs
}

// committedSecretsEvidence is gatherSecretsEvidence run once per test binary.
// Callers that change the inputs must clone them first.
func committedSecretsEvidence(t *testing.T) secretsEvidenceInputs {
	t.Helper()
	secretsEvidenceMemo.Lock()
	defer secretsEvidenceMemo.Unlock()
	if secretsEvidenceMemo.in == nil {
		in := gatherSecretsEvidence(t)
		secretsEvidenceMemo.in = &in
	}
	return *secretsEvidenceMemo.in
}

// TestSecretsEvidenceDoc is the regeneration gate (Review Focus 4): two
// renders, the second from a fresh evaluation of the case table and a fresh
// load of the census, are byte-identical and equal the committed file. The
// workflow rows run once per test binary (secretsRowRuns, shared with
// TestSecretsWorkflowTable); a row that differs between runs makes the
// committed file stale.
func TestSecretsEvidenceDoc(t *testing.T) {
	first := committedSecretsEvidence(t)
	cases, err := secrettest.Cases()
	if err != nil {
		t.Fatal(err)
	}
	acc, err := secrettest.Evaluate(t.Context(), cases)
	if err != nil {
		t.Fatal(err)
	}
	got := renderSecretsEvidence(first.acc, first.census, first.rows)
	if again := renderSecretsEvidence(acc, loadCensusDoc(t), first.rows); got != again {
		t.Fatal("two renders of the Secrets evidence differ: the report is not deterministic")
	}
	if *updateSecretsEvidence {
		if err := os.WriteFile(secretsEvidencePath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(secretsEvidencePath)
	if err != nil {
		t.Fatalf("%v; generate it with: go test ./cmd/golem -run TestSecretsEvidenceDoc -update-secrets-evidence", err)
	}
	if got != string(want) {
		t.Errorf("%s is stale; regenerate it with: go test ./cmd/golem -run TestSecretsEvidenceDoc -update-secrets-evidence", secretsEvidencePath)
	}
}

// The no-live-credentials wording, verbatim from the plan's Global
// Constraints and spec §5.1.
const noLiveCredentials = "No live credentials; generated fixtures minimize committed secret-shaped material; GitHub enforcement is verified separately."

// TestSecretsEvidenceSections pins the report's sections, in order, the exact
// no-live-credentials wording, and ruling R9's per-row reading of misses.
func TestSecretsEvidenceSections(t *testing.T) {
	in := committedSecretsEvidence(t)
	doc := renderSecretsEvidence(in.acc, in.census, in.rows)
	at := 0
	for _, h := range []string{
		"## Accuracy by kind and carrier",
		"## Lookalikes",
		"## Fixtures",
		"## Coverage gaps",
		"## Census (Secrets side)",
		"## Workflow table",
		"## Gate evaluation (spec §5.6)",
		"## What this report does not claim",
	} {
		i := strings.Index(doc[at:], "\n"+h+"\n")
		if i < 0 {
			t.Fatalf("heading %q missing, or out of order after byte %d", h, at)
		}
		at += i + len(h)
	}
	if !strings.Contains(doc, noLiveCredentials) {
		t.Error("the exact no-live-credentials wording is missing")
	}

	// R9: every FN is listed by case ID under the accuracy section, and the
	// boundary probes among them are counted (20 of 28, from ruling R9; all
	// four pending class disagreements are lookalike-versus-benign, never FN).
	accuracy := section(t, doc, "## Accuracy by kind and carrier")
	fn := map[string]bool{}
	for _, o := range in.acc {
		if o.Score() == "FN" {
			fn[o.CaseID] = true
		}
	}
	for id := range fn {
		if !strings.Contains(accuracy, "`"+id+"`") {
			t.Errorf("FN case %s is not listed in the accuracy section", id)
		}
	}
	if len(fn) != 28 {
		t.Errorf("%d FN cases, want 28 (ruling R9)", len(fn))
	}
	if !strings.Contains(accuracy, "20 of the 28 FN cases are synthetic boundary probes") {
		t.Error("the accuracy section does not state that 20 of the 28 FN cases are synthetic boundary probes")
	}
	// Item 2's qualifier (spec §3 F9): the lookalike cases that fire in the
	// model's own write arguments, where a block is terminal, counted here
	// from the Evaluate outcomes, in the Lookalikes section and on item 2.
	terminalLookalikes := map[string]bool{}
	for _, o := range in.acc {
		if o.Class == "benign-lookalike" && o.Carrier == "tool-args" && o.Hook == "output" && o.Any {
			terminalLookalikes[o.CaseID] = true
		}
	}
	qualifier := fmt.Sprintf("%d lookalike cases fire at tool-args/output, which is terminal in production; no §5.5 sentinel row writes a lookalike, so item 2 measures the rows only.", len(terminalLookalikes))
	if !strings.Contains(section(t, doc, "## Lookalikes"), qualifier) {
		t.Errorf("the Lookalikes section lacks %q", qualifier)
	}
	if !regexp.MustCompile(`(?m)^- Item 2: (PASS|FAIL)\. .*` + regexp.QuoteMeta(qualifier)).MatchString(doc) {
		t.Errorf("gate item 2 lacks %q", qualifier)
	}
	// The coverage-gap families are the gap cases themselves (R24: a class
	// change is only a regeneration), each by ID and role.
	cases, err := secrettest.Cases()
	if err != nil {
		t.Fatal(err)
	}
	gaps := section(t, doc, "## Coverage gaps")
	nGaps := 0
	for _, c := range cases {
		if c.CoverageGap {
			nGaps++
			if line := "\n- `" + c.ID + "`: " + c.Role + "\n"; !strings.Contains(gaps, line) {
				t.Errorf("the Coverage gaps section does not name gap case %s by its role", c.ID)
			}
		}
	}
	if nGaps == 0 {
		t.Error("the case table holds no coverage-gap case")
	}
	// R6/R9: n is printed beside every rate.
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "%") || regexp.MustCompile(`\b[01]\.\d\d\b`).MatchString(line) {
			if !strings.Contains(line, "n=") && !strings.Contains(line, "/") {
				t.Errorf("a rate without its n: %q", line)
			}
		}
	}
}

// firedSecretsTally is the test's own derivation of the census's Secrets
// side, from the committed fired rows and the manifest strata rather than the
// aggregates and renderer code: an observation counts when a Secrets finding
// blocks it, no guard blocks it (G would too) and no detector blocks its call
// (G+S never reaches the hooks; cmd/llm-bench deriveArm).
type firedSecretsTally struct {
	blocks     map[[2]string]int             // (stratum, carrier): benign observations
	callBlocks int                           // of those, blocks of the call
	files      map[string]map[string]bool    // stratum: benign paths, any file carrier
	ruleFiles  map[[2]string]map[string]bool // (stratum, rule without sensitive_): benign paths
	advObs     map[string]int                // stratum: adversarial-fixture observations
	advFiles   map[string]map[string]bool    // stratum: adversarial-fixture paths
}

func tallyFiredSecrets(c censusResultsDoc) firedSecretsTally {
	adversarial := map[[2]string]bool{}
	for _, s := range c.Strata {
		for p := range s.Adversarial {
			adversarial[[2]string{s.Name, p}] = true
		}
	}
	add := func(m map[string]map[string]bool, k, path string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][path] = true
	}
	tally := firedSecretsTally{blocks: map[[2]string]int{}, files: map[string]map[string]bool{}, ruleFiles: map[[2]string]map[string]bool{},
		advObs: map[string]int{}, advFiles: map[string]map[string]bool{}}
	for _, o := range c.Fired {
		var rules []string
		guard, detectorCall, call := false, false, false
		for _, f := range o.Findings {
			if f.Verdict != "block" {
				continue
			}
			ingress := f.Target == "message" || f.Target == "alternative"
			switch f.Interceptor {
			case "secrets":
				rules = append(rules, strings.TrimPrefix(f.Rule, "sensitive_"))
				call = call || !ingress
			case "invariants", "egress", "child_scope_denials":
				guard = true
			default:
				detectorCall = detectorCall || !ingress
			}
		}
		if len(rules) == 0 || guard || detectorCall {
			continue
		}
		if o.Carrier != "search" && adversarial[[2]string{o.Stratum, o.Path}] {
			tally.advObs[o.Stratum]++
			add(tally.advFiles, o.Stratum, o.Path)
			continue
		}
		tally.blocks[[2]string{o.Stratum, o.Carrier}]++
		if call {
			tally.callBlocks++
		}
		if o.Carrier == "search" {
			continue
		}
		add(tally.files, o.Stratum, o.Path)
		for _, r := range rules {
			k := [2]string{o.Stratum, r}
			if tally.ruleFiles[k] == nil {
				tally.ruleFiles[k] = map[string]bool{}
			}
			tally.ruleFiles[k][o.Path] = true
		}
	}
	return tally
}

// section returns the text of the "## " section headed h, up to the next one.
func section(t *testing.T, doc, h string) string {
	t.Helper()
	i := strings.Index(doc, "\n"+h+"\n")
	if i < 0 {
		t.Fatalf("section %q missing", h)
	}
	rest := doc[i+len(h)+2:]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// The frozen budgets (spec §2.1, K1 and K4), spelled out here and never read
// from the renderer.
const (
	k1TaskFailureBudget = 0   // terminal blocks on benign or lookalike sentinel workflows
	k1BlockBudgetNum    = 1   // unnecessary blocks ...
	k1BlockBudgetDen    = 100 // ... per this many benign observations, in every stratum
)

var gateItemLine = regexp.MustCompile(`(?m)^- Item ([1-6]): (PASS|FAIL)\.`)

// gateVerdicts parses the six "- Item N: PASS|FAIL." lines of the gate section.
func gateVerdicts(t *testing.T, doc string) map[int]bool {
	t.Helper()
	gate := section(t, doc, "## Gate evaluation (spec §5.6)")
	got := map[int]bool{}
	for _, m := range gateItemLine.FindAllStringSubmatch(gate, -1) {
		got[int(m[1][0]-'0')] = m[2] == "PASS"
	}
	if len(got) != 6 {
		t.Fatalf("gate section evaluates %d items, want the six of spec §5.6:\n%s", len(got), gate)
	}
	return got
}

// decisionLine returns the gate section's "Secrets default:" line.
func decisionLine(t *testing.T, doc string) string {
	t.Helper()
	for _, line := range strings.Split(section(t, doc, "## Gate evaluation (spec §5.6)"), "\n") {
		if strings.Contains(line, "Secrets default:") {
			return line
		}
	}
	t.Fatal("the gate section prints no Secrets default")
	return ""
}

// TestSecretsGateEvaluation evaluates the six spec §5.6 items itself, from the
// inputs and the literal K1/K4 budgets, and requires the renderer to agree;
// with the committed inputs the decision is retain opt-in, naming item 4 (row
// 3 terminal) and item 6 (#518).
func TestSecretsGateEvaluation(t *testing.T) {
	in := committedSecretsEvidence(t)
	cases, err := secrettest.Cases()
	if err != nil {
		t.Fatal(err)
	}
	mandatory := map[string]bool{}
	for _, c := range cases {
		mandatory[c.ID] = c.Mandatory
	}

	// want evaluates the items independently of the renderer.
	want := func(in secretsEvidenceInputs) map[int]bool {
		v := map[int]bool{1: true, 2: true, 3: true, 4: true, 5: false, 6: false}
		for _, o := range in.acc { // item 1: every mandatory outcome fires
			if mandatory[o.CaseID] && !o.Any {
				v[1] = false
			}
		}
		terminal := 0 // item 2: K1 task-failure budget
		for _, r := range in.rows {
			benign := r.Row.Class == "benign" || r.Row.Class == "benign-lookalike"
			if benign && r.GS.RunOutcome == "terminal" && r.G.RunOutcome != "terminal" {
				terminal++
			}
		}
		tally := tallyFiredSecrets(in.census)
		sBlocks, sCallBlocks := tally.blocks, tally.callBlocks
		if terminal+sCallBlocks > k1TaskFailureBudget {
			v[2] = false
		}
		for _, a := range in.census.Aggregates {
			if a.Arm == "guards+secrets" && sBlocks[[2]string{a.Stratum, a.Carrier}]*k1BlockBudgetDen > a.Observations*k1BlockBudgetNum {
				v[3] = false
			}
		}
		rows14, protected := 0, 0 // item 5: rows 14a-c deliver under G only
		for _, r := range in.rows {
			if strings.HasPrefix(r.Row.ID, "14") {
				rows14++
				if r.G.Delivered && !r.GS.Delivered {
					protected++
				}
			}
		}
		v[5] = rows14 == 3 && protected == 3
		for _, r := range in.rows { // item 4: K4, plus completion losses
			if r.Row.Class == "synthetic-fixture" && (r.GS.RunOutcome == "terminal" || r.G.ContinuationSucceeds && !r.GS.ContinuationSucceeds) {
				v[4] = false
			}
		}
		return v // item 6: §8.4 needs #518, which has not landed
	}

	doc := renderSecretsEvidence(in.acc, in.census, in.rows)
	got := gateVerdicts(t, doc)
	if w := want(in); !maps.Equal(got, w) {
		t.Errorf("committed inputs: gate items %v, want %v", got, w)
	}
	// Every census number the report prints, re-derived from the committed
	// fired rows and manifest strata (spec §5.4: by observation and by file).
	tally := tallyFiredSecrets(in.census)
	censusText := section(t, doc, "## Census (Secrets side)")
	advDenominator := map[string]int{}
	for _, a := range in.census.Aggregates {
		if a.Arm != "guards+secrets" {
			continue
		}
		advDenominator[a.Stratum] += a.AdversarialFixture
		row := fmt.Sprintf("| %s | %s | %d | %d | %d/%d (", a.Stratum, a.Carrier, a.Observations, a.Files, tally.blocks[[2]string{a.Stratum, a.Carrier}], a.Observations)
		if !strings.Contains(censusText, row) {
			t.Errorf("by-observation table has no row starting %q", row)
		}
	}
	for _, s := range in.census.Strata {
		n := s.Files - len(s.Adversarial)
		if row := fmt.Sprintf("\n| %s | %d | %d/%d (", s.Name, n, len(tally.files[s.Name]), n); !strings.Contains(censusText, row) {
			t.Errorf("by-file table has no row starting %q", row[1:])
		}
		want := fmt.Sprintf("\n- %s: no file is classed adversarial-fixture.\n", s.Name)
		if len(s.Adversarial) > 0 {
			want = fmt.Sprintf("\n- %s: Secrets blocks %d/%d (", s.Name, tally.advObs[s.Name], advDenominator[s.Name])
			files := fmt.Sprintf(" in %d/%d (", len(tally.advFiles[s.Name]), len(s.Adversarial))
			i := strings.Index(censusText, want)
			if line, _, _ := strings.Cut(censusText[max(i, 0)+1:], "\n"); i < 0 || !strings.Contains(line, files) {
				t.Errorf("adversarial-fixture line for %s: want %q with %q", s.Name, want[1:], files)
			}
			continue
		}
		if !strings.Contains(censusText, want) {
			t.Errorf("adversarial-fixture line for %s: want %q", s.Name, want[1:])
		}
	}
	// Item 3 read per stratum (K1, BC5): Secrets blocks from the fired rows
	// over the G+S benign observations, summed over every carrier.
	poolBlocks, poolObs := map[string]int{}, map[string]int{}
	for _, a := range in.census.Aggregates {
		if a.Arm == "guards+secrets" {
			poolBlocks[a.Stratum] += tally.blocks[[2]string{a.Stratum, a.Carrier}]
			poolObs[a.Stratum] += a.Observations
		}
	}
	item3 := regexp.MustCompile(`(?m)^- Item 3: .*$`).FindString(doc)
	var pooledOver []string
	for _, st := range slices.Sorted(maps.Keys(poolObs)) {
		if !strings.Contains(item3, fmt.Sprintf(" %s %d/%d (", st, poolBlocks[st], poolObs[st])) {
			t.Errorf("item 3 has no pooled line %s %d/%d: %q", st, poolBlocks[st], poolObs[st], item3)
		}
		if poolBlocks[st]*k1BlockBudgetDen > poolObs[st]*k1BlockBudgetNum {
			pooledOver = append(pooledOver, st)
		}
	}
	if len(pooledOver) == 0 {
		pooledOver = []string{"none"}
	}
	if want := "over budget read that way: " + strings.Join(pooledOver, ", ") + "."; !strings.Contains(item3, want) {
		t.Errorf("item 3 lacks %q", want)
	}
	if !strings.Contains(item3, "The verdict uses the per-cell reading") {
		t.Error("item 3 does not say which reading its verdict uses")
	}
	gotRules := map[[2]string]int{}
	for _, m := range regexp.MustCompile("(?m)^\\| (\\S+) \\| `([a-z_]+)` \\| (\\d+) \\|$").FindAllStringSubmatch(censusText, -1) {
		n, _ := strconv.Atoi(m[3])
		gotRules[[2]string{m[1], m[2]}] = n
	}
	wantRules := map[[2]string]int{}
	for k, paths := range tally.ruleFiles {
		wantRules[k] = len(paths)
	}
	if !maps.Equal(gotRules, wantRules) {
		t.Errorf("per-rule file table %v, want %v", gotRules, wantRules)
	}
	line := decisionLine(t, doc)
	for _, s := range []string{"Secrets default: retain opt-in", "item 4 (row 3 terminal", "item 6 (#518"} {
		if !strings.Contains(line, s) {
			t.Errorf("decision line %q does not contain %q", line, s)
		}
	}
	if got[3] || got[4] || got[6] || !got[1] || !got[2] || !got[5] {
		t.Errorf("committed inputs: gate items %v, want items 3, 4 and 6 failing and 1, 2, 5 passing", got)
	}

	// Each item moves with its own input; the budgets are checked at their
	// boundaries (Review Focus 5: 1 in 100 passes, 2 in 199 fails).
	clone := func() secretsEvidenceInputs {
		c := in
		c.acc, c.rows = slices.Clone(in.acc), slices.Clone(in.rows)
		c.census.Aggregates = slices.Clone(in.census.Aggregates)
		return c
	}
	// oneCell is a census of one stratum and carrier whose fired rows and
	// aggregates agree: blocks observations blocked by Secrets, calls of them
	// at the call.
	oneCell := func(blocks, calls, observations int) censusResultsDoc {
		c := censusResultsDoc{Schema: in.census.Schema, ManifestSHA256: in.census.ManifestSHA256, Aggregates: []censusAggregateDoc{
			{Stratum: "s", Carrier: "mcp", Arm: "golem-default-guards", Observations: observations, Files: observations},
			{Stratum: "s", Carrier: "mcp", Arm: "guards+secrets", Observations: observations, Files: observations, Blocked: blocks - calls, CallBlocked: calls},
		}, Fired: []censusFiredDoc{}, Strata: []censusStratumDoc{{Name: "s", Files: observations, Adversarial: map[string]bool{}}}}
		for i := range blocks {
			target := "message"
			if i < calls {
				target = "output_tool_call"
			}
			c.Fired = append(c.Fired, censusFiredDoc{Stratum: "s", Path: fmt.Sprintf("f%d.go", i), Carrier: "mcp", Origin: "foreign",
				Findings: []censusFindingDoc{{Interceptor: "secrets", Rule: "sensitive_payment_card", Verdict: "block", Target: target, Group: -1, Alt: -1}}})
		}
		return c
	}
	setRow := func(c *secretsEvidenceInputs, id string, f func(*secretsRowRecord)) {
		i := slices.IndexFunc(c.rows, func(r secretsRowRecord) bool { return r.Row.ID == id })
		if i < 0 {
			t.Fatalf("no workflow row %s", id)
		}
		f(&c.rows[i])
	}
	for _, tt := range []struct {
		name   string
		mutate func(*secretsEvidenceInputs)
		item   int
		pass   bool
	}{
		{"item 3 at exactly 1 in 100", func(c *secretsEvidenceInputs) { c.census = oneCell(1, 0, 100) }, 3, true},
		{"item 3 at 2 in 199", func(c *secretsEvidenceInputs) { c.census = oneCell(2, 0, 199) }, 3, false},
		{"item 3 with no blocks", func(c *secretsEvidenceInputs) { c.census = oneCell(0, 0, 10) }, 3, true},
		{"item 3 counts call blocks: 2 in 199 at the call", func(c *secretsEvidenceInputs) { c.census = oneCell(2, 2, 199) }, 3, false},
		{"item 3 leaves a guard's block to G: 2 blocked, 1 by a guard, in 100", func(c *secretsEvidenceInputs) {
			c.census = oneCell(2, 0, 100)
			c.census.Aggregates[0].Blocked = 1
			c.census.Fired[1].Findings = append(c.census.Fired[1].Findings, censusFindingDoc{Interceptor: "invariants", Rule: "credential_path", Verdict: "block", Target: "tool_call", Group: -1, Alt: -1})
		}, 3, true},
		{"item 2: one census call block", func(c *secretsEvidenceInputs) { c.census = oneCell(1, 1, 100) }, 2, false},
		{"item 2: a lookalike workflow ends terminal", func(c *secretsEvidenceInputs) {
			setRow(c, "1", func(r *secretsRowRecord) {
				r.GS = secretsRowResult{FirstBoundary: "secrets", Hook: "input", RunOutcome: "terminal", Message: "run blocked: sensitive content detected"}
			})
		}, 2, false},
		{"item 1: a mandatory case misses", func(c *secretsEvidenceInputs) {
			i := slices.IndexFunc(c.acc, func(o secrettest.Outcome) bool { return mandatory[o.CaseID] })
			c.acc[i].Any, c.acc[i].Kinds = false, nil
		}, 1, false},
		{"item 5: G+S delivers on row 14b", func(c *secretsEvidenceInputs) {
			setRow(c, "14b", func(r *secretsRowRecord) { r.GS.Delivered = true })
		}, 5, false},
		{"item 5: G does not deliver on row 14c", func(c *secretsEvidenceInputs) {
			setRow(c, "14c", func(r *secretsRowRecord) { r.G.Delivered = false })
		}, 5, false},
		{"item 4: only row 6 loses its continuation", func(c *secretsEvidenceInputs) {
			for i, r := range c.rows {
				if r.Row.Class == "synthetic-fixture" && r.Row.ID != "6" {
					c.rows[i].GS = r.G
				}
			}
		}, 4, false},
		{"item 4: no fixture workflow loses anything", func(c *secretsEvidenceInputs) {
			for i, r := range c.rows {
				if r.Row.Class == "synthetic-fixture" {
					c.rows[i].GS = r.G
				}
			}
		}, 4, true},
		{"items 1 to 5 pass: item 6 alone keeps Secrets opt-in", func(c *secretsEvidenceInputs) {
			c.census = oneCell(1, 0, 100)
			for i, r := range c.rows {
				if r.Row.Class == "synthetic-fixture" {
					c.rows[i].GS = r.G
				}
			}
		}, 6, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := clone()
			tt.mutate(&c)
			doc := renderSecretsEvidence(c.acc, c.census, c.rows)
			got := gateVerdicts(t, doc)
			if got[tt.item] != tt.pass {
				t.Errorf("item %d PASS = %t, want %t", tt.item, got[tt.item], tt.pass)
			}
			if w := want(c); !maps.Equal(got, w) {
				t.Errorf("gate items %v, want %v", got, w)
			}
			line := decisionLine(t, doc)
			if !strings.Contains(line, "Secrets default: retain opt-in") || !strings.Contains(line, "item 6 (#518") {
				t.Errorf("decision line %q: item 6 alone keeps Secrets opt-in", line)
			}
			if named := strings.Contains(line, fmt.Sprintf("item %d (", tt.item)); named == tt.pass {
				t.Errorf("decision line %q names item %d = %t, want %t", line, tt.item, named, !tt.pass)
			}
		})
	}
}

// The census arms the Secrets side compares (plan Global Constraints).
const (
	armG  = "golem-default-guards"
	armGS = "guards+secrets"
)

// The gate's frozen budgets (spec §2.1, K1 and K4).
const (
	taskFailureBudget = 0 // terminal blocks on benign or lookalike sentinel workflows
	blockBudgetNum    = 1 // unnecessary blocks per blockBudgetDen benign observations,
	blockBudgetDen    = 100
	// issue518Landed is spec §8.4: a Secrets default flip is hard-blocked on
	// #518 (versioned bounded block reporting and its v1/v2 selection path for
	// Firn, coordinated with #506). Set it only when #518 has merged.
	issue518Landed = false
)

// censusStrataPlanned are the four census strata of spec §2.2; a planned
// stratum missing from the results is reported as pending.
var censusStrataPlanned = []string{"go-llm", "go-src", "llm-app", "i18n-emoji"}

// secretsCaseIndex is the embedded case table by ID, for each outcome's role
// and mandatory flag.
func secretsCaseIndex() map[string]secrettest.Case {
	cases, err := secrettest.Cases()
	if err != nil {
		panic(err) // TestCasesValidate owns this failure
	}
	byID := map[string]secrettest.Case{}
	for _, c := range cases {
		byID[c.ID] = c
	}
	return byID
}

// boundaryProbe names the synthetic boundary probe a case ID declares, or "".
// ponytail: read from the ID suffixes the table uses; add a Case field if a
// probe is ever named otherwise.
func boundaryProbe(id string) string {
	switch {
	case strings.Contains(id, "-below-min"):
		return "one byte below the minimum length"
	case strings.HasSuffix(id, "-strings-glued"):
		return "prefix glued to the preceding string"
	case strings.Contains(id, "-truncated"):
		return "truncated PEM"
	}
	return ""
}

// secretsCensusCell is the Secrets side of one (stratum, carrier): what G+S
// blocks that G does not.
type secretsCensusCell struct {
	stratum, carrier                              string
	observations, files, blocks, callBlocks       int
	refused, adversarial, unobserved, guardBlocks int
}

func secretsCensusCells(c censusResultsDoc) []secretsCensusCell {
	g := map[[2]string]censusAggregateDoc{}
	for _, a := range c.Aggregates {
		if a.Arm == armG {
			g[[2]string{a.Stratum, a.Carrier}] = a
		}
	}
	var cells []secretsCensusCell
	for _, a := range c.Aggregates { // sorted by stratum, carrier, arm
		if a.Arm != armGS {
			continue
		}
		base := g[[2]string{a.Stratum, a.Carrier}]
		cells = append(cells, secretsCensusCell{
			stratum: a.Stratum, carrier: a.Carrier, observations: a.Observations, files: a.Files,
			blocks:     a.Blocked + a.CallBlocked - base.Blocked - base.CallBlocked,
			callBlocks: a.CallBlocked - base.CallBlocked,
			refused:    a.Refused, adversarial: a.AdversarialFixture, unobserved: a.Unobserved,
			guardBlocks: base.Blocked + base.CallBlocked,
		})
	}
	return cells
}

// withinBudget is K1's unnecessary-block budget: at most 1 block per 100.
func withinBudget(blocks, observations int) bool {
	return blocks*blockBudgetDen <= observations*blockBudgetNum
}

// secretsBlocks reports whether G+S blocks a fired observation and G does
// not: cmd/llm-bench deriveArm for the two arms. A guard's block (G blocks
// too) or a detector's block of the call (G+S never reaches the hooks) is not
// Secrets'.
func secretsBlocks(o censusFiredDoc) bool {
	s := false
	for _, f := range o.Findings {
		if f.Verdict != "block" {
			continue
		}
		switch {
		case f.Interceptor == "secrets":
			s = true
		case f.Interceptor == "invariants" || f.Interceptor == "egress" || f.Interceptor == "child_scope_denials":
			return false
		case f.Target != "message" && f.Target != "alternative":
			return false
		}
	}
	return s
}

func secretsTerminal(r secretsRowResult) bool {
	return r.RunOutcome == "terminal" && r.FirstBoundary == "secrets"
}

func rowNumber(id string) string { return strings.TrimRight(id, "abcdefghijklmnopqrstuvwxyz") }

// pct renders k of n with the rate as a percentage; n is always beside it.
func pct(k, n int) string {
	if n == 0 {
		return fmt.Sprintf("%d/0", k)
	}
	return fmt.Sprintf("%d/%d (%.2f%%)", k, n, 100*float64(k)/float64(n))
}

// ratio renders k of n with the rate as a fraction; n is always beside it.
func ratio(k, n int) string {
	if n == 0 {
		return "- (n=0)"
	}
	return fmt.Sprintf("%.2f (%d/%d)", float64(k)/float64(n), k, n)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func orNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ", ")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// rowList names workflow rows: "none", "row 6" or "rows 2c, 11".
func rowList(ids []string) string {
	switch len(ids) {
	case 0:
		return "none"
	case 1:
		return "row " + ids[0]
	}
	return "rows " + strings.Join(ids, ", ")
}

// secretsGateItem is one spec §5.6 item: its verdict, the criterion with its
// evidence, and a short reason the decision line names when it fails.
type secretsGateItem struct {
	pass         bool
	text, reason string
}

// lookalikeTerminal is the item 2 qualifier: how many lookalike cases fire in
// the model's own write arguments (tool-args at hook output), where a block
// is terminal in production, and why item 2 still reads the rows only.
func lookalikeTerminal(acc []secrettest.Outcome) string {
	n := 0
	for _, o := range acc {
		if o.Class == secrettest.BenignLookalike && o.Carrier == secrettest.ToolArgs && o.Hook == "output" && o.Any {
			n++
		}
	}
	return fmt.Sprintf("%d lookalike cases fire at tool-args/output, which is terminal in production; no §5.5 sentinel row writes a lookalike, so item 2 measures the rows only.", n)
}

// secretsGate evaluates the six spec §5.6 items against the frozen budgets.
func secretsGate(acc []secrettest.Outcome, census censusResultsDoc, rows []secretsRowRecord) [6]secretsGateItem {
	var items [6]secretsGateItem
	byID := secretsCaseIndex()

	mandatory, outs := map[string]bool{}, 0
	var missed []string
	for _, o := range acc {
		if !byID[o.CaseID].Mandatory {
			continue
		}
		mandatory[o.CaseID] = true
		outs++
		if !o.Any && !slices.Contains(missed, o.CaseID) {
			missed = append(missed, o.CaseID)
		}
	}
	items[0] = secretsGateItem{
		pass: outs > 0 && len(missed) == 0,
		text: fmt.Sprintf("Correct detection on every mandatory supported case. %d mandatory cases (%d outcomes, every carrier and hook); missed: %s.",
			len(mandatory), outs, orNone(missed)),
		reason: "mandatory misses: " + orNone(missed),
	}

	cells := secretsCensusCells(census)
	var benignRows, benignTerminal []string
	for _, r := range rows {
		if r.Row.Class == string(secrettest.Benign) || r.Row.Class == string(secrettest.BenignLookalike) {
			benignRows = append(benignRows, r.Row.ID)
			if secretsTerminal(r.GS) {
				benignTerminal = append(benignTerminal, r.Row.ID)
			}
		}
	}
	callBlocks := 0
	for _, c := range cells {
		callBlocks += c.callBlocks
	}
	items[1] = secretsGateItem{
		pass: len(benignTerminal)+callBlocks <= taskFailureBudget,
		text: fmt.Sprintf("Zero terminal blocks on benign and lookalike sentinel workflows (K1 task-failure budget: %d). "+
			"Benign and lookalike workflow rows (%s) ending terminal under G+S: %s. "+
			"Census call blocks by Secrets, an upper bound on terminal blocks because `call_blocked` also counts recoverable tool-call blocks: %d. %s",
			taskFailureBudget, strings.Join(benignRows, ", "), orNone(benignTerminal), callBlocks, lookalikeTerminal(acc)),
		reason: fmt.Sprintf("terminal blocks on benign or lookalike workflows: %s; census call blocks: %d", rowList(benignTerminal), callBlocks),
	}

	var over, strata, pooled, pooledOver []string
	poolBlocks, poolObs := map[string]int{}, map[string]int{}
	for _, c := range cells {
		if !withinBudget(c.blocks, c.observations) {
			over = append(over, fmt.Sprintf("%s/%s %s", c.stratum, c.carrier, pct(c.blocks, c.observations)))
		}
		if !slices.Contains(strata, c.stratum) {
			strata = append(strata, c.stratum)
		}
		poolBlocks[c.stratum] += c.blocks
		poolObs[c.stratum] += c.observations
	}
	for _, st := range strata {
		pooled = append(pooled, st+" "+pct(poolBlocks[st], poolObs[st]))
		if !withinBudget(poolBlocks[st], poolObs[st]) {
			pooledOver = append(pooledOver, st)
		}
	}
	items[2] = secretsGateItem{
		pass: len(cells) > 0 && len(over) == 0,
		text: fmt.Sprintf("The false-block rate on benign plus lookalike content, by observation, within K1's unnecessary-block budget of %d per %d in every stratum, "+
			"on the census (the benign side, spec §5.4), checked per (stratum, carrier) cell. Over budget: %s. "+
			"Pooled per stratum over every carrier, the reading K1 and BC5 phrase (benign Secrets blocks / benign observations): %s; over budget read that way: %s. "+
			"The verdict uses the per-cell reading, which fails whenever the per-stratum one does.",
			blockBudgetNum, blockBudgetDen, orNone(over), strings.Join(pooled, ", "), orNone(pooledOver)),
		reason: fmt.Sprintf("census false-block rate above %d/%d in %d of %d stratum and carrier cells", blockBudgetNum, blockBudgetDen, len(over), len(cells)),
	}

	var fixtureTerminal, fixtureLost []string
	row3Terminal := false
	for _, r := range rows {
		if r.Row.Class != string(secrettest.SyntheticFixture) {
			continue
		}
		switch {
		case secretsTerminal(r.GS) && rowNumber(r.Row.ID) == "3":
			row3Terminal = true
		case secretsTerminal(r.GS):
			fixtureTerminal = append(fixtureTerminal, r.Row.ID)
		case r.G.ContinuationSucceeds && !r.GS.ContinuationSucceeds:
			fixtureLost = append(fixtureLost, r.Row.ID)
		}
	}
	var why []string
	if row3Terminal {
		why = append(why, "row 3 terminal")
	}
	if len(fixtureTerminal) > 0 {
		why = append(why, "also terminal: "+rowList(fixtureTerminal))
	}
	if len(fixtureLost) > 0 {
		why = append(why, "continuation lost without a terminal block: "+rowList(fixtureLost))
	}
	items[3] = secretsGateItem{
		pass: !row3Terminal && len(fixtureTerminal) == 0 && len(fixtureLost) == 0,
		text: fmt.Sprintf("Fixture-workflow completion losses within the K4 decision: a terminal block on a deliberate fixture workflow disqualifies a default-on Secrets, and row 3 alone decides K4. "+
			"Row 3 terminal under G+S: %s. Other synthetic-fixture rows terminal under G+S: %s. Fixture rows whose continuation G keeps and G+S loses without a terminal block: %s.",
			yesNo(row3Terminal), orNone(fixtureTerminal), orNone(fixtureLost)),
		reason: strings.Join(why, "; "),
	}

	var protected, unprotected []string
	for _, r := range rows {
		if rowNumber(r.Row.ID) != "14" {
			continue
		}
		if r.G.Delivered && !r.GS.Delivered {
			protected = append(protected, r.Row.ID)
		} else {
			unprotected = append(unprotected, r.Row.ID)
		}
	}
	items[4] = secretsGateItem{
		pass: len(protected) > 0 && len(unprotected) == 0,
		text: fmt.Sprintf("A protection benefit on expected-sensitive surrogates, shown by the deterministic protection rows (BC4): the surrogate reaches its sink under G and not under G+S. "+
			"Protected: %s; not protected: %s.", rowList(protected), rowList(unprotected)),
		reason: "no protection on " + rowList(unprotected),
	}

	met, landed := "not met", "has not landed"
	if issue518Landed {
		met, landed = "met", "has landed"
	}
	items[5] = secretsGateItem{
		pass: issue518Landed,
		text: fmt.Sprintf("Spec §8.4 (a Secrets flip requires #518's versioned block reporting): %s. A Secrets default flip is hard-blocked on #518 (versioned bounded block reporting and the explicit v1/v2 selection path for Firn, coordinated with #506), with no interactive-only default; #518 %s.",
			met, landed),
		reason: "#518 not landed, spec §8.4",
	}
	return items
}

// handleLuhnProbability is the exact probability that a background job
// handle holds a maximal run of 13 to 19 decimal digits whose last digit makes
// it pass Luhn. A handle is "bg-" plus 32 hex digits from 16 uniform random
// bytes (agent/tools background_manager.go readHandle): each position is a
// decimal digit with probability 10/16, independently, and a run of uniform
// digits passes Luhn with probability 1/10. clean[L] is the probability that
// the positions read so far hold no flagged run and end in a run of L digits.
func handleLuhnProbability() *big.Rat {
	const positions = 32
	digit, letter := big.NewRat(10, 16), big.NewRat(6, 16)
	survive := func(run int) *big.Rat { // a closing run is not flagged
		if run >= 13 && run <= 19 {
			return big.NewRat(9, 10)
		}
		return big.NewRat(1, 1)
	}
	clean := []*big.Rat{big.NewRat(1, 1)}
	for range positions {
		next := make([]*big.Rat, len(clean)+1)
		for i := range next {
			next[i] = new(big.Rat)
		}
		for run, p := range clean {
			next[run+1].Add(next[run+1], new(big.Rat).Mul(p, digit))
			next[0].Add(next[0], new(big.Rat).Mul(new(big.Rat).Mul(p, letter), survive(run)))
		}
		clean = next
	}
	none := new(big.Rat)
	for run, p := range clean {
		none.Add(none, new(big.Rat).Mul(p, survive(run)))
	}
	return new(big.Rat).Sub(big.NewRat(1, 1), none)
}

// renderSecretsEvidence renders docs/security/secrets-evidence.md: the
// accuracy report from the case table, the Secrets side of the census, the
// workflow table under G and G+S, and the spec §5.6 gate. Everything printed
// is a case ID, role, kind, rule, path or count: never a rendered value.
func renderSecretsEvidence(acc []secrettest.Outcome, census censusResultsDoc, rows []secretsRowRecord) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	byID := secretsCaseIndex()
	gate := secretsGate(acc, census, rows)
	var failed []string
	for i, it := range gate {
		if !it.pass {
			failed = append(failed, fmt.Sprintf("item %d (%s)", i+1, it.reason))
		}
	}
	decision := "**Secrets default: default-on is supported by this evidence.**"
	if len(failed) > 0 {
		decision = "**Secrets default: retain opt-in.** Failed criteria: " + strings.Join(failed, ", ") + "."
	}

	classOrder := []secrettest.Class{secrettest.ExpectedSensitive, secrettest.SyntheticFixture, secrettest.BenignLookalike, secrettest.Benign, secrettest.Unclassified}
	caseClass := map[string]secrettest.Class{}
	var caseOrder []string
	for _, o := range acc {
		if _, ok := caseClass[o.CaseID]; !ok {
			caseClass[o.CaseID] = o.Class
			caseOrder = append(caseOrder, o.CaseID)
		}
	}
	var classCounts []string
	for _, c := range classOrder {
		n := 0
		for _, id := range caseOrder {
			if caseClass[id] == c {
				n++
			}
		}
		classCounts = append(classCounts, fmt.Sprintf("%s %d", c, n))
	}

	p("# Secrets evidence (#517)\n\n")
	p("Generated by `go test ./cmd/golem -run TestSecretsEvidenceDoc -update-secrets-evidence`; do not edit by hand. A regeneration gate renders it twice from fresh inputs and compares both renders with this file.\n\n")
	p("This is the Secrets half of #517 (spec §5): an accuracy report and a workflow-disruption report, kept apart, for `interceptor.Secrets` as shipped (shape-only, no threshold tuned on these results). %s\n\n", noLiveCredentials)
	p("Inputs:\n\n")
	p("- the case table `internal/secretscan/secrettest/cases.json`: %d cases (%s), classed by documented role before any scanning (spec §5.2) and inspected by `secrettest.Evaluate`;\n", len(caseOrder), strings.Join(classCounts, ", "))
	p("- the committed census results `docs/llm/census/census-results.json` (`%s`, manifest sha256 `%s…`);\n", census.Schema, short(census.ManifestSHA256))
	p("- the workflow table `cmd/golem/secrets_workflow_test.go`: %d rows, each run under G (the always-on guards) and G+S (guards plus Secrets).\n\n", len(rows))
	p("%s\n", decision)

	// Accuracy.
	p("\n## Accuracy by kind and carrier\n\n")
	p("Truth is the case's class. `Outcome.Score` puts an expected-sensitive outcome in TP or FN and a benign one in FP or TN; lookalikes, fixtures, unclassified cases and coverage gaps stay outside this matrix (their own tables follow). " +
		"An observation case is inspected once, at hook `input`, as a `read_file` tool message with workspace origin; a tool-args case twice, at hooks `output` and `tool_call`, as the JSON string field `content` of one `write_file` call. " +
		"The case table is a stratified probe set, not a population: every rate is per cell with its n, and no pooled recall is given.\n\n")
	kindLabel := func(k string) string {
		if k == "" {
			return "(no kind)"
		}
		return "`" + k + "`"
	}
	p("| Kind | Carrier | Hook | TP | FN | Recall | FP | TN | FP rate |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range secrettest.Confusions(acc) {
		p("| %s | %s | %s | %d | %d | %s | %d | %d | %s |\n", kindLabel(c.Kind), c.Carrier, c.Hook, c.TP, c.FN, ratio(c.TP, c.TP+c.FN), c.FP, c.TN, ratio(c.FP, c.FP+c.TN))
	}
	p("\n### Any finding versus kind attribution\n\n")
	p("`Scan` merges overlapping spans by precedence, so a case can fire under another kind (spec §5.4). Fired counts the expected-sensitive, non-gap outcomes where any finding fired; Attributed counts those whose findings named the case's own kind.\n\n")
	p("| Kind | Carrier | Hook | Fired | Attributed |\n|---|---|---|---|---|\n")
	for _, a := range secrettest.Attribution(acc) {
		p("| %s | %s | %s | %d | %s |\n", kindLabel(a.Kind), a.Carrier, a.Hook, a.Fired, ratio(a.Attributed, a.Fired))
	}

	type miss struct {
		id, kind string
		where    []string
	}
	var misses []*miss
	for _, o := range acc {
		if o.Score() != "FN" {
			continue
		}
		i := slices.IndexFunc(misses, func(m *miss) bool { return m.id == o.CaseID })
		if i < 0 {
			misses = append(misses, &miss{id: o.CaseID, kind: o.Kind})
			i = len(misses) - 1
		}
		misses[i].where = append(misses[i].where, string(o.Carrier)+"/"+o.Hook)
	}
	var missKinds []string
	probes := map[string]int{}
	nProbes := 0
	for _, m := range misses {
		if !slices.Contains(missKinds, m.kind) {
			missKinds = append(missKinds, m.kind)
		}
		if bp := boundaryProbe(m.id); bp != "" {
			probes[bp]++
			nProbes++
		}
	}
	slices.Sort(missKinds)
	p("\n### Misses (FN), by kind\n\n")
	p("Every expected-sensitive case with a FN outcome, by case ID, with where it was missed and its documented role. "+
		"%d of the %d FN cases are synthetic boundary probes (%d %s, %d %s, %d %s): they sit just outside a rule by construction, so recall is read per row, not as a headline.\n",
		nProbes, len(misses),
		probes["one byte below the minimum length"], "one byte below the minimum length",
		probes["prefix glued to the preceding string"], "with the prefix glued to the preceding string",
		probes["truncated PEM"], "truncated PEMs")
	for _, k := range missKinds {
		n := 0
		for _, m := range misses {
			if m.kind == k {
				n++
			}
		}
		noun := "cases"
		if n == 1 {
			noun = "case"
		}
		p("\n%s (%d %s):\n\n", kindLabel(k), n, noun)
		for _, m := range misses {
			if m.kind != k {
				continue
			}
			probe := ""
			if bp := boundaryProbe(m.id); bp != "" {
				probe = " Boundary probe: " + bp + "."
			}
			p("- `%s`, missed at %s.%s Role: %s\n", m.id, strings.Join(m.where, ", "), probe, byID[m.id].Role)
		}
	}
	p("\n### Mandatory cases (spec §5.6 item 1)\n\n%s\n", gate[0].text)

	// Per-case outcome tables for lookalikes, fixtures and coverage gaps.
	caseTable := func(outs []secrettest.Outcome, withClass bool) {
		cols := []string{"observation/input", "tool-args/output", "tool-args/tool_call"}
		var ids []string
		cell := map[[2]string]string{}
		casesFired, outsFired := map[string]bool{}, 0
		for _, o := range outs {
			if !slices.Contains(ids, o.CaseID) {
				ids = append(ids, o.CaseID)
			}
			v := "silent"
			if o.Any {
				v = "fired: " + strings.Join(o.Kinds, ", ")
				casesFired[o.CaseID] = true
				outsFired++
			}
			cell[[2]string{o.CaseID, string(o.Carrier) + "/" + o.Hook}] = v
		}
		p("Secrets fired on %s by case and %s by outcome.\n\n", pct(len(casesFired), len(ids)), pct(outsFired, len(outs)))
		head, rule := "| Case | Kind |", "|---|---|"
		if withClass {
			head, rule = "| Case | Class |", "|---|---|"
		} else {
			head, rule = head+" Coverage gap |", rule+"---|"
		}
		p("%s %s |\n%s%s\n", head, strings.Join(cols, " | "), rule, strings.Repeat("---|", len(cols)))
		for _, id := range ids {
			c := byID[id]
			second := kindLabel(c.Kind)
			if withClass {
				second = string(c.Class)
			}
			p("| `%s` | %s |", id, second)
			if !withClass {
				gap := "no"
				if c.CoverageGap {
					gap = "yes"
				}
				p(" %s |", gap)
			}
			for _, col := range cols {
				v, ok := cell[[2]string{id, col}]
				if !ok {
					v = "-"
				}
				p(" %s |", v)
			}
			p("\n")
		}
	}
	lookalike, fixture := secrettest.Tables(acc)
	p("\n## Lookalikes\n\n")
	p("Benign lookalikes: content proven not to be a credential, such as identifiers, checksums, order numbers, unresolved references, redactions and placeholders (spec §5.2). They stay out of the confusion matrix. A firing here is a block a default-on Secrets would impose on content that only looks like a secret. These are designed probes, not a population; the census below gives the rate. %s\n\n", lookalikeTerminal(acc))
	caseTable(lookalike, false)
	p("\n## Fixtures\n\n")
	p("Synthetic fixtures: inert examples used as test or documentation data (spec §5.2). Fixture status does not override exemptions, and expected detection is recorded separately. A firing in the model's own write arguments is terminal (Workflow table, row 3), which is what K4 decides.\n\n")
	caseTable(fixture, false)
	var gaps []secrettest.Outcome
	for _, o := range acc {
		if o.CoverageGap {
			gaps = append(gaps, o)
		}
	}
	p("\n## Coverage gaps\n\n")
	p("Families the scanner has no rule for at all (spec F11), one case each, named by its documented role. They are reported, not counted as false negatives against the supported contract; a firing here comes from another rule.\n\n")
	var gapIDs []string
	for _, o := range gaps {
		if !slices.Contains(gapIDs, o.CaseID) {
			gapIDs = append(gapIDs, o.CaseID)
			p("- `%s`: %s\n", o.CaseID, byID[o.CaseID].Role)
		}
	}
	p("\n")
	caseTable(gaps, true)

	// Census.
	cells := secretsCensusCells(census)
	p("\n## Census (Secrets side)\n\n")
	p("The benign side of the accuracy report (spec §5.4): files as Golem's production tools render them, one observation per file per carrier (`search`: one per registered query), from `docs/llm/census/census-results.json`; `docs/llm/census/README.md` describes the carriers and every count. " +
		"A Secrets block is an observation blocked under G+S (`guards+secrets`) and not under G (`golem-default-guards`): a guard's refusal is not Secrets disruption. " +
		"Files classed `adversarial-fixture` (BC5) are excluded from every rate and reported apart.\n\n")
	var present []string
	for _, s := range census.Strata {
		present = append(present, s.Name)
		p("- `%s`: %s, commit `%s`, %s; %d files, %d classed adversarial-fixture.\n", s.Name, s.Source, short(s.Commit), s.License, s.Files, len(s.Adversarial))
	}
	var pending []string
	for _, s := range censusStrataPlanned {
		if !slices.Contains(present, s) {
			pending = append(pending, "`"+s+"`")
		}
	}
	if len(pending) > 0 {
		p("\nThese results cover the local strata only. The third-party strata %s are registered in the census README but await approval to download, and no rate here speaks for them.\n", strings.Join(pending, " and "))
	}
	p("\n### By observation\n\n")
	p("| Stratum | Carrier | Benign observations | Files | Secrets blocks | Within %d/%d | At the call | Guard blocks (G) | Refused (not counted) | Unobserved | Adversarial fixture (excluded) |\n|---|---|---|---|---|---|---|---|---|---|---|\n", blockBudgetNum, blockBudgetDen)
	for _, c := range cells {
		within := "yes"
		if !withinBudget(c.blocks, c.observations) {
			within = "no"
		}
		p("| %s | %s | %d | %d | %s | %s | %d | %d | %d | %d | %d |\n", c.stratum, c.carrier, c.observations, c.files, pct(c.blocks, c.observations), within, c.callBlocks, c.guardBlocks, c.refused, c.unobserved, c.adversarial)
	}
	p("\nFiles equals benign observations for a file carrier by construction and is 0 for `search`, whose observations are per query. `At the call` counts blocks of the scripted call, at model output (terminal in production) or at the tool-call hook (recoverable); the census does not separate the two.\n")

	type fileKey struct{ stratum, path string }
	benignFiles, advFiles := map[fileKey]bool{}, map[fileKey]bool{}
	advObs := map[string]int{}
	ruleFiles := map[[2]string]map[string]bool{}
	adversarial := func(o censusFiredDoc) bool {
		for _, s := range census.Strata {
			if s.Name == o.Stratum {
				return o.Carrier != "search" && s.Adversarial[o.Path]
			}
		}
		return false
	}
	for _, o := range census.Fired {
		if o.Carrier == "search" || !secretsBlocks(o) {
			continue
		}
		k := fileKey{o.Stratum, o.Path}
		if adversarial(o) {
			advFiles[k] = true
			advObs[o.Stratum]++
			continue
		}
		benignFiles[k] = true
		for _, f := range o.Findings {
			if f.Interceptor == "secrets" && f.Verdict == "block" {
				rk := [2]string{o.Stratum, strings.TrimPrefix(f.Rule, "sensitive_")}
				if ruleFiles[rk] == nil {
					ruleFiles[rk] = map[string]bool{}
				}
				ruleFiles[rk][o.Path] = true
			}
		}
	}
	count := func(m map[fileKey]bool, stratum string) int {
		n := 0
		for k := range m {
			if k.stratum == stratum {
				n++
			}
		}
		return n
	}
	p("\n### By file\n\n")
	p("A file counts once if Secrets blocks any carrier's observation of it. The denominator is the stratum's manifest files minus its adversarial fixtures, including files `read_file` refuses as binary, which the other carriers still observe.\n\n")
	p("| Stratum | Benign files | Files with a Secrets block |\n|---|---|---|\n")
	for _, s := range census.Strata {
		n := s.Files - len(s.Adversarial)
		p("| %s | %d | %s |\n", s.Name, n, pct(count(benignFiles, s.Name), n))
	}
	var ruleKeys [][2]string
	for k := range ruleFiles {
		ruleKeys = append(ruleKeys, k)
	}
	slices.SortFunc(ruleKeys, func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	p("\nBenign files by the Secrets rule that blocked them (a file blocked under two rules counts under both):\n\n| Stratum | Rule | Files |\n|---|---|---|\n")
	for _, k := range ruleKeys {
		p("| %s | `%s` | %d |\n", k[0], k[1], len(ruleFiles[k]))
	}
	p("\n### Adversarial fixtures (BC5, reported apart)\n\n")
	for _, s := range census.Strata {
		obs := 0
		for _, c := range cells {
			if c.stratum == s.Name {
				obs += c.adversarial
			}
		}
		if len(s.Adversarial) == 0 {
			p("- %s: no file is classed adversarial-fixture.\n", s.Name)
			continue
		}
		p("- %s: Secrets blocks %s of the adversarial-fixture observations, in %s of those files.\n", s.Name, pct(advObs[s.Name], obs), pct(count(advFiles, s.Name), len(s.Adversarial)))
	}

	prob := handleLuhnProbability()
	pf, _ := prob.Float64()
	p("\n### Background job handles (a separate workflow effect)\n\n")
	p("A background job's handle is `bg-` plus 32 hex digits from 16 random bytes (`agent/tools/background_manager.go`, `readHandle`), and the `start_command`, `command_status`, `command_tail` and `command_stop` results print it on its own line. " +
		"The payment-card rule reads a maximal run of 13 to 19 digits. In a handle each hex digit is a decimal digit with probability 10/16, independently, and a run of uniform digits passes Luhn with probability 1/10. " +
		"An exact sum over the 32 positions (a dynamic program over run lengths, in rational arithmetic) gives the probability that a handle holds a Luhn-valid run of 13 to 19 digits:\n\n")
	p("p = %.4f%% per handle, about 1 in %.0f jobs (computed exactly from the 10/16 and 1/10 probabilities above, not sampled).\n\n", 100*pf, 1/pf)
	p("The rule also exempts repeated digits and the 7 published test numbers, which lowers p by less than one part in ten billion, so p is a tight upper bound. " +
		"Under a default-on Secrets every result that prints such a handle is replaced, whatever the job's output: the job runs, but the model never sees its handle or its output. " +
		"The census redraws these handles so that its rates stay a function of the file, so this effect is not in the `command_tail` rates above and adds to them.\n")

	// Workflow table.
	p("\n## Workflow table\n\n")
	p("A real `agent.Orchestrator` built by Golem's factory, real tools in temp workspaces and a scripted model (spec §5.5), each row run under G and under G+S. The 14 rows of spec §5.5 appear as %d lettered variants, one per distinct outcome; the class is that of the value the row carries (spec §5.2).\n\n", len(rows))
	for _, r := range rows {
		p("- **%s** (%s): %s\n", r.Row.ID, r.Row.Class, r.Row.Surface)
	}
	p("\n| Row | Class | Arm | First boundary | Hook | Run outcome | Parent | Message | Continuation succeeds | Surrogate delivered (protection rows 14a-c only) |\n|---|---|---|---|---|---|---|---|---|---|\n")
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, r := range rows {
		for _, arm := range []struct {
			name string
			res  secretsRowResult
		}{{"G", r.G}, {"G+S", r.GS}} {
			msg, delivered := "-", "-"
			if arm.res.Message != "" {
				msg = "`" + arm.res.Message + "`"
			}
			if rowNumber(r.Row.ID) == "14" {
				delivered = yesNo(arm.res.Delivered)
			}
			p("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", r.Row.ID, r.Row.Class, arm.name, dash(arm.res.FirstBoundary), dash(arm.res.Hook),
				arm.res.RunOutcome, dash(arm.res.ParentOutcome), msg, yesNo(arm.res.ContinuationSucceeds), delivered)
		}
	}
	p("\nHow to read it:\n\n")
	p("- First boundary is what stopped the value first: `guard` (an always-on guard refused the call), `secrets`, or `indexer` (rag scrubbed it at index time, outside the hooks). Hook is the agent hook where it acted.\n")
	p("- Run outcome is `terminal` for a run a block ended, otherwise the run's stop reason. Row 13a holds the advisory preview's disposition instead (`staged` or `refused`), after which the goal runs without the advice. Row 10 holds the dispatch child's stop reason, Parent the parent run's outcome, and its Hook is read from the replaced line's prefix, not observed.\n")
	p("- Message is the user- or model-visible block text, one of four pinned texts. Row 12's message carries an annotation: whether the blocked answer had already streamed to the display.\n")
	p("- Continuation succeeds is the row's completion probe (file bytes, a second run, the delivered answer). Surrogate delivered is read from the next wire request (14a), the file bytes (14b) or the server-recorded MCP argument (14c), and only on those rows.\n")
	var notes []string
	for _, r := range rows {
		if before, after, ok := strings.Cut(r.GS.Message, " (overblock: "); ok {
			notes = append(notes, fmt.Sprintf("Overblock, row %s under G+S: `%s` replaced the whole result, yet %s (a shadow run without Secrets shows it).", r.Row.ID, before, strings.TrimSuffix(after, ")")))
		}
		benign := r.Row.Class == string(secrettest.Benign) || r.Row.Class == string(secrettest.BenignLookalike)
		if benign && r.G.FirstBoundary == "guard" && r.GS.FirstBoundary == "guard" {
			notes = append(notes, fmt.Sprintf("Row %s is class %s yet refused by an always-on guard under both arms: a non-Secrets boundary, not Secrets disruption.", r.Row.ID, r.Row.Class))
		}
	}
	for _, n := range notes {
		p("- %s\n", n)
	}
	var terminal, replaced, lost []string
	for _, r := range rows {
		switch {
		case secretsTerminal(r.GS):
			terminal = append(terminal, r.Row.ID)
		case r.G.ContinuationSucceeds && !r.GS.ContinuationSucceeds:
			lost = append(lost, r.Row.ID)
		case r.GS.FirstBoundary == "secrets":
			replaced = append(replaced, r.Row.ID)
		}
	}
	p("\nUnder G+S: terminal in %s; continuation lost without a terminal block in %s; Secrets acted and the continuation held in %s.\n", rowList(terminal), rowList(lost), rowList(replaced))

	// Gate.
	p("\n## Gate evaluation (spec §5.6)\n\n")
	p("Default-on Secrets requires all six items; if any fails, Secrets stays opt-in and the failed criteria are recorded. The thresholds are frozen (spec §2.1): K1's task-failure budget is %d terminal blocks on benign or lookalike sentinel workflows, K1's unnecessary-block budget is at most %d block per %d benign observations in every stratum, and K4 makes a terminal block on a deliberate fixture workflow disqualifying.\n\n",
		taskFailureBudget, blockBudgetNum, blockBudgetDen)
	for i, it := range gate {
		v := "FAIL"
		if it.pass {
			v = "PASS"
		}
		p("- Item %d: %s. %s\n", i+1, v, it.text)
	}
	p("\n%s\n", decision)

	// Limits.
	p("\n## What this report does not claim\n\n")
	p("- %s\n", noLiveCredentials)
	p("- No model-recovery claim: whether a model recovers after a block is not measured. Bounded, paired live runs on the affected workflows are the first task of an eventual post-#518 flip PR (spec §5.6).\n")
	p("- The case table is a stratified probe set, not a population. Its rates are per cell, and no headline recall is claimed.\n")
	p("- The tool-args carrier always puts the value in a single JSON string field named `content`. Key-aware argument scanning (`ScanAssignment` on JSON keys, nested objects) is outside the measurement.\n")
	p("- The census covers only the strata listed above. It classes only `adversarial-fixture` files (BC5), so a benign file whose role is deliberate secret-shaped test data counts toward the false-block rate, except where it sits under the BC5 adversarial-fixture paths.\n")
	p("- Summary generation calls the provider outside the hooks and is not protected; canary minting is untouched (spec §5.5).\n")
	p("- #424 exact configured-secret matching and its outbound checks are not covered (spec §9).\n")
	return b.String()
}

// TestSecretsEvidenceHandleProbability checks the closed form two ways: against the
// value an independent recursion (over where each maximal digit run starts,
// in exact fractions) gives, and against the real scanner on a million
// handles in the readHandle format, drawn from a fixed seed.
func TestSecretsEvidenceHandleProbability(t *testing.T) {
	const independent = 0.0017584025475436693
	got, _ := handleLuhnProbability().Float64()
	if math.Abs(got-independent) > 1e-15 {
		t.Errorf("handleLuhnProbability = %.19f, want %.19f", got, independent)
	}
	const draws = 1_000_000
	src := rand.NewChaCha8([32]byte{5, 1, 7})
	var b [16]byte
	hits := 0
	for range draws {
		_, _ = src.Read(b[:])
		if len(secretscan.Scan("handle: bg-"+hex.EncodeToString(b[:])+"\n")) > 0 {
			hits++
		}
	}
	// Five standard deviations of a binomial count.
	mean := got * draws
	if sd := math.Sqrt(mean * (1 - got)); math.Abs(float64(hits)-mean) > 5*sd {
		t.Errorf("%d of %d sampled handles are secret-shaped, want %.0f within 5 sd (%.0f)", hits, draws, mean, 5*sd)
	}
}

// TestSecretsEvidenceBlocks pins the by-file rule against cmd/llm-bench deriveArm for
// G and G+S: only a Secrets block that G would not make, on an observation G+S
// reaches, is Secrets'.
func TestSecretsEvidenceBlocks(t *testing.T) {
	f := func(interceptor, verdict, target string) censusFindingDoc {
		return censusFindingDoc{Interceptor: interceptor, Rule: "r", Verdict: verdict, Target: target, Group: -1, Alt: -1}
	}
	for _, tt := range []struct {
		name     string
		findings []censusFindingDoc
		want     bool
	}{
		{"secrets blocks the message", []censusFindingDoc{f("secrets", "block", "message")}, true},
		{"secrets blocks an alternative", []censusFindingDoc{f("secrets", "block", "alternative")}, true},
		{"secrets blocks the call", []censusFindingDoc{f("secrets", "block", "output_tool_call")}, true},
		{"a guard blocks the call too", []censusFindingDoc{f("secrets", "block", "message"), f("egress", "block", "tool_call")}, false},
		{"a guard blocks the message too", []censusFindingDoc{f("secrets", "block", "message"), f("invariants", "block", "message")}, false},
		{"a detector blocks the call first", []censusFindingDoc{f("zero_width", "block", "tool_call"), f("secrets", "block", "message")}, false},
		{"a detector blocks the message (not a G+S member)", []censusFindingDoc{f("zero_width", "block", "message"), f("secrets", "block", "message")}, true},
		{"secrets only tags", []censusFindingDoc{f("secrets", "tag", "message")}, false},
		{"only a detector blocks", []censusFindingDoc{f("encoding", "block", "message")}, false},
	} {
		if got := secretsBlocks(censusFiredDoc{Findings: tt.findings}); got != tt.want {
			t.Errorf("%s: secretsBlocks = %t, want %t", tt.name, got, tt.want)
		}
	}
}
