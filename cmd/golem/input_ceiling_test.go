package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

type inputCeilingTestRegistry struct {
	profiles     map[provider.ModelKey]*provider.ModelProfile
	lookupErrs   map[provider.ModelKey]error
	recommended  []*provider.ModelProfile
	recommendErr error
	explanations map[provider.ModelKey]provider.ToolCallExplanation
}

func (r inputCeilingTestRegistry) Lookup(_ context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	if err := r.lookupErrs[key]; err != nil {
		return nil, err
	}
	if profile := r.profiles[key]; profile != nil {
		return profile, nil
	}
	return nil, errors.New("model not found")
}

func (r inputCeilingTestRegistry) LookupAny(_ context.Context, model string) ([]*provider.ModelProfile, error) {
	var profiles []*provider.ModelProfile
	for key, profile := range r.profiles {
		if key.Model == model {
			profiles = append(profiles, profile)
		}
	}
	if len(profiles) == 0 {
		return nil, errors.New("model not found")
	}
	return profiles, nil
}

func (r inputCeilingTestRegistry) Recommend(_ context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	if r.recommendErr != nil {
		return nil, r.recommendErr
	}
	var profiles []*provider.ModelProfile
	for _, profile := range r.recommended {
		if profile != nil && profile.Caps.Has(opts.RequiredCaps) {
			profiles = append(profiles, profile)
		}
	}
	return profiles, nil
}

func (r inputCeilingTestRegistry) ExplainToolCall(_ context.Context, key provider.ModelKey) (provider.ToolCallExplanation, error) {
	return r.explanations[key], nil
}

