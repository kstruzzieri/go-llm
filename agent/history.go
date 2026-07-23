package agent

import (
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

// cloneChatMessage returns a copy of m that shares no mutable backing storage
// with the caller. Scalar fields copy by value; the ToolCalls slice and each
// nested Arguments buffer are deep-cloned. This locks runtime ownership of
// seeded History so a future tool-chain history child cannot accidentally alias
// caller memory.
func cloneChatMessage(m provider.ChatMessage) provider.ChatMessage {
	m.ToolCalls = cloneToolCalls(m.ToolCalls)
	return m
}

// validateHistory enforces the #200 allowlist: prior turns may be only plain
// user/assistant chat with non-empty content and no tool-call residue. Tool-chain
// history (tool role / assistant ToolCalls) is a future child; rejecting it here
// keeps the seam's contract crisp and prevents partial tool residue from leaking
// into State. System content belongs in Request.System, never in History.
func validateHistory(history []provider.ChatMessage) error {
	for i, m := range history {
		switch m.Role {
		case "user", "assistant":
		default:
			return fmt.Errorf("agent: invalid history: entry %d has role %q, want user or assistant", i, m.Role)
		}
		if m.Content == "" {
			return fmt.Errorf("agent: invalid history: entry %d has empty content", i)
		}
		if len(m.ToolCalls) != 0 {
			return fmt.Errorf("agent: invalid history: entry %d carries tool calls (unsupported)", i)
		}
		if m.ToolName != "" || m.ToolCallID != "" {
			return fmt.Errorf("agent: invalid history: entry %d carries tool metadata (unsupported)", i)
		}
	}
	return nil
}

func cloneToolCalls(in []provider.ToolCall) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := append([]provider.ToolCall(nil), in...)
	for i := range out {
		if len(out[i].Function.Arguments) > 0 {
			out[i].Function.Arguments = append([]byte(nil), out[i].Function.Arguments...)
		}
	}
	return out
}
