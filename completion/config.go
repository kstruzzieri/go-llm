package completion

import (
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

// ProviderConfig supplies model-specific parameters for FIM completion.
type ProviderConfig struct {
	FIM           *provider.FIMConfig
	ContextWindow int
	QualityTier   provider.Tier
}

// ProviderConfigFromProfile converts a resolved model profile into a validated
// completion.ProviderConfig. Returns an error when the model does not support FIM.
func ProviderConfigFromProfile(profile *provider.ModelProfile) (ProviderConfig, error) {
	if profile.FIM == nil {
		return ProviderConfig{}, fmt.Errorf("completion: model %q does not support FIM", profile.Name)
	}
	if err := profile.FIM.Validate(); err != nil {
		return ProviderConfig{}, fmt.Errorf("completion: invalid FIM config: %w", err)
	}
	return ProviderConfig{
		FIM:           profile.FIM,
		ContextWindow: profile.EffectiveContextWindow("fim"),
		QualityTier:   profile.Quality,
	}, nil
}
