package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CorpusPartition is the closed set of Round-2 corpus partitions. Natural and
// challenge are accepted-run model evidence and answer different questions (so
// they are reported separately, never averaged); judge-validation items are
// scorer-calibration evidence only and never count as model-workload evidence.
type CorpusPartition string

const (
	PartitionNatural         CorpusPartition = "natural"
	PartitionChallenge       CorpusPartition = "challenge"
	PartitionJudgeValidation CorpusPartition = "judge-validation"
)

// corpusPartitionOrder is the canonical display order for partition tables.
var corpusPartitionOrder = []CorpusPartition{PartitionNatural, PartitionChallenge, PartitionJudgeValidation}

func validCorpusPartition(p CorpusPartition) bool {
	switch p {
	case PartitionNatural, PartitionChallenge, PartitionJudgeValidation:
		return true
	default:
		return false
	}
}

// ManifestEntry classifies one trace: its partition, category, provenance
// source, and whether it may be cited as accepted-run model evidence (curated
// judge-validation fixtures set this false so they never inflate model scores).
type ManifestEntry struct {
	TraceID                string          `json:"trace_id"`
	Partition              CorpusPartition `json:"partition"`
	Category               string          `json:"category"`
	Source                 string          `json:"source,omitempty"`
	AllowedAsModelEvidence bool            `json:"allowed_as_model_evidence"`
}

// Manifest is the corpus descriptor: one ManifestEntry per trace. It is a
// partition/category descriptor, distinct from traceSetManifestHash (a content
// fingerprint of the trace bytes).
type Manifest struct {
	Entries []ManifestEntry
}

// writeManifest writes the manifest as JSONL (one entry per line), mirroring
// the artifacts/labels conventions.
func writeManifest(path string, m Manifest) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("manifest: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("manifest: open output: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("manifest: close output: %w", closeErr)
		}
	}()
	enc := json.NewEncoder(f)
	for _, e := range m.Entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("manifest: encode entry %q: %w", e.TraceID, err)
		}
	}
	return nil
}

// loadManifest reads and validates a JSONL manifest: every entry must have a
// known partition, a non-empty category, and a trace ID unique across the file;
// an empty manifest is an error.
func loadManifest(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var m Manifest
	seen := make(map[string]struct{})
	dec := json.NewDecoder(f)
	for dec.More() {
		var e ManifestEntry
		if err := dec.Decode(&e); err != nil {
			return Manifest{}, fmt.Errorf("manifest: decode entry: %w", err)
		}
		if strings.TrimSpace(e.TraceID) == "" {
			return Manifest{}, fmt.Errorf("manifest: entry with empty trace_id")
		}
		if !validCorpusPartition(e.Partition) {
			return Manifest{}, fmt.Errorf("manifest: entry %q has unknown partition %q (want natural, challenge, or judge-validation)", e.TraceID, e.Partition)
		}
		if strings.TrimSpace(e.Category) == "" {
			return Manifest{}, fmt.Errorf("manifest: entry %q has empty category", e.TraceID)
		}
		if _, dup := seen[e.TraceID]; dup {
			return Manifest{}, fmt.Errorf("manifest: duplicate trace_id %q", e.TraceID)
		}
		seen[e.TraceID] = struct{}{}
		m.Entries = append(m.Entries, e)
	}
	if len(m.Entries) == 0 {
		return Manifest{}, fmt.Errorf("manifest: %q is empty", path)
	}
	return m, nil
}

// corpusCounts tallies a manifest by partition and category.
type corpusCounts struct {
	ByPartition map[CorpusPartition]int
	ByCategory  map[string]int
	Total       int
}

// Counts tallies the manifest entries by partition and category.
func (m Manifest) Counts() corpusCounts {
	c := corpusCounts{
		ByPartition: make(map[CorpusPartition]int),
		ByCategory:  make(map[string]int),
		Total:       len(m.Entries),
	}
	for _, e := range m.Entries {
		c.ByPartition[e.Partition]++
		c.ByCategory[e.Category]++
	}
	return c
}

// corpusSelection filters a manifest into a run. Empty Partitions/Categories
// mean "all"; OnlyModelEvidence restricts to entries flagged as accepted-run
// model evidence.
type corpusSelection struct {
	Partitions        []CorpusPartition
	Categories        []string
	OnlyModelEvidence bool
}

// Select returns the trace IDs matching the selection — the builder that
// assembles a run from manifest selections.
func (m Manifest) Select(sel corpusSelection) []string {
	partOK := make(map[CorpusPartition]struct{}, len(sel.Partitions))
	for _, p := range sel.Partitions {
		partOK[p] = struct{}{}
	}
	catOK := make(map[string]struct{}, len(sel.Categories))
	for _, c := range sel.Categories {
		catOK[c] = struct{}{}
	}
	var out []string
	for _, e := range m.Entries {
		if len(partOK) > 0 {
			if _, ok := partOK[e.Partition]; !ok {
				continue
			}
		}
		if len(catOK) > 0 {
			if _, ok := catOK[e.Category]; !ok {
				continue
			}
		}
		if sel.OnlyModelEvidence && !e.AllowedAsModelEvidence {
			continue
		}
		out = append(out, e.TraceID)
	}
	return out
}

