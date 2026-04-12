package mcp

import (
	"context"
	"encoding/json"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/analysis"
)

func (s *Server) registerAnalysisTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "code_review",
		Description: "Review code using the local LLM with optional RAG context from the codebase.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code":     map[string]any{"type": "string", "description": "Code to review"},
				"language": map[string]any{"type": "string", "description": "Programming language"},
				"focus":    map[string]any{"type": "string", "description": "Focus area (e.g. security, performance)"},
				"model":    map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"code"},
		},
	}, s.handleCodeReview)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "explain_code",
		Description: "Explain a code snippet using the local LLM.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"code":  map[string]any{"type": "string", "description": "Code to explain"},
				"model": map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"code"},
		},
	}, s.handleExplainCode)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "analyze_training",
		Description: "Analyze ML training metrics and provide insights.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"metrics": map[string]any{"type": "object", "description": "TrainingMetrics JSON (epoch, loss, loss_history, reward_mean, etc.)"},
				"model":   map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"metrics"},
		},
	}, s.handleAnalyzeTraining)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "explain_anomaly",
		Description: "Explain a detected ML training anomaly.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"anomaly": map[string]any{"type": "object", "description": "AnomalyInfo JSON (type, severity, description, metrics)"},
				"model":   map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"anomaly"},
		},
	}, s.handleExplainAnomaly)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "analyze_strategy",
		Description: "Analyze a trading strategy's performance metrics.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":    map[string]any{"type": "string", "description": "Strategy name"},
				"metrics": map[string]any{"type": "object", "description": "Strategy metrics (e.g. sharpe_ratio, max_drawdown)"},
				"model":   map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"name", "metrics"},
		},
	}, s.handleAnalyzeStrategy)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "compare_strategies",
		Description: "Compare multiple trading strategies side by side.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"strategies": map[string]any{"type": "object", "description": "Map of strategy name to metrics object"},
				"model":      map[string]any{"type": "string", "description": "Model name (uses configured analysis default if omitted)"},
			},
			"required": []string{"strategies"},
		},
	}, s.handleCompareStrategies)
}

func (s *Server) handleCodeReview(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Code     string `json:"code"`
		Language string `json:"language,omitempty"`
		Focus    string `json:"focus,omitempty"`
		Model    string `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Code == "" {
		return toolError("validation", "code must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	// CodeReviewer accepts a nil retriever (no RAG context).
	s.mu.RLock()
	retriever := s.retriever
	s.mu.RUnlock()

	reviewer, err := analysis.NewCodeReviewer(s.client, retriever, model)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	var opts []analysis.ReviewOption
	if args.Language != "" {
		opts = append(opts, analysis.WithLanguage(args.Language))
	}
	if args.Focus != "" {
		opts = append(opts, analysis.WithFocus(args.Focus))
	}

	result, err := reviewer.Review(ctx, args.Code, opts...)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleExplainCode(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Code  string `json:"code"`
		Model string `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Code == "" {
		return toolError("validation", "code must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	result, err := analysis.Explain(ctx, s.client, model, args.Code)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleAnalyzeTraining(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Metrics analysis.TrainingMetrics `json:"metrics"`
		Model   string                   `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	analyzer, err := analysis.NewMetricsAnalyzer(s.client, model)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	result, err := analyzer.AnalyzeTraining(ctx, args.Metrics)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleExplainAnomaly(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Anomaly analysis.AnomalyInfo `json:"anomaly"`
		Model   string               `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	analyzer, err := analysis.NewMetricsAnalyzer(s.client, model)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	result, err := analyzer.ExplainAnomaly(ctx, args.Anomaly)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleAnalyzeStrategy(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Name    string             `json:"name"`
		Metrics map[string]float64 `json:"metrics"`
		Model   string             `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Name == "" {
		return toolError("validation", "name must not be empty"), nil
	}
	if len(args.Metrics) == 0 {
		return toolError("validation", "metrics must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	analyzer, err := analysis.NewStrategyAnalyzer(s.client, model)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	result, err := analyzer.AnalyzeStrategy(ctx, args.Name, args.Metrics)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleCompareStrategies(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Strategies map[string]map[string]float64 `json:"strategies"`
		Model      string                        `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if len(args.Strategies) == 0 {
		return toolError("validation", "strategies must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "analysis")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	analyzer, err := analysis.NewStrategyAnalyzer(s.client, model)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	result, err := analyzer.CompareStrategies(ctx, args.Strategies)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}
