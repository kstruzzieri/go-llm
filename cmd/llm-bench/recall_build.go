package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// recallCasesSchema is the version tag a recall cases file must carry.
const recallCasesSchema = "recall-label-cases/v1"

// recallCases is the recall-label fixture file: templates and the families
// built from them.
type recallCases struct {
	Schema    string           `json:"schema"`
	Templates []recallTemplate `json:"templates"`
	Families  []recallFamily   `json:"families"`
}

// recallTemplate is a task template: the value type its families carry and
// whether the task names the work directly or asks for it as a task.
type recallTemplate struct {
	ID        string `json:"id"`
	ValueType string `json:"value_type"` // number | identifier
	TaskStyle string `json:"task_style"` // direct | task
}

// recallFamily is one fixture family: a recalled lesson, the file it was
// learned from before and after the change, and the task asked.
type recallFamily struct {
	ID           string            `json:"id"`
	Template     string            `json:"template"`
	Split        string            `json:"split"`
	Lesson       string            `json:"lesson"`
	NamedFile    string            `json:"named_file"`
	FileBefore   string            `json:"file_before"`
	FileS1       string            `json:"file_s1"`
	FileS2       string            `json:"file_s2"`
	Task         string            `json:"task"`
	Query        string            `json:"query"`
	Value        RecallValue       `json:"value"`
	TaskPaths    []string          `json:"task_paths,omitempty"`
	OtherFiles   map[string]string `json:"other_files,omitempty"`
	Distractors  []string          `json:"distractors"`
	Rank         *int              `json:"rank"`
	S7Distractor *int              `json:"s7_distractor"`
}

// recallTraceSource is the Source every recall trace carries.
const recallTraceSource = "recall-label"

// rank returns the validated rank; a nil Rank reads as 0, which validate rejects.
func (f recallFamily) rank() int {
	if f.Rank == nil {
		return 0
	}
	return *f.Rank
}

// s7 returns the validated S7 distractor; a nil value reads as 0 after validate has rejected nil.
func (f recallFamily) s7() int {
	if f.S7Distractor == nil {
		return 0
	}
	return *f.S7Distractor
}

// recallBuildOptions are test-only switches; the CLI never sets them.
type recallBuildOptions struct{ SkipBalance bool }

// recallErr formats a recall build error.
func recallErr(format string, args ...any) error {
	return fmt.Errorf("recall build: "+format, args...)
}

// loadRecallCases reads and strictly decodes a recall cases file.
func loadRecallCases(path string) (recallCases, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return recallCases{}, recallErr("read cases: %w", err)
	}
	var c recallCases
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return recallCases{}, recallErr("parse cases %q: %w", path, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return recallCases{}, recallErr("parse cases %q: data after the top-level object", path)
	}
	return c, nil
}

// recallRelPath rejects an absolute, escaping or padded workspace path, and
// one that names no file under the /workspace root (recallWorkspaceFile, the
// rule the frozen trace check applies).
func recallRelPath(p string) error {
	if p == "" || !filepath.IsLocal(filepath.FromSlash(p)) || strings.Contains(p, "\\") ||
		p != strings.TrimSpace(p) || recallWorkspaceFile(p) == "" {
		return fmt.Errorf("path %q must be a relative path naming a file inside the workspace", p)
	}
	return nil
}

// recallSystemText is the system prompt every recall trace carries.
func recallSystemText() string {
	return golem.SystemPrompt(false, false) + recallGolemMemoryFragment
}

// validate checks the cases file for split: per-family content rules first
// (each rejection points at its family), then, unless skipBalance, the
// counterbalance counts over the families of that split.
func (c recallCases) validate(split string, skipBalance bool) error {
	if c.Schema != recallCasesSchema {
		return recallErr("schema %q is not %q", c.Schema, recallCasesSchema)
	}
	if split != "pilot" && split != "confirmatory" {
		return recallErr("split %q invalid (want pilot or confirmatory)", split)
	}
	tmpls := map[string]recallTemplate{}
	for _, t := range c.Templates {
		if !injectionNamePattern.MatchString(t.ID) || tmpls[t.ID].ID != "" {
			return recallErr("invalid or duplicate template id %q", t.ID)
		}
		if t.ValueType != "number" && t.ValueType != "identifier" {
			return recallErr("template %s: value_type %q invalid", t.ID, t.ValueType)
		}
		if t.TaskStyle != "direct" && t.TaskStyle != "task" {
			return recallErr("template %s: task_style %q invalid (want direct or task)", t.ID, t.TaskStyle)
		}
		tmpls[t.ID] = t
	}
	sys := recallSystemText()
	ids := map[string]bool{}
	for _, f := range c.Families {
		if err := f.validate(tmpls, sys); err != nil {
			return recallErr("family %s: %w", f.ID, err)
		}
		if ids[f.ID] {
			return recallErr("family %s: duplicate id", f.ID)
		}
		ids[f.ID] = true
	}
	var sel []recallFamily
	for _, f := range c.Families {
		if f.Split == split {
			sel = append(sel, f)
		}
	}
	if len(sel) == 0 {
		return recallErr("no families in split %s", split)
	}
	if skipBalance {
		return nil
	}
	perTemplate := map[string]int{}
	var ranks [4]int
	s7 := 0
	for _, f := range sel {
		perTemplate[f.Template]++
		ranks[f.rank()]++
		s7 += f.s7()
	}
	want := recallSplitFamilyCount(split) / 4
	for _, t := range c.Templates {
		if perTemplate[t.ID] != want {
			return recallErr("template %s has %d %s families, want %d", t.ID, perTemplate[t.ID], split, want)
		}
	}
	// Both splits use one template for each value type and task style.
	pairs := map[[2]string]bool{}
	for _, t := range c.Templates {
		pairs[[2]string{t.ValueType, t.TaskStyle}] = true
	}
	if len(c.Templates) != 4 || len(pairs) != 4 {
		return recallErr("%s needs exactly 4 templates covering each (value_type, task_style) pair once; got %d templates over %d pairs", split, len(c.Templates), len(pairs))
	}
	n := len(sel)
	for r := 1; r <= 3; r++ {
		// |count - n/3| <= 1, in integers
		if d := 3*ranks[r] - n; d < -3 || d > 3 {
			lo, hi := recallCountRange(n, 3, 3)
			return recallErr("rank %d has %d of %d families; want %d..%d (within 1 of n/3)", r, ranks[r], n, lo, hi)
		}
	}
	if split == "confirmatory" {
		if d := 2*s7 - n; d < -2 || d > 2 {
			lo, hi := recallCountRange(n, 2, 2)
			return recallErr("s7_distractor 1 has %d of %d families; want %d..%d (within 1 of n/2)", s7, n, lo, hi)
		}
	}
	return nil
}

