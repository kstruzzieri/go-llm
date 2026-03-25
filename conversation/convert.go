package conversation

import (
	"encoding/json"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
)

// FromChatMessages converts Ollama messages to provider-agnostic Messages.
// ToolCalls are marshaled to json.RawMessage for shape-preserving storage.
func FromChatMessages(msgs []ollama.ChatMessage) ([]Message, error) {
	result := make([]Message, len(msgs))
	for i, cm := range msgs {
		m := Message{
			Role:       cm.Role,
			Content:    cm.Content,
			ToolName:   cm.ToolName,
			ToolCallID: cm.ToolCallID,
		}
		if len(cm.ToolCalls) > 0 {
			raw, err := json.Marshal(cm.ToolCalls)
			if err != nil {
				return nil, fmt.Errorf("conversation: marshal tool calls at index %d: %w", i, err)
			}
			m.ToolCalls = raw
		}
		result[i] = m
	}
	return result, nil
}

// ToChatMessages converts stored Messages back to Ollama messages.
// ToolCalls are unmarshaled from json.RawMessage back to []ollama.ToolCall.
func ToChatMessages(msgs []Message) ([]ollama.ChatMessage, error) {
	result := make([]ollama.ChatMessage, len(msgs))
	for i, m := range msgs {
		cm := ollama.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
		}
		if len(m.ToolCalls) > 0 {
			if err := json.Unmarshal(m.ToolCalls, &cm.ToolCalls); err != nil {
				return nil, fmt.Errorf("conversation: unmarshal tool calls at index %d: %w", i, err)
			}
		}
		result[i] = cm
	}
	return result, nil
}