func TestResolveInputCeiling(t *testing.T) {
	key := func(model string) provider.ModelKey { return provider.ModelKey{Provider: "test", Model: model} }
	profile := func(model string, window int) *provider.ModelProfile {
		return &provider.ModelProfile{Key: key(model), ContextWindow: window, Caps: toolRouteCaps}
	}

	tests := []struct {
		name          string
		reg           capChecker
		chain         []string
		explicit      int
		outputReserve int
		resolveTool   bool
		want          int
		wantSource    inputCeilingSource
	}{
		{
			name:       "explicit override wins without metadata",
			explicit:   12_345,
			want:       12_345,
			wantSource: inputCeilingExplicit,
		},
		{
			// 32_768 minus the router's implicit 2_048 "agent" output reserve:
			// with -output-reserve 0 the router still budgets that reserve, so
			// the derived ceiling must leave room for it.
			name:       "known chain uses smallest window minus implicit output reserve",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("large"): profile("large", 65_536), key("small"): profile("small", 32_768)}},
			chain:      []string{"test/large", "test/small"},
			want:       30_720,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name:          "nonzero output reserve keeps full window",
			reg:           inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("small"): profile("small", 32_768)}},
			chain:         []string{"test/small"},
			outputReserve: 512,
			want:          32_768,
			wantSource:    inputCeilingChainMinimum,
		},
		{
			// "agent" is not a quality-sensitive use case, so the router
			// validates against the full ContextWindow, not QualityCtxCeiling;
			// the derivation must match or the two budgets diverge again.
			name: "quality ceiling does not shrink agent window",
			reg: inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
				key("yarn"): {Key: key("yarn"), ContextWindow: 32_768, QualityCtxCeiling: 16_384, Caps: toolRouteCaps},
			}},
			chain:      []string{"test/yarn"},
			want:       30_720,
			wantSource: inputCeilingChainMinimum,
		},
		{
			// Degenerate window at or below the implicit reserve: keep the raw
			// window rather than deriving a zero/negative ceiling.
			name:       "window at implicit reserve stays raw",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("micro"): profile("micro", 2_048)}},
			chain:      []string{"test/micro"},
			want:       2_048,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name:       "unknown fallback contributes safe default",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("large"): profile("large", 32_768), key("unknown"): profile("unknown", 0)}},
			chain:      []string{"test/large", "test/unknown"},
			want:       8_192,
			wantSource: inputCeilingSafeFallback,
		},
		{
			name: "failed fallback lookup contributes safe default",
			reg: inputCeilingTestRegistry{
				profiles:   map[provider.ModelKey]*provider.ModelProfile{key("large"): profile("large", 32_768)},
				lookupErrs: map[provider.ModelKey]error{key("down"): errors.New("provider down")},
			},
			chain:      []string{"test/large", "test/down"},
			want:       8_192,
			wantSource: inputCeilingSafeFallback,
		},
		{
			name:       "known model may be smaller than safe default",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("tiny"): profile("tiny", 4_096)}},
			chain:      []string{"test/tiny"},
			want:       2_048,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name: "recommend mode uses smallest eligible profile",
			reg: inputCeilingTestRegistry{recommended: []*provider.ModelProfile{
				profile("large", 65_536), profile("small", 16_384),
			}},
			want:       14_336,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name: "recommend mode includes probeable candidate",
			reg: inputCeilingTestRegistry{recommended: []*provider.ModelProfile{
				profile("large", 65_536),
				{Key: key("probeable"), ContextWindow: 16_384, Caps: provider.CapChat | provider.CapStream},
			}},
			resolveTool: true,
			want:        14_336,
			wantSource:  inputCeilingChainMinimum,
		},
		{
			name: "recommend mode without resolution excludes unknown tool capability",
			reg: inputCeilingTestRegistry{recommended: []*provider.ModelProfile{
				profile("large", 65_536),
				{Key: key("unknown-tool"), ContextWindow: 16_384, Caps: provider.CapChat | provider.CapStream},
			}},
			want:       63_488,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name: "explicit non-tool fallback is ineligible",
			reg: inputCeilingTestRegistry{
				profiles: map[provider.ModelKey]*provider.ModelProfile{
					key("large"):    profile("large", 32_768),
					key("non-tool"): {Key: key("non-tool"), ContextWindow: 4_096, Caps: provider.CapChat | provider.CapStream},
				},
				explanations: map[provider.ModelKey]provider.ToolCallExplanation{
					key("non-tool"): {Source: "explicit", Has: false},
				},
			},
			chain:       []string{"test/large", "test/non-tool"},
			resolveTool: true,
			want:        30_720,
			wantSource:  inputCeilingChainMinimum,
		},
		{
			name: "chain without resolution excludes unknown tool capability",
			reg: inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
				key("large"):        profile("large", 32_768),
				key("unknown-tool"): {Key: key("unknown-tool"), ContextWindow: 4_096, Caps: provider.CapChat | provider.CapStream},
			}},
			chain:      []string{"test/large", "test/unknown-tool"},
			want:       30_720,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name: "valid cached negative fallback is ineligible",
			reg: inputCeilingTestRegistry{
				profiles: map[provider.ModelKey]*provider.ModelProfile{
					key("large"):    profile("large", 32_768),
					key("probe-no"): {Key: key("probe-no"), ContextWindow: 4_096, Caps: provider.CapChat | provider.CapStream},
				},
				explanations: map[provider.ModelKey]provider.ToolCallExplanation{
					key("probe-no"): {Source: "probe", State: fingerprint.CapProbeNo, Valid: true},
				},
			},
			chain:       []string{"test/large", "test/probe-no"},
			resolveTool: true,
			want:        30_720,
			wantSource:  inputCeilingChainMinimum,
		},
		{
			name:       "empty recommendation uses safe default",
			reg:        inputCeilingTestRegistry{},
			want:       8_192,
			wantSource: inputCeilingSafeFallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveInputCeiling(context.Background(), tt.reg, tt.chain, tt.explicit, tt.outputReserve, tt.resolveTool)
			if got.ceiling != tt.want || got.source != tt.wantSource {
				t.Fatalf("resolveInputCeiling() = %+v, want ceiling=%d source=%q", got, tt.want, tt.wantSource)
			}
		})
	}
}

func TestResolveInputCeilingRecomputesForChangedChain(t *testing.T) {
	reg := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
		{Provider: "test", Model: "first"}:  {ContextWindow: 32_768, Caps: toolRouteCaps},
		{Provider: "test", Model: "second"}: {ContextWindow: 131_072, Caps: toolRouteCaps},
	}}
	first := resolveInputCeiling(context.Background(), reg, []string{"test/first"}, 0, 0, false)
	second := resolveInputCeiling(context.Background(), reg, []string{"test/second"}, 0, 0, false)
	if first.ceiling != 30_720 || second.ceiling != 129_024 {
		t.Fatalf("changed chain ceilings = %d then %d, want 30720 then 129024", first.ceiling, second.ceiling)
	}
}

func TestInputCeilingResolutionLineReportsValueAndSource(t *testing.T) {
	got := (inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback}).line()
	want := "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable)"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}