// recallCountRange returns the smallest and largest count c with
// |k*c - n| <= tol.
func recallCountRange(n, k, tol int) (lo, hi int) {
	lo, hi = -1, -1
	for c := 0; c <= n; c++ {
		if d := k*c - n; d >= -tol && d <= tol {
			if lo < 0 {
				lo = c
			}
			hi = c
		}
	}
	return lo, hi
}

// recallForbiddenToken names the label or placebo shape found in text, or "".
// Family text must never carry one: it would put label-shaped provenance on
// every arm.
func recallForbiddenToken(text string) string {
	low := strings.ToLower(text)
	for _, tok := range []string{"files=", "seq="} {
		if strings.Contains(low, tok) {
			return tok
		}
	}
	return ""
}

// validate checks one family's content rules.
func (f recallFamily) validate(tmpls map[string]recallTemplate, sys string) error {
	v := f.Value
	switch {
	case !injectionNamePattern.MatchString(f.ID):
		return errors.New("id must match [A-Za-z0-9][A-Za-z0-9_-]*")
	case f.Split != "pilot" && f.Split != "confirmatory":
		return fmt.Errorf("split %q invalid", f.Split)
	case f.Rank == nil:
		return errors.New("rank is missing")
	case *f.Rank < 1 || *f.Rank > 3:
		return fmt.Errorf("rank %d outside 1..3", *f.Rank)
	case f.S7Distractor == nil:
		return errors.New("s7_distractor is missing")
	case *f.S7Distractor < 0 || *f.S7Distractor > 1:
		return fmt.Errorf("s7_distractor %d outside 0..1", *f.S7Distractor)
	case len(f.Distractors) != 2:
		return fmt.Errorf("distractors has %d entries, want exactly 2", len(f.Distractors))
	case strings.TrimSpace(f.Query) == "":
		return errors.New("query is blank")
	case strings.TrimSpace(f.Task) == "":
		return errors.New("task is blank")
	}
	tmpl, ok := tmpls[f.Template]
	if !ok {
		return fmt.Errorf("unknown template %q", f.Template)
	}
	if tmpl.ValueType != v.Type {
		return fmt.Errorf("value type %q differs from template %s (%s)", v.Type, tmpl.ID, tmpl.ValueType)
	}
	if v.Type == "number" && (v.Lesson == "0" || v.Lesson == "1" || v.Changed == "0" || v.Changed == "1") {
		return errors.New("number lesson and changed values must not be 0 or 1")
	}
	if err := recallRelPath(f.NamedFile); err != nil {
		return fmt.Errorf("named_file: %w", err)
	}
	for _, p := range f.TaskPaths {
		if err := recallRelPath(p); err != nil {
			return fmt.Errorf("task_paths: %w", err)
		}
	}
	seenPaths := map[string]string{strings.ToLower(recallWorkspaceFile(f.NamedFile)): "named_file"}
	for p, body := range f.OtherFiles {
		if err := recallRelPath(p); err != nil {
			return fmt.Errorf("other_files: %w", err)
		}
		key := strings.ToLower(recallWorkspaceFile(p))
		if prior, dup := seenPaths[key]; dup {
			return fmt.Errorf("other_files entry %q collides with %s (paths compared case-insensitively): it shadows named_file or another file", p, prior)
		}
		seenPaths[key] = "other_files entry " + p
		if statesValue(body, v, v.Lesson) {
			return fmt.Errorf("other_files %q states the lesson value %q", p, v.Lesson)
		}
	}
	for _, r := range []string{f.Lesson, f.Distractors[0], f.Distractors[1]} {
		if strings.TrimSpace(r) == "" || strings.ContainsAny(r, "\r\n") {
			return errors.New("record contents must be non-blank and hold no newline")
		}
	}
	// Label and placebo shapes are the arms' own text; none may come from the fixture.
	texts := []struct{ name, text string }{
		{"lesson", f.Lesson}, {"distractor 0", f.Distractors[0]}, {"distractor 1", f.Distractors[1]},
		{"task", f.Task}, {"query", f.Query}, {"named_file", f.NamedFile},
		{"file_before", f.FileBefore}, {"file_s1", f.FileS1}, {"file_s2", f.FileS2},
	}
	for p, body := range f.OtherFiles {
		texts = append(texts, struct{ name, text string }{"other_files key", p}, struct{ name, text string }{"other_files " + p, body})
	}
	for _, t := range texts {
		if tok := recallForbiddenToken(t.text); tok != "" {
			return fmt.Errorf("%s contains %q, which only the label and placebo arms may add", t.name, tok)
		}
	}
	switch {
	case !statesValue(f.Lesson, v, v.Lesson):
		return fmt.Errorf("lesson record does not state the lesson value %q", v.Lesson)
	case !statesValue(f.FileBefore, v, v.Lesson):
		return fmt.Errorf("file_before does not state the lesson value %q", v.Lesson)
	case statesValue(f.FileS1, v, v.Lesson):
		return fmt.Errorf("file_s1 states the lesson value %q", v.Lesson)
	case !statesValue(f.FileS1, v, v.Changed):
		return fmt.Errorf("file_s1 does not state the changed value %q", v.Changed)
	case !statesValue(f.FileS2, v, v.Lesson):
		return fmt.Errorf("file_s2 does not state the lesson value %q", v.Lesson)
	case !slices.Equal(strings.Fields(f.FileS2), strings.Fields(f.FileBefore)):
		return errors.New("file_s2 differs from file_before beyond whitespace")
	case f.FileS2 == f.FileBefore:
		return errors.New("file_s2 is byte-identical to file_before; S2 is the file changed by formatting only")
	case statesValue(f.Task, v, v.Lesson):
		return fmt.Errorf("task states the lesson value %q", v.Lesson)
	case statesValue(f.Query, v, v.Lesson):
		return fmt.Errorf("query states the lesson value %q", v.Lesson)
	case statesValue(f.Distractors[0], v, v.Lesson) || statesValue(f.Distractors[1], v, v.Lesson):
		return fmt.Errorf("a distractor states the lesson value %q", v.Lesson)
	case statesValue(sys, v, v.Lesson):
		return fmt.Errorf("the system prompt states the lesson value %q", v.Lesson)
	case !strings.Contains(f.Lesson, f.NamedFile):
		return fmt.Errorf("lesson record does not name named_file %q", f.NamedFile)
	}
	if err := f.validateSource(); err != nil {
		return err
	}
	for _, p := range f.TaskPaths {
		if !strings.Contains(f.Task, p) {
			return fmt.Errorf("task_paths entry %q does not appear in task", p)
		}
	}
	// A distractor is a relevant-looking record about another file: it names a
	// path from other_files and never named_file.
	for i, d := range f.Distractors {
		named := false
		for p := range f.OtherFiles {
			named = named || strings.Contains(d, p)
		}
		if !named || strings.Contains(d, f.NamedFile) {
			return fmt.Errorf("distractor %d must name a path from other_files and not named_file %q", i, f.NamedFile)
		}
	}
	return nil
}

