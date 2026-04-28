package mcp

import (
	"context"
	"encoding/json"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/provider"
)

// generateArgs are the parameters for the generate tool.
type generateArgs struct {
	Prompt      string   `json:"prompt"`
	Model       string   `json:"model,omitempty"`
	System      string   `json:"system,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

func (s *Server) registerGenerateTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "generate",
		Description: "Raw text generation without chat formatting. Returns the generated text.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":      map[string]any{"type": "string", "description": "The prompt to generate from"},
				"model":       map[string]any{"type": "string", "description": "Model name (uses configured chat default if omitted)"},
				"system":      map[string]any{"type": "string", "description": "System prompt"},
				"temperature": map[string]any{"type": "number", "description": "Sampling temperature"},
				"max_tokens":  map[string]any{"type": "integer", "description": "Maximum tokens to generate"},
			},
			"required": []string{"prompt"},
		},
	}, s.handleGenerate)
}

func (s *Server) handleGenerate(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args generateArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Prompt == "" {
		return toolError("validation", "prompt must not be empty"), nil
	}
	router := s.routerSnapshot()
	if router == nil {
		return toolError("config", "router unavailable"), nil
	}

	opts := provider.ModelOptions{}
	if args.Temperature != nil {
		opts.Temperature = args.Temperature
	}
	if args.MaxTokens > 0 {
		opts.NumPredict = args.MaxTokens
	}
	expectedOutput := provider.DefaultExpectedOutput("chat")
	if args.MaxTokens > 0 {
		expectedOutput = args.MaxTokens
	}

	rr := provider.RoutingRequest{
		Model:          args.Model,
		UseCase:        "chat", // generate routes via the chat use-case
		RequiredCaps:   provider.CapGenerate,
		Prompt:         args.Prompt,
		System:         args.System,
		Options:        opts,
		ExpectedOutput: expectedOutput,
		Priority:       provider.PriorityNormal,
	}
	if rr.Model == "" {
		chain, err := s.chainFor("chat")
		if err != nil {
			return toolError("config", "%v", err), nil
		}
		rr.PreferredChain = chain
	}

	plan, err := router.Route(ctx, rr)
	if err != nil {
		return toolError("router", "%v", err), nil
	}
	resp, err := plan.ExecuteGenerate(ctx)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}
	return toolResult(resp.Response), nil
}
