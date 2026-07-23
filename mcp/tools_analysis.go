package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/analysis"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// useCaseToConfigRole maps router use-case names back to config Defaults keys.
// "code-review" and "analysis" both consult the "analysis" config role.
func useCaseToConfigRole(useCase string) string {
	switch useCase {
	case "code-review", "analysis":
		return "analysis"
	case "embedding":
		return "embedding"
	case config.UseCaseVerify, config.UseCaseExtract:
		// Pass through; chainFor -> RoleFallbackChain -> RoleForUseCase resolves
		// the side-task fallback (analysis -> chat) configured in #60.
		return useCase
	default:
		return "chat"
	}
}

// analysisChatFunc builds the analysis.ChatFunc closure that routes analysis
// tool requests through the Router. Caller supplies the explicit pin (or "")
// via req.Model; chain selection from config happens here.
func (s *Server) analysisChatFunc() analysis.ChatFunc {
	return func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		router := s.routerSnapshot()
		if router == nil {
			return nil, fmt.Errorf("mcp: router unavailable")
		}
		caps := provider.CapChat
		if len(req.Tools) > 0 {
			caps |= provider.CapToolCall
		}
		rr := provider.RoutingRequest{
			Model:          req.Model,
			UseCase:        useCase,
			RequiredCaps:   caps,
			Messages:       req.Messages,
			Options:        req.Options,
			Tools:          req.Tools,
			ExpectedOutput: provider.DefaultExpectedOutput(useCase),
			Priority:       provider.PriorityNormal,
		}
		if rr.Model == "" {
			chain, err := s.chainFor(useCaseToConfigRole(useCase))
			if err != nil {
				return nil, err
			}
			rr.PreferredChain = chain
		}
		plan, err := router.Route(ctx, rr)
		if err != nil {
			return nil, err
		}
		return plan.ExecuteChat(ctx)
	}
}

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

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "verify_support",
		Description: "Judge whether an answer is supported by retrieved evidence. Retrieves context for the question, extracts the answer's claims, verifies each against the evidence, and returns a machine-readable support report.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answer":   map[string]any{"type": "string", "description": "The answer to verify against retrieved evidence"},
				"question": map[string]any{"type": "string", "description": "The question used to retrieve supporting evidence"},
				"top_k":    map[string]any{"type": "integer", "description": "Number of chunks to retrieve (default: 5)"},
				"model":    map[string]any{"type": "string", "description": "Model name (defers to the configured verify/extract roles if omitted)"},
			},
			"required": []string{"answer", "question"},
		},
	}, s.handleVerifySupport)
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

	s.mu.RLock()
	retriever := s.retriever
	s.mu.RUnlock()

	var ctxRetriever analysis.ContextRetriever
	if retriever != nil {
		ctxRetriever = retriever
	}

	reviewer, err := analysis.NewCodeReviewerWithChat(s.analysisChatFunc(), ctxRetriever, args.Model)
	if err != nil {
		return toolError("config", "%v", err), nil
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

	result, err := analysis.ExplainWithChat(ctx, s.analysisChatFunc(), args.Model, args.Code)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleAnalyzeTraining(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args struct {
		Metrics json.RawMessage `json:"metrics"`
		Model   string          `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}

	metrics, err := decodeTrainingMetrics(args.Metrics)
	if err != nil {
		return toolError("validation", "%v", err), nil
	}

	analyzer, err := analysis.NewMetricsAnalyzerWithChat(s.analysisChatFunc(), args.Model)
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	result, err := analyzer.AnalyzeTraining(ctx, metrics)
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

	analyzer, err := analysis.NewMetricsAnalyzerWithChat(s.analysisChatFunc(), args.Model)
	if err != nil {
		return toolError("config", "%v", err), nil
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

	analyzer, err := analysis.NewStrategyAnalyzerWithChat(s.analysisChatFunc(), args.Model)
	if err != nil {
		return toolError("config", "%v", err), nil
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
	if len(args.Strategies) < 2 {
		return toolError("validation", "at least 2 strategies are required"), nil
	}

	analyzer, err := analysis.NewStrategyAnalyzerWithChat(s.analysisChatFunc(), args.Model)
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	result, err := analyzer.CompareStrategies(ctx, args.Strategies)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(result), nil
}

func (s *Server) handleVerifySupport(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	var args struct {
		Answer   string `json:"answer"`
		Question string `json:"question"`
		TopK     int    `json:"top_k,omitempty"`
		Model    string `json:"model,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Answer == "" {
		return toolError("validation", "answer must not be empty"), nil
	}
	if args.Question == "" {
		return toolError("validation", "question must not be empty"), nil
	}

	s.mu.RLock()
	retriever := s.retriever
	s.mu.RUnlock()
	if retriever == nil {
		return toolError("rag", "retriever unavailable; embedding model may not be resolved"), nil
	}

	topK := args.TopK
	if topK <= 0 {
		topK = 5
	}
	policy, _, err := s.retrievalPolicyRequest(ctx, req)
	if err != nil {
		return retrievalPolicyRequestError(err), nil
	}
	response, err := retriever.RetrieveRequest(ctx, rag.RetrievalRequest{
		Query: args.Question, K: topK, Policy: policy,
	})
	if err != nil {
		if result := retrievalPolicyToolError(response.Policy, err); result != nil {
			return result, nil
		}
		return toolError("rag", "retrieve: %v", err), nil
	}
	evidence := flattenRetrievalResults(response.Results, true)

	judge, err := analysis.NewSupportJudgeWithChat(s.analysisChatFunc(), args.Model)
	if err != nil {
		return withRetrievalPolicyMeta(toolError("config", "%v", err), response.Policy), nil
	}
	report, err := judge.Judge(ctx, args.Answer, evidence)
	if err != nil {
		return withRetrievalPolicyMeta(toolError("analysis", "%v", err), response.Policy), nil
	}
	data, err := json.Marshal(report)
	if err != nil {
		return withRetrievalPolicyMeta(toolError("analysis", "marshal report: %v", err), response.Policy), nil
	}
	return withRetrievalPolicyMeta(toolResult(string(data)), response.Policy), nil
}

func decodeTrainingMetrics(raw json.RawMessage) (analysis.TrainingMetrics, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return analysis.TrainingMetrics{}, errMetricsRequired
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return analysis.TrainingMetrics{}, err
	}
	if len(fields) == 0 {
		return analysis.TrainingMetrics{}, errEmptyMetrics
	}

	var metrics analysis.TrainingMetrics
	recognized := false

	for _, field := range []struct {
		aliases []string
		dest    any
	}{
		{aliases: []string{"epoch", "Epoch"}, dest: &metrics.Epoch},
		{aliases: []string{"loss", "Loss"}, dest: &metrics.Loss},
		{aliases: []string{"loss_history", "LossHistory"}, dest: &metrics.LossHistory},
		{aliases: []string{"reward_mean", "RewardMean"}, dest: &metrics.RewardMean},
		{aliases: []string{"reward_history", "RewardHistory"}, dest: &metrics.RewardHistory},
		{aliases: []string{"kl_divergence", "KLDivergence"}, dest: &metrics.KLDivergence},
		{aliases: []string{"learning_rate", "LearningRate"}, dest: &metrics.LearningRate},
		{aliases: []string{"custom_metrics", "CustomMetrics"}, dest: &metrics.CustomMetrics},
	} {
		matched, err := unmarshalAliasedJSONField(fields, field.aliases, field.dest)
		if err != nil {
			return analysis.TrainingMetrics{}, err
		}
		recognized = recognized || matched
	}

	if !recognized {
		return analysis.TrainingMetrics{}, errEmptyMetrics
	}

	return metrics, nil
}

var (
	errMetricsRequired = errors.New("metrics are required")
	errEmptyMetrics    = errors.New("metrics must not be empty")
)

func unmarshalAliasedJSONField(fields map[string]json.RawMessage, aliases []string, dest any) (bool, error) {
	for _, alias := range aliases {
		raw, ok := fields[alias]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, dest); err != nil {
			return true, err
		}
		return true, nil
	}
	return false, nil
}
