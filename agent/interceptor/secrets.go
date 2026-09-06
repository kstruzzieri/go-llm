package interceptor

import (
	"context"
	"encoding/json"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

// Secrets blocks supported credentials and payment-card values at every origin.
// Findings contain only kind and target metadata, never matched content.
type Secrets struct{}

var _ agent.Interceptor = Secrets{}

// Name returns "secrets".
func (Secrets) Name() string { return "secrets" }

// InspectInput checks the system prompt, summary, messages, and alternatives.
func (Secrets) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	var findings []agent.Finding
	eachText(in, func(t target, text string) {
		findings = append(findings, scanSecrets(nil, t, text)...)
	})
	return findings, nil
}

// InspectOutput checks content and thinking separately, then every tool call's
// raw and decoded arguments before the response is recorded or published.
func (Secrets) InspectOutput(_ context.Context, out agent.OutputInspection) ([]agent.Finding, error) {
	t := toolCallTarget("")
	t.kind = agent.TargetOutputContent
	findings := scanSecrets(nil, t, out.Content)
	findings = scanSecrets(findings, t, out.Thinking)
	for _, call := range out.ToolCalls {
		t := toolCallTarget(call.ID)
		t.kind = agent.TargetOutputToolCall
		findings = append(findings, inspectSecretArguments(t, call.Function.Arguments)...)
	}
	return findings, nil
}

// InspectToolCall repeats raw and decoded argument checks before planning,
// approval, or invocation.
func (Secrets) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	return inspectSecretArguments(toolCallTarget(call.Call.ID), call.Call.Function.Arguments), nil
}

func inspectSecretArguments(t target, raw json.RawMessage) []agent.Finding {
	var findings []agent.Finding
	walkToolCall(raw, func(text, key string) {
		findings = scanSecrets(findings, t, text)
		if key != "" && secretscan.ScanAssignment(key, text) {
			findings = appendSecretKind(findings, t, secretscan.SecretAssignment)
		}
	})
	return findings
}

func scanSecrets(findings []agent.Finding, t target, text string) []agent.Finding {
	for _, finding := range secretscan.Scan(text) {
		findings = appendSecretKind(findings, t, finding.Kind)
	}
	return findings
}

// appendSecretKind deduplicates within one target, preserving discovery order.
func appendSecretKind(findings []agent.Finding, t target, kind secretscan.Kind) []agent.Finding {
	rule := "sensitive_" + string(kind)
	for _, finding := range findings {
		if finding.Rule == rule {
			return findings
		}
	}
	return append(findings, t.finding(rule, agent.VerdictBlock, 100, "detected "+string(kind)))
}