// recallSeededID is the k-th candidate record ID for a family's role
// (relevant, d0, d1): 32 lowercase hex digits, the shape of a real Golem record
// ID, derived from a digest so it carries neither the family ID nor the role.
func recallSeededID(familyID, role string, k int) string {
	sum := sha256.Sum256([]byte("llm-bench-recall-record:" + familyID + ":" + role + ":" + strconv.Itoa(k)))
	return hex.EncodeToString(sum[:])[:32]
}

// recallRecordIDMaxTries caps the record-ID search. Each candidate is accepted
// with probability well above 1/3, so exhausting the cap is a defect, not luck.
const recallRecordIDMaxTries = 1000

// recordID returns the family's record ID for role: the first candidate
// (k = 0, 1, ...) that starts with a letter a-f and does not itself state the
// lesson value. A leading letter keeps every digit run in the hex ID preceded
// by a word byte, so no number token can be read from it. It fails after
// recallRecordIDMaxTries candidates.
func (f recallFamily) recordID(role string) (string, error) {
	return searchRecallRecordID(f.ID, role, recallRecordIDMaxTries, func(id string) bool {
		return id[0] >= 'a' && !statesValue(id, f.Value, f.Value.Lesson)
	})
}

// searchRecallRecordID returns the first of maxTries seeded candidates (k = 0,
// 1, ...) that accept admits, or an error naming the family and role when none
// is. recordID is its only production caller; the parameters exist so a test
// can force exhaustion without a real hash search.
func searchRecallRecordID(familyID, role string, maxTries int, accept func(id string) bool) (string, error) {
	for k := 0; k < maxTries; k++ {
		if id := recallSeededID(familyID, role, k); accept(id) {
			return id, nil
		}
	}
	return "", fmt.Errorf("recall build: family %s: no acceptable %s record ID in %d candidates", familyID, role, maxTries)
}

