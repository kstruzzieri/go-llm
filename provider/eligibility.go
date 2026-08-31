package provider

import "sort"

// CapVerdict is the tri-state answer to "do these capabilities satisfy this
// requirement?". Unknown is honest absence of knowledge and must never be
// collapsed into ineligible. String values are the configview v1 wire enum.
type CapVerdict string

const (
	CapEligible   CapVerdict = "eligible"
	CapIneligible CapVerdict = "ineligible"
	CapUnknown    CapVerdict = "unknown"
)

// maxReasons bounds any reason list crossing a projection boundary.
const maxReasons = 16

// CanonicalCaps returns the OR of every canonical capability bit. Derived
// from the same table String() and Names() render from, so the three can
// never drift apart.
func CanonicalCaps() Capability {
	var all Capability
	for _, cn := range capNames {
		all |= cn.bit
	}
	return all
}

// EvaluateCaps evaluates required bits against (caps, knownMask). A set caps
// bit counts as known regardless of the mask. Any definitively missing bit →
// ineligible; else any unknown bit — including required bits OUTSIDE the
// canonical set, which name-based iteration cannot see — → unknown; else
// eligible. req == 0 → unknown with reason no_requirements. Reasons are
// sorted, deduplicated, globally capped at maxReasons bounded codes:
// missing_capability:<name>, capability_unknown:<name>,
// unknown_requirement_bits, no_requirements.
func EvaluateCaps(req, caps, known Capability) (CapVerdict, []string) {
	if req == 0 {
		return CapUnknown, []string{"no_requirements"}
	}
	known |= caps
	reasons := map[string]bool{}
	verdict := CapEligible
	for _, cn := range capNames {
		if req&cn.bit == 0 {
			continue
		}
		switch {
		case caps.Has(cn.bit):
		case known.Has(cn.bit):
			reasons["missing_capability:"+cn.name] = true
			verdict = CapIneligible
		default:
			reasons["capability_unknown:"+cn.name] = true
			if verdict != CapIneligible {
				verdict = CapUnknown
			}
		}
	}
	if req&^CanonicalCaps() != 0 {
		reasons["unknown_requirement_bits"] = true
		if verdict != CapIneligible {
			verdict = CapUnknown
		}
	}
	if verdict == CapEligible {
		return CapEligible, nil
	}
	out := make([]string, 0, len(reasons))
	for r := range reasons {
		out = append(out, r)
	}
	sort.Strings(out)
	if len(out) > maxReasons {
		out = out[:maxReasons]
	}
	return verdict, out
}
