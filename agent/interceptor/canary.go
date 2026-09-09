package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

var errInvalidCanaryNonce = errors.New("interceptor: canary nonce must be 64 lowercase hexadecimal characters")

// Canary is an immutable detector that emits abort findings when its nonce
// appears in inspected text. The caller owns nonce minting, planting in the
// system instructions, and rotation.
//
// It matches complete nonce substrings, including ASCII hexadecimal case
// variants. Only tool arguments additionally receive JSON decoding of keys and
// string values. Collected-output checks do not suppress streaming content that
// has already been emitted.
type Canary struct {
	nonce string
}

var _ agent.Interceptor = Canary{}

// NewCanary validates and freezes a nonce of exactly 64 lowercase ASCII
// hexadecimal characters, representing 32 bytes.
func NewCanary(nonce string) (Canary, error) {
	if len(nonce) != 64 {
		return Canary{}, errInvalidCanaryNonce
	}
	for i := range nonce {
		b := nonce[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return Canary{}, errInvalidCanaryNonce
		}
	}
	return Canary{nonce: nonce}, nil
}

// Name returns "canary".
func (Canary) Name() string { return "canary" }

// InspectInput checks the summary, message content, and alternative content.
// System is the nonce's authorized location and is deliberately skipped.
func (c Canary) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	if c.nonce == "" {
		return nil, errInvalidCanaryNonce
	}
	if c.matches(in.Summary) {
		return []agent.Finding{newCanaryFinding("canary_in_input", "canary detected in input", agent.TargetNone)}, nil
	}
	for _, message := range in.Messages {
		if c.matches(message.Content) {
			return []agent.Finding{newCanaryFinding("canary_in_input", "canary detected in input", agent.TargetNone)}, nil
		}
		for _, alternative := range message.Alternatives {
			if c.matches(alternative.Content) {
				return []agent.Finding{newCanaryFinding("canary_in_input", "canary detected in input", agent.TargetNone)}, nil
			}
		}
	}
	return nil, nil
}

// InspectOutput checks collected content, thinking, tool arguments, and tool
// call IDs, types, and function names before the response is recorded or
// dispatched.
func (c Canary) InspectOutput(_ context.Context, out agent.OutputInspection) ([]agent.Finding, error) {
	if c.nonce == "" {
		return nil, errInvalidCanaryNonce
	}
	var findings []agent.Finding
	if c.matches(out.Content) {
		findings = append(findings, newCanaryFinding("canary_in_content", "canary detected in content", agent.TargetOutputContent))
	}
	if c.matches(out.Thinking) {
		findings = append(findings, newCanaryFinding("canary_in_thinking", "canary detected in thinking", agent.TargetOutputContent))
	}

	var arguments, metadata bool
	for _, call := range out.ToolCalls {
		arguments = arguments || c.matchesToolArguments(call.Function.Arguments)
		metadata = metadata || c.matches(call.ID) || c.matches(call.Type) || c.matches(call.Function.Name)
	}
	if arguments {
		findings = append(findings, newCanaryFinding("canary_in_tool_arguments", "canary detected in tool arguments", agent.TargetNone))
	}
	if metadata {
		findings = append(findings, newCanaryFinding("canary_in_tool_metadata", "canary detected in tool metadata", agent.TargetNone))
	}
	return findings, nil
}

// InspectToolCall repeats raw and JSON-decoded argument checks before dispatch.
// It checks arguments only; InspectOutput checks tool call metadata.
func (c Canary) InspectToolCall(_ context.Context, call agent.ToolCallInspection) ([]agent.Finding, error) {
	if c.nonce == "" {
		return nil, errInvalidCanaryNonce
	}
	if !c.matchesToolArguments(call.Call.Function.Arguments) {
		return nil, nil
	}
	return []agent.Finding{newCanaryFinding("canary_in_tool_arguments", "canary detected in tool arguments", agent.TargetNone)}, nil
}

func newCanaryFinding(rule, detail string, target agent.TargetKind) agent.Finding {
	return agent.Finding{
		Rule: rule, Verdict: agent.VerdictAbort, Risk: 100, Detail: detail,
		Target: target, StateIndex: -1, Group: -1, Alternative: -1,
	}
}

func (c Canary) matchesToolArguments(raw json.RawMessage) bool {
	found := false
	walkToolCall(raw, func(text, _ string) {
		found = found || c.matches(text)
	})
	return found
}

func (c Canary) matches(text string) bool {
	return c.nonce != "" && strings.Contains(strings.ToLower(text), c.nonce)
}
