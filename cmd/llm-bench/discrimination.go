package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	// Step 0: no declared top cluster cannot be classified. Without this the
	// "all top == 1.0" check below is vacuously true and misreports saturated.
	if len(topModels) == 0 {
		return stateUnpaired
	}
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

// round3ChallengeSource is the default provenance source whose
// valid-discriminator count the §9.2 K-gate is evaluated against (override per
// run with discriminationOptions.GateSource / -gate-source). Kept as a named
// const so the gate's default coupling to the R3-fresh stratum is explicit.
// Other strata (e.g. the round2-challenge regression anchor) are still
// classified and tabulated, but the PASS/INCONCLUSIVE verdict is decided only
// on the gate source.
const round3ChallengeSource = "round3-challenge"

// buildTraceModelQualityFromMatched groups matched labels into
// traceID -> canonical-model -> quality, after dropping judge-validation and
// non-model-evidence rows via the shared corpusEvidenceFilter. It rejects a
// duplicate (trace, model) — the classifier must see exactly one quality per
// (trace, model), keyed identically to the quality map.
//
// The {0.0, 0.5, 1.0} domain that classifyTrace's == 1.0 logic depends on is
// enforced once, at load, by loadLabels (calibration.go) for every mode; an
// out-of-domain label aborts before reaching here, so this function does not
// re-validate. The remaining label-schema limitation (an omitted
// expected_answer_quality decodes to a real-looking 0.0) is global, documented
// at loadLabels, and out of scope for discrimination.
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
	GateSource               string // provenance source the §9.2 K-gate is decided on; "" = round3ChallengeSource
}

// traceClassification is one trace's resolved state plus the inputs that
// produced it, kept for the auditable per-trace table.
type traceClassification struct {
	TraceID       string
	Source        string
	State         discriminationState
	MissingModels []string
	FloorInverted bool // floor model outscored the tied top cluster (a data anomaly)
}

// stratumFunnel tallies one provenance source: authored (manifest entries),
// captured (traces with ≥1 model-evidence label), and the per-state counts.
type stratumFunnel struct {
	Source   string
	Authored int
	Captured int
	States   map[discriminationState]int
}

// runDiscriminationReport loads (labels, artifacts, manifest), classifies every
// captured trace via classifyTrace, tallies a per-source authored/captured/state
// funnel, writes the derived valid-discriminator manifest, and returns the
// rendered report. TopModels and FloorModel are required. It tolerates partial
// labeling: missing cells classify as unpaired/missing rather than aborting.
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

	// Resolve panel selectors against the captured keyspace. An exact match wins;
	// otherwise the transport prefix is ignored (consistent with
	// modelSelectorsEqual used elsewhere) so "gemma4:31b" resolves to a captured
	// "ollama/gemma4:31b" instead of silently classifying the trace as unpaired.
	// An ambiguous selector (same base model under two transports) is an operator
	// input error and fails loud, not a silent gate deflation. The resolved top
	// cluster is de-duplicated so two spellings of one model cannot double-list.
	present := presentModelKeys(qual)
	topCanon := make([]string, 0, len(opts.TopModels))
	seenTop := make(map[string]struct{}, len(opts.TopModels))
	for _, m := range opts.TopModels {
		r, rerr := resolvePanelSelector(m, present)
		if rerr != nil {
			return "", rerr
		}
		if _, dup := seenTop[r]; dup {
			continue
		}
		seenTop[r] = struct{}{}
		topCanon = append(topCanon, r)
	}
	floorCanon, ferr := resolvePanelSelector(opts.FloorModel, present)
	if ferr != nil {
		return "", ferr
	}

	gateSource := strings.TrimSpace(opts.GateSource)
	if gateSource == "" {
		gateSource = round3ChallengeSource
	}

	// sourceByTrace yields "" for a trace absent from the manifest. That cannot
	// happen here: buildTraceModelQuality routes through corpusEvidenceFilter,
	// which only keeps manifest-known trace IDs, so every tid in qual has a
	// source. An empty-source stratum would otherwise surface visibly in the
	// funnel (it never matches gateSource, so it cannot inflate the K-gate).
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
			FloorInverted: floorBeatsTop(models, topCanon, floorQ, floorOK),
		})
	}
	sort.Slice(classifications, func(i, j int) bool {
		if classifications[i].Source != classifications[j].Source {
			return classifications[i].Source < classifications[j].Source
		}
		return classifications[i].TraceID < classifications[j].TraceID
	})

	funnels := buildStratumFunnels(manifest, captured, classifications)
	if err := writeDiscriminatorManifest(opts.DiscriminatorManifestOut, opts.ManifestPath, manifest, classifications); err != nil {
		return "", err
	}
	return formatDiscriminationReport(classifications, funnels, topCanon, floorCanon, gateSource, opts.DiscriminatorManifestOut), nil
}

