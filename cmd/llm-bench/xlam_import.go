package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
)

// xLAM-irrelevance import: convert MadeAgents/xlam-irrelevance-7.5k (CC-BY-4.0)
// into go-llm golden-empty Traces so the label-free restraint metric scores them
// when replayed. Each source record is an "irrelevance" case — tools offered,
// none relevant, correct action = call nothing — so its empty `answers` IS the
// golden-empty label, with no human authoring or sign-off needed.
const (
	xlamSource         = "xlam-irrelevance"
	xlamDifficulty     = "tempting"
	xlamFailureMode    = "irrelevant-tools-offered"
	xlamSystemPrompt   = "You are a helpful assistant with access to the tools listed for this task. Call a tool only when it is required to answer the user's request; if none of the available tools is relevant, answer directly without calling any tool."
	xlamGoldenCriteria = "None of the offered tools is relevant to the query; the correct behavior is to respond directly (or state it cannot help) WITHOUT calling any function."
	xlamRationale      = "The offered tools do not match the query's intent, so no tool call is warranted; the model should answer without invoking a function."
)

// xlamRecord is one MadeAgents/xlam-irrelevance-7.5k row. Tools and Answers are
// JSON-encoded strings (the dataset double-encodes them).
type xlamRecord struct {
	Query   string `json:"query"`
	Tools   string `json:"tools"`
	Answers string `json:"answers"`
}

// xlamRecordToTrace converts one irrelevance record into a golden-empty Trace.
// It returns an error if the record is not an irrelevance case (non-empty
// answers) or is malformed (empty query, unparseable tools/answers). idx seeds
// the trace ID. The result is validated so it cannot fail the harness loader.
func xlamRecordToTrace(rec xlamRecord, idx int) (Trace, error) {
	query := strings.TrimSpace(rec.Query)
	if query == "" {
		return Trace{}, fmt.Errorf("xlam record %d: empty query", idx)
	}
	var answers []json.RawMessage
	if err := json.Unmarshal([]byte(rec.Answers), &answers); err != nil {
		return Trace{}, fmt.Errorf("xlam record %d: parse answers: %w", idx, err)
	}
	if len(answers) != 0 {
		return Trace{}, fmt.Errorf("xlam record %d: %d answer(s) present — not an irrelevance case", idx, len(answers))
	}
	var tools []json.RawMessage
	if err := json.Unmarshal([]byte(rec.Tools), &tools); err != nil {
		return Trace{}, fmt.Errorf("xlam record %d: parse tools: %w", idx, err)
	}
	tr := Trace{
		ID:     fmt.Sprintf("xlam-irrel-%04d", idx),
		Source: xlamSource,
		System: xlamSystemPrompt,
		Tools:  tools,
		Turns:  []Turn{{Role: "user", Content: query}},
		Golden: Golden{
			ToolCalls:           []string{},
			FinalAnswerCriteria: xlamGoldenCriteria,
			Difficulty:          xlamDifficulty,
			RestraintRationale:  xlamRationale,
			FailureMode:         xlamFailureMode,
		},
	}
	if err := validateTrace(tr); err != nil {
		return Trace{}, fmt.Errorf("xlam record %d: %w", idx, err)
	}
	return tr, nil
}

// xlamImportOptions configures importXlamIrrelevance.
type xlamImportOptions struct {
	SrcPath      string // path to xlam-7.5k-irrelevancek.json
	OutDir       string // directory for emitted trace JSON files
	ManifestPath string // path for the emitted corpus manifest JSONL
	N            int    // sample size (<=0 or >available = all)
	Seed         int64  // deterministic sampling seed
	MinTools     int    // drop records with fewer than this many offered tools
}

// xlamImportResult reports the outcome of an import. Eligible counts records
// that passed the filter (before sampling); Written is how many of those were
// sampled and emitted; Filtered is records dropped as ineligible/malformed.
type xlamImportResult struct {
	Written  int
	Eligible int
	Filtered int
}

