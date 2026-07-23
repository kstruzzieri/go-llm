package provider

import (
	"context"
	"testing"
)

// newThinkOverrideTestRegistry builds a registry with a single mock provider
// serving "qwen3:8b". The static catalog gives the qwen3 family
// ThinkMode=ThinkToggle and ThinkTags <think>/</think>, which is the merged
// baseline the think-override tests replace against.
func newThinkOverrideTestRegistry(t *testing.T) *ModelRegistry {
	t.Helper()
	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	return mr
}

// TestThinkOverrideReplacesModePerField pins the per-field REPLACE contract:
// a nil pointer keeps the merged value, a non-nil pointer replaces it — in
// BOTH directions and independently per field (#219 lesson: replace
// semantics must be explicit and tested both ways).
func TestThinkOverrideReplacesModePerField(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	baseTags := ThinkTags{Open: "<think>", Close: "</think>"}
	customTags := ThinkTags{Open: "<r>", Close: "</r>"}

	tests := []struct {
		name     string
		override ThinkOverride
		wantMode ThinkMode
		wantTags ThinkTags
	}{
		{
			name: "mode-only replaces mode, keeps tags",
			override: func(_ ModelKey) (*ThinkMode, *ThinkTags) {
				m := ThinkAlways
				return &m, nil
			},
			wantMode: ThinkAlways,
			wantTags: baseTags,
		},
		{
			name: "tags-only replaces tags, keeps mode",
			override: func(_ ModelKey) (*ThinkMode, *ThinkTags) {
				tags := customTags
				return nil, &tags
			},
			wantMode: ThinkToggle,
			wantTags: customTags,
		},
		{
			name: "both replaced",
			override: func(_ ModelKey) (*ThinkMode, *ThinkTags) {
				m := ThinkNone
				tags := customTags
				return &m, &tags
			},
			wantMode: ThinkNone,
			wantTags: customTags,
		},
		{
			name: "nil-nil keeps merged values",
			override: func(_ ModelKey) (*ThinkMode, *ThinkTags) {
				return nil, nil
			},
			wantMode: ThinkToggle,
			wantTags: baseTags,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr := newThinkOverrideTestRegistry(t)
			mr.SetThinkOverride(tt.override)

			profile, err := mr.Lookup(ctx, key)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if profile.ThinkMode != tt.wantMode {
				t.Errorf("ThinkMode = %v, want %v", profile.ThinkMode, tt.wantMode)
			}
			if profile.ThinkTags == nil {
				t.Fatalf("ThinkTags = nil, want %+v", tt.wantTags)
			}
			if *profile.ThinkTags != tt.wantTags {
				t.Errorf("ThinkTags = %+v, want %+v", *profile.ThinkTags, tt.wantTags)
			}
		})
	}
}

// TestThinkOverrideDoesNotImplyCapThinking verifies the override changes
// parser behavior only: forcing mode=always on a model without CapThinking
// must NOT grant the capability bit (routing gates stay honest).
func TestThinkOverrideDoesNotImplyCapThinking(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	mr := newThinkOverrideTestRegistry(t)
	baseline, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("baseline Lookup: %v", err)
	}
	if baseline.Caps.Has(CapThinking) {
		t.Fatal("precondition: baseline profile must not have CapThinking")
	}

	mr.SetThinkOverride(func(_ ModelKey) (*ThinkMode, *ThinkTags) {
		m := ThinkAlways
		return &m, nil
	})
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-override Lookup: %v", err)
	}
	if profile.Caps.Has(CapThinking) {
		t.Error("CapThinking gained via think override; the override must not touch Caps")
	}
	if profile.Caps != baseline.Caps {
		t.Errorf("Caps changed by think override: %v -> %v", baseline.Caps, profile.Caps)
	}
}

// TestSetThinkOverrideInvalidatesCache verifies SetThinkOverride shares
// SetCapabilityOverride's cache-flush semantics: installing after a warm
// Lookup takes effect on the next Lookup, and clearing restores the merged
// value (no stale overridden profile served after revert).
func TestSetThinkOverrideInvalidatesCache(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	mr := newThinkOverrideTestRegistry(t)

	warmed, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("warm Lookup: %v", err)
	}
	if warmed.ThinkMode != ThinkToggle {
		t.Fatalf("precondition: warmed ThinkMode = %v, want %v", warmed.ThinkMode, ThinkToggle)
	}

	mr.SetThinkOverride(func(_ ModelKey) (*ThinkMode, *ThinkTags) {
		m := ThinkAlways
		return &m, nil
	})
	overridden, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-install Lookup: %v", err)
	}
	if overridden.ThinkMode != ThinkAlways {
		t.Errorf("ThinkMode = %v, want %v (override installed after warm cache must flush)", overridden.ThinkMode, ThinkAlways)
	}

	mr.SetThinkOverride(nil)
	reverted, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-clear Lookup: %v", err)
	}
	if reverted.ThinkMode != ThinkToggle {
		t.Errorf("ThinkMode after clearing override = %v, want %v (clearing must also flush)", reverted.ThinkMode, ThinkToggle)
	}
}
