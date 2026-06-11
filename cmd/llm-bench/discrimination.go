package main

import "fmt"

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

var _ = fmt.Sprintf // retained for report formatting added in Task A5
