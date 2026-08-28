package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

type inputCeilingTestRegistry struct {
	profiles     map[provider.ModelKey]*provider.ModelProfile
	lookupErrs   map[provider.ModelKey]error
	recommended  []*provider.ModelProfile
	recommendErr error
	explanations map[provider.ModelKey]provider.ToolCallExplanation
	explainErrs  map[provider.ModelKey]error
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
	if err := r.explainErrs[key]; err != nil {
		return provider.ToolCallExplanation{}, err
	}
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
			// A window at or below the implicit reserve gives the router a
			// zero budget: the model can never be admitted, so it must not
			// set the chain minimum. Alone in the chain, derivation falls
			// back to the safe default.
			name:       "router-inadmissible window alone falls back",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("micro"): profile("micro", 2_048)}},
			chain:      []string{"test/micro"},
			want:       8_192,
			wantSource: inputCeilingSafeFallback,
		},
		{
			name:       "router-inadmissible window does not pin chain minimum",
			reg:        inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("micro"): profile("micro", 2_048), key("small"): profile("small", 32_768)}},
			chain:      []string{"test/micro", "test/small"},
			want:       30_720,
			wantSource: inputCeilingChainMinimum,
		},
		{
			// Inadmissibility is judged against the explicit reserve when one
			// is set: window <= reserve means a zero router budget.
			name:          "window consumed by explicit reserve falls back",
			reg:           inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{key("tiny"): profile("tiny", 4_096)}},
			chain:         []string{"test/tiny"},
			outputReserve: 4_096,
			want:          8_192,
			wantSource:    inputCeilingSafeFallback,
		},
		{
			// A stale probe verdict (Valid=false) must not exclude the model:
			// eligibility fails open and the smaller window still counts.
			name: "stale negative probe keeps model eligible",
			reg: inputCeilingTestRegistry{
				profiles: map[provider.ModelKey]*provider.ModelProfile{
					key("large"):    profile("large", 32_768),
					key("probe-no"): {Key: key("probe-no"), ContextWindow: 16_384, Caps: provider.CapChat | provider.CapStream},
				},
				explanations: map[provider.ModelKey]provider.ToolCallExplanation{
					key("probe-no"): {Source: "probe", State: fingerprint.CapProbeNo, Valid: false},
				},
			},
			chain:       []string{"test/large", "test/probe-no"},
			resolveTool: true,
			want:        14_336,
			wantSource:  inputCeilingChainMinimum,
		},
		{
			// A failing explainer must not exclude the model either.
			name: "explain error keeps model eligible",
			reg: inputCeilingTestRegistry{
				profiles: map[provider.ModelKey]*provider.ModelProfile{
					key("large"): profile("large", 32_768),
					key("flaky"): {Key: key("flaky"), ContextWindow: 16_384, Caps: provider.CapChat | provider.CapStream},
				},
				explainErrs: map[provider.ModelKey]error{key("flaky"): errors.New("explain down")},
			},
			chain:       []string{"test/large", "test/flaky"},
			resolveTool: true,
			want:        14_336,
			wantSource:  inputCeilingChainMinimum,
		},
		{
			// Missing chat/stream prerequisites exclude a model outright.
			name: "missing stream capability excludes model",
			reg: inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
				key("large"):     profile("large", 32_768),
				key("no-stream"): {Key: key("no-stream"), ContextWindow: 4_096, Caps: provider.CapChat | provider.CapToolCall},
			}},
			chain:      []string{"test/large", "test/no-stream"},
			want:       30_720,
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
			// agentUseCase throughout: every expectation in this table is the
			// pre-#476 value, so an unchanged result here is the proof that
			// parameterizing the use case did not move ordinary execution.
			got := resolveInputCeiling(context.Background(), tt.reg, tt.chain, agentUseCase, tt.explicit, tt.outputReserve, tt.resolveTool)
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
	first := resolveInputCeiling(context.Background(), reg, []string{"test/first"}, agentUseCase, 0, 0, false)
	second := resolveInputCeiling(context.Background(), reg, []string{"test/second"}, agentUseCase, 0, 0, false)
	if first.ceiling != 30_720 || second.ceiling != 129_024 {
		t.Fatalf("changed chain ceilings = %d then %d, want 30720 then 129024", first.ceiling, second.ceiling)
	}
}

