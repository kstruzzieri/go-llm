package configview

import (
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestEligibilityTriState(t *testing.T) {
	req := provider.CapChat | provider.CapToolCall
	tests := []struct {
		name        string
		caps, known provider.Capability
		req         provider.Capability
		want        Eligibility
		wantReasons []string
	}{
		{"all known present", req, req, req, EligibilityEligible, nil},
		{"known missing tool_call", provider.CapChat, req, req,
			EligibilityIneligible, []string{"missing_capability:tool_call"}},
		{"tool_call unknown", provider.CapChat, provider.CapChat, req,
			EligibilityUnknown, []string{"capability_unknown:tool_call"}},
		{"no requirements", provider.CapChat, provider.CapChat, 0,
			EligibilityUnknown, []string{"no_requirements"}},
		{"set bit is knowledge even if mask omits it", req, 0, req,
			EligibilityEligible, nil},
		{"missing beats unknown", 0, provider.CapChat, req,
			EligibilityIneligible, []string{"capability_unknown:tool_call", "missing_capability:chat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reasons := eligibilityFor(tt.req, tt.caps, tt.known)
			if got != tt.want || !reflect.DeepEqual(reasons, tt.wantReasons) {
				t.Fatalf("= %v %v, want %v %v", got, reasons, tt.want, tt.wantReasons)
			}
		})
	}
}

// capBitByName must cover every canonical capability token — drift tripwire.
func TestCapBitByNameCoversCanonical(t *testing.T) {
	for _, name := range provider.CanonicalCapabilityNames {
		if _, ok := capBitByName[name]; !ok {
			t.Fatalf("capBitByName missing canonical token %q", name)
		}
	}
	if len(capBitByName) != len(provider.CanonicalCapabilityNames) {
		t.Fatalf("capBitByName has %d entries, canonical set has %d",
			len(capBitByName), len(provider.CanonicalCapabilityNames))
	}
}
