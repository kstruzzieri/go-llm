package agent

import "github.com/kstruzzieri/go-llm/provider"

// cloneChatMessage returns a copy of m that shares no mutable backing storage
// with the caller. Scalar fields copy by value; the ToolCalls slice and each
// nested Arguments buffer are deep-cloned. This locks runtime ownership of
// seeded History so a future tool-chain history child cannot accidentally alias
// caller memory.
func cloneChatMessage(m provider.ChatMessage) provider.ChatMessage {
	m.ToolCalls = cloneToolCalls(m.ToolCalls)
	return m
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
