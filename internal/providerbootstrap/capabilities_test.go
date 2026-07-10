package providerbootstrap

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestCapabilitiesForKey_ExplicitCaps(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{
		"chat": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
	}}
	got := capabilitiesForKey(cfg, provider.ModelKey{Provider: "lc", Model: "qwen"})
	if !reflect.DeepEqual(got, []string{"chat", "tool_call"}) {
		t.Fatalf("caps = %v", got)
	}
}

func TestCapabilitiesForKey_UnknownKeyNil(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	if got := capabilitiesForKey(cfg, provider.ModelKey{Provider: "x", Model: "y"}); got != nil {
		t.Fatalf("caps = %v, want nil", got)
	}
}

func TestBuildModelDefaults_ConflictErrors(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{
		"chat": {
			Provider: "lc", Name: "qwen",
			Options: &config.SamplingOptions{TopK: provider.Ptr(20)},
		},
		"review": {
			Provider: "lc", Name: "qwen",
			Options: &config.SamplingOptions{TopK: provider.Ptr(40)},
		},
	}}
	if _, err := buildModelDefaults(cfg); err == nil {
		t.Fatal("expected conflict error for divergent sampling defaults on the same model")
	}
}

func TestBuildCapabilityOverrides_OpenAICompatModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat"}},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
		},
	}
	got, err := buildCapabilityOverrides(cfg)
	if err != nil {
		t.Fatalf("buildCapabilityOverrides: %v", err)
	}
	caps := got[provider.ModelKey{Provider: "lc", Model: "qwen"}]
	if !reflect.DeepEqual(caps, []string{"chat", "tool_call"}) {
		t.Fatalf("override caps = %v", caps)
	}
}

func TestBuildCapabilityOverrides_ConflictErrors(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat"}},
		Models: map[string]config.ModelConfig{
			"chat":   {Provider: "lc", Name: "qwen", Capabilities: []string{"chat"}},
			"review": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
		},
	}
	if _, err := buildCapabilityOverrides(cfg); err == nil {
		t.Fatalf("expected conflict error for divergent caps on same key")
	}
}

