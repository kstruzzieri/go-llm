package main

import (
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func TestResolveAgentChain_Present(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"ollama": {}},
		Models: map[string]config.ModelConfig{
			"primary": {Name: "m1", Provider: "ollama", Fallbacks: []string{"backup"}},
			"backup":  {Name: "m2", Provider: "ollama"},
		},
		Defaults: map[string]string{"agent": "primary"},
	}
	plan, err := resolveAgentChain(cfg)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if plan.useRecommend {
		t.Error("useRecommend = true, want chain")
	}
	want := []string{"ollama/m1", "ollama/m2"}
	if len(plan.chain) != 2 || plan.chain[0] != want[0] || plan.chain[1] != want[1] {
		t.Errorf("chain = %v, want %v", plan.chain, want)
	}
}

func TestResolveAgentChain_AbsentUsesRecommend(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama"}},
		Defaults: map[string]string{},
	}
	plan, err := resolveAgentChain(cfg)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !plan.useRecommend || len(plan.chain) != 0 {
		t.Errorf("plan = %+v, want useRecommend with empty chain", plan)
	}
}

func TestResolveAgentChain_NilConfigUsesRecommend(t *testing.T) {
	plan, err := resolveAgentChain(nil)
	if err != nil || !plan.useRecommend {
		t.Errorf("nil cfg: plan=%+v err=%v, want useRecommend", plan, err)
	}
}

func TestResolveAgentChain_MisconfiguredFatal(t *testing.T) {
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama", Fallbacks: []string{"ghost"}}},
		Defaults: map[string]string{"agent": "primary"},
	}
	if _, err := resolveAgentChain(cfg); err == nil {
		t.Fatal("err = nil for unknown fallback role, want fatal")
	}
}

func TestResolveAgentChain_EmptyAgentKeyFatal(t *testing.T) {
	// Key present but mapping to "" is misconfiguration, not "unset": the
	// presence check passes, RoleFallbackChain("agent") then fails on role "".
	cfg := &config.Config{
		Models:   map[string]config.ModelConfig{"primary": {Name: "m1", Provider: "ollama"}},
		Defaults: map[string]string{"agent": ""},
	}
	if _, err := resolveAgentChain(cfg); err == nil {
		t.Fatal("err = nil for empty agent role, want fatal")
	}
}

func TestLoadConfig_ExplicitBadPathFatal(t *testing.T) {
	if _, err := loadConfig("/nonexistent/path/models.json"); err == nil {
		t.Fatal("err = nil for nonexistent explicit config path, want fatal")
	}
}
