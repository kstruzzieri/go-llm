package provider

import (
	"reflect"
	"testing"
)

func TestEvaluateCapsTriState(t *testing.T) {
	req := CapChat | CapToolCall
	tests := []struct {
		name        string
		caps, known Capability
		req         Capability
		want        CapVerdict
		wantReasons []string
	}{
		{"all known present", req, req, req, CapEligible, nil},
		{"known missing", CapChat, req, req, CapIneligible, []string{"missing_capability:tool_call"}},
		{"unknown bit", CapChat, CapChat, req, CapUnknown, []string{"capability_unknown:tool_call"}},
		{"no requirements", CapChat, CapChat, 0, CapUnknown, []string{"no_requirements"}},
		{"set bit is knowledge", req, 0, req, CapEligible, nil},
		{"missing beats unknown", 0, CapChat, req, CapIneligible,
			[]string{"capability_unknown:tool_call", "missing_capability:chat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reasons := EvaluateCaps(tt.req, tt.caps, tt.known)
			if got != tt.want || !reflect.DeepEqual(reasons, tt.wantReasons) {
				t.Fatalf("= %v %v, want %v %v", got, reasons, tt.want, tt.wantReasons)
			}
		})
	}
}

// A required bit OUTSIDE the canonical set must yield unknown, never a
// silent drop (name-based iteration cannot see it).
func TestEvaluateCapsNonCanonicalRequirementIsUnknown(t *testing.T) {
	v, reasons := EvaluateCaps(CapChat|Capability(1<<31), CapChat, CapChat)
	if v != CapUnknown {
		t.Fatalf("verdict = %v, want unknown", v)
	}
	found := false
	for _, r := range reasons {
		if r == "unknown_requirement_bits" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want unknown_requirement_bits", reasons)
	}
}

// CanonicalCaps is the OR of every canonical bit — single source shared by
// config's carve-down gate and configview's known-all mask.
func TestCanonicalCaps(t *testing.T) {
	want := CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall | CapThinking
	if CanonicalCaps() != want {
		t.Fatalf("CanonicalCaps() = %v, want %v", CanonicalCaps(), want)
	}
}

// Reason lists are globally capped and deduplicated, each item bounded.
func TestEvaluateCapsReasonsBoundedDeduped(t *testing.T) {
	all := CanonicalCaps()
	_, reasons := EvaluateCaps(all, 0, 0) // every bit unknown
	if len(reasons) > maxReasons {
		t.Fatalf("%d reasons, cap is %d", len(reasons), maxReasons)
	}
	seen := map[string]bool{}
	for _, r := range reasons {
		if seen[r] {
			t.Fatalf("duplicate reason %q", r)
		}
		seen[r] = true
		if len(r) > 64 {
			t.Fatalf("reason exceeds 64 bytes: %q", r[:64])
		}
	}
}

// Every canonical token resolves through the shared mapping — swapped-bit
// drift is impossible by construction; this pins the wiring.
func TestEvaluateCapsCanonicalCoverage(t *testing.T) {
	for _, name := range CanonicalCapabilityNames {
		bit, err := ParseCapsStrict([]string{name})
		if err != nil {
			t.Fatalf("ParseCapsStrict(%q): %v", name, err)
		}
		v, reasons := EvaluateCaps(bit, 0, bit)
		if v != CapIneligible || len(reasons) != 1 || reasons[0] != "missing_capability:"+name {
			t.Fatalf("%q: = %v %v", name, v, reasons)
		}
	}
}
