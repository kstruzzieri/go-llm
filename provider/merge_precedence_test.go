// provider/merge_precedence_test.go
//
// Characterization of the no-log profile merge (#736).
// Every expectation in TestMerge_NoLogMeansTodaysBehavior was captured from
// the code as it stood before the capability-precedence change and is written
// out literally. A registry without an evidence log must keep producing
// exactly these profiles. The tests after it pin the log path, where one
// precedence (ExplainEvidence) decides the capability mask.
package provider

import (
	"context"
	"errors"
	"fmt"
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

// --- The log path (#736): one precedence decides the mask ---

const catalogToolModel = "qwen3:8b" // the catalog gives it chat, generate, stream and tool_call

var catalogToolCaps = CapChat | CapGenerate | CapStream | CapToolCall

// logBoundRegistry is a registry over provider "local", whose listing holds
// info, bound to b over store (the binder answers bound), on fpStore (nil
// allowed), with its clock stopped at logT0.
func logBoundRegistry(t *testing.T, store fingerprint.EvidenceStore, b EvidenceBinding, bound bool, fpStore fingerprint.Store, info ModelInfo) *ModelRegistry {
	t.Helper()
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"local": newStubProvider("local", info)}}, fpStore,
		WithEvidenceLog(store, func(string) (EvidenceBinding, bool) { return b, bound }))
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	newLogClock(mr, logT0)
	return mr
}

// rowFor is row filed under model.
func rowFor(model string, row fingerprint.EvidenceRow) fingerprint.EvidenceRow {
	row.Model = model
	return row
}

// observed is row as an in-use observation: a direct answer.
func observed(row fingerprint.EvidenceRow) fingerprint.EvidenceRow {
	row.Source = fingerprint.EvidenceSourceObserved
	return row
}

func lookupProfile(t *testing.T, mr *ModelRegistry, model string) *ModelProfile {
	t.Helper()
	p, err := mr.Lookup(t.Context(), ModelKey{Provider: "local", Model: model})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return p
}

func TestMerge_CheckedNoBeatsCatalogYes(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	store := newEvidenceLog(t)
	appendRows(t, store, rowFor(catalogToolModel, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(-logDay))))
	mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: catalogToolModel, Digest: logDigest})
	wantCaps(t, "catalog yes, probe no", lookupProfile(t, mr, catalogToolModel).Caps, catalogToolCaps&^CapToolCall)
}

// The openai-compat floor is the type tier.
func TestMerge_CheckedNoBeatsTypeYes(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	store := newEvidenceLog(t)
	appendRows(t, store, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(-logDay)))
	mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: logModel, Digest: logDigest})
	mr.SetCapabilityFloor(func(ModelKey) []string { return []string{"tool_call"} })
	wantCaps(t, "type yes, probe no", lookupProfile(t, mr, logModel).Caps, 0)
}

// A direct answer outranks what the provider reports live.
func TestMerge_CheckedNoBeatsRuntimeYes(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	store := newEvidenceLog(t)
	appendRows(t, store, observed(showRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(-logDay))))
	mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: logModel, Digest: logDigest, Capabilities: []string{"tools"}})
	wantCaps(t, "live tools, observed no", lookupProfile(t, mr, logModel).Caps, 0)
}

// An Ollama capabilities array without "tools" is the provider's no.
// ["completion"] is the realistic such array; its live chat, generate and
// stream yeses agree with the catalog and do not touch the tool_call assertion.
func TestMerge_ProviderNoBeatsCatalogYes(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	mr := logBoundRegistry(t, newEvidenceLog(t), b, true, nil,
		ModelInfo{Name: catalogToolModel, Digest: logDigest, Capabilities: []string{"completion"}})
	wantCaps(t, "catalog yes, live array without tools", lookupProfile(t, mr, catalogToolModel).Caps, catalogToolCaps&^CapToolCall)
}

func TestMerge_ExplicitStillWins(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	tests := []struct {
		name     string
		override []string
		state    fingerprint.CapProbeState
		want     Capability
	}{
		{"an explicit list without tool_call beside a probe yes", []string{"chat"}, fingerprint.CapProbeYes, CapChat},
		{"an explicit list with tool_call beside a probe no", []string{"chat", "tool_call"}, fingerprint.CapProbeNo, CapChat | CapToolCall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newEvidenceLog(t)
			appendRows(t, store, logRow(b, capabilityToolCall, tt.state, logDigest, logT0.Add(-logDay)))
			mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: logModel, Digest: logDigest})
			mr.SetCapabilityOverride(func(ModelKey) []string { return tt.override })
			wantCaps(t, tt.name, lookupProfile(t, mr, logModel).Caps, tt.want)
		})
	}
}

