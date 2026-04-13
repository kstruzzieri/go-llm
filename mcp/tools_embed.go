package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxBatchSize = 100

// embedArgs are the parameters for the embed tool.
type embedArgs struct {
	Text  string `json:"text"`
	Model string `json:"model,omitempty"`
}

// embedBatchArgs are the parameters for the embed_batch tool.
type embedBatchArgs struct {
	Texts []string `json:"texts"`
	Model string   `json:"model,omitempty"`
}

func (s *Server) registerEmbedTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "embed",
		Description: "Generate an embedding vector for a single text.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":  map[string]any{"type": "string", "description": "Text to embed"},
				"model": map[string]any{"type": "string", "description": "Embedding model (uses configured default if omitted)"},
			},
			"required": []string{"text"},
		},
	}, s.handleEmbed)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "embed_batch",
		Description: fmt.Sprintf("Generate embeddings for multiple texts (max %d).", maxBatchSize),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"texts": map[string]any{
					"type":        "array",
					"description": fmt.Sprintf("Texts to embed (max %d)", maxBatchSize),
					"items":       map[string]any{"type": "string"},
				},
				"model": map[string]any{"type": "string", "description": "Embedding model (uses configured default if omitted)"},
			},
			"required": []string{"texts"},
		},
	}, s.handleEmbedBatch)
}

func (s *Server) handleEmbed(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args embedArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Text == "" {
		return toolError("validation", "text must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "embedding")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	embedding, err := s.client.Embed(ctx, model, args.Text)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	data, err := json.Marshal(embedding)
	if err != nil {
		return toolError("ollama", "marshal embedding: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handleEmbedBatch(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args embedBatchArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if len(args.Texts) == 0 {
		return toolError("validation", "texts must not be empty"), nil
	}
	if len(args.Texts) > maxBatchSize {
		return toolError("validation", "texts exceeds maximum batch size of %d (got %d)", maxBatchSize, len(args.Texts)), nil
	}

	model, err := s.resolveModel(args.Model, "embedding")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	embeddings, err := s.client.EmbedBatch(ctx, model, args.Texts)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	data, err := json.Marshal(embeddings)
	if err != nil {
		return toolError("ollama", "marshal embeddings: %v", err), nil
	}
	return toolResult(string(data)), nil
}