// layout derives the seeded records of a family: IDs from recordID, contents,
// and the order a search must return. The record at position p gets the date
// 2026-03-(28-p), newest first. Every record matches the query, so bm25 ties
// among records of equal token and query-term counts and the dates
// (updated_at DESC) decide the order; checkRecallInvariants enforces the
// result equals Rank. The days 26..28 lie outside the 2..20 a fixture value
// takes, so a reply quoting a record's date never restates a value.
func (f recallFamily) layout() (recs []recallSeedRecord, relID string, distIDs [2]string, order []string, err error) {
	if relID, err = f.recordID("relevant"); err != nil {
		return nil, "", distIDs, nil, err
	}
	for i, role := range []string{"d0", "d1"} {
		if distIDs[i], err = f.recordID(role); err != nil {
			return nil, "", distIDs, nil, err
		}
	}
	d := 0
	for p := 0; p < 3; p++ {
		r := recallSeedRecord{CreatedAt: time.Date(2026, 3, 28-p, 0, 0, 0, 0, time.UTC)}
		if p == f.rank()-1 {
			r.ID, r.Content = relID, f.Lesson
		} else {
			r.ID, r.Content = distIDs[d], f.Distractors[d]
			d++
		}
		recs = append(recs, r)
		order = append(order, r.ID)
	}
	return recs, relID, distIDs, order, nil
}

// recallCell is one trace of a family: its ID suffix, the eval block fields,
// and what the capture adds.
type recallCell struct {
	Suffix, Trace, Arm string
	Cases              []string
	S2, Repeat         bool
	Explain            bool
	Mark               string // "" | label | placebo | s7
}

// recallCells lists a split's cells in build order.
func recallCells(split string) []recallCell {
	s1s2 := []string{"S1", "S2"}
	cells := []recallCell{
		{Suffix: "A-0", Trace: "A", Arm: "0", Cases: s1s2},
		{Suffix: "A-1", Trace: "A", Arm: "1", Cases: s1s2, Explain: true},
	}
	if split == "confirmatory" {
		cells = append(cells,
			recallCell{Suffix: "A-2", Trace: "A", Arm: "2", Cases: s1s2, Explain: true, Mark: "label"},
			recallCell{Suffix: "A-2-s7", Trace: "A", Arm: "2", Cases: []string{"S7"}, Explain: true, Mark: "s7"})
	}
	cells = append(cells, recallCell{Suffix: "B-s1-1", Trace: "B", Arm: "1", Cases: []string{"S1"}, Explain: true})
	if split == "confirmatory" {
		cells = append(cells, recallCell{Suffix: "B-s1-2", Trace: "B", Arm: "2", Cases: []string{"S1"}, Explain: true, Mark: "label"})
	}
	cells = append(cells, recallCell{Suffix: "B-s2-1", Trace: "B", Arm: "1", Cases: []string{"S2"}, S2: true, Explain: true})
	if split == "confirmatory" {
		cells = append(cells, recallCell{Suffix: "B-s2-2", Trace: "B", Arm: "2", Cases: []string{"S2"}, S2: true, Explain: true, Mark: "label"})
	}
	cells = append(cells, recallCell{Suffix: "B-s2-P", Trace: "B", Arm: "P", Cases: []string{"S2"}, S2: true, Explain: true, Mark: "placebo"})
	if split == "confirmatory" {
		cells = append(cells, recallCell{Suffix: "B-s2-1-repeat", Trace: "B", Arm: "1", Cases: []string{"S2"}, S2: true, Explain: true, Repeat: true})
	}
	return cells
}

// parseRecallTraceID splits a built recall trace ID, "recall-<family>-<suffix>",
// into its family ID and its cell suffix (a recallCells suffix). Family IDs may
// contain '-', so the suffix is the longest cell suffix the ID ends with.
func parseRecallTraceID(id string) (familyID, cell string, err error) {
	rest, ok := strings.CutPrefix(id, "recall-")
	if !ok {
		return "", "", fmt.Errorf("recall trace id %q does not start with %q", id, "recall-")
	}
	for _, c := range recallCells("confirmatory") {
		fam, ok := strings.CutSuffix(rest, "-"+c.Suffix)
		if ok && fam != "" && len(c.Suffix) > len(cell) {
			familyID, cell = fam, c.Suffix
		}
	}
	if cell == "" {
		return "", "", fmt.Errorf("recall trace id %q does not end in a recall cell suffix", id)
	}
	return familyID, cell, nil
}

