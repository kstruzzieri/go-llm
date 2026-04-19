package completion

import (
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

const defaultFIMContextWindow = 8192

// ProviderConfig supplies model-specific parameters for FIM completion.
type ProviderConfig struct {
	FIM           *provider.FIMConfig
	ContextWindow int
	QualityTier   provider.Tier
}

// ProviderConfigFromProfile converts a resolved model profile into a validated
// completion.ProviderConfig. Returns an error when the model does not support
// native prompt+suffix FIM.
func ProviderConfigFromProfile(profile *provider.ModelProfile) (ProviderConfig, error) {
	if profile == nil {
		return ProviderConfig{}, fmt.Errorf("completion: missing model profile")
	}
	if !profile.SupportsFIM() {
		reason := profile.FIMUnsupportedReason()
		if reason == "" {
			return ProviderConfig{}, fmt.Errorf("completion: model %q does not support native FIM", profile.Name)
		}
		return ProviderConfig{}, fmt.Errorf("completion: model %q does not support native FIM: %s", profile.Name, reason)
	}
	fim := profile.FIM
	if fim == nil {
		fim = &provider.FIMConfig{}
	}
	if err := fim.Validate(); err != nil {
		return ProviderConfig{}, fmt.Errorf("completion: invalid FIM config: %w", err)
	}
	ctxWindow := profile.EffectiveContextWindow("fim")
	if ctxWindow <= 0 {
		ctxWindow = defaultFIMContextWindow
	}
	return ProviderConfig{
		FIM:           fim,
		ContextWindow: ctxWindow,
		QualityTier:   profile.Quality,
	}, nil
}
