package ollama

import (
	"context"
	"encoding/json"
	"fmt"
)

// Chat sends a non-streaming chat completion request and returns the full response.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("ollama: chat: model name is required")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("ollama: chat: at least one message is required")
	}
	req.Stream = false
	var resp ChatResponse
	if err := c.doJSON(ctx, "POST", "/api/chat", req, &resp); err != nil {
		return nil, fmt.Errorf("ollama: chat: %w", err)
	}
	return &resp, nil
}

// ChatStream sends a streaming chat request, calling fn for each response chunk.
// The final chunk will have Done=true and include timing statistics.
// If fn returns an error, streaming stops and that error is returned.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	if req.Model == "" {
		return fmt.Errorf("ollama: chat stream: model name is required")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("ollama: chat stream: at least one message is required")
	}
	if fn == nil {
		return fmt.Errorf("ollama: chat stream: callback function is required")
	}
	req.Stream = true
	return c.doStream(ctx, "/api/chat", req, func(raw json.RawMessage) error {
		var resp ChatResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("ollama: decode chat chunk: %w", err)
		}
		return fn(resp)
	})
}
