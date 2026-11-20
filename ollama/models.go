package ollama

import (
	"context"
	"encoding/json"
	"fmt"
)

// ListModels returns all models available in the Ollama instance, with the
// digest /api/tags reports for each. A body with no "models" key returns a nil
// slice and nil error; a present key, even null, returns a non-nil slice.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var resp listModelsRaw
	if err := c.doJSON(ctx, "GET", "/api/tags", nil, &resp); err != nil {
		return nil, fmt.Errorf("ollama: list models: %w", err)
	}
	if resp.Models == nil {
		return nil, nil
	}
	var entries []listModelEntry
	if err := json.Unmarshal(resp.Models, &entries); err != nil {
		return nil, fmt.Errorf("ollama: list models: decode response: %w", err)
	}

	models := make([]ModelInfo, len(entries))
	for i, m := range entries {
		models[i] = ModelInfo{
			Name:   m.Name,
			Size:   m.Size,
			Digest: m.Digest,
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
	if name == "" {
		return nil, fmt.Errorf("ollama: show model: model name is required")
	}
	body := struct {
		Name string `json:"name"`
	}{Name: name}

	var resp showModelResponse
	if err := c.doJSON(ctx, "POST", "/api/show", body, &resp); err != nil {
		return nil, fmt.Errorf("ollama: show model %q: %w", name, err)
	}

	return &ModelInfo{
		Name:         name,
		ParamSize:    resp.Details.ParamSize,
		QuantLevel:   resp.Details.QuantLevel,
		Digest:       resp.Digest,
		Family:       resp.Details.Family,
		Template:     resp.Template,
		Capabilities: resp.Capabilities,
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
