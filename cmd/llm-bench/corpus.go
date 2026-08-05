package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

type filePublication struct {
	target string
	data   []byte
	mode   os.FileMode
}

type filePublicationOutcome struct {
	cleanupWarnings []error
}

type publicationTarget struct {
	path      string
	canonical string
	info      os.FileInfo
}

type filePublicationOps struct {
	rename  func(string, string) error
	remove  func(string) error
	inspect func(string) (publicationTarget, error)
}

func defaultFilePublicationOps() filePublicationOps {
	return filePublicationOps{rename: os.Rename, remove: os.Remove, inspect: inspectFilePublicationTarget}
}

func inspectFilePublicationTarget(path string) (publicationTarget, error) {
	if strings.TrimSpace(path) == "" {
		return publicationTarget{}, errors.New("empty path")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return publicationTarget{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil && !os.IsNotExist(err) {
		return publicationTarget{}, err
	}
	if os.IsNotExist(err) {
		info = nil
	}
	parent, _, err := canonicalFuturePath(filepath.Dir(abs))
	if err != nil {
		return publicationTarget{}, err
	}
	return publicationTarget{path: path, canonical: filepath.Join(parent, filepath.Base(abs)), info: info}, nil
}

func publicationTargetsAlias(left, right publicationTarget) bool {
	if left.canonical == right.canonical {
		return true
	}
	if left.info != nil && right.info != nil {
		return os.SameFile(left.info, right.info)
	}
	return left.info == nil && right.info == nil && strings.EqualFold(left.canonical, right.canonical)
}

func publishFileSet(replacements []filePublication, removals []string) (filePublicationOutcome, error) {
	return publishFileSetWithOps(replacements, removals, defaultFilePublicationOps())
}

func publishFileSetWithRename(replacements []filePublication, removals []string, rename func(string, string) error) (filePublicationOutcome, error) {
	ops := defaultFilePublicationOps()
	ops.rename = rename
	return publishFileSetWithOps(replacements, removals, ops)
}

func publishFileSetWithOps(replacements []filePublication, removals []string, ops filePublicationOps) (filePublicationOutcome, error) {
	if ops.rename == nil || ops.remove == nil || ops.inspect == nil {
		return filePublicationOutcome{}, errors.New("publish file set: incomplete file operations")
	}
	targetPaths := make([]string, 0, len(replacements)+len(removals))
	for _, replacement := range replacements {
		targetPaths = append(targetPaths, replacement.target)
	}
	targetPaths = append(targetPaths, removals...)
	targets := make([]publicationTarget, 0, len(targetPaths))
	for i, path := range targetPaths {
		target, err := ops.inspect(path)
		if err != nil {
			return filePublicationOutcome{}, fmt.Errorf("publish file set: inspect target %d %q: %w", i, path, err)
		}
		if target.info != nil && target.info.Mode()&os.ModeSymlink != 0 {
			return filePublicationOutcome{}, fmt.Errorf("publish file set: refuse symlink target %q", path)
		}
		if target.info != nil && !target.info.Mode().IsRegular() {
			return filePublicationOutcome{}, fmt.Errorf("publish file set: refuse non-regular target %q", path)
		}
		for j, prior := range targets[:i] {
			if publicationTargetsAlias(target, prior) {
				return filePublicationOutcome{}, fmt.Errorf("publish file set: target %q aliases target %d %q", path, j, prior.path)
			}
		}
		targets = append(targets, target)
	}

	type stagedFile struct {
		target string
		path   string
	}
	staged := make([]stagedFile, 0, len(replacements))
	cleanStages := func() error {
		var errs []error
		for _, stage := range staged {
			if stage.path == "" {
				continue
			}
			if err := ops.remove(stage.path); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove unused stage %q: %w", stage.path, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, replacement := range replacements {
		stage, err := os.CreateTemp(filepath.Dir(replacement.target), "."+filepath.Base(replacement.target)+".publish-*")
		if err != nil {
			return filePublicationOutcome{}, errors.Join(fmt.Errorf("publish file set: stage %q: %w", replacement.target, err), cleanStages())
		}
		stagePath := stage.Name()
		staged = append(staged, stagedFile{target: replacement.target, path: stagePath})
		if err := stage.Chmod(replacement.mode.Perm()); err != nil {
			_ = stage.Close()
			return filePublicationOutcome{}, errors.Join(fmt.Errorf("publish file set: chmod stage for %q: %w", replacement.target, err), cleanStages())
		}
		if _, err := stage.Write(replacement.data); err != nil {
			_ = stage.Close()
			return filePublicationOutcome{}, errors.Join(fmt.Errorf("publish file set: write stage for %q: %w", replacement.target, err), cleanStages())
		}
		if err := stage.Close(); err != nil {
			return filePublicationOutcome{}, errors.Join(fmt.Errorf("publish file set: close stage for %q: %w", replacement.target, err), cleanStages())
		}
	}

	type backupFile struct {
		target string
		path   string
	}
	backups := make([]backupFile, 0, len(targets))
	published := make([]string, 0, len(replacements))
	rollback := func(original error) error {
		errs := []error{original}
		for i := len(published) - 1; i >= 0; i-- {
			if err := ops.remove(published[i]); err != nil && !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("remove newly published target %q: %w", published[i], err))
			}
		}
		for i := len(backups) - 1; i >= 0; i-- {
			backup := backups[i]
			if err := ops.rename(backup.path, backup.target); err != nil {
				errs = append(errs, fmt.Errorf("restore %q from recovery backup %q: %w", backup.target, backup.path, err))
			}
		}
		if err := cleanStages(); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	}
	for _, target := range targets {
		if target.info == nil {
			continue
		}
		placeholder, err := os.CreateTemp(filepath.Dir(target.path), "."+filepath.Base(target.path)+".backup-*")
		if err != nil {
			return filePublicationOutcome{}, rollback(fmt.Errorf("publish file set: reserve backup for %q: %w", target.path, err))
		}
		backupPath := placeholder.Name()
		if err := placeholder.Close(); err != nil {
			_ = ops.remove(backupPath)
			return filePublicationOutcome{}, rollback(fmt.Errorf("publish file set: close backup placeholder for %q: %w", target.path, err))
		}
		if err := ops.remove(backupPath); err != nil {
			return filePublicationOutcome{}, rollback(fmt.Errorf("publish file set: prepare backup for %q: %w", target.path, err))
		}
		if err := ops.rename(target.path, backupPath); err != nil {
			return filePublicationOutcome{}, rollback(fmt.Errorf("publish file set: back up %q: %w", target.path, err))
		}
		backups = append(backups, backupFile{target: target.path, path: backupPath})
	}
	for i := range staged {
		if err := ops.rename(staged[i].path, staged[i].target); err != nil {
			return filePublicationOutcome{}, rollback(fmt.Errorf("publish file set: publish %q: %w", staged[i].target, err))
		}
		published = append(published, staged[i].target)
		staged[i].path = ""
	}
	var outcome filePublicationOutcome
	for _, backup := range backups {
		if err := ops.remove(backup.path); err != nil && !os.IsNotExist(err) {
			outcome.cleanupWarnings = append(outcome.cleanupWarnings, fmt.Errorf("remove backup %q: %w", backup.path, err))
		}
	}
	return outcome, nil
}

func writeFilePublicationWarnings(w io.Writer, scope string, outcome filePublicationOutcome) {
	for _, warning := range outcome.cleanupWarnings {
		_, _ = fmt.Fprintf(w, "%s: WARNING evidence set published but backup cleanup failed: %v\n", scope, warning)
	}
}

func marshalManifestJSONL(m Manifest) ([]byte, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	for _, entry := range m.Entries {
		if err := enc.Encode(entry); err != nil {
			return nil, fmt.Errorf("manifest: encode entry %q: %w", entry.TraceID, err)
		}
	}
	return raw.Bytes(), nil
}

// writeManifest writes the manifest as JSONL (one entry per line), mirroring
// the artifacts/labels conventions.
func writeManifest(path string, m Manifest) (retErr error) {
	raw, err := marshalManifestJSONL(m)
	if err != nil {
		return err
	}
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
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("manifest: write output: %w", err)
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
	if err := requireJSONDecoderEOF(dec, "manifest"); err != nil {
		return Manifest{}, err
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

// corpusSelection filters a manifest into a run. Empty
// Partitions/Categories/Sources mean "all"; OnlyModelEvidence restricts to
// entries flagged as accepted-run model evidence. Predicates AND together.
type corpusSelection struct {
	Partitions        []CorpusPartition
	Categories        []string
	Sources           []string
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
	srcOK := make(map[string]struct{}, len(sel.Sources))
	for _, s := range sel.Sources {
		srcOK[s] = struct{}{}
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
		if len(srcOK) > 0 {
			if _, ok := srcOK[e.Source]; !ok {
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
