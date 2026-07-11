package main

// vsKind classifies the outcome of the vector-space gate for message formatting.
type vsKind int

const (
	vsOK           vsKind = iota // register; no warning
	vsLegacy                     // register; corpus has no recorded vsid (cannot verify)
	vsMismatch                   // disable; single known vsid not in the current chain
	vsInconsistent               // disable; mixed known vsids, or known+legacy mix
)

// vsDecision is the gate result. stored holds the corpus's sole known vsid for
// both accepted and mismatched corpora.
type vsDecision struct {
	register bool
	kind     vsKind
	stored   string
}

// vsGateDecision implements spec §6.1. known is the set of distinct non-empty
// vector-space IDs stored in the corpus (VectorSpaceProbe.KnownIDs); hasUnknown
// is true when legacy empty-vsid rows remain; expected is the set of
// provider-qualified vsids the current embedding chain could produce.
func vsGateDecision(known []string, hasUnknown bool, expected []string) vsDecision {
	switch {
	case len(known) == 0 && hasUnknown:
		return vsDecision{register: true, kind: vsLegacy}
	case len(known) == 0:
		return vsDecision{register: true, kind: vsOK}
	case len(known) == 1 && !hasUnknown:
		if containsString(expected, known[0]) {
			return vsDecision{register: true, kind: vsOK, stored: known[0]}
		}
		return vsDecision{register: false, kind: vsMismatch, stored: known[0]}
	default:
		return vsDecision{register: false, kind: vsInconsistent}
	}
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}
