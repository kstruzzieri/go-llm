package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// discriminationState is the per-trace classification from the Round-3 design
// spec §9.1. The states are mutually exclusive; an ordered decision procedure
// (classifyTrace) resolves literal-wording overlap by precedence.
type discriminationState string

const (
	// stateValidDiscriminator: the top cluster splits AND at least one top
	// model solved it (1.0) — the only state that counts toward the K-gate.
	stateValidDiscriminator discriminationState = "valid-discriminator"
	// stateSaturated: every top model aced it — no ranking signal (the
	// Round-2 failure mode).
	stateSaturated discriminationState = "saturated"
	// stateUnsolved: top cluster splits but no top model reached 1.0 — too
	// hard / possibly unfair; review for trivia.
	stateUnsolved discriminationState = "unsolved"
	// stateFloorOnly: top cluster tied below 1.0, only the floor model
	// separates — the qwen8-only spread that fooled Round-2's floor gate.
	stateFloorOnly discriminationState = "floor-only"
	// stateNoSignal: all models tied below 1.0 — degenerate.
	stateNoSignal discriminationState = "no-signal"
	// stateUnpaired: a top model (or the floor model when it is needed to
	// decide) lacks a label — cannot classify; surfaced loudly, never
	// silently treated as agreement.
	stateUnpaired discriminationState = "unpaired/missing"
)

// allDiscriminationStates is the canonical report/order list. Keeping it
// explicit makes a renamed or added state a deliberate, test-visible change.
func allDiscriminationStates() []discriminationState {
	return []discriminationState{
		stateValidDiscriminator, stateSaturated, stateUnsolved,
		stateFloorOnly, stateNoSignal, stateUnpaired,
	}
}

// classifyTrace applies the ordered §9.1 decision procedure to one trace.
//   - top:        canonical-model -> label quality for the labels present.
//   - topModels:  the declared top cluster, canonical selectors, in order.
//   - floor:      the floor model's label quality.
//   - floorOK:    whether the floor model has a label for this trace.
//
// A top model absent from top yields stateUnpaired (step 1). When the
// procedure reaches step 4 (top fully tied below 1.0) and the floor label is
// missing, the trace cannot be split into floor-only vs no-signal, so it is
// also stateUnpaired rather than a guessed agreement.
func classifyTrace(top map[string]float64, topModels []string, floor float64, floorOK bool) discriminationState {
	// Step 1: any top model missing a label.
	for _, m := range topModels {
		if _, ok := top[m]; !ok {
			return stateUnpaired
		}
	}
	// Step 2: all top models == 1.0.
	all1 := true
	for _, m := range topModels {
		if top[m] != 1.0 {
			all1 = false
			break
		}
	}
	if all1 {
		return stateSaturated
	}
	// Step 3: top models NOT all equal.
	if !topAllEqual(top, topModels) {
		for _, m := range topModels {
			if top[m] == 1.0 {
				return stateValidDiscriminator
			}
		}
		return stateUnsolved
	}
	// Step 4: top fully tied, value < 1.0.
	if !floorOK {
		return stateUnpaired
	}
	tied := top[topModels[0]]
	if floor != tied {
		return stateFloorOnly
	}
	return stateNoSignal
}

// topAllEqual reports whether every declared top model shares one label value.
// Callers guarantee each topModels entry is present in top (step 1 ran first).
func topAllEqual(top map[string]float64, topModels []string) bool {
	if len(topModels) == 0 {
		return true
	}
	first := top[topModels[0]]
	for _, m := range topModels[1:] {
		if top[m] != first {
			return false
		}
	}
	return true
}

// round3DiscriminatorGateK is the §9.2 inconclusive-run gate: the R3-fresh
// stratum must yield at least this many valid discriminators or the run is
// recorded as under-resolved and NOT promoted as a frontier-vs-local
// conclusion. It is NOT the accepted-run promotion gate (that stays ≥50
// labels / ≥20 fully paired retained traces — see §9.2 / L9).
const round3DiscriminatorGateK = 10