// writeRecallWorkspace writes the family's files into dir: the named file
// (S1 or S2 content) and every other file.
func writeRecallWorkspace(dir string, f recallFamily, s2 bool) error {
	named := f.FileS1
	if s2 {
		named = f.FileS2
	}
	files := map[string]string{f.NamedFile: named}
	for p, body := range f.OtherFiles {
		files[p] = body
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		body := files[p]
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// captureRecallCell builds one cell's request from a freshly seeded store and
// a fresh workspace, both removed before it returns.
func captureRecallCell(ctx context.Context, f recallFamily, cell recallCell) (provider.ChatRequest, error) {
	dir, err := os.MkdirTemp("", "recall-build-")
	if err != nil {
		return provider.ChatRequest{}, err
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "recall build: cleanup warning: remove %s: %v\n", dir, err)
		}
	}()
	ws := filepath.Join(dir, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		return provider.ChatRequest{}, err
	}
	if err := writeRecallWorkspace(ws, f, cell.S2); err != nil {
		return provider.ChatRequest{}, fmt.Errorf("workspace: %w", err)
	}
	recs, relID, distIDs, _, err := f.layout()
	if err != nil {
		return provider.ChatRequest{}, err
	}
	store, closeDB, err := seedRecallStore(ctx, filepath.Join(dir, "mem.db"), recs)
	if err != nil {
		return provider.ChatRequest{}, err
	}
	defer func() {
		if err := closeDB(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "recall build: cleanup warning: close store for %s: %v\n", f.ID, err)
		}
	}()
	segs := map[string]string{}
	switch cell.Mark {
	case "label":
		segs[relID] = recallLabelSegment
	case "placebo":
		segs[relID] = recallPlaceboSegment
	case "s7":
		segs[distIDs[f.s7()]] = recallLabelSegment
	}
	return captureRecallRequest(ctx, recallCaptureInput{
		FamilyID: f.ID, Task: f.Task, Query: f.Query, ReadPath: f.NamedFile,
		TraceB: cell.Trace == "B", Workspace: ws, Store: store,
		Search: recallLabelTool{
			Inner:   agenttools.AgentMemorySearch{S: store, WorkspaceID: recallWorkspaceID, SessionID: func() string { return recallSeedSession }},
			Explain: cell.Explain, Segments: segs,
		},
	})
}

// recallCaptureFunc captures one cell's request; captureRecallCell in
// production, a test seam in tests.
type recallCaptureFunc func(ctx context.Context, f recallFamily, cell recallCell) (provider.ChatRequest, error)

// buildRecallFamily captures every cell of a family for split and returns the
// traces and the raw requests keyed by cell suffix. It does not check the
// cross-arm invariants; buildRecallCorpusWith does.
func buildRecallFamily(ctx context.Context, f recallFamily, tmpl recallTemplate, split string) ([]Trace, map[string]provider.ChatRequest, error) {
	return buildRecallFamilyWith(ctx, f, tmpl, split, captureRecallCell)
}

// buildRecallFamilyWith is buildRecallFamily over capture. After the cells it
// captures Trace A arm 1 once more over the S2 workspace and requires the
// request byte-identical to A-1: the one Trace A serves both S1 and S2 only if
// the workspace cannot reach it.
func buildRecallFamilyWith(ctx context.Context, f recallFamily, tmpl recallTemplate, split string, capture recallCaptureFunc) ([]Trace, map[string]provider.ChatRequest, error) {
	reqs := map[string]provider.ChatRequest{}
	var traces []Trace
	for _, cell := range recallCells(split) {
		id := "recall-" + f.ID + "-" + cell.Suffix
		req, err := capture(ctx, f, cell)
		if err != nil {
			return nil, nil, recallErr("family %s: %s: %w", f.ID, cell.Suffix, err)
		}
		t, err := traceFromRequest(id, recallTraceSource, req)
		if err != nil {
			return nil, nil, recallErr("family %s: %s: %w", f.ID, cell.Suffix, err)
		}
		t.Recall = &RecallEval{
			FamilyID: f.ID, Template: f.Template, TaskStyle: tmpl.TaskStyle, Split: split,
			Trace: cell.Trace, Arm: cell.Arm, Cases: cell.Cases, Repeat: cell.Repeat,
			Value: f.Value, NamedFile: f.NamedFile, TaskPaths: f.TaskPaths,
		}
		reqs[cell.Suffix] = req
		traces = append(traces, t)
	}
	for _, cell := range recallCells(split) {
		if cell.Suffix != "A-1" {
			continue
		}
		cell.S2 = true
		req, err := capture(ctx, f, cell)
		if err != nil {
			return nil, nil, recallErr("family %s: A-1 over the S2 workspace: %w", f.ID, err)
		}
		if !recallSameRequest(req, reqs["A-1"]) {
			return nil, nil, recallErr("family %s: the Trace A request over the S2 workspace differs from the one over S1: the workspace leaked into Trace A, so one Trace A cannot serve both cases", f.ID)
		}
	}
	return traces, reqs, nil
}

// recallRecordLines lists the record IDs of the record lines of a tool message,
// in order, and the lines themselves.
func recallRecordLines(content string) (ids []string, lines []string) {
	for _, l := range strings.Split(content, "\n") {
		if parts := strings.SplitN(l, " · ", 3); len(parts) == 3 {
			ids = append(ids, parts[1])
			lines = append(lines, l)
		}
	}
	return ids, lines
}

// recallFirstTool returns the index of the request's first tool message.
func recallFirstTool(req provider.ChatRequest) int {
	for i, m := range req.Messages {
		if m.Role == "tool" {
			return i
		}
	}
	return -1
}

// recallSameRequest reports whether two requests marshal identically.
func recallSameRequest(a, b provider.ChatRequest) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// recallWithout returns a copy of req with the first occurrence of segment
// removed from the first tool message, and how many occurrences that message
// held. Occurrences elsewhere stay, so they break equality with the base.
func recallWithout(req provider.ChatRequest, segment string) (provider.ChatRequest, int) {
	out := req
	out.Messages = append([]provider.ChatMessage(nil), req.Messages...)
	i := recallFirstTool(req)
	if i < 0 {
		return out, 0
	}
	n := strings.Count(out.Messages[i].Content, segment)
	out.Messages[i].Content = strings.Replace(out.Messages[i].Content, segment, "", 1)
	return out, n
}

