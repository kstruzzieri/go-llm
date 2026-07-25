package golem

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
)

func bootstrapOrchestrator(ctx context.Context, configPath string, onWarning func(error)) (*agent.Orchestrator, *providerbootstrap.Bundle, conversation.Summarizer, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return nil, nil, nil, err
	}
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{Config: cfg})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("golem: bootstrap providers: %w", err)
	}
	for _, warning := range bundle.Warnings {
		if onWarning != nil {
			onWarning(warning)
		}
	}
	var chain []string
	if _, ok := bundle.Config.Defaults["agent"]; ok {
		chain, err = bundle.Config.RoleFallbackChain("agent")
		if err != nil {
			_ = bundle.Close()
			return nil, nil, nil, fmt.Errorf("golem: resolve agent chain: %w", err)
		}
	}
	var summarizeChain []string
	if _, ok := bundle.Config.RoleForUseCase(config.UseCaseSummarize); ok {
		summarizeChain, err = bundle.Config.RoleFallbackChain(config.UseCaseSummarize)
		if err != nil {
			_ = bundle.Close()
			return nil, nil, nil, fmt.Errorf("golem: resolve summarize chain: %w", err)
		}
	}
	return agent.New(agent.NewRouterModelCallerWithChain(bundle.Router, chain), agent.ContextManager{}),
		bundle, agent.NewRouterSummarizer(bundle.Router, summarizeChain), nil
}

func loadConfig(path string) (*config.Config, error) {
	if path != "" {
		cfg, err := config.Load(path)
		if err != nil {
			return nil, fmt.Errorf("golem: load config %q: %w", path, err)
		}
		return cfg, nil
	}
	cfg, err := config.Default()
	if errors.Is(err, config.ErrConfigNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("golem: discover config: %w", err)
	}
	return cfg, nil
}
