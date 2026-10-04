package interceptor_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestSearchDescriptionNamesCredentialSet (#627): the search description
// spells the skipped credential set out for the model, so a name added to the
// read rule must appear there too or the description goes stale silently.
func TestSearchDescriptionNamesCredentialSet(t *testing.T) {
	desc := (*tools.Search)(nil).Spec().Description
	words := descriptionWords(desc)
	for _, name := range interceptor.CredentialRuleNames() {
		if !slices.Contains(words, name) {
			t.Errorf("search description does not name %q: %s", name, desc)
		}
	}
}

// descriptionWords splits a description into whole names, so ".env" must
// appear as itself, not only as the prefix of ".env.example".
func descriptionWords(desc string) []string {
	return strings.FieldsFunc(desc, func(r rune) bool { return strings.ContainsRune(" ,;()", r) })
}

type nopCaller struct{}

func (nopCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{}, nil
}

// TestDispatchDescriptionNamesProtectedScopes (#627): where scoped dispatch
// exists, its description names every protected directory a scope may not
// sit in, so the refusal set and the model-facing text cannot drift apart.
func TestDispatchDescriptionNamesProtectedScopes(t *testing.T) {
	readers, err := tools.NewFileTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, err := tools.NewDispatch(nopCaller{}, agent.ContextManager{}, readers, tools.DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	desc := d.Spec().Description
	if !strings.Contains(desc, "scope") {
		t.Skip("scoped dispatch is not supported on this platform")
	}
	words := descriptionWords(desc)
	for _, name := range append([]string{".git"}, interceptor.CredentialRuleNames()...) {
		if interceptor.IsProtectedPath(name) && !slices.Contains(words, name) {
			t.Errorf("dispatch description does not name protected %q: %s", name, desc)
		}
	}
}
