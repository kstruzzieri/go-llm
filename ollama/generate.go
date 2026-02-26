package ollama

import (
	"context"
	"encoding/json"
	"fmt"
)

// Generate sends a non-streaming text generation request and returns the full response.
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	req.Stream = false
	var resp GenerateResponse
	if err := c.doJSON(ctx, "POST", "/api/generate", req, &resp); err != nil {
		return nil, fmt.Errorf("ollama: generate: %w", err)
	}
	return &resp, nil
}

// GenerateStream sends a streaming text generation request, calling fn for each chunk.
// The final chunk will have Done=true and include timing statistics.
// If fn returns an error, streaming stops and that error is returned.
func (c *Client) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	req.Stream = true
	return c.doStream(ctx, "/api/generate", req, func(raw json.RawMessage) error {
		var resp GenerateResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("ollama: decode generate chunk: %w", err)
		}
		return fn(resp)
	})
}