func TestBuildCapabilityFloors_DerivedOpenAICompatOnly(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			"ollama":   {BaseURL: "http://localhost:11434"},
			"ollama2":  {APIFormat: "ollama"},
		},
		Models: map[string]config.ModelConfig{
			"agent":     {Provider: "llamacpp", Name: "byo-model", Type: "dense"},                                                       // undeclared => floor
			"chat":      {Provider: "llamacpp", Name: "declared", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}}, // explicit => override, NOT floor
			"embed":     {Provider: "ollama", Name: "qwen3-embedding:8b", Type: "embedding"},                                            // default-empty format => ollama => neither
			"summarize": {Provider: "ollama2", Name: "small", Type: "dense"},                                                            // explicit ollama format => no floor
			"orphan":    {Provider: "ghost", Name: "m", Type: "dense"},                                                                  // provider missing from cfg.Providers => no floor
			"notype":    {Provider: "llamacpp", Name: "untyped", Type: ""},                                                              // empty derived set => no floor
		},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	if got := floors[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; len(got) == 0 {
		t.Fatalf("undeclared openai-compat model missing floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; ok {
		t.Fatalf("explicitly declared model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ollama", Model: "qwen3-embedding:8b"}]; ok {
		t.Fatalf("ollama model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ollama2", Model: "small"}]; ok {
		t.Fatalf("explicit ollama-format model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ghost", Model: "m"}]; ok {
		t.Fatalf("model on a provider missing from cfg.Providers must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "llamacpp", Model: "untyped"}]; ok {
		t.Fatalf("openai-compat model with empty derived set must not get a floor")
	}

	overrides, err := buildCapabilityOverrides(cfg)
	if err != nil {
		t.Fatalf("overrides: %v", err)
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; ok {
		t.Fatalf("undeclared openai-compat model must no longer get a REPLACE override")
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; !ok {
		t.Fatalf("explicit declaration must remain an override")
	}
}

func TestBuildCapabilityFloors_SharedKeyUnionsDerived(t *testing.T) {
	// Two roles, same key, different types => floors union (OR semantics
	// make conflicts impossible; no error path for floors).
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "llamacpp", Name: "m", Type: "dense"},
			"embed": {Provider: "llamacpp", Name: "m", Type: "embedding"},
		},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	got := floors[provider.ModelKey{Provider: "llamacpp", Model: "m"}]
	// dense derives chat,generate,stream; embedding derives embed => union of all four.
	want := map[string]bool{"chat": true, "generate": true, "stream": true, "embed": true}
	if len(got) != len(want) {
		t.Fatalf("floor union = %v, want keys %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Fatalf("unexpected floor token %q in %v", tok, got)
		}
	}
}

func TestInstallCapabilityFloors_NilSafe(t *testing.T) {
	if err := installCapabilityFloors(nil, nil); err != nil {
		t.Fatalf("installCapabilityFloors(nil,nil) = %v, want nil", err)
	}
}

func TestInstallCapabilityOverrides_NilSafe(t *testing.T) {
	if err := installCapabilityOverrides(nil, nil); err != nil {
		t.Fatalf("installCapabilityOverrides(nil,nil) = %v, want nil", err)
	}
}

func TestInstallThinkOverrides_NilSafe(t *testing.T) {
	if err := installThinkOverrides(nil, nil); err != nil {
		t.Fatalf("installThinkOverrides(nil,nil) = %v, want nil", err)
	}
}

func TestBuildThinkOverridesSameKeyCompatibility(t *testing.T) {
	tags := &config.ThinkTagsConfig{Open: "<r>", Close: "</r>"}
	otherTags := &config.ThinkTagsConfig{Open: "<t>", Close: "</t>"}
	key := provider.ModelKey{Provider: "lc", Model: "qwen"}

	tests := []struct {
		name     string
		models   map[string]config.ModelConfig
		wantErr  bool
		errNames []string // role names the error must mention
		check    func(t *testing.T, got map[provider.ModelKey]thinkOverrideEntry)
	}{
		{
			name: "identical modes on same key are compatible",
			models: map[string]config.ModelConfig{
				"chat":   {Provider: "lc", Name: "qwen", ThinkMode: "always"},
				"review": {Provider: "lc", Name: "qwen", ThinkMode: "always"},
			},
			check: func(t *testing.T, got map[provider.ModelKey]thinkOverrideEntry) {
				e := got[key]
				if e.mode == nil || *e.mode != provider.ThinkAlways {
					t.Fatalf("mode = %v, want always", e.mode)
				}
			},
		},
		{
			name: "identical tags on same key are compatible",
			models: map[string]config.ModelConfig{
				// Distinct pointers, equal values: compatibility must compare
				// tag values, not re-declaration per se.
				"chat":   {Provider: "lc", Name: "qwen", ThinkTags: tags},
				"review": {Provider: "lc", Name: "qwen", ThinkTags: &config.ThinkTagsConfig{Open: "<r>", Close: "</r>"}},
			},
			check: func(t *testing.T, got map[provider.ModelKey]thinkOverrideEntry) {
				e := got[key]
				if e.tags == nil || *e.tags != (provider.ThinkTags{Open: "<r>", Close: "</r>"}) {
					t.Fatalf("tags = %v, want <r>/</r>", e.tags)
				}
			},
		},
		{
			name: "mode-only and tags-only combine per field",
			models: map[string]config.ModelConfig{
				"chat":   {Provider: "lc", Name: "qwen", ThinkMode: "toggle"},
				"review": {Provider: "lc", Name: "qwen", ThinkTags: tags},
			},
			check: func(t *testing.T, got map[provider.ModelKey]thinkOverrideEntry) {
				e := got[key]
				if e.mode == nil || *e.mode != provider.ThinkToggle {
					t.Fatalf("mode = %v, want toggle", e.mode)
				}
				if e.tags == nil || *e.tags != (provider.ThinkTags{Open: "<r>", Close: "</r>"}) {
					t.Fatalf("tags = %v, want <r>/</r>", e.tags)
				}
			},
		},
		{
			name: "conflicting modes error naming both roles",
			models: map[string]config.ModelConfig{
				"chat":   {Provider: "lc", Name: "qwen", ThinkMode: "always"},
				"review": {Provider: "lc", Name: "qwen", ThinkMode: "none"},
			},
			wantErr:  true,
			errNames: []string{"chat", "review"},
		},
		{
			name: "conflicting tags error naming both roles",
			models: map[string]config.ModelConfig{
				"chat":   {Provider: "lc", Name: "qwen", ThinkTags: tags},
				"review": {Provider: "lc", Name: "qwen", ThinkTags: otherTags},
			},
			wantErr:  true,
			errNames: []string{"chat", "review"},
		},
		{
			name: "invalid mode fails loud for programmatic configs",
			models: map[string]config.ModelConfig{
				"chat": {Provider: "lc", Name: "qwen", ThinkMode: "bogus"},
			},
			wantErr:  true,
			errNames: []string{"chat"},
		},
		{
			name: "entries with neither field are skipped",
			models: map[string]config.ModelConfig{
				"chat": {Provider: "lc", Name: "qwen"},
			},
			check: func(t *testing.T, got map[provider.ModelKey]thinkOverrideEntry) {
				if len(got) != 0 {
					t.Fatalf("overrides = %v, want empty", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildThinkOverrides(&config.Config{Models: tt.models})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				for _, name := range tt.errNames {
					if !strings.Contains(err.Error(), "\""+name+"\"") {
						t.Fatalf("error %q does not name role %q", err, name)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("buildThinkOverrides: %v", err)
			}
			tt.check(t, got)
		})
	}
}
