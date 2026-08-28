// Package provider_test holds the cross-package pins. It lives in the
// external test package because config imports provider: an in-package test
// importing config would cycle, while an external test package that nothing
// imports does not.
package provider_test

import (
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// provider.NewDestination duplicates config.ValidateProviderName's rule
// because it cannot import config. This pins the relationship the duplication
// relies on: every name provider accepts must also be one config accepts.
//
// The converse is deliberately NOT required. provider is allowed to be
// stricter (it additionally rejects control characters, which would let a
// config forge lines in the admission manifest), and stricter can only refuse
// to build a destination -- never admit one config would have rejected.
//
// If config's rule is ever relaxed or tightened, this test is what notices.
func TestDestinationProviderNameIsSubsetOfConfigRule(t *testing.T) {
	const validURL = "https://api.example.com/v1"

	names := []string{
		"ollama", "opencode", "llamacpp", "a", "with-dash", "with_underscore",
		"with.dot", "MiXedCase", "digits123", "unicodé",
		"", "has/slash", "a\nb", "a\rb", "a\tb", "a\x00b", "a b", "  padded  ",
		"\x1b[31mred", "a\x7fb",
	}

	for _, name := range names {
		t.Run(nameLabel(name), func(t *testing.T) {
			configErr := config.ValidateProviderName(name)
			_, destErr := provider.NewDestination(name, validURL)

			if destErr == nil && configErr != nil {
				t.Errorf("provider accepts %q but config rejects it (%v): "+
					"the duplicated rule is now MORE permissive than config, "+
					"which can admit a destination config would have refused",
					name, configErr)
			}
			if destErr != nil && !errors.Is(destErr, provider.ErrDestinationInvalid) {
				t.Errorf("rejection of %q must match ErrDestinationInvalid, got %v",
					name, destErr)
			}
		})
	}
}

// The control-character rejection is the one intended asymmetry. Pinning it
// explicitly means a future edit that "simplifies" the duplicate back down to
// config's rule fails here instead of silently reopening manifest forgery.
func TestDestinationRejectsControlCharsConfigAllows(t *testing.T) {
	const validURL = "https://api.example.com/v1"

	for _, name := range []string{
		"a\nopencode", "a\rb", "a\tb", "a\x00b", "a\x7fb",
		"a\u0085b", "a\u2028b", "a\u2029b",
	} {
		t.Run(nameLabel(name), func(t *testing.T) {
			if err := config.ValidateProviderName(name); err != nil {
				t.Skipf("config already rejects %q (%v); asymmetry no longer applies", name, err)
			}
			if _, err := provider.NewDestination(name, validURL); err == nil {
				t.Errorf("NewDestination accepted %q; a provider name carrying a "+
					"control character can forge lines in the admission manifest", name)
			}
		})
	}
}

func nameLabel(s string) string {
	if s == "" {
		return "empty"
	}
	return s
}
