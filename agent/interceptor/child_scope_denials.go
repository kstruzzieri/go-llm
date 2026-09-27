package interceptor

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/agent"
)

// ChildScopeDenialRisk is the score per refused request, capped at ten requests
// per child. A refusal is policy telemetry, not proof of malicious intent.
const ChildScopeDenialRisk = 10

// ChildScopeDenials reports native scoped-child refusals at parent ingress.
// It does not infer evidence from content or change filesystem enforcement.
type ChildScopeDenials struct{}

// Name returns "child_scope_denials".
func (ChildScopeDenials) Name() string { return "child_scope_denials" }

// InspectInput emits one informational finding per affected dispatch task.
func (ChildScopeDenials) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	var findings []agent.Finding
	for _, m := range in.Messages {
		for _, d := range m.ChildScopeDenials {
			if d.Task < 0 || d.Requests <= 0 {
				continue
			}
			t := target{kind: agent.TargetMessage, origin: m.Origin, stateIndex: m.StateIndex, group: -1, alt: -1, toolCallID: m.ToolCallID}
			findings = append(findings, t.finding("child_scope_denied", agent.VerdictAllow,
				ChildScopeDenialRisk*int(min(d.Requests, 10)),
				fmt.Sprintf("dispatch task %d: %d request(s) denied by workspace policy", d.Task+1, d.Requests)))
		}
	}
	return findings, nil
}

// InspectOutput returns no findings; model text cannot supply native evidence.
func (ChildScopeDenials) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

// InspectToolCall returns no findings; arguments cannot supply native evidence.
func (ChildScopeDenials) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}
