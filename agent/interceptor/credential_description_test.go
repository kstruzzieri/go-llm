package interceptor_test

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/agent/tools"
)

// TestSearchDescriptionNamesCredentialSet (#627): the search description
// spells the skipped credential set out for the model, so a name added to the
// read rule must appear there too or the description goes stale silently.
func TestSearchDescriptionNamesCredentialSet(t *testing.T) {
	desc := (*tools.Search)(nil).Spec().Description
	for _, name := range interceptor.CredentialRuleNames() {
		if !strings.Contains(desc, name) {
			t.Errorf("search description does not name %q: %s", name, desc)
		}
	}
}