// presentModelKeys is the set of canonical model keys actually appearing in the
// quality map. Panel selectors resolve against it.
func presentModelKeys(qual map[string]map[string]float64) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, models := range qual {
		for k := range models {
			keys[k] = struct{}{}
		}
	}
	return keys
}

// resolvePanelSelector maps a user-supplied -top-models/-floor-model selector to
// the captured artifact key it denotes. An exact normalized match wins;
// otherwise the transport prefix is ignored (matching modelSelectorsEqual used
// elsewhere in the tool), so "gemma4:31b" resolves to a captured
// "ollama/gemma4:31b". A selector matching the same base model under two
// transports is ambiguous operator input and errors loudly. An unresolved
// selector returns its normalized form, which is absent from the keyspace and so
// surfaces as unpaired/missing rather than being silently mis-bound.
func resolvePanelSelector(sel string, present map[string]struct{}) (string, error) {
	n := normalizeModelSelector(sel)
	if _, ok := present[n]; ok {
		return n, nil
	}
	target := strings.ToLower(modelSelectorWithoutBenchProvider(sel))
	var matches []string
	for k := range present {
		if strings.ToLower(modelSelectorWithoutBenchProvider(k)) == target {
			matches = append(matches, k)
		}
	}
	if len(matches) > 1 {
		sort.Strings(matches)
		return "", fmt.Errorf("discrimination: panel selector %q is ambiguous; it matches %v — qualify it with the transport prefix", sel, matches)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return n, nil
}

// floorBeatsTop reports whether the floor model strictly outscored the best top
// model — an anomaly (a cheap floor model beating the frontier panel) worth
// flagging in any state it can occur, not just floor-only. It requires every top
// model present (an incomplete top cannot be cleanly compared).
func floorBeatsTop(models map[string]float64, topModels []string, floorQ float64, floorOK bool) bool {
	if !floorOK || len(topModels) == 0 {
		return false
	}
	maxTop, ok := models[topModels[0]]
	if !ok {
		return false
	}
	for _, m := range topModels[1:] {
		v, ok := models[m]
		if !ok {
			return false
		}
		if v > maxTop {
			maxTop = v
		}
	}
	return floorQ > maxTop
}

// validDiscriminatorIDs is the set of trace IDs that classified as
// valid-discriminator. It is the single source of truth for both the derived
// manifest's contents and the report's valid-discriminator count, so the two
// cannot drift.
func validDiscriminatorIDs(cls []traceClassification) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, c := range cls {
		if c.State == stateValidDiscriminator {
			ids[c.TraceID] = struct{}{}
		}
	}
	return ids
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
	// Count each trace once per source; a trace listed under multiple manifest
	// entries must not inflate Authored/Captured past the unique trace count.
	authoredSeen := map[string]map[string]bool{}
	for _, e := range m.Entries {
		if e.Partition == PartitionJudgeValidation || !e.AllowedAsModelEvidence {
			continue
		}
		seen := authoredSeen[e.Source]
		if seen == nil {
			seen = map[string]bool{}
			authoredSeen[e.Source] = seen
		}
		if seen[e.TraceID] {
			continue
		}
		seen[e.TraceID] = true
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
func writeDiscriminatorManifest(path, sourceManifestPath string, m Manifest, cls []traceClassification) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if err := validateDiscriminatorManifestOutputPath(path, sourceManifestPath); err != nil {
		return err
	}
	sub := m.entriesFor(validDiscriminatorIDs(cls))
	return writeManifest(path, sub)
}

func validateDiscriminatorManifestOutputPath(path, sourceManifestPath string) error {
	sourceManifestPath = strings.TrimSpace(sourceManifestPath)
	if sourceManifestPath == "" {
		return nil
	}
	if sameCleanedPath(path, sourceManifestPath) {
		return fmt.Errorf("discrimination: -discriminator-manifest-out %q must differ from -corpus-manifest %q", path, sourceManifestPath)
	}
	sourceInfo, err := os.Stat(sourceManifestPath)
	if err != nil {
		return fmt.Errorf("discrimination: stat -corpus-manifest %q: %w", sourceManifestPath, err)
	}
	outInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("discrimination: stat -discriminator-manifest-out %q: %w", path, err)
	}
	if os.SameFile(outInfo, sourceInfo) {
		return fmt.Errorf("discrimination: -discriminator-manifest-out %q resolves to the same file as -corpus-manifest %q; choose a different output path", path, sourceManifestPath)
	}
	return nil
}