// importXlamIrrelevance reads the xLAM-irrelevance JSON array, keeps records with
// at least MinTools offered tools and an empty answer set, seeded-samples N of
// them, converts each to a golden-empty Trace, and writes the trace files plus a
// corpus manifest. Sampling is deterministic for a given (Seed, input).
func importXlamIrrelevance(opts xlamImportOptions) (xlamImportResult, error) {
	data, err := os.ReadFile(opts.SrcPath)
	if err != nil {
		return xlamImportResult{}, fmt.Errorf("xlam import: read source: %w", err)
	}
	var recs []xlamRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return xlamImportResult{}, fmt.Errorf("xlam import: parse source: %w", err)
	}

	res := xlamImportResult{}
	var eligible []xlamRecord
	for _, r := range recs {
		// Eligibility is deep: a record must pass the shallow filter AND convert
		// cleanly (idx is irrelevant to validity). Recording convert-failures here
		// as Filtered keeps one malformed third-party row from aborting the batch
		// and makes the post-sample write loop's failure a true invariant.
		if !xlamRecordEligible(r, opts.MinTools) {
			res.Filtered++
			continue
		}
		if _, err := xlamRecordToTrace(r, 0); err != nil {
			res.Filtered++
			continue
		}
		eligible = append(eligible, r)
	}
	res.Eligible = len(eligible)

	rng := rand.New(rand.NewSource(opts.Seed))
	rng.Shuffle(len(eligible), func(i, j int) { eligible[i], eligible[j] = eligible[j], eligible[i] })
	if opts.N > 0 && opts.N < len(eligible) {
		eligible = eligible[:opts.N]
	}

	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return xlamImportResult{}, fmt.Errorf("xlam import: mkdir out: %w", err)
	}
	// Clear prior xlam traces so the output dir always matches the new manifest
	// (a smaller re-run must not leave orphans that a *.json replay would pick up).
	stale, err := filepath.Glob(filepath.Join(opts.OutDir, "xlam-irrel-*.json"))
	if err != nil {
		return xlamImportResult{}, fmt.Errorf("xlam import: scan stale traces: %w", err)
	}
	for _, p := range stale {
		if err := os.Remove(p); err != nil {
			return xlamImportResult{}, fmt.Errorf("xlam import: remove stale %s: %w", p, err)
		}
	}
	var manifest Manifest
	for i, r := range eligible {
		tr, err := xlamRecordToTrace(r, i)
		if err != nil {
			// The eligibility filter already proved each record convertible, so a
			// failure here is a logic bug, not bad data — fail loud, don't undercount.
			return xlamImportResult{}, fmt.Errorf("xlam import: convert eligible record %d: %w", i, err)
		}
		path := filepath.Join(opts.OutDir, safeTraceFilename(tr.ID)+".json")
		blob, err := json.MarshalIndent(tr, "", "  ")
		if err != nil {
			return xlamImportResult{}, fmt.Errorf("xlam import: marshal %s: %w", tr.ID, err)
		}
		if err := os.WriteFile(path, blob, 0o600); err != nil {
			return xlamImportResult{}, fmt.Errorf("xlam import: write %s: %w", path, err)
		}
		manifest.Entries = append(manifest.Entries, ManifestEntry{
			TraceID:                tr.ID,
			Partition:              PartitionChallenge,
			Category:               "irrelevance",
			Source:                 xlamSource,
			AllowedAsModelEvidence: true,
		})
		res.Written++
	}
	if res.Written == 0 {
		return res, fmt.Errorf("xlam import: no eligible records written (have %d records, min-tools=%d)", len(recs), opts.MinTools)
	}
	if err := writeManifest(opts.ManifestPath, manifest); err != nil {
		return xlamImportResult{}, fmt.Errorf("xlam import: %w", err)
	}
	return res, nil
}

// xlamRecordEligible reports whether a record is a usable irrelevance case:
// non-empty query, parseable tools with at least minTools entries, and an empty
// answer set (the golden-empty signal).
func xlamRecordEligible(r xlamRecord, minTools int) bool {
	if strings.TrimSpace(r.Query) == "" {
		return false
	}
	var tools []json.RawMessage
	if err := json.Unmarshal([]byte(r.Tools), &tools); err != nil || len(tools) < minTools {
		return false
	}
	var answers []json.RawMessage
	if err := json.Unmarshal([]byte(r.Answers), &answers); err != nil || len(answers) != 0 {
		return false
	}
	return true
}
