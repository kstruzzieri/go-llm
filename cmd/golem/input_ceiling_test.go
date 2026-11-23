package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

type inputCeilingTestRegistry struct {
	profiles     map[provider.ModelKey]*provider.ModelProfile
	lookupErrs   map[provider.ModelKey]error
	anyErrs      map[string]error
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
	if err := r.anyErrs[model]; err != nil {
		return nil, err
	}
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
		return &provider.ModelProfile{Key: key(model), ContextWindow: window, Caps: agent.ModelCallCapabilities(true)}
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
				key("yarn"): {Key: key("yarn"), ContextWindow: 32_768, QualityCtxCeiling: 16_384, Caps: agent.ModelCallCapabilities(true)},
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
			// "agent" throughout: every expectation in this table is the
			// pre-#476 value, so an unchanged result here is the proof that
			// parameterizing the use case did not move ordinary execution.
			got := resolveInputCeiling(context.Background(), tt.reg, tt.chain, "agent", tt.explicit, tt.outputReserve, tt.resolveTool)
			if got.ceiling != tt.want || got.source != tt.wantSource {
				t.Fatalf("resolveInputCeiling() = %+v, want ceiling=%d source=%q", got, tt.want, tt.wantSource)
			}
		})
	}
}

func TestResolveInputCeilingRecomputesForChangedChain(t *testing.T) {
	reg := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
		{Provider: "test", Model: "first"}:  {ContextWindow: 32_768, Caps: agent.ModelCallCapabilities(true)},
		{Provider: "test", Model: "second"}: {ContextWindow: 131_072, Caps: agent.ModelCallCapabilities(true)},
	}}
	first := resolveInputCeiling(context.Background(), reg, []string{"test/first"}, "agent", 0, 0, false)
	second := resolveInputCeiling(context.Background(), reg, []string{"test/second"}, "agent", 0, 0, false)
	if first.ceiling != 30_720 || second.ceiling != 129_024 {
		t.Fatalf("changed chain ceilings = %d then %d, want 30720 then 129024", first.ceiling, second.ceiling)
	}
}

