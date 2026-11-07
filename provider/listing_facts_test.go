package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// positive tokens as the catalog/runtime vocabulary writes them, in two cases.
var listingTokens = []string{
	"chat", "generate", "stream", "embed", "tool_call", "thinking", "insert",
	"completion", "tools", "embedding",
}

func TestOllamaListingFacts_AllAliasesMatchExistingPositiveMapping(t *testing.T) {
	for _, tok := range listingTokens {
		for _, spelled := range []string{tok, strings.ToUpper(tok)} {
			caps, _ := ListingFactMasks(OllamaListingFacts(ModelInfo{Capabilities: []string{spelled}}))
			if want := parseCaps([]string{spelled}); caps != want {
				t.Errorf("token %q: listing facts say %v, parseCaps says %v", spelled, caps, want)
			}
		}
	}
	// completion includes generate; pinned literally, not derived.
	caps, _ := ListingFactMasks(OllamaListingFacts(ModelInfo{Capabilities: []string{"completion"}}))
	if caps != CapChat|CapGenerate|CapStream {
		t.Errorf("completion = %v", caps)
	}
}

func TestOllamaListingFacts_TemplateAndMissingArray(t *testing.T) {
	facts := OllamaListingFacts(ModelInfo{Template: "{{ .Suffix }}"})
	if len(facts) != 1 || facts["insert"] != fingerprint.CapProbeYes {
		t.Fatalf("nil array with a suffix template = %v, want only insert yes", facts)
	}
	if facts := OllamaListingFacts(ModelInfo{}); len(facts) != 0 {
		t.Fatalf("nothing reported = %v, want no facts", facts)
	}
	facts = OllamaListingFacts(ModelInfo{Capabilities: []string{"completion"}})
	if _, ok := facts["embed"]; ok {
		t.Fatalf("invented an embed negative: %v", facts)
	}
}

func TestTemplateUsesSuffix_SharedImplementation(t *testing.T) {
	for _, tmpl := range []string{"", "x", "{{ .Suffix }}", ".suffix", "{{ .Prompt }}"} {
		if templateUsesSuffix(tmpl) != fingerprint.TemplateUsesSuffix(tmpl) {
			t.Errorf("drift on %q", tmpl)
		}
	}
	if !templateUsesSuffix("a .Suffix b") || templateUsesSuffix(".suffix") {
		t.Error("the .Suffix rule changed")
	}
}

func TestOllamaShowRecipe_CapabilitiesAreTheCanonicalNames(t *testing.T) {
	recipe, ok := fingerprint.RecipeFor(fingerprint.MethodOllamaShow)
	if !ok {
		t.Fatal("no ollama-show recipe")
	}
	got, want := slices.Clone(recipe.Capabilities), slices.Clone(CanonicalCapabilityNames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("recipe capabilities %v != canonical names %v", got, want)
	}
}
