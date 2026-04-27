package analysis

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// TrainingMetrics holds ML training metrics for a single epoch or checkpoint.
type TrainingMetrics struct {
	Epoch         int
	Loss          float64
	LossHistory   []float64
	RewardMean    float64
	RewardHistory []float64
	KLDivergence  float64
	LearningRate  float64
	CustomMetrics map[string]float64
}

// AnomalyInfo describes a detected anomaly in training metrics.
type AnomalyInfo struct {
	Type        string             // e.g. "reward_hack", "kl_drift", "loss_spike"
	Severity    string             // "warning" or "critical"
	Description string
	Metrics     map[string]float64
}

// MetricsAnalyzer generates natural-language analysis of ML training metrics.
type MetricsAnalyzer struct {
	chat  ChatFunc
	model string
}

// NewMetricsAnalyzer is the *ollama.Client-backed compat shim.
// New code should prefer NewMetricsAnalyzerWithChat.
func NewMetricsAnalyzer(client *ollama.Client, model string) (*MetricsAnalyzer, error) {
	if client == nil {
		return nil, fmt.Errorf("analysis: new metrics analyzer: client is required")
	}
	if model == "" {
		return nil, fmt.Errorf("analysis: new metrics analyzer: model is required")
	}
	return NewMetricsAnalyzerWithChat(chatFuncFromOllamaClient(client), model)
}

// NewMetricsAnalyzerWithChat builds a MetricsAnalyzer that routes through chat.
// Empty model defers selection to the chat implementation.
func NewMetricsAnalyzerWithChat(chat ChatFunc, model string) (*MetricsAnalyzer, error) {
	if chat == nil {
		return nil, fmt.Errorf("analysis: new metrics analyzer: chat is required")
	}
	return &MetricsAnalyzer{chat: chat, model: model}, nil
}

// AnalyzeTraining generates a natural-language analysis of training metrics,
// including trend detection and recommendations.
func (ma *MetricsAnalyzer) AnalyzeTraining(ctx context.Context, metrics TrainingMetrics) (string, error) {
	if metrics.Epoch < 0 {
		return "", fmt.Errorf("analysis: analyze training: epoch must be non-negative, got %d", metrics.Epoch)
	}
	if metrics.LearningRate < 0 {
		return "", fmt.Errorf("analysis: analyze training: learning rate must be non-negative, got %f", metrics.LearningRate)
	}

	prompt := buildTrainingPrompt(metrics)

	resp, err := ma.chat(ctx, "analysis", provider.ChatRequest{
		Model: ma.model,
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "You are an ML training expert. Analyze training metrics and provide actionable insights about convergence, stability, and potential issues."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: analyze training: %w", err)
	}

	return resp.Content, nil
}

// ExplainAnomaly generates a natural-language explanation and remediation advice
// for a detected training anomaly.
func (ma *MetricsAnalyzer) ExplainAnomaly(ctx context.Context, anomaly AnomalyInfo) (string, error) {
	if anomaly.Type == "" {
		return "", fmt.Errorf("analysis: explain anomaly: type is required")
	}
	if anomaly.Severity == "" {
		return "", fmt.Errorf("analysis: explain anomaly: severity is required")
	}

	prompt := buildAnomalyPrompt(anomaly)

	resp, err := ma.chat(ctx, "analysis", provider.ChatRequest{
		Model: ma.model,
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "You are an ML training expert. Explain training anomalies clearly and suggest concrete remediation steps."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: explain anomaly: %w", err)
	}

	return resp.Content, nil
}

func buildTrainingPrompt(m TrainingMetrics) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Analyze the following ML training metrics at epoch %d:\n\n", m.Epoch)
	fmt.Fprintf(&b, "- Current Loss: %.6f\n", m.Loss)
	fmt.Fprintf(&b, "- Learning Rate: %g\n", m.LearningRate)
	fmt.Fprintf(&b, "- KL Divergence: %.6f\n", m.KLDivergence)
	fmt.Fprintf(&b, "- Mean Reward: %.6f\n", m.RewardMean)

	if len(m.LossHistory) > 0 {
		fmt.Fprintf(&b, "- Loss History (last %d): %v\n", len(m.LossHistory), m.LossHistory)
	}
	if len(m.RewardHistory) > 0 {
		fmt.Fprintf(&b, "- Reward History (last %d): %v\n", len(m.RewardHistory), m.RewardHistory)
	}
	if len(m.CustomMetrics) > 0 {
		b.WriteString("- Custom Metrics:\n")
		for k, v := range m.CustomMetrics {
			fmt.Fprintf(&b, "  - %s: %.6f\n", k, v)
		}
	}

	b.WriteString("\nProvide:\n1. Overall training health assessment\n2. Trend analysis\n3. Potential issues or concerns\n4. Recommendations for next steps")
	return b.String()
}

func buildAnomalyPrompt(a AnomalyInfo) string {
	var b strings.Builder
	b.WriteString("Explain the following training anomaly:\n\n")
	fmt.Fprintf(&b, "- Type: %s\n", a.Type)
	fmt.Fprintf(&b, "- Severity: %s\n", a.Severity)
	fmt.Fprintf(&b, "- Description: %s\n", a.Description)

	if len(a.Metrics) > 0 {
		b.WriteString("- Associated Metrics:\n")
		for k, v := range a.Metrics {
			fmt.Fprintf(&b, "  - %s: %.6f\n", k, v)
		}
	}

	b.WriteString("\nProvide:\n1. What this anomaly means\n2. Likely causes\n3. Impact on training\n4. Recommended remediation steps")
	return b.String()
}
