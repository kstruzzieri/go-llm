package mcp

import (
	"context"
	"encoding/json"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/completion"
)

// completionArgs are the parameters for the complete_code tool.
type completionArgs struct {
	Prefix    string `json:"prefix"`
	Suffix    string `json:"suffix"`
	FilePath  string `json:"file_path,omitempty"`
	Model     string `json:"model,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Language  string `json:"language,omitempty"`
}

func (s *Server) registerCompletionTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "complete_code",
		Description: "Fill-in-the-middle code completion. Returns the generated code to insert between prefix and suffix.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prefix":     map[string]any{"type": "string", "description": "Code before the cursor"},
				"suffix":     map[string]any{"type": "string", "description": "Code after the cursor"},
				"file_path":  map[string]any{"type": "string", "description": "File path for language detection"},
				"model":      map[string]any{"type": "string", "description": "Model name (uses configured completion default if omitted)"},
				"max_tokens": map[string]any{"type": "integer", "description": "Maximum tokens to generate (default: 128)"},
				"language":   map[string]any{"type": "string", "description": "Language hint (auto-detected from file_path if omitted)"},
			},
			"required": []string{"prefix", "suffix"},
		},
	}, s.handleCompletion)
}

func (s *Server) handleCompletion(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args completionArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}

	model, err := s.resolveModel(args.Model, "completion")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	// Provider creation is cheap (struct with client ref + model name).
	// Always create per-request to match the resolved/explicit model.
	provider := completion.NewProvider(s.client, model)

	resp, err := provider.Complete(ctx, completion.FIMRequest{
		Prefix:    args.Prefix,
		Suffix:    args.Suffix,
		FilePath:  args.FilePath,
		MaxTokens: args.MaxTokens,
		Language:  args.Language,
	})
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(resp.Completion), nil
}