// buildTraceModelQualityFromMatched groups matched labels into
// traceID -> canonical-model -> quality, after dropping judge-validation and
// non-model-evidence rows via the shared corpusEvidenceFilter. It rejects a
// duplicate (trace, model) — the classifier must see exactly one quality per
// (trace, model), keyed identically to the quality map.
func buildTraceModelQualityFromMatched(matched []matchedLabel, filter *corpusFilter) (map[string]map[string]float64, error) {
	if filter != nil {
		keep, _, _ := corpusEvidenceFilter(filter.Manifest, filter.Selection, tracesFromArtifacts(matchedArtifacts(matched)))
		// Unlike manual/paired reports, discrimination tolerates partial
		// labeling: missing-selected evidence does not abort the report. Gaps
		// surface in the authored-vs-captured funnel downstream.
		matched = filterMatchedByTrace(matched, keep)
	}

	qual := make(map[string]map[string]float64)
	seen := make(map[manualLabelKey]string, len(matched))
	for _, m := range matched {
		tid := m.Artifact.TraceID
		model := normalizeModelSelector(m.Artifact.CandidateModel)
		key := manualLabelKey{traceID: tid, model: model}
		id := tid + "/" + model
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf("discrimination: %s and %s map to the same (trace, model); one artifact per (trace, model) is required", prev, id)
		}
		seen[key] = id
		if qual[tid] == nil {
			qual[tid] = make(map[string]float64)
		}
		qual[tid][model] = m.Label.ExpectedAnswerQuality
	}
	return qual, nil
}

// buildTraceModelQuality loads (labels, artifacts) and returns the per-trace
// per-model quality map. It is the IO wrapper around
// buildTraceModelQualityFromMatched used by runDiscriminationReport.
func buildTraceModelQuality(labelsPath, artifactsPath string, filter *corpusFilter) (map[string]map[string]float64, error) {
	matched, _, err := loadLabelsMatchedAgainst(labelsPath, artifactsPath)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		return nil, fmt.Errorf("discrimination: no labels matched artifacts in %q / %q", labelsPath, artifactsPath)
	}
	return buildTraceModelQualityFromMatched(matched, filter)
}

// discriminationOptions configures runDiscriminationReport.
type discriminationOptions struct {
	LabelsPath               string
	ArtifactsPath            string
	ManifestPath             string
	TopModels                []string // canonical-or-prefixed selectors, ANDed as the top cluster
	FloorModel               string
	DiscriminatorManifestOut string // gitignored derived subset manifest; "" disables
}

// traceClassification is one trace's resolved state plus the inputs that
// produced it, kept for the auditable per-trace table.
type traceClassification struct {
	TraceID       string
	Source        string
	State         discriminationState
	MissingModels []string
}

// stratumFunnel tallies one provenance source: authored (manifest entries),
// captured (traces with ≥1 model-evidence label), and the per-state counts.
type stratumFunnel struct {
	Source   string
	Authored int
	Captured int
	States   map[discriminationState]int
}

func runDiscriminationReport(opts discriminationOptions) (string, error) {
	if len(opts.TopModels) == 0 {
		return "", fmt.Errorf("discrimination: -top-models is required")
	}
	if strings.TrimSpace(opts.FloorModel) == "" {
		return "", fmt.Errorf("discrimination: -floor-model is required")
	}
	manifest, err := loadManifest(opts.ManifestPath)
	if err != nil {
		return "", fmt.Errorf("discrimination: load manifest: %w", err)
	}
	filter := &corpusFilter{Manifest: manifest, Selection: corpusSelection{}}
	qual, err := buildTraceModelQuality(opts.LabelsPath, opts.ArtifactsPath, filter)
	if err != nil {
		return "", err
	}

	topCanon := make([]string, len(opts.TopModels))
	for i, m := range opts.TopModels {
		topCanon[i] = normalizeModelSelector(m)
	}
	floorCanon := normalizeModelSelector(opts.FloorModel)

	sourceByTrace := make(map[string]string, len(manifest.Entries))
	for _, e := range manifest.Entries {
		sourceByTrace[e.TraceID] = e.Source
	}

	var classifications []traceClassification
	captured := make(map[string]bool)
	for tid, models := range qual {
		captured[tid] = true
		floorQ, floorOK := models[floorCanon]
		state := classifyTrace(models, topCanon, floorQ, floorOK)
		classifications = append(classifications, traceClassification{
			TraceID:       tid,
			Source:        sourceByTrace[tid],
			State:         state,
			MissingModels: missingModelsForClassification(models, topCanon, floorCanon, state),
		})
	}
	sort.Slice(classifications, func(i, j int) bool {
		if classifications[i].Source != classifications[j].Source {
			return classifications[i].Source < classifications[j].Source
		}
		return classifications[i].TraceID < classifications[j].TraceID
	})

	funnels := buildStratumFunnels(manifest, captured, classifications)
	if err := writeDiscriminatorManifest(opts.DiscriminatorManifestOut, manifest, classifications); err != nil {
		return "", err
	}
	return formatDiscriminationReport(classifications, funnels, topCanon, floorCanon), nil
}

