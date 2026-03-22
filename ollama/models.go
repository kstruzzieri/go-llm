package ollama

import (
	"context"
	"encoding/json"
	"fmt"
)

// ListModels returns all models available in the Ollama instance.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var resp listModelsResponse
	if err := c.doJSON(ctx, "GET", "/api/tags", nil, &resp); err != nil {
		return nil, fmt.Errorf("ollama: list models: %w", err)
	}

	models := make([]ModelInfo, len(resp.Models))
	for i, m := range resp.Models {
		models[i] = ModelInfo{
			Name: m.Name,
			Size: m.Size,
		}
	}
	return models, nil
}

// AvailableModels returns the names of all models available in the Ollama instance.
// This method satisfies the config.ModelChecker interface.
func (c *Client) AvailableModels(ctx context.Context) ([]string, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	return names, nil
}

// ShowModel returns detailed information about a specific model.
func (c *Client) ShowModel(ctx context.Context, name string) (*ModelInfo, error) {
	body := struct {
		Name string `json:"name"`
	}{Name: name}

	var resp showModelResponse
	if err := c.doJSON(ctx, "POST", "/api/show", body, &resp); err != nil {
		return nil, fmt.Errorf("ollama: show model %q: %w", name, err)
	}

	return &ModelInfo{
		Name:       name,
		ParamSize:  resp.Details.ParamSize,
		QuantLevel: resp.Details.QuantLevel,
		Digest:     resp.Digest,
	}, nil
}

// PullModel downloads a model from the Ollama registry.
// The fn callback receives progress updates; pass nil to ignore progress.
func (c *Client) PullModel(ctx context.Context, name string, fn func(status string, completed, total int64)) error {
	req := pullModelRequest{
		Name:   name,
		Stream: fn != nil,
	}

	if fn == nil {
		return c.doJSON(ctx, "POST", "/api/pull", req, nil)
	}

	return c.doStream(ctx, "/api/pull", req, func(raw json.RawMessage) error {
		var resp pullModelResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("ollama: decode pull progress: %w", err)
		}
		fn(resp.Status, resp.Completed, resp.Total)
		return nil
	})
}