func TestResolveInputCeiling_HonorsTheCallersUseCase(t *testing.T) {
	// The probe use case is "reasoning", NOT "planning". Since #479 planning
	// honors QualityCtxCeiling too, but planning and agent still share chat's
	// 2048 implicit reserve (neither has a defaultExpectedOutputs entry), so
	// only "reasoning" differs from "agent" on BOTH axes -- which is what makes
	// a hard-coded "agent" detectable through either function.
	//
	//	EffectiveContextWindow: reasoning honors QualityCtxCeiling; agent does not.
	//	DefaultExpectedOutput:  reasoning is 4096; agent falls back to chat's 2048.
	reg := inputCeilingTestRegistry{profiles: map[provider.ModelKey]*provider.ModelProfile{
		{Provider: "test", Model: "yarn"}: {
			Key:               provider.ModelKey{Provider: "test", Model: "yarn"},
			ContextWindow:     100_000,
			QualityCtxCeiling: 32_768,
			Caps:              agent.ModelCallCapabilities(true),
		},
	}}
	chain := []string{"test/yarn"}

	t.Run("zero reserve: both window and implicit reserve follow the use case", func(t *testing.T) {
		// agent: full 100000 window, minus the implicit 2048 chat-default reserve.
		if got := resolveInputCeiling(context.Background(), reg, chain, "agent", 0, 0, false); got.ceiling != 97_952 {
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
		if got := resolveInputCeiling(context.Background(), reg, chain, "agent", 0, 8_000, false); got.ceiling != 100_000 {
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
				Caps:          agent.ModelCallCapabilities(true),
			},
		}}
		tightChain := []string{"test/tiny"}
		if got := resolveInputCeiling(context.Background(), tight, tightChain, "agent", 0, 0, false); got.ceiling != 952 {
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

// TestInputCeiling_ProviderNoIsIneligible pins that a model whose provider said
// no to tool_call, stored or live, does not set the chain minimum; that a
// provider yes beside a valid legacy no stays eligible; and that a valid
// inconclusive probe stays excluded during its cooldown, as before.
func TestInputCeiling_ProviderNoIsIneligible(t *testing.T) {
	key := func(model string) provider.ModelKey { return provider.ModelKey{Provider: "test", Model: model} }
	profiles := map[provider.ModelKey]*provider.ModelProfile{
		key("small"): {Key: key("small"), ContextWindow: 8_192, Caps: agent.ModelCallCapabilities(false)},
		key("large"): {Key: key("large"), ContextWindow: 65_536, Caps: agent.ModelCallCapabilities(true)},
	}
	const withoutSmall, withSmall = 65_536 - 2_048, 8_192 - 2_048
	tests := []struct {
		name string
		exp  provider.ToolCallExplanation
		want int
	}{
		{"a stored provider no", provider.ToolCallExplanation{Source: "runtime", State: fingerprint.CapProbeNo, Valid: true}, withoutSmall},
		{"a live provider no", provider.ToolCallExplanation{Source: "runtime"}, withoutSmall},
		{"a live provider no beside an expired attempt", provider.ToolCallExplanation{Source: "runtime", State: fingerprint.CapProbeInconclusive}, withoutSmall},
		{"a provider yes beside a valid legacy no", provider.ToolCallExplanation{Has: true, Source: "runtime", State: fingerprint.CapProbeNo, Valid: true}, withSmall},
		{"a valid inconclusive probe in its cooldown", provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeInconclusive, Valid: true}, withoutSmall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := inputCeilingTestRegistry{profiles: profiles, explanations: map[provider.ModelKey]provider.ToolCallExplanation{key("small"): tt.exp}}
			got := resolveInputCeiling(t.Context(), reg, []string{"test/small", "test/large"}, "agent", 0, 0, true)
			if got.ceiling != tt.want {
				t.Fatalf("ceiling = %d; want %d", got.ceiling, tt.want)
			}
		})
	}
}

// #856: a chain entry its provider does not serve can never route a turn, so
// it must not set the budget. An entry whose provider cannot be reached may
// come back mid-session, so it still costs the safe default, and the line
// names every entry that put the safe default in.
func TestResolveInputCeiling_NotFoundEntryDoesNotSetCeiling(t *testing.T) {
	key := func(model string) provider.ModelKey { return provider.ModelKey{Provider: "test", Model: model} }
	notFound := fmt.Errorf("provider: lookup: model not served: %w", provider.ErrModelNotFound)
	reg := inputCeilingTestRegistry{
		profiles: map[provider.ModelKey]*provider.ModelProfile{
			key("big"):     {Key: key("big"), ContextWindow: 256_000, Caps: agent.ModelCallCapabilities(true)},
			key("nowin"):   {Key: key("nowin"), Caps: agent.ModelCallCapabilities(true)},
			key("nilprof"): nil, // a capChecker may answer with no profile and no error
			key("small"):   {Key: key("small"), ContextWindow: 4_096, Caps: agent.ModelCallCapabilities(true)},
			key("tie"):     {Key: key("tie"), ContextWindow: 8_192 + 2_048, Caps: agent.ModelCallCapabilities(true)},
		},
		lookupErrs: map[provider.ModelKey]error{
			key("gone"):  notFound,
			key("gone2"): notFound,
			key("down"):  errors.New(`provider: lookup test/down: query models from "test": connection refused`),
		},
		anyErrs: map[string]error{"gone-any": notFound, "down-any": errors.New("provider: no providers found")},
	}
	const bigLessReserve = 256_000 - 2_048
	tests := []struct {
		name     string
		chain    []string
		want     inputCeilingResolution
		wantLine string
	}{
		{
			name:     "qualified not-found fallback is skipped",
			chain:    []string{"test/big", "test/gone"},
			want:     inputCeilingResolution{ceiling: bigLessReserve, source: inputCeilingChainMinimum},
			wantLine: "input ceiling: 253952 tokens (chain minimum)",
		},
		{
			name:     "unqualified not-found fallback is skipped",
			chain:    []string{"test/big", "gone-any"},
			want:     inputCeilingResolution{ceiling: bigLessReserve, source: inputCeilingChainMinimum},
			wantLine: "input ceiling: 253952 tokens (chain minimum)",
		},
		{
			name:     "unreachable fallback keeps the safe default and is named",
			chain:    []string{"test/big", "test/gone", "test/down"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback, fallbackFrom: "test/down"},
			wantLine: "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable for test/down)",
		},
		{
			name:     "every safe-default entry is named in chain order",
			chain:    []string{"down-any", "test/big", "nowin"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback, fallbackFrom: "down-any, test/nowin"},
			wantLine: "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable for down-any, test/nowin)",
		},
		{
			name:     "an entry resolving to no profile keeps the safe default and is named",
			chain:    []string{"test/big", "nilprof"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback, fallbackFrom: "nilprof"},
			wantLine: "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable for nilprof)",
		},
		{
			name:     "a model reached by two selectors is named once",
			chain:    []string{"test/nowin", "nowin"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback, fallbackFrom: "test/nowin"},
			wantLine: "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable for test/nowin)",
		},
		{
			// The safe default only names entries when it is what binds:
			// fixing the unreachable provider would not raise this budget.
			name:     "a smaller known window wins over the safe default, in any order",
			chain:    []string{"test/down", "test/small"},
			want:     inputCeilingResolution{ceiling: 2_048, source: inputCeilingChainMinimum},
			wantLine: "input ceiling: 2048 tokens (chain minimum)",
		},
		{
			name:     "a known window equal to the safe default is the chain minimum",
			chain:    []string{"test/tie", "test/down"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingChainMinimum},
			wantLine: "input ceiling: 8192 tokens (chain minimum)",
		},
		{
			name:     "no entry resolving keeps the safe default",
			chain:    []string{"test/gone", "test/gone2"},
			want:     inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback},
			wantLine: "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable)",
		},
	}
	t.Run("recommend mode names no entry", func(t *testing.T) {
		// No chain was configured, so there is no entry to fix, and the
		// recommendation can hold any number of models without a window.
		rec := inputCeilingTestRegistry{recommended: []*provider.ModelProfile{reg.profiles[key("big")], reg.profiles[key("nowin")]}}
		got := resolveInputCeiling(context.Background(), rec, nil, "agent", 0, 0, false)
		if want := (inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback}); got != want {
			t.Errorf("resolveInputCeiling() = %+v, want %+v", got, want)
		}
	})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveInputCeiling(context.Background(), reg, tt.chain, "agent", 0, 0, false)
			if got != tt.want {
				t.Errorf("resolveInputCeiling() = %+v, want %+v", got, tt.want)
			}
			if line := got.line(); line != tt.wantLine {
				t.Errorf("line = %q, want %q", line, tt.wantLine)
			}
		})
	}
}