// recallWithoutExplanation returns a copy of req with the explanation
// sentence trimmed from every tool description, and how many carried it.
func recallWithoutExplanation(req provider.ChatRequest) (provider.ChatRequest, int) {
	out := req
	out.Tools = append([]provider.Tool(nil), req.Tools...)
	n := 0
	for i, t := range out.Tools {
		if strings.HasSuffix(t.Function.Description, recallExplanationSentence) {
			out.Tools[i].Function.Description = strings.TrimSuffix(t.Function.Description, recallExplanationSentence)
			n++
		}
	}
	return out, n
}

// recallStrings appends every string key and value of a decoded JSON value,
// and every number as its plain decimal text (booleans carry no value).
func recallStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case float64:
		*out = append(*out, strconv.FormatFloat(x, 'f', -1, 64))
	case []any:
		for _, e := range x {
			recallStrings(e, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			*out = append(*out, k)
			recallStrings(x[k], out)
		}
	}
}

// recallLeak returns an error when the lesson value is stated in any decoded
// string of the request (message content, tool-call names and raw arguments,
// tool names and descriptions, and every key and value of each tool's
// parameter schema) except the relevant record's own search-result line and,
// on a Trace B S2 cell, the read_file result (FileS2 states it by design).
func recallLeak(f recallFamily, suffix string, req provider.ChatRequest) error {
	v := f.Value
	_, relID, _, _, err := f.layout()
	if err != nil {
		return err
	}
	check := func(where, text string) error {
		if statesValue(text, v, v.Lesson) {
			return fmt.Errorf("%s states the lesson value %q", where, v.Lesson)
		}
		return nil
	}
	tools := 0
	for i, m := range req.Messages {
		text := m.Content
		if m.Role == "tool" {
			tools++
			if tools == 1 {
				var keep []string
				for _, l := range strings.Split(text, "\n") {
					if parts := strings.SplitN(l, " · ", 3); len(parts) == 3 && parts[1] == relID {
						continue
					}
					keep = append(keep, l)
				}
				text = strings.Join(keep, "\n")
			} else if tools == 2 && strings.HasPrefix(suffix, "B-s2-") {
				continue
			}
		}
		where := fmt.Sprintf("message %d (%s)", i, m.Role)
		for _, c := range []struct{ name, text string }{{"", text}, {" tool name", m.ToolName}, {" tool call id", m.ToolCallID}} {
			if err := check(where+c.name, c.text); err != nil {
				return err
			}
		}
		for _, tc := range m.ToolCalls {
			for _, c := range []struct{ name, text string }{{"name", tc.Function.Name}, {"arguments", string(tc.Function.Arguments)}} {
				if err := check(fmt.Sprintf("%s tool call %s %s", where, tc.Function.Name, c.name), c.text); err != nil {
					return err
				}
			}
		}
	}
	for _, tl := range req.Tools {
		if err := check("tool "+tl.Function.Name+" name", tl.Function.Name); err != nil {
			return err
		}
		if err := check("tool "+tl.Function.Name+" description", tl.Function.Description); err != nil {
			return err
		}
		var schema any
		if err := json.Unmarshal(tl.Function.Parameters, &schema); err != nil {
			return fmt.Errorf("tool %s parameters do not decode: %w", tl.Function.Name, err)
		}
		var strs []string
		recallStrings(schema, &strs)
		for _, str := range strs {
			if err := check("tool "+tl.Function.Name+" parameter schema", str); err != nil {
				return err
			}
		}
	}
	return nil
}

// recallPair relates a cell to the base it differs from by one thing.
type recallPair struct {
	Suffix, Base, Kind string // Kind: explain | label | placebo | s7 | same
}

// recallPairs lists the arm relations of a split.
var recallPairs = []recallPair{
	{"A-1", "A-0", "explain"},
	{"A-2", "A-1", "label"},
	{"A-2-s7", "A-1", "s7"},
	{"B-s1-2", "B-s1-1", "label"},
	{"B-s2-2", "B-s2-1", "label"},
	{"B-s2-P", "B-s2-1", "placebo"},
	{"B-s2-1-repeat", "B-s2-1", "same"},
	// S1 and S2 cells of one arm differ only inside the read_file result.
	{"B-s2-1", "B-s1-1", "readonly"},
	{"B-s2-2", "B-s1-2", "readonly"},
	// A Trace A request is the prefix of the same arm's Trace B request.
	{"B-s1-1", "A-1", "prefix"},
	{"B-s2-1", "A-1", "prefix"},
	{"B-s1-2", "A-2", "prefix"},
	{"B-s2-2", "A-2", "prefix"},
}

// recallBlankRead returns a copy of req with the second tool message (the
// read_file result) emptied.
func recallBlankRead(req provider.ChatRequest) provider.ChatRequest {
	out := req
	out.Messages = append([]provider.ChatMessage(nil), req.Messages...)
	n := 0
	for i, m := range out.Messages {
		if m.Role == "tool" {
			if n++; n == 2 {
				out.Messages[i].Content = ""
			}
		}
	}
	return out
}

