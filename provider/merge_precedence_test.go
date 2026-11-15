// provider/merge_precedence_test.go
//
// Characterization of the no-log profile merge (#736).
// Every expectation below was captured from the code as it stood before the
// capability-precedence change and is written out literally. A registry
// without an evidence log must keep producing exactly these profiles.
package provider

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

func TestMerge_NoLogMeansTodaysBehavior(t *testing.T) {
	const (
		prov     = "prov"
		mystery  = "mystery-model"
		catalogM = "qwen3:8b"
	)
	thinkAlways := ThinkAlways
	tags := ThinkTags{Open: "<t>", Close: "</t>"}

	// The two starting points, written out literally. An uncatalogued model
	// the runtime says nothing about, and the catalog entry for qwen3:8b.
	mysteryBase := func() ModelProfile {
		return ModelProfile{
			Key: ModelKey{Provider: prov, Model: mystery}, Name: mystery, Provider: prov,
			Family: "mystery-model", ThinkMode: ThinkAuto, Quality: TierBasic, Speed: TierGreat,
			Source: SourceMerged,
		}
	}
	qwenBase := func() ModelProfile {
		return ModelProfile{
			Key: ModelKey{Provider: prov, Model: catalogM}, Name: catalogM, Provider: prov,
			Family: "qwen3", Caps: CapChat | CapGenerate | CapStream | CapToolCall,
			FIM:       &FIMConfig{StopTokens: []string{"<|endoftext|>", "<|fim_pad|>"}, PrefixBudgetPct: 75},
			ThinkMode: ThinkToggle, ThinkTags: &ThinkTags{Open: "<think>", Close: "</think>"},
			Quality: TierGood, Speed: TierGreat, ContextWindow: 40960,
			Resources: ResourceProfile{RAMRequired: 5, RAMRecommended: 8, VRAMRecommended: 5},
			Source:    SourceMerged,
		}
	}
	with := func(base ModelProfile, edit func(*ModelProfile)) ModelProfile {
		edit(&base)
		return base
	}

	type probe struct {
		state fingerprint.CapProbeState
		stale bool // wrong digest, so the row is not valid
	}
	all := CapChat | CapGenerate | CapStream | CapToolCall
	const suffixTmpl = "{{ .Prompt }}{{ .Suffix }}"
	toolsRuntime := ModelInfo{Capabilities: []string{"tools"}}

	tests := []struct {
		name         string
		model        string
		runtime      ModelInfo // Name is filled in
		floor        []string
		override     []string
		think        *ThinkMode
		thinkTag     *ThinkTags
		ctxOverride  int
		ceilOverride int
		fpCaps       []string
		fpMemMB      int64
		fpCtx        int
		fpDigest     string // stored profile digest; default is the runtime digest or key
		factory      string // when set, a prober factory declaring this digest is installed
		probe        *probe
		want         ModelProfile
		wantRejected int // rejection hook firings
	}{
		{name: "unknown model, no sources", model: mystery, want: mysteryBase()},
		{name: "catalog only", model: catalogM, want: qwenBase()},
		{
			name: "floor adds tool_call", model: mystery, floor: []string{"tool_call"},
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "floor beside catalog", model: catalogM, floor: []string{"embed"},
			want: with(qwenBase(), func(p *ModelProfile) { p.Caps |= CapEmbed }),
		},
		{
			name: "floor with alias token is dropped and reported", model: mystery, floor: []string{"tools"},
			want: mysteryBase(), wantRejected: 1,
		},
		{
			name: "stored fingerprint profile, no factory", model: mystery,
			fpCaps: []string{"tools", "completion"}, fpMemMB: 20480, fpCtx: 16384,
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.Caps = all
				p.Resources.RAMRequired = 20
				p.ContextWindow = 16384
			}),
		},
		{
			name: "stored fingerprint profile, no factory, digest ignored", model: mystery,
			fpCaps: []string{"tools"}, fpDigest: "sha256:other",
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "stored fingerprint profile accepted by factory digest", model: mystery,
			fpCaps: []string{"tools"}, fpDigest: "sha256:fp", factory: "sha256:fp",
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "stored fingerprint profile rejected by factory digest", model: mystery,
			fpCaps: []string{"tools"}, fpDigest: "sha256:old", factory: "sha256:fp",
			want: mysteryBase(),
		},
		{
			name: "legacy probe yes valid", model: mystery, probe: &probe{state: fingerprint.CapProbeYes},
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "legacy probe yes stale", model: mystery,
			probe: &probe{state: fingerprint.CapProbeYes, stale: true}, want: mysteryBase(),
		},
		{
			name: "legacy probe no valid", model: mystery,
			probe: &probe{state: fingerprint.CapProbeNo}, want: mysteryBase(),
		},
		{
			name: "catalog yes beside legacy probe no", model: catalogM,
			probe: &probe{state: fingerprint.CapProbeNo}, want: qwenBase(),
		},
		{
			name: "floor tool_call beside legacy probe no", model: mystery,
			floor: []string{"tool_call"}, probe: &probe{state: fingerprint.CapProbeNo},
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "runtime tools beside legacy probe no", model: mystery,
			runtime: toolsRuntime, probe: &probe{state: fingerprint.CapProbeNo},
			want: with(mysteryBase(), func(p *ModelProfile) { p.Caps = CapToolCall }),
		},
		{
			name: "catalog yes beside runtime capabilities without tools", model: catalogM,
			runtime: ModelInfo{Capabilities: []string{"completion"}},
			want:    qwenBase(),
		},
		{
			name:  "runtime capabilities and metadata",
			model: mystery,
			runtime: ModelInfo{
				Family: "Mystery", ParameterSize: "7B", QuantLevel: "Q4_K_M", ContextWindow: 8192,
				Digest: "sha256:abc", Capabilities: []string{"completion", "tools"},
			},
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.Family = "mystery"
				p.Caps = all
				p.Resources = ResourceProfile{ParameterSize: "7B", QuantLevel: "Q4_K_M", RAMRequired: 4.6, RAMRecommended: 6}
				p.Quality, p.Speed = TierGood, TierGood
				p.ContextWindow = 8192
				p.Digest = "sha256:abc"
			}),
		},
		{
			name: "runtime suffix template adds insert", model: mystery,
			runtime: ModelInfo{Template: suffixTmpl},
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.Caps = CapInsert
				p.Template = suffixTmpl
			}),
		},
		{
			name: "runtime plain template clears insert", model: mystery,
			runtime: ModelInfo{Template: "{{ .Prompt }}", Capabilities: []string{"insert"}},
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.Caps = CapGenerate | CapStream
				p.Template = "{{ .Prompt }}"
			}),
		},
		{
			name: "override replaces every source", model: catalogM, floor: []string{"tool_call"},
			probe: &probe{state: fingerprint.CapProbeYes}, runtime: toolsRuntime, override: []string{"chat"},
			want: with(qwenBase(), func(p *ModelProfile) { p.Caps = CapChat }),
		},
		{
			name: "override with alias token is dropped and reported", model: catalogM,
			override: []string{"tools"}, want: qwenBase(), wantRejected: 1,
		},
		{
			name: "think override", model: mystery, think: &thinkAlways, thinkTag: &tags,
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.ThinkMode = ThinkAlways
				p.ThinkTags = &ThinkTags{Open: "<t>", Close: "</t>"}
			}),
		},
		{
			name: "think override, tags only", model: catalogM, thinkTag: &tags,
			want: with(qwenBase(), func(p *ModelProfile) { p.ThinkTags = &ThinkTags{Open: "<t>", Close: "</t>"} }),
		},
		{
			name: "context window and quality ceiling overrides", model: mystery,
			runtime:     ModelInfo{ContextWindow: 8192},
			ctxOverride: 4096, ceilOverride: 2048,
			want: with(mysteryBase(), func(p *ModelProfile) {
				p.ContextWindow = 4096
				p.QualityCtxCeiling = 2048
			}),
		},
		{
			// Every layer but the capability override at once; "vision" maps to no bit.
			name: "every layer except the capability override", model: catalogM,
			floor: []string{"embed"}, fpCaps: []string{"completion"}, fpMemMB: 40960, fpCtx: 4096,
			probe:   &probe{state: fingerprint.CapProbeYes},
			runtime: ModelInfo{ContextWindow: 8192, Capabilities: []string{"vision"}, Template: "{{ .Suffix }}"},
			think:   &thinkAlways,
			want: with(qwenBase(), func(p *ModelProfile) {
				p.Caps = CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall
				p.Resources.RAMRequired = 40
				p.ContextWindow = 8192
				p.Template = "{{ .Suffix }}"
				p.ThinkMode = ThinkAlways
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			key := ModelKey{Provider: prov, Model: tt.model}
			info := tt.runtime
			info.Name = tt.model
			reg := &mrMockProviderRegistry{providers: map[string]Provider{
				prov: &mrMockProvider{name: prov, models: []ModelInfo{info}},
			}}
			rowDigest := key.String()
			if info.Digest != "" {
				rowDigest = info.Digest
			}
			fpStore := newMrMockFingerprintStore()
			opts := []ModelRegistryOption{}
			if tt.fpCaps != nil || tt.fpMemMB != 0 || tt.fpCtx != 0 {
				digest := rowDigest
				if tt.fpDigest != "" {
					digest = tt.fpDigest
				}
				if err := fpStore.Save(ctx, fingerprint.Profile{
					BackendID: prov, ModelName: tt.model, ModelDigest: digest,
					ProfileVersion: fingerprint.CurrentProfileVersion,
					Capabilities:   tt.fpCaps, PeakMemoryMB: tt.fpMemMB, EffectiveContext: tt.fpCtx,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.factory != "" {
				// No prober: the factory only declares the digest the stored profile must match.
				opts = append(opts, WithFingerprintProberFactory(
					func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
						return &FingerprintProberSpec{ModelDigest: tt.factory}, nil
					}))
			}
			if tt.probe != nil {
				cps := newFakeCapProbeStore()
				digest := rowDigest
				if tt.probe.stale {
					digest = "stale-digest"
				}
				if err := cps.SaveCapProbe(ctx, fingerprint.CapProbe{
					BackendID: prov, ModelName: tt.model, Capability: "tool_call",
					State: tt.probe.state, ModelDigest: digest,
					ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
				opts = append(opts, WithCapabilityProbeStore(cps))
			}
			mr, err := NewModelRegistry(reg, fpStore, opts...)
			if err != nil {
				t.Fatal(err)
			}
			rejected := 0
			mr.SetOverrideRejectionHook(func(ModelKey, []string, error) { rejected++ })
			if tt.floor != nil {
				mr.SetCapabilityFloor(func(ModelKey) []string { return tt.floor })
			}
			if tt.override != nil {
				mr.SetCapabilityOverride(func(ModelKey) []string { return tt.override })
			}
			if tt.think != nil || tt.thinkTag != nil {
				mr.SetThinkOverride(func(ModelKey) (*ThinkMode, *ThinkTags) { return tt.think, tt.thinkTag })
			}
			if tt.ctxOverride != 0 {
				mr.SetContextWindowOverride(func(ModelKey) int { return tt.ctxOverride })
			}
			if tt.ceilOverride != 0 {
				mr.SetQualityCtxCeilingOverride(func(ModelKey) int { return tt.ceilOverride })
			}
			got, err := mr.Lookup(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			g := *got
			g.UpdatedAt = time.Time{}
			if !reflect.DeepEqual(g, tt.want) {
				t.Errorf("profile mismatch\n got: %#v\nwant: %#v", g, tt.want)
			}
			if rejected != tt.wantRejected {
				t.Errorf("rejection hook fired %d times; want %d", rejected, tt.wantRejected)
			}
		})
	}
}
