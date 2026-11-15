package completion

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
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
			name:    "nil profile rejected",
			profile: nil,
			wantErr: true,
		},
		{
			name: "FIM-enabled model",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					StopTokens:      []string{"<|endoftext|>", "<|fim_pad|>"},
					PrefixBudgetPct: 75,
				},
				ContextWindow: 32768,
				Quality:       provider.TierGreat,
				Template:      "{{ .Prompt }}{{ .Suffix }}",
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
					StopTokens: []string{""},
				},
				ContextWindow: 8192,
				Quality:       provider.TierGood,
				Template:      "{{ .Prompt }}{{ .Suffix }}",
			},
			wantErr: true,
		},
		{
			name: "uses EffectiveContextWindow for fim",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					PrefixBudgetPct: 75,
				},
				ContextWindow:     131072,
				QualityCtxCeiling: 32768,
				Quality:           provider.TierBest,
				Template:          "{{ .Prompt }}{{ .Suffix }}",
			},
			wantErr:  false,
			wantCtx:  131072, // "fim" is NOT quality-sensitive, returns full ContextWindow
			wantTier: provider.TierBest,
		},
		{
			name: "falls back to default context window when runtime context missing",
			profile: &provider.ModelProfile{
				FIM: &provider.FIMConfig{
					PrefixBudgetPct: 75,
				},
				Quality:  provider.TierGood,
				Template: "{{ .Prompt }}{{ .Suffix }}",
			},
			wantErr:  false,
			wantCtx:  defaultFIMContextWindow,
			wantTier: provider.TierGood,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ProviderConfigFromProfile(tt.profile)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ProviderConfigFromProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.profile == nil && err.Error() != "completion: missing model profile" {
					t.Fatalf("ProviderConfigFromProfile() error = %q, want missing model profile", err)
				}
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

// evidenceStub is an empty evidence log: every read finds nothing.
type evidenceStub struct{ fingerprint.EvidenceStore }

func (evidenceStub) ReadSnapshot(context.Context, string, string, *string, string, []string, time.Time) (fingerprint.EvidenceSnapshot, error) {
	return fingerprint.EvidenceSnapshot{}, nil
}
func (evidenceStub) Generation(context.Context) (int64, error) { return 0, nil }

type fimProvider struct {
	provider.Provider
	models []provider.ModelInfo
}

func (p fimProvider) Name() string { return "local" }
func (p fimProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return p.models, nil
}

type fimResolver struct{ p provider.Provider }

func (r fimResolver) Resolve(provider.ModelKey) (provider.Provider, error) { return r.p, nil }
func (r fimResolver) ProvidersForModel(string) ([]provider.Provider, error) {
	return []provider.Provider{r.p}, nil
}
func (r fimResolver) All() []provider.Provider { return []provider.Provider{r.p} }

func TestProviderConfigFromProfile_ExplicitWithoutInsertOnLogPath(t *testing.T) {
	b, err := provider.EvidenceBindingFor("http://127.0.0.1:11434", "ollama")
	if err != nil {
		t.Fatal(err)
	}
	info := provider.ModelInfo{Name: "m", Digest: "sha256:d", Template: "{{ .Prompt }}{{ .Suffix }}"}
	mr, err := provider.NewModelRegistry(fimResolver{fimProvider{models: []provider.ModelInfo{info}}}, nil,
		provider.WithEvidenceLog(evidenceStub{}, func(string) (provider.EvidenceBinding, bool) { return b, true }))
	if err != nil {
		t.Fatal(err)
	}
	mr.SetCapabilityOverride(func(provider.ModelKey) []string { return []string{"chat", "stream"} })
	p, err := mr.Lookup(t.Context(), provider.ModelKey{Provider: "local", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProviderConfigFromProfile(p)
	if err == nil || !strings.Contains(err.Error(), "decided capabilities exclude insert") {
		t.Fatalf("error = %v, want one naming the decided capabilities", err)
	}
}