// recallPrefixOf reports whether a's messages are the leading messages of b
// and every other field is equal.
func recallPrefixOf(a, b provider.ChatRequest) bool {
	if len(a.Messages) >= len(b.Messages) {
		return false
	}
	cut := b
	cut.Messages = b.Messages[:len(a.Messages)]
	return recallSameRequest(a, cut)
}

// recallExplainCarriers counts the tools whose description ends with the
// explanation sentence and names them.
func recallExplainCarriers(req provider.ChatRequest) (n int, names []string) {
	for _, t := range req.Tools {
		if strings.HasSuffix(t.Function.Description, recallExplanationSentence) {
			n++
			names = append(names, t.Function.Name)
		}
	}
	return n, names
}

// checkRecallInvariants fails a family's build unless: every cell exists; each
// request's records come back in rank order; the lesson value is stated only
// where it must be; and each arm differs from its base by exactly one thing
// (the sentence, the label on the relevant record, the label on the chosen
// distractor, the placebo on the relevant record, or nothing for the repeat).
func checkRecallInvariants(f recallFamily, split string, reqs map[string]provider.ChatRequest) error {
	_, relID, distIDs, order, err := f.layout()
	if err != nil {
		return err
	}
	for _, cell := range recallCells(split) {
		req, ok := reqs[cell.Suffix]
		if !ok {
			return recallErr("family %s: cell %s was not built", f.ID, cell.Suffix)
		}
		i := recallFirstTool(req)
		if i < 0 {
			return recallErr("family %s: %s: no tool message", f.ID, cell.Suffix)
		}
		if ids, _ := recallRecordLines(req.Messages[i].Content); !slices.Equal(ids, order) {
			return recallErr("family %s: %s: record order %v differs from the rank-%d order %v: search ranks by bm25 before date, so give the three records the same token count and the same count of each query term (a path counts as its parts: worker/doc.go is worker, doc, go)", f.ID, cell.Suffix, ids, f.rank(), order)
		}
		if err := recallLeak(f, cell.Suffix, req); err != nil {
			return recallErr("family %s: %s: %w", f.ID, cell.Suffix, err)
		}
		n, names := recallExplainCarriers(req)
		switch {
		case cell.Explain && (n != 1 || names[0] != agenttools.AgentMemorySearchToolName):
			return recallErr("family %s: %s: the explanation sentence must sit on %s only (found on %v)", f.ID, cell.Suffix, agenttools.AgentMemorySearchToolName, names)
		case !cell.Explain && n != 0:
			return recallErr("family %s: %s: arm 0 carries the explanation sentence on %v", f.ID, cell.Suffix, names)
		}
	}
	for _, p := range recallPairs {
		req, ok := reqs[p.Suffix]
		if !ok {
			continue
		}
		base := reqs[p.Base] // present: the cell loop above required every cell
		var got provider.ChatRequest
		switch p.Kind {
		case "explain":
			stripped, n := recallWithoutExplanation(req)
			if _, none := recallWithoutExplanation(base); n != 1 || none != 0 {
				return recallErr("family %s: %s must carry the explanation sentence and %s must not", f.ID, p.Suffix, p.Base)
			}
			got = stripped
		case "label", "placebo", "s7":
			seg, want := recallLabelSegment, relID
			if p.Kind == "placebo" {
				seg = recallPlaceboSegment
			}
			if p.Kind == "s7" {
				want = distIDs[f.s7()]
			}
			stripped, n := recallWithout(req, seg)
			if n != 1 {
				return recallErr("family %s: %s: segment %q occurs %d times in the search result, want 1", f.ID, p.Suffix, seg, n)
			}
			_, lines := recallRecordLines(req.Messages[recallFirstTool(req)].Content)
			on := ""
			for _, l := range lines {
				if strings.Contains(l, seg) {
					on = strings.SplitN(l, " · ", 3)[1]
				}
			}
			if on != want {
				return recallErr("family %s: %s: segment %q is on record %q, want %q", f.ID, p.Suffix, seg, on, want)
			}
			got = stripped
		case "same":
			got = req
		case "readonly":
			got, base = recallBlankRead(req), recallBlankRead(base)
		case "prefix":
			if !recallPrefixOf(base, req) {
				return recallErr("family %s: %s does not extend %s (same arm): its messages are not a strict leading prefix of this request's, or another field differs", f.ID, p.Suffix, p.Base)
			}
			continue
		}
		if !recallSameRequest(got, base) {
			return recallErr("family %s: %s differs from %s by more than its one change (%s)", f.ID, p.Suffix, p.Base, p.Kind)
		}
	}
	return nil
}

// buildRecallCorpus builds the recall-label traces of split from casesPath
// into outDir and returns the cases digest (recallCasesDigest) of the file.
func buildRecallCorpus(ctx context.Context, casesPath, outDir, split string) (string, error) {
	return buildRecallCorpusWith(ctx, casesPath, outDir, split, recallBuildOptions{})
}