// A stored fingerprint profile's capabilities do not count on the log path;
// its memory and context still do.
func TestMerge_StoredFingerprintCapsIgnoredOnLogPath(t *testing.T) {
	ctx := t.Context()
	fpStore := newMrMockFingerprintStore()
	if err := fpStore.Save(ctx, fingerprint.Profile{
		BackendID: "local", ModelName: logModel, ModelDigest: logDigest,
		ProfileVersion: fingerprint.CurrentProfileVersion,
		Capabilities:   []string{"tools", "completion"}, PeakMemoryMB: 20480, EffectiveContext: 16384,
	}); err != nil {
		t.Fatal(err)
	}
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	p := lookupProfile(t, logBoundRegistry(t, newEvidenceLog(t), b, true, fpStore, ModelInfo{Name: logModel, Digest: logDigest}), logModel)
	if p.Caps != 0 || p.Resources.RAMRequired != 20 || p.ContextWindow != 16384 {
		t.Fatalf("Caps=%v RAMRequired=%v ContextWindow=%d; want no capability, 20 GB, 16384", p.Caps, p.Resources.RAMRequired, p.ContextWindow)
	}
}

// Live Ollama facts count whatever the binding says: with no binding, and with
// a log that cannot be read.
func TestMerge_UnboundKeepsRuntimeFacts(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	info := ModelInfo{Name: logModel, Digest: logDigest, Capabilities: []string{"tools"}}
	t.Run("the binder does not resolve the provider", func(t *testing.T) {
		mr := logBoundRegistry(t, newEvidenceLog(t), b, false, nil, info)
		wantCaps(t, "unbound", lookupProfile(t, mr, logModel).Caps, CapToolCall)
	})
	t.Run("the log cannot be read", func(t *testing.T) {
		store := &hookedEvidenceStore{EvidenceStore: newEvidenceLog(t), snapshotErr: errors.New("database is locked")}
		mr := logBoundRegistry(t, store, b, true, nil, info)
		wantCaps(t, "unreadable", lookupProfile(t, mr, logModel).Caps, CapToolCall)
	})
}

// Every layer but the capability mask builds the same profile with and without
// the log.
func TestMerge_NonCapabilityFieldsUnchanged(t *testing.T) {
	thinkAlways := ThinkAlways
	tests := []struct {
		name    string
		info    ModelInfo
		fp      bool // a stored fingerprint profile with memory and context
		think   bool
		ctxCeil bool // context window and quality ceiling overrides
	}{
		{
			name: "catalog model with every layer",
			info: ModelInfo{
				Name: catalogToolModel, Family: "Qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 8192,
				Digest: logDigest, Template: "{{ .Prompt }}{{ .Suffix }}", Capabilities: []string{"completion"},
			},
			fp: true, think: true,
		},
		{
			name: "uncatalogued model inferred from the runtime",
			info: ModelInfo{
				Name: logModel, Family: "Mystery", ParameterSize: "7B", QuantLevel: "Q4_K_M", ContextWindow: 8192,
				Digest: logDigest, Template: "{{ .Prompt }}", Capabilities: []string{"completion", "tools"},
			},
		},
		{
			name: "context and quality ceiling overrides",
			info: ModelInfo{Name: catalogToolModel, ContextWindow: 8192, Digest: logDigest},
			fp:   true, ctxCeil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			build := func(withLog bool) ModelProfile {
				fpStore := newMrMockFingerprintStore()
				if tt.fp {
					if err := fpStore.Save(t.Context(), fingerprint.Profile{
						BackendID: "local", ModelName: tt.info.Name, ModelDigest: logDigest,
						ProfileVersion: fingerprint.CurrentProfileVersion, PeakMemoryMB: 40960, EffectiveContext: 4096,
					}); err != nil {
						t.Fatal(err)
					}
				}
				prov := map[string]Provider{"local": newStubProvider("local", tt.info)}
				var opts []ModelRegistryOption
				if withLog {
					b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
					opts = append(opts, WithEvidenceLog(newEvidenceLog(t), func(string) (EvidenceBinding, bool) { return b, true }))
				}
				mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: prov}, fpStore, opts...)
				if err != nil {
					t.Fatal(err)
				}
				if tt.think {
					mr.SetThinkOverride(func(ModelKey) (*ThinkMode, *ThinkTags) {
						return &thinkAlways, &ThinkTags{Open: "<t>", Close: "</t>"}
					})
				}
				if tt.ctxCeil {
					mr.SetContextWindowOverride(func(ModelKey) int { return 4000 })
					mr.SetQualityCtxCeilingOverride(func(ModelKey) int { return 2000 })
				}
				p := *lookupProfile(t, mr, tt.info.Name)
				p.Caps, p.UpdatedAt = 0, time.Time{}
				return p
			}
			if noLog, withLog := build(false), build(true); !reflect.DeepEqual(withLog, noLog) {
				t.Fatalf("profiles differ beyond Caps\n log: %#v\nnone: %#v", withLog, noLog)
			}
		})
	}
}

