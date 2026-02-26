package ollama

import (
	"context"
	"fmt"
)

// Embed generates an embedding vector for a single text using the specified model.
func (c *Client) Embed(ctx context.Context, model string, text string) ([]float64, error) {
	req := EmbedRequest{
		Model: model,
		Input: text,
	}
	var resp EmbedResponse
	if err := c.doJSON(ctx, "POST", "/api/embed", req, &resp); err != nil {
		return nil, fmt.Errorf("ollama: embed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama: embed: no embeddings returned")
	}
	return resp.Embeddings[0], nil
}

// EmbedBatch generates embeddings for multiple texts by calling Embed for each one.
// This is suitable for batch indexing where individual failures should not block the rest.
func (c *Client) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float64, error) {
	results := make([][]float64, 0, len(texts))
	for _, text := range texts {
		emb, err := c.Embed(ctx, model, text)
		if err != nil {
			return nil, fmt.Errorf("ollama: embed batch: %w", err)
		}
		results = append(results, emb)
	}
	return results, nil
}