// buildRecallCorpusWith is buildRecallCorpus with test options.
func buildRecallCorpusWith(ctx context.Context, casesPath, outDir, split string, opts recallBuildOptions) (string, error) {
	cases, err := loadRecallCases(casesPath)
	if err != nil {
		return "", err
	}
	if err := cases.validate(split, opts.SkipBalance); err != nil {
		return "", err
	}
	digest, err := recallCasesDigest(cases, split)
	if err != nil {
		return "", err
	}
	tmpls := map[string]recallTemplate{}
	for _, t := range cases.Templates {
		tmpls[t.ID] = t
	}
	var files []filePublication
	names := map[string]bool{}
	for _, f := range cases.Families {
		if f.Split != split {
			continue
		}
		traces, reqs, err := buildRecallFamily(ctx, f, tmpls[f.Template], split)
		if err != nil {
			return "", err
		}
		if err := checkRecallInvariants(f, split, reqs); err != nil {
			return "", err
		}
		for _, t := range traces {
			if names[t.ID] {
				return "", recallErr("duplicate trace id %s", t.ID)
			}
			names[t.ID] = true
			if err := validateTrace(t); err != nil {
				return "", recallErr("built trace %s: %w", t.ID, err)
			}
			data, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				return "", err
			}
			files = append(files, filePublication{target: filepath.Join(outDir, t.ID+".json"), data: append(data, '\n'), mode: 0o644})
		}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", recallErr("%w", err)
	}
	// Serialize the ownership check and publication across both splits.
	lock := filepath.Join(outDir, ".recall-build.lock")
	if err := os.Mkdir(lock, 0o700); err != nil {
		if os.IsExist(err) {
			return "", recallErr("output lock %s exists; if a previous build was interrupted, remove the empty lock directory after it exits", lock)
		}
		return "", recallErr("acquire output lock %s: %w", lock, err)
	}
	defer func() {
		if err := os.Remove(lock); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "recall build: release output lock: %v\n", err)
		}
	}()
	removals, err := recallOutputRemovals(casesPath, outDir, split, files)
	if err != nil {
		return "", recallErr("%w", err)
	}
	out, err := publishFileSet(files, removals)
	for _, w := range out.cleanupWarnings {
		_, _ = fmt.Fprintf(os.Stderr, "recall build: cleanup warning: %v\n", w)
	}
	if err != nil {
		return "", err
	}
	return digest, nil
}

// recallOutputRemovals preflights every replacement and finds obsolete outputs
// of this split. A recall-like filename alone never confers ownership.
func recallOutputRemovals(casesPath, dir, split string, files []filePublication) ([]string, error) {
	input, err := os.Stat(casesPath)
	if err != nil {
		return nil, err
	}
	entries, err := filesWithPrefix(dir, "recall-")
	if err != nil {
		return nil, err
	}
	planned := make(map[string]bool, len(files))
	for _, f := range files {
		planned[f.target] = true
	}
	// Lstat the requested spellings too: on a case-insensitive filesystem a
	// differently cased directory entry can still occupy a requested path.
	for _, p := range entries {
		if _, exists := planned[p]; !exists {
			planned[p] = false
		}
	}
	var removals []string
	for p, replace := range planned {
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if os.SameFile(input, info) {
			if replace {
				return nil, fmt.Errorf("output %q aliases the cases input", p)
			}
			continue
		}
		owned, err := ownedRecallOutput(p, split, info)
		if err != nil {
			return nil, err
		}
		if replace && !owned {
			return nil, fmt.Errorf("refuse to replace %q: not a %s recall output", p, split)
		}
		if !replace && owned {
			removals = append(removals, p)
		}
	}
	sort.Strings(removals)
	return removals, nil
}

func ownedRecallOutput(path, split string, info os.FileInfo) (bool, error) {
	id := strings.TrimSuffix(filepath.Base(path), ".json")
	family, cell, err := parseRecallTraceID(id)
	if !info.Mode().IsRegular() || err != nil {
		return false, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var tr Trace
	if json.Unmarshal(raw, &tr) != nil || tr.ID != id || tr.Source != recallTraceSource || tr.Recall == nil {
		return false, nil
	}
	return tr.Recall.Split == split && tr.Recall.FamilyID == family && recallCellKey(tr) == cell, nil
}

// recallCasesDigest is "sha256:" plus the hex SHA-256 of the canonical JSON of
// split's selected families and the templates they use (in file order), so
// appending families of the other split leaves it unchanged. It is the value a
// registration's cases_sha256 records.
func recallCasesDigest(c recallCases, split string) (string, error) {
	used := map[string]bool{}
	sel := struct {
		Split     string           `json:"split"`
		Templates []recallTemplate `json:"templates"`
		Families  []recallFamily   `json:"families"`
	}{Split: split, Templates: []recallTemplate{}, Families: []recallFamily{}}
	for _, f := range c.Families {
		if f.Split == split {
			sel.Families = append(sel.Families, f)
			used[f.Template] = true
		}
	}
	if len(sel.Families) == 0 {
		return "", recallErr("no families in split %s", split)
	}
	for _, t := range c.Templates {
		if used[t.ID] {
			sel.Templates = append(sel.Templates, t)
		}
	}
	raw, err := json.Marshal(sel)
	if err != nil {
		return "", recallErr("digest cases: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// filesWithPrefix lists the entries of dir named prefix*.json, as full paths.
// It matches on the base name, so glob characters in dir are never a pattern.
func filesWithPrefix(dir, prefix string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".json") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	return out, nil
}