func TestResolveInputCeiling_HonorsTheCallersUseCase(t *testing.T) {
	// The probe use case is "reasoning", NOT "planning". Planning and agent
	// agree on both functions under test today -- neither is in
	// qualitySensitiveUseCases, and neither has a defaultExpectedOutputs entry
	// -- so a planning-vs-agent comparison could not fail whichever literal
	// the implementation used. "reasoning" differs on both axes, which is what
	// makes a hard-coded "agent" detectable at all.
	//
	//	EffectiveContextWindow: reasoning honors QualityCtxCeiling; agent does not.
	//	DefaultExpectedOutput:  reasoning is 4096; agent falls back to chat's 2048.
	reg := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
		{Provider: "test", Model: "yarn"}: {
			Key:               provider.ModelKey{Provider: "test", Model: "yarn"},
			ContextWindow:     100_000,
			QualityCtxCeiling: 32_768,
			Caps:              toolRouteCaps,
		},
	}}
	chain := []string{"test/yarn"}

	t.Run("zero reserve: both window and implicit reserve follow the use case", func(t *testing.T) {
		// agent: full 100000 window, minus the implicit 2048 chat-default reserve.
		if got := resolveInputCeiling(context.Background(), reg, chain, agentUseCase, 0, 0, false); got.ceiling != 97_952 {
			t.Errorf("agent ceiling = %d, want 97952 (100000 - 2048)", got.ceiling)
		}
		// reasoning: quality ceiling 32768, minus its own 4096 reserve.
		if got := resolveInputCeiling(context.Background(), reg, chain, "reasoning", 0, 0, false); got.ceiling != 28_672 {
			t.Errorf("reasoning ceiling = %d, want 28672 (32768 - 4096)", got.ceiling)
		}
	})

	t.Run("nonzero reserve isolates the window from the reserve", func(t *testing.T) {
		// An explicit reserve is subtracted by turnBudget, not here, so the
		// only remaining use-case input is EffectiveContextWindow.
		if got := resolveInputCeiling(context.Background(), reg, chain, agentUseCase, 0, 8_000, false); got.ceiling != 100_000 {
			t.Errorf("agent ceiling = %d, want the full 100000 window", got.ceiling)
		}
		if got := resolveInputCeiling(context.Background(), reg, chain, "reasoning", 0, 8_000, false); got.ceiling != 32_768 {
			t.Errorf("reasoning ceiling = %d, want the 32768 quality ceiling", got.ceiling)
		}
	})

	t.Run("the unadmittable-model skip threshold follows the use case", func(t *testing.T) {
		// window 3000 sits BETWEEN agent's 2048 reserve and reasoning's 4096:
		// admittable as agent, never admittable as reasoning. The reasoning
		// case must therefore skip this model and fall back rather than let a
		// model that can never be admitted set the chain minimum.
		tight := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
			{Provider: "test", Model: "tiny"}: {
				Key:           provider.ModelKey{Provider: "test", Model: "tiny"},
				ContextWindow: 3_000,
				Caps:          toolRouteCaps,
			},
		}}
		tightChain := []string{"test/tiny"}
		if got := resolveInputCeiling(context.Background(), tight, tightChain, agentUseCase, 0, 0, false); got.ceiling != 952 {
			t.Errorf("agent ceiling = %d, want 952 (3000 - 2048)", got.ceiling)
		}
		got := resolveInputCeiling(context.Background(), tight, tightChain, "reasoning", 0, 0, false)
		if got.ceiling != agent.DefaultInputCeiling || got.source != inputCeilingSafeFallback {
			t.Errorf("reasoning resolution = %+v, want the safe fallback (%d): 3000 <= 4096 is never admittable",
				got, agent.DefaultInputCeiling)
		}
	})
}

func TestInputCeilingResolutionLineReportsValueAndSource(t *testing.T) {
	got := (inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback}).line()
	want := "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable)"
	if got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}
