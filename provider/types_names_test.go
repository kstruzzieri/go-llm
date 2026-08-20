package provider

import (
	"strings"
	"testing"
)

// Names must never drift from String(): one rendering, two shapes.
func TestCapabilityNamesMatchesString(t *testing.T) {
	for _, c := range []Capability{
		0,
		CapChat,
		CapChat | CapStream | CapToolCall,
		CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall | CapThinking,
	} {
		names := c.Names()
		if c == 0 {
			if names != nil {
				t.Fatalf("Names(0) = %v, want nil", names)
			}
			continue
		}
		if got, want := strings.Join(names, "|"), c.String(); got != want {
			t.Fatalf("Names joined = %q, String = %q", got, want)
		}
	}
}