// missingModelsForClassification returns the declared panel members whose
// labels were missing when the trace resolved to unpaired/missing. This keeps
// missing coverage auditable in the per-trace table instead of hiding it in a
// state count.
func missingModelsForClassification(models map[string]float64, topModels []string, floorModel string, state discriminationState) []string {
	if state != stateUnpaired {
		return nil
	}
	var missing []string
	for _, m := range topModels {
		if _, ok := models[m]; !ok {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return missing
	}
	if _, ok := models[floorModel]; !ok {
		missing = append(missing, floorModel)
	}
	return missing
}

// buildStratumFunnels tallies authored/captured/state counts per source. Only
// model-evidence challenge entries are counted as authored (judge-validation
// canaries are excluded from the discrimination view).
func buildStratumFunnels(m Manifest, captured map[string]bool, cls []traceClassification) []stratumFunnel {
	bySource := map[string]*stratumFunnel{}
	order := []string{}
	get := func(src string) *stratumFunnel {
		f, ok := bySource[src]
		if !ok {
			f = &stratumFunnel{Source: src, States: map[discriminationState]int{}}
			bySource[src] = f
			order = append(order, src)
		}
		return f
	}
	for _, e := range m.Entries {
		if e.Partition == PartitionJudgeValidation || !e.AllowedAsModelEvidence {
			continue
		}
		f := get(e.Source)
		f.Authored++
		if captured[e.TraceID] {
			f.Captured++
		}
	}
	for _, c := range cls {
		get(c.Source).States[c.State]++
	}
	sort.Strings(order)
	out := make([]stratumFunnel, 0, len(order))
	for _, src := range order {
		out = append(out, *bySource[src])
	}
	return out
}

// writeDiscriminatorManifest emits the valid-discriminator subset as a
// gitignored manifest in the same ManifestEntry format, preserving manifest
// order so downstream -manual-report / -paired-report can consume it directly
// via -corpus-manifest. Empty path disables.
func writeDiscriminatorManifest(path string, m Manifest, cls []traceClassification) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	valid := make(map[string]struct{})
	for _, c := range cls {
		if c.State == stateValidDiscriminator {
			valid[c.TraceID] = struct{}{}
		}
	}
	sub := m.entriesFor(valid)
	if len(sub.Entries) == 0 {
		// Truncate/clear any stale file from a previous run, but still allow
		// the discrimination report to be emitted. loadManifest rejects an
		// empty file, which is the right failure if someone accidentally tries
		// to consume a zero-discriminator selector downstream.
		return os.WriteFile(path, nil, 0o600)
	}
	return writeManifest(path, sub)
}

func formatDiscriminationReport(cls []traceClassification, funnels []stratumFunnel, topCanon []string, floorCanon string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# llm-bench — discrimination report (spec §9)")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Top cluster: %s\n", strings.Join(topCanon, ", "))
	fmt.Fprintf(&b, "Floor model: %s\n\n", floorCanon)

	fmt.Fprintln(&b, "## Funnel by stratum (source)")
	fmt.Fprintln(&b, "| Source | Authored | Captured | valid-discriminator | saturated | unsolved | floor-only | no-signal | unpaired/missing |")
	fmt.Fprintln(&b, "|---|--:|--:|--:|--:|--:|--:|--:|--:|")
	for _, f := range funnels {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d | %d |\n",
			markdownCell(f.Source), f.Authored, f.Captured,
			f.States[stateValidDiscriminator], f.States[stateSaturated], f.States[stateUnsolved],
			f.States[stateFloorOnly], f.States[stateNoSignal], f.States[stateUnpaired])
	}
	fmt.Fprintln(&b)

	// K-gate verdict on the R3-fresh stratum.
	r3Valid := 0
	for _, f := range funnels {
		if f.Source == "round3-challenge" {
			r3Valid = f.States[stateValidDiscriminator]
		}
	}
	verdict := "PASS"
	if r3Valid < round3DiscriminatorGateK {
		verdict = "INCONCLUSIVE (under-resolved; do not cite as a frontier-vs-local conclusion)"
	}
	fmt.Fprintf(&b, "## K-gate (spec §9.2): VALID_DISCRIMINATORS=%d of K=%d → %s\n\n", r3Valid, round3DiscriminatorGateK, verdict)

	fmt.Fprintln(&b, "## Per-trace classification")
	fmt.Fprintln(&b, "| Trace | Source | State | Details |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, c := range cls {
		details := ""
		if len(c.MissingModels) > 0 {
			details = "missing: " + strings.Join(c.MissingModels, ", ")
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", markdownCell(c.TraceID), markdownCell(c.Source), c.State, markdownCell(details))
	}
	fmt.Fprintln(&b)
	return redactPaths(b.String())
}