// parseCorpusPartitions parses a comma-separated -corpus-partitions flag into a
// validated partition list. Empty input returns nil (meaning "all"); an unknown
// partition is an error.
func parseCorpusPartitions(s string) ([]CorpusPartition, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []CorpusPartition
	for _, raw := range strings.Split(s, ",") {
		p := CorpusPartition(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if !validCorpusPartition(p) {
			return nil, fmt.Errorf("unknown corpus partition %q (want natural, challenge, or judge-validation)", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// splitCommaList splits a comma-separated flag value, trimming each item and
// dropping empties. Empty input returns nil ("all"). Used for free-form
// category selection (categories are not a closed enum, so no validation).
func splitCommaList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(s, ",") {
		if v := strings.TrimSpace(raw); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// entriesFor returns the sub-manifest of entries whose trace ID is in keep,
// preserving manifest order.
func (m Manifest) entriesFor(keep map[string]struct{}) Manifest {
	var sub Manifest
	for _, e := range m.Entries {
		if _, ok := keep[e.TraceID]; ok {
			sub.Entries = append(sub.Entries, e)
		}
	}
	return sub
}

// buildCorpusRun assembles a run from a manifest selection: it returns the
// loaded traces matching the selection (in manifest order), corpus report data
// scoped to that run subset, and the trace IDs that were selected but not
// present in the loaded set (so a missing-trace gap is visible, not silent).
func buildCorpusRun(m Manifest, sel corpusSelection, loaded []Trace) (run []Trace, data *corpusReportData, missing []string) {
	loadedByID := make(map[string]Trace, len(loaded))
	for _, tr := range loaded {
		loadedByID[tr.ID] = tr
	}
	manifestByID := make(map[string]ManifestEntry, len(m.Entries))
	for _, e := range m.Entries {
		manifestByID[e.TraceID] = e
	}
	keep := make(map[string]struct{})
	for _, id := range m.Select(sel) {
		if tr, ok := loadedByID[id]; ok {
			run = append(run, tr)
			keep[id] = struct{}{}
		} else {
			missing = append(missing, id)
		}
	}
	var unclassifiedLoaded []string
	for _, tr := range loaded {
		if _, ok := manifestByID[tr.ID]; !ok {
			unclassifiedLoaded = append(unclassifiedLoaded, tr.ID)
		}
	}
	sub := m.entriesFor(keep)
	data = &corpusReportData{
		Counts:               sub.Counts(),
		TraceToPartition:     sub.partitionByTrace(),
		TraceToModelEvidence: sub.modelEvidenceByTrace(),
		MissingSelected:      append([]string(nil), missing...),
		UnclassifiedLoaded:   unclassifiedLoaded,
	}
	return run, data, missing
}

type corpusResultExclusions struct {
	JudgeValidation  int
	NonModelEvidence int
	Unclassified     int
}

// modelEvidenceResults returns only results that may enter model-quality
// aggregates. Corpus reports exclude judge-validation, loaded-without-manifest,
// and manifest rows explicitly marked not allowed as model evidence.
func modelEvidenceResults(results []Result, data *corpusReportData) ([]Result, corpusResultExclusions) {
	if data == nil {
		return results, corpusResultExclusions{}
	}
	var kept []Result
	var excluded corpusResultExclusions
	for _, r := range results {
		p, ok := data.TraceToPartition[r.TraceID]
		if !ok {
			excluded.Unclassified++
			continue
		}
		if p == PartitionJudgeValidation {
			excluded.JudgeValidation++
			continue
		}
		if !data.allowsModelEvidence(r.TraceID) {
			excluded.NonModelEvidence++
			continue
		}
		kept = append(kept, r)
	}
	return kept, excluded
}

// partitionByTrace maps each trace ID to its partition for report wiring.
func (m Manifest) partitionByTrace() map[string]CorpusPartition {
	out := make(map[string]CorpusPartition, len(m.Entries))
	for _, e := range m.Entries {
		out[e.TraceID] = e.Partition
	}
	return out
}

// modelEvidenceByTrace maps each trace ID to whether it may contribute to
// accepted-run model-quality evidence.
func (m Manifest) modelEvidenceByTrace() map[string]bool {
	out := make(map[string]bool, len(m.Entries))
	for _, e := range m.Entries {
		out[e.TraceID] = e.AllowedAsModelEvidence
	}
	return out
}

// sortedCategories returns the manifest's distinct categories in stable order
// for deterministic report rendering.
func (c corpusCounts) sortedCategories() []string {
	cats := make([]string, 0, len(c.ByCategory))
	for cat := range c.ByCategory {
		cats = append(cats, cat)
	}
	sort.Strings(cats)
	return cats
}
