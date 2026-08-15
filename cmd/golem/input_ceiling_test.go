package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type inputCeilingTestRegistry struct {
	profiles     map[provider.ModelKey]*provider.ModelProfile
	lookupErrs   map[provider.ModelKey]error
	recommended  []*provider.ModelProfile
	recommendErr error
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

func (r inputCeilingTestRegistry) Recommend(context.Context, provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	return r.recommended, r.recommendErr
}

func TestResolveInputCeiling(t *testing.T) {
	key := func(model string) provider.ModelKey { return provider.ModelKey{Provider: "test", Model: model} }
	profile := func(model string, window int) *provider.ModelProfile {
		return &provider.ModelProfile{Key: key(model), ContextWindow: window}
	}

	tests := []struct {
		name       string
		reg        capChecker
		chain      []string
		explicit   int
		want       int
		wantSource inputCeilingSource
	}{
		{
			name:       "explicit override wins without metadata",
			explicit:   12_345,
			want:       12_345,
			wantSource: inputCeilingExplicit,
		},
		{
			name:       "known chain uses smallest full context window",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("large"): profile("large", 65_536), key("small"): profile("small", 32_768)}},
			chain:      []string{"test/large", "test/small"},
			want:       32_768,
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
			want:       4_096,
			wantSource: inputCeilingChainMinimum,
		},
		{
			name: "recommend mode uses smallest eligible profile",
			reg: inputCeilingTestRegistry{recommended: []*provider.ModelProfile{
				profile("large", 65_536), profile("small", 16_384),
			}},
			want:       16_384,
			wantSource: inputCeilingChainMinimum,
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
			got := resolveInputCeiling(context.Background(), tt.reg, tt.chain, tt.explicit)
			if got.ceiling != tt.want || got.source != tt.wantSource {
				t.Fatalf("resolveInputCeiling() = %+v, want ceiling=%d source=%q", got, tt.want, tt.wantSource)
			}
		})
	}
}

func TestResolveInputCeilingRecomputesForChangedChain(t *testing.T) {
	reg := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
		{Provider: "test", Model: "first"}:  {ContextWindow: 32_768},
		{Provider: "test", Model: "second"}: {ContextWindow: 131_072},
	}}
	first := resolveInputCeiling(context.Background(), reg, []string{"test/first"}, 0)
	second := resolveInputCeiling(context.Background(), reg, []string{"test/second"}, 0)
	if first.ceiling != 32_768 || second.ceiling != 131_072 {
		t.Fatalf("changed chain ceilings = %d then %d, want 32768 then 131072", first.ceiling, second.ceiling)
	}
}

func TestInputCeilingResolutionLineReportsValueAndSource(t *testing.T) {
	got := (inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback}).line()
	want := "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable)"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}