// --- Resolution: a stored answer that settles resolution is the answer
// routing uses. Each fixture is an evidence log, WithCapabilityProber and a
// fake fingerprint.CapabilityProber (newLogResolver, whose chat and stream
// floor never answers tool_call); checks the prober runs are counted apart
// from the runtime metadata queries (Models) the registry makes. ---

// stubOf is the stub provider newLogResolver built for name.
func stubOf(t *testing.T, mr *ModelRegistry, name string) *logStubProvider {
	t.Helper()
	p, ok := mr.providers.(*mrMockProviderRegistry).providers[name].(*logStubProvider)
	if !ok {
		t.Fatalf("no stub provider %q", name)
	}
	return p
}

// The model is deliberately a catalog one (newLogResolver's doc says its
// fixtures have none): the catalog's yes is the source the stored no beats.
func TestResolveToolCall_CheckedNoDoesNotReprobe(t *testing.T) {
	ctx := t.Context()
	b := openAIBinding(t)
	store := newEvidenceLog(t)
	appendRows(t, store, rowFor(catalogToolModel, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(-logDay))))
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, store, prober, oneProv(t, infos(catalogToolModel)...))
	mr.syncGeneration(ctx) // the first boundary, so the profile Lookup caches stays
	key := localKey(catalogToolModel)

	if p := lookupProfile(t, mr, catalogToolModel); p.Caps.Has(CapToolCall) {
		t.Fatalf("Lookup caps = %v; want no tool_call beside the stored no", p.Caps)
	}
	state, err := mr.ResolveToolCall(ctx, key)
	wantState(t, "resolve", state, err, fingerprint.CapProbeNo)
	wantCalls(t, "checks", prober, 0)
	// Lookup, then the resolver's own runtime query.
	wantModelsCalls(t, "metadata queries", stubOf(t, mr, "local"), 2)
}

func TestResolveToolCall_StoredProviderBeatsLiveFact(t *testing.T) {
	ctx := t.Context()
	native := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	store := newEvidenceLog(t)
	appendRows(t, store, showRow(native, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0.Add(-logDay)))
	prober := &capProbeFake{}
	live := ModelInfo{Name: logModel, Digest: logDigest, ContextWindow: 32768, Capabilities: []string{"completion"}}
	mr, _ := newLogResolver(t, store, prober, []logProv{{name: "local", binding: native, models: []ModelInfo{live}}})
	mr.syncGeneration(ctx)

	if p := lookupProfile(t, mr, logModel); !p.Caps.Has(CapToolCall) {
		t.Fatalf("Lookup caps = %v; want the stored provider yes", p.Caps)
	}
	state, err := mr.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
	if _, err := routerOver(t, mr, logModel).Route(ctx, RoutingRequest{Model: "local/" + logModel, UseCase: "agent", RequiredCaps: agentCaps}); err != nil {
		t.Fatalf("Route: %v; want the model admitted", err)
	}
	wantCalls(t, "checks", prober, 0)
	// Lookup only: the cached profile answers resolution and the route.
	wantModelsCalls(t, "metadata queries", stubOf(t, mr, "local"), 1)
}

