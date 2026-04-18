package completion

import (
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestProviderConfigFromProfile(t *testing.T) {
	tests := []struct {
		name      string
		profile   *provider.ModelProfile
		wantErr   bool
		wantCtx   int
		wantTier  provider.Tier
		wantStops int
	}{
		{
			name: "FIM-enabled model",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					Prefix:          "<|fim_prefix|>",
					Suffix:          "<|fim_suffix|>",
					Middle:          "<|fim_middle|>",
					StopTokens:      []string{"<|endoftext|>", "<|fim_pad|>"},
					PrefixBudgetPct: 75,
				},
				ContextWindow: 32768,
				Quality:       provider.TierGreat,
			},
			wantErr:   false,
			wantCtx:   32768,
			wantTier:  provider.TierGreat,
			wantStops: 2,
		},
		{
			name: "non-FIM model rejected",
			profile: &provider.ModelProfile{
				FIM:           nil,
				ContextWindow: 8192,
				Quality:       provider.TierGood,
			},
			wantErr: true,
		},
		{
			name: "FIM with invalid config rejected",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					Prefix: "",
					Suffix: "<|fim_suffix|>",
					Middle: "<|fim_middle|>",
				},
				ContextWindow: 8192,
				Quality:       provider.TierGood,
			},
			wantErr: true,
		},
		{
			name: "uses EffectiveContextWindow for fim",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					Prefix: "<|fim_prefix|>",
					Suffix: "<|fim_suffix|>",
					Middle: "<|fim_middle|>",
				},
				ContextWindow:     131072,
				QualityCtxCeiling: 32768,
				Quality:           provider.TierBest,
			},
			wantErr:  false,
			wantCtx:  131072, // "fim" is NOT quality-sensitive, returns full ContextWindow
			wantTier: provider.TierBest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ProviderConfigFromProfile(tt.profile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ProviderConfigFromProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if cfg.ContextWindow != tt.wantCtx {
				t.Errorf("ContextWindow = %d, want %d", cfg.ContextWindow, tt.wantCtx)
			}
			if cfg.QualityTier != tt.wantTier {
				t.Errorf("QualityTier = %v, want %v", cfg.QualityTier, tt.wantTier)
			}
			if cfg.FIM == nil {
				t.Fatal("FIM is nil")
			}
			if len(cfg.FIM.StopTokens) != tt.wantStops {
				t.Errorf("StopTokens len = %d, want %d", len(cfg.FIM.StopTokens), tt.wantStops)
			}
		})
	}
}
