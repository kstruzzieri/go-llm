package main

import (
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
)

// chainPlan is the resolved agent-routing strategy. Exactly one of chain or
// useRecommend is meaningful; both are zero only when resolveAgentChain also
// returns an error, so callers must check err before reading these fields.
type chainPlan struct {
	chain        []string // strict PreferredChain when non-empty
	useRecommend bool     // true => empty-Model recommend (defaults.agent unset or nil cfg)
}

// loadConfig applies golem's config-discovery rules. An explicit path that
// fails to load is fatal; auto-discovery that finds nothing returns (nil, nil)
// so the caller passes Config:nil to providerbootstrap.New (default Ollama).
func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("golem: load config %q: %w", path, err)
		}
		return cfg, nil
	}
	cfg, err := config.Default()
	if err != nil {
		if errors.Is(err, config.ErrConfigNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("golem: discover config: %w", err)
	}
	return cfg, nil
}

// resolveAgentChain gates strict-chain vs recommend on the EFFECTIVE config
// (bundle.Config). defaults.agent absent (or nil cfg) => recommend; present
// and resolvable => strict chain; present but unresolvable => fatal.
func resolveAgentChain(cfg *config.Config) (chainPlan, error) {
	if cfg == nil {
		return chainPlan{useRecommend: true}, nil
	}
	if _, ok := cfg.Defaults["agent"]; !ok {
		return chainPlan{useRecommend: true}, nil
	}
	chain, err := cfg.RoleFallbackChain("agent")
	if err != nil {
		return chainPlan{}, fmt.Errorf("golem: resolve agent chain: %w", err)
	}
	return chainPlan{chain: chain}, nil
}

func resolveSummarizeChain(cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	if _, ok := cfg.Defaults["summarize"]; !ok {
		if _, ok := cfg.Defaults["analysis"]; !ok {
			if _, ok := cfg.Defaults["chat"]; !ok {
				return nil, nil
			}
		}
	}
	chain, err := cfg.RoleFallbackChain("summarize")
	if err != nil {
		return nil, fmt.Errorf("golem: resolve summarize chain: %w", err)
	}
	return chain, nil
}