// As before #736: live facts with no stored row leave resolution to one native
// check, which spends no allowance and is written, and its no then settles.
func TestResolveToolCall_LiveNoWithoutRowsRunsNativeCheck(t *testing.T) {
	ctx := t.Context()
	native := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	store := newEvidenceLog(t)
	prober := &capProbeFake{run: scripted(fingerprint.CapabilityProbeResult{
		Method: fingerprint.MethodOllamaShow, Requests: 1,
		Answers: []fingerprint.ProbeAnswer{{Capability: capabilityToolCall, Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeNo}},
	}, nil)}
	live := ModelInfo{Name: logModel, Digest: logDigest, ContextWindow: 32768, Capabilities: []string{"completion"}}
	mr, _ := newLogResolver(t, store, prober, []logProv{{name: "local", binding: native, models: []ModelInfo{live}}})
	mr.syncGeneration(ctx)
	key := localKey(logModel)

	if p := lookupProfile(t, mr, logModel); p.Caps.Has(CapToolCall) {
		t.Fatalf("Lookup caps = %v; want no tool_call from the live array", p.Caps)
	}
	state, err := mr.ResolveToolCall(ctx, key)
	wantState(t, "the check", state, err, fingerprint.CapProbeNo)
	wantCalls(t, "checks", prober, 1)
	if req := prober.reqs[0]; req.Method != fingerprint.MethodOllamaShow {
		t.Fatalf("check method = %q; want ollama-show", req.Method)
	}
	if n := allowanceOf(mr); n != automaticProbeAllowance {
		t.Fatalf("allowance = %d; want untouched %d", n, automaticProbeAllowance)
	}
	if got := toolCallAnswer(t, store, native, logDigest, logT0); got != fingerprint.CapProbeNo {
		t.Fatalf("stored tool_call answer = %q; want no", got)
	}

	state, err = mr.ResolveToolCall(ctx, key)
	wantState(t, "after the check", state, err, fingerprint.CapProbeNo)
	wantCalls(t, "checks after the stored no", prober, 1)
	// Lookup, the first resolution's query, the rebuilt profile's, the second's.
	wantModelsCalls(t, "metadata queries", stubOf(t, mr, "local"), 4)
}

// --- Projection ---

func TestProjectListed_KnownNegativeSurvivesProjection(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	store := newEvidenceLog(t)
	appendRows(t, store, rowFor(catalogToolModel, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(-logDay))))
	mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: "unused"})
	facts, err := mr.ProjectListedModels(t.Context(), "local", []ModelInfo{{Name: catalogToolModel, Digest: logDigest}})
	if err != nil {
		t.Fatalf("ProjectListedModels: %v", err)
	}
	if f := facts[0]; f.Caps != catalogToolCaps&^CapToolCall || f.KnownMask != catalogToolCaps {
		t.Fatalf("Caps=%v KnownMask=%v; want the catalog without tool_call, and tool_call known", f.Caps, f.KnownMask)
	}
}

// Without a log the projection is as before #736, written out literally: the
// legacy row answers tool_call alone, and a listing array without tools claims
// no known no.
func TestProjectListed_NoLogUnchanged(t *testing.T) {
	ctx := t.Context()
	legacy := newFakeCapProbeStore()
	for model, state := range map[string]fingerprint.CapProbeState{"said-no": fingerprint.CapProbeNo, catalogToolModel: fingerprint.CapProbeNo, "said-yes": fingerprint.CapProbeYes} {
		if err := legacy.SaveCapProbe(ctx, fingerprint.CapProbe{
			BackendID: "prov", ModelName: model, Capability: capabilityToolCall, State: state,
			ModelDigest: logDigest, ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"prov": &mrMockProvider{name: "prov"}}}, nil, WithCapabilityProbeStore(legacy))
	if err != nil {
		t.Fatal(err)
	}
	mr.SetCapabilityFloor(func(k ModelKey) []string {
		if k.Model == "floored" {
			return []string{"tool_call"}
		}
		return nil
	})
	facts, err := mr.ProjectListedModels(ctx, "prov", []ModelInfo{
		{Name: catalogToolModel, Digest: logDigest},
		{Name: "said-no", Digest: logDigest},
		{Name: "said-yes", Digest: logDigest},
		{Name: "array-without-tools", Capabilities: []string{"completion"}},
		{Name: "floored"},
	})
	if err != nil {
		t.Fatalf("ProjectListedModels: %v", err)
	}
	chatGen := CapChat | CapGenerate | CapStream
	want := []struct {
		model       string
		caps, known Capability
	}{
		{catalogToolModel, catalogToolCaps, catalogToolCaps},
		{"said-no", 0, CapToolCall},
		{"said-yes", CapToolCall, CapToolCall},
		{"array-without-tools", chatGen, chatGen},
		{"floored", CapToolCall, CapToolCall},
	}
	for i, w := range want {
		if f := facts[i]; f.Key.Model != w.model || f.Caps != w.caps || f.KnownMask != w.known {
			t.Errorf("facts[%d] = %s Caps=%v KnownMask=%v; want %s Caps=%v KnownMask=%v", i, f.Key.Model, f.Caps, f.KnownMask, w.model, w.caps, w.known)
		}
	}
}