// #856 through the real provider and model registries: the provider is up and
// lists "big" but not "gone", so the registry's own not-found error must reach
// the resolver as not found.
func TestResolveInputCeiling_RealRegistrySkipsModelTheProviderDoesNotList(t *testing.T) {
	_, models := newListedRegistry(t, provider.ModelInfo{
		Name: "big", ContextWindow: 256_000, Capabilities: []string{"completion", "tools"},
	})
	got := resolveInputCeiling(context.Background(), models, []string{"yarn/big", "yarn/gone"}, "agent", 0, 0, false)
	want := inputCeilingResolution{ceiling: 256_000 - 2_048, source: inputCeilingChainMinimum}
	if got != want {
		t.Fatalf("resolveInputCeiling() = %+v, want %+v", got, want)
	}
}

// #856: entry names come from models.json, which Golem may load from the
// working directory, so the line renders them as one inert line.
func TestInputCeilingLineRendersEntryNamesInert(t *testing.T) {
	got := inputCeilingResolution{ceiling: 8_192, source: inputCeilingSafeFallback, fallbackFrom: "evil/\x1b[2Jm\nforged: line"}.line()
	if strings.ContainsAny(got, "\x1b\n") {
		t.Fatalf("line = %q, want no raw escape or newline", got)
	}
	if !strings.Contains(got, `\x1b[2J`) {
		t.Errorf("line = %q, want the escape shown as text", got)
	}
}