func sameCleanedPath(a, b string) bool {
	cleanA := filepath.Clean(a)
	cleanB := filepath.Clean(b)
	absA, errA := filepath.Abs(cleanA)
	absB, errB := filepath.Abs(cleanB)
	if errA == nil && errB == nil {
		return absA == absB
	}
	return cleanA == cleanB
}

// formatDiscriminationReport renders the funnel table, the §9.2 K-gate verdict
// (decided on gateSource when that stratum exists), and the per-trace
// classification table. cls is expected pre-sorted by (source, trace) by the
// caller. The whole output is run through redactPaths before return.
func formatDiscriminationReport(cls []traceClassification, funnels []stratumFunnel, topCanon []string, floorCanon, gateSource, discriminatorManifestOut string) string {
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

	// K-gate verdict on the gate stratum. Round-2 anchor discovery and other
	// non-gated reports are valid uses of this tool, but they do not have a gate
	// stratum present.
	gateValid := 0
	hasGate := false
	sourcesSeen := make([]string, 0, len(funnels))
	for _, f := range funnels {
		sourcesSeen = append(sourcesSeen, f.Source)
		if f.Source == gateSource {
			hasGate = true
			gateValid = f.States[stateValidDiscriminator]
		}
	}
	if !hasGate {
		seen := "(none)"
		if len(sourcesSeen) > 0 {
			seen = strings.Join(sourcesSeen, ", ")
		}
		// Naming the sources present makes a manifest source typo (which would
		// otherwise read as a benign "nothing to gate") auditable at a glance.
		fmt.Fprintf(&b, "## K-gate (spec §9.2): N/A — no %s stratum present (sources seen: %s)\n\n", gateSource, seen)
	} else {
		verdict := "PASS"
		if gateValid < round3DiscriminatorGateK {
			verdict = "INCONCLUSIVE (under-resolved; do not cite as a frontier-vs-local conclusion)"
		}
		fmt.Fprintf(&b, "## K-gate (spec §9.2): VALID_DISCRIMINATORS=%d of K=%d → %s\n\n", gateValid, round3DiscriminatorGateK, verdict)
	}

	if strings.TrimSpace(discriminatorManifestOut) != "" {
		if len(validDiscriminatorIDs(cls)) == 0 {
			// The derived manifest was truncated to empty; loadManifest rejects an
			// empty file, so do not send the operator off to consume it.
			fmt.Fprintln(&b, "Derived manifest: 0 valid discriminators — the derived manifest is empty and has nothing to consume downstream.")
		} else {
			fmt.Fprintf(&b, "Derived manifest note: contains every valid-discriminator trace across all reported sources. For the gate stratum's top-resolution view, consume it with `-corpus-sources %s`; use other source filters for anchor or regression views.\n", gateSource)
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "## Per-trace classification")
	fmt.Fprintln(&b, "| Trace | Source | State | Details |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, c := range cls {
		details := ""
		switch {
		case len(c.MissingModels) > 0:
			details = "missing: " + strings.Join(c.MissingModels, ", ")
		case c.FloorInverted:
			details = "anomaly: floor outscored top cluster"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", markdownCell(c.TraceID), markdownCell(c.Source), c.State, markdownCell(details))
	}
	fmt.Fprintln(&b)
	return redactPaths(b.String())
}