// A registry reads an Ollama model's rows under both spellings,
// m and m:latest, stored or kept, and never another tag's.
func TestEvidenceLog_ReadsBothOllamaSpellings(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	tests := []struct {
		configured, stored string
		kept               bool
	}{
		{"mystery", "mystery:latest", false},
		{"mystery:latest", "mystery", false},
		{"mystery", "mystery:latest", true},
		{"mystery:latest", "mystery", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s reads %s (kept %v)", tt.configured, tt.stored, tt.kept), func(t *testing.T) {
			store := newEvidenceLog(t)
			// Another tag's no must not reach either spelling.
			appendRows(t, store, rowFor("mystery:8b", showRow(b, "thinking", fingerprint.CapProbeNo, logDigest, logT0)))
			yes := rowFor(tt.stored, showRow(b, "thinking", fingerprint.CapProbeYes, logDigest, logT0.Add(-time.Hour)))
			mr := logBoundRegistry(t, store, b, true, nil, ModelInfo{Name: tt.configured, Digest: logDigest})
			if tt.kept {
				mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{yes})
			} else {
				appendRows(t, store, yes)
			}
			wantCaps(t, "Lookup", lookupProfile(t, mr, tt.configured).Caps, CapThinking)
			facts, err := mr.ProjectListedModels(t.Context(), "local", []ModelInfo{{Name: tt.configured, Digest: logDigest}})
			if err != nil {
				t.Fatalf("ProjectListedModels: %v", err)
			}
			if f := facts[0]; f.Caps != CapThinking || f.KnownMask != CapThinking {
				t.Fatalf("projection Caps=%v KnownMask=%v; want thinking in both", f.Caps, f.KnownMask)
			}
		})
	}
}

// The floor and the explicit list reach the log-path projection through the
// same policy Lookup uses: projected Caps equal Lookup's, and KnownMask is what
// the precedence answers yes or no.
func TestProjectListed_PolicyAgreesWithLookup(t *testing.T) {
	b := mustBinding(t, "http://127.0.0.1:8080", "openai-compat")
	allCanonical := CapChat | CapGenerate | CapStream | CapEmbed | CapToolCall | CapThinking | CapInsert
	tests := []struct {
		name            string
		floor, override []string
		state           fingerprint.CapProbeState // a stored tool_call answer, "" for none
		caps, known     Capability
	}{
		{name: "type floor", floor: []string{"chat", "tool_call"}, caps: CapChat | CapToolCall, known: CapChat | CapToolCall},
		// An explicit list answers every capability: yes for its own, no for the rest.
		{name: "explicit list over a stored yes", override: []string{"chat"}, state: fingerprint.CapProbeYes, caps: CapChat, known: allCanonical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newEvidenceLog(t)
			if tt.state != "" {
				appendRows(t, store, logRow(b, capabilityToolCall, tt.state, logDigest, logT0.Add(-logDay)))
			}
			info := ModelInfo{Name: logModel, Digest: logDigest}
			mr := logBoundRegistry(t, store, b, true, nil, info)
			if tt.floor != nil {
				mr.SetCapabilityFloor(func(ModelKey) []string { return tt.floor })
			}
			if tt.override != nil {
				mr.SetCapabilityOverride(func(ModelKey) []string { return tt.override })
			}
			facts, err := mr.ProjectListedModels(t.Context(), "local", []ModelInfo{info})
			if err != nil {
				t.Fatalf("ProjectListedModels: %v", err)
			}
			if f := facts[0]; f.Caps != tt.caps || f.KnownMask != tt.known {
				t.Fatalf("Caps=%v KnownMask=%v; want %v, %v", f.Caps, f.KnownMask, tt.caps, tt.known)
			}
			wantCaps(t, "Lookup", lookupProfile(t, mr, logModel).Caps, facts[0].Caps)
		})
	}
}
