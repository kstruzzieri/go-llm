package analysis

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// StrategyReport holds the results of a trading strategy analysis.
type StrategyReport struct {
	StrategyName string
	Analysis     string
}

// StrategyAnalyzer generates natural-language analysis of trading strategies.
type StrategyAnalyzer struct {
	client *ollama.Client
	model  string
}

// NewStrategyAnalyzer creates a StrategyAnalyzer with the given client and model.
func NewStrategyAnalyzer(client *ollama.Client, model string) (*StrategyAnalyzer, error) {
	if client == nil {
		return nil, fmt.Errorf("analysis: new strategy analyzer: client is required")
	}
	if model == "" {
		return nil, fmt.Errorf("analysis: new strategy analyzer: model is required")
	}
	return &StrategyAnalyzer{
		client: client,
		model:  model,
	}, nil
}

// AnalyzeStrategy generates a natural-language analysis of a trading strategy
// based on its name and performance metrics.
func (sa *StrategyAnalyzer) AnalyzeStrategy(ctx context.Context, name string, metrics map[string]float64) (string, error) {
	if name == "" {
		return "", fmt.Errorf("analysis: analyze strategy: name is required")
	}
	if len(metrics) == 0 {
		return "", fmt.Errorf("analysis: analyze strategy: metrics are required")
	}

	prompt := buildStrategyPrompt(name, metrics)

	resp, err := sa.client.Chat(ctx, ollama.ChatRequest{
		Model: sa.model,
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "You are a quantitative finance expert. Analyze trading strategy performance and provide actionable insights about risk, returns, and potential improvements."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: analyze strategy: %w", err)
	}

	return resp.Message.Content, nil
}

// CompareStrategies generates a comparative analysis of multiple trading strategies.
// Each entry in the map is a strategy name to its performance metrics.
func (sa *StrategyAnalyzer) CompareStrategies(ctx context.Context, strategies map[string]map[string]float64) (string, error) {
	if len(strategies) < 2 {
		return "", fmt.Errorf("analysis: compare strategies: at least 2 strategies are required, got %d", len(strategies))
	}

	prompt := buildComparisonPrompt(strategies)

	resp, err := sa.client.Chat(ctx, ollama.ChatRequest{
		Model: sa.model,
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "You are a quantitative finance expert. Compare trading strategies objectively, highlighting relative strengths, weaknesses, and risk-adjusted performance."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: compare strategies: %w", err)
	}

	return resp.Message.Content, nil
}

func buildStrategyPrompt(name string, metrics map[string]float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyze the trading strategy %q with the following performance metrics:\n\n", name)

	for k, v := range metrics {
		fmt.Fprintf(&b, "- %s: %.6f\n", k, v)
	}

	b.WriteString("\nProvide:\n1. Overall performance assessment\n2. Risk analysis\n3. Strengths and weaknesses\n4. Recommendations for improvement")
	return b.String()
}

func buildComparisonPrompt(strategies map[string]map[string]float64) string {
	var b strings.Builder
	b.WriteString("Compare the following trading strategies:\n\n")

	for name, metrics := range strategies {
		fmt.Fprintf(&b, "Strategy: %s\n", name)
		for k, v := range metrics {
			fmt.Fprintf(&b, "  - %s: %.6f\n", k, v)
		}
		b.WriteString("\n")
	}

	b.WriteString("Provide:\n1. Side-by-side comparison\n2. Risk-adjusted performance ranking\n3. Best strategy for different market conditions\n4. Overall recommendation")
	return b.String()
}
