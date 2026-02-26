package ollama

import (
	"context"
	"fmt"
)

// Embed generates an embedding vector for a single text using the specified model.
func (c *Client) Embed(ctx context.Context, model string, text string) ([]float64, error) {
	if model == "" {
		return nil, fmt.Errorf("ollama: embed: model name is required")
	}
	if text == "" {
		return nil, fmt.Errorf("ollama: embed: text is required")
	}

	req := EmbedRequest{
		Model: model,
		Input: text,
	}
	var resp EmbedResponse
	if err := c.doJSON(ctx, "POST", "/api/embed", req, &resp); err != nil {
		return nil, fmt.Errorf("ollama: embed: %w", err)
	}
	if len(resp.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama: embed: server returned no embeddings for model %q", model)
	}
	return resp.Embeddings[0], nil
}

// EmbedBatch generates embeddings for multiple texts in batches.
// Uses the Ollama batch API where possible, falling back to sequential requests.
func (c *Client) EmbedBatch(ctx context.Context, model string, texts []string) ([][]float64, error) {
	if model == "" {
		return nil, fmt.Errorf("ollama: embed batch: model name is required")
	}
	if len(texts) == 0 {
		return nil, nil
	}

	// Process in batches of 32 for efficiency
	const batchSize = 32
	results := make([][]float64, 0, len(texts))

	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[i:end]

		for _, text := range batch {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("ollama: embed batch: %w", ctx.Err())
			}
			emb, err := c.Embed(ctx, model, text)
			if err != nil {
				return nil, fmt.Errorf("ollama: embed batch item %d: %w", i, err)
			}
			results = append(results, emb)
		}
	}
	return results, nil
}
