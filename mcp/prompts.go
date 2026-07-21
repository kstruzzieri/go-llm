package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/rag"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerPrompts() {
	s.mcpServer.AddPrompt(&gomcp.Prompt{
		Name:        "code-review",
		Description: "Structured code review prompt for the client's LLM",
		Arguments: []*gomcp.PromptArgument{
			{Name: "code", Description: "Code to review", Required: true},
			{Name: "language", Description: "Programming language"},
			{Name: "focus", Description: "Focus area (e.g. security, performance)"},
		},
	}, s.handleCodeReviewPrompt)

	s.mcpServer.AddPrompt(&gomcp.Prompt{
		Name:        "explain",
		Description: "Code explanation prompt at adjustable depth",
		Arguments: []*gomcp.PromptArgument{
			{Name: "code", Description: "Code to explain", Required: true},
			{Name: "depth", Description: "Explanation depth: brief or detailed (default: detailed)"},
		},
	}, s.handleExplainPrompt)

	s.mcpServer.AddPrompt(&gomcp.Prompt{
		Name:        "rag-query",
		Description: "Ask a question with automatic RAG context from the codebase",
		Arguments: []*gomcp.PromptArgument{
			{Name: "question", Description: "Question to answer", Required: true},
			{Name: "top_k", Description: "Number of context chunks (default: 5)"},
		},
	}, s.handleRAGQueryPrompt)

	s.mcpServer.AddPrompt(&gomcp.Prompt{
		Name:        "refactor",
		Description: "Suggest refactoring with constraints",
		Arguments: []*gomcp.PromptArgument{
			{Name: "code", Description: "Code to refactor", Required: true},
			{Name: "goal", Description: "Refactoring goal", Required: true},
			{Name: "language", Description: "Programming language"},
		},
	}, s.handleRefactorPrompt)
}

func (s *Server) handleCodeReviewPrompt(_ context.Context, req *gomcp.GetPromptRequest) (*gomcp.GetPromptResult, error) {
	code := req.Params.Arguments["code"]
	if code == "" {
		return nil, fmt.Errorf("code argument is required")
	}

	lang := req.Params.Arguments["language"]
	focus := req.Params.Arguments["focus"]

	systemText := "You are an expert code reviewer. Provide a thorough code review covering correctness, performance, security, and readability."
	if lang != "" {
		systemText += fmt.Sprintf(" The code is written in %s.", lang)
	}
	if focus != "" {
		systemText += fmt.Sprintf(" Focus especially on: %s.", focus)
	}

	return &gomcp.GetPromptResult{
		Description: "Code review prompt",
		Messages: []*gomcp.PromptMessage{
			{Role: "assistant", Content: &gomcp.TextContent{Text: systemText}},
			{Role: "user", Content: &gomcp.TextContent{Text: fmt.Sprintf("Please review this code:\n\n```\n%s\n```", code)}},
		},
	}, nil
}

func (s *Server) handleExplainPrompt(_ context.Context, req *gomcp.GetPromptRequest) (*gomcp.GetPromptResult, error) {
	code := req.Params.Arguments["code"]
	if code == "" {
		return nil, fmt.Errorf("code argument is required")
	}

	depth := req.Params.Arguments["depth"]
	if depth == "" {
		depth = "detailed"
	}

	var systemText string
	if depth == "brief" {
		systemText = "You are a concise code explainer. Provide a brief, high-level explanation of what the code does in 2-3 sentences."
	} else {
		systemText = "You are a thorough code explainer. Provide a detailed explanation covering what the code does, how it works, key algorithms or patterns used, and any notable design decisions."
	}

	return &gomcp.GetPromptResult{
		Description: "Code explanation prompt",
		Messages: []*gomcp.PromptMessage{
			{Role: "assistant", Content: &gomcp.TextContent{Text: systemText}},
			{Role: "user", Content: &gomcp.TextContent{Text: fmt.Sprintf("Please explain this code:\n\n```\n%s\n```", code)}},
		},
	}, nil
}

func (s *Server) handleRAGQueryPrompt(ctx context.Context, req *gomcp.GetPromptRequest) (*gomcp.GetPromptResult, error) {
	question := req.Params.Arguments["question"]
	if question == "" {
		return nil, fmt.Errorf("question argument is required")
	}

	if s.ragDisabled {
		return nil, fmt.Errorf("RAG is disabled on this server")
	}

	s.mu.RLock()
	retriever := s.retriever
	s.mu.RUnlock()

	if retriever == nil {
		return nil, fmt.Errorf("RAG retriever unavailable; embedding model may not be resolved")
	}

	topK := 5
	if tk := req.Params.Arguments["top_k"]; tk != "" {
		// Parse top_k from string arguments.
		var n int
		if _, err := fmt.Sscanf(tk, "%d", &n); err == nil && n > 0 {
			topK = n
		}
	}

	policy, _, err := s.retrievalPolicyRequest(ctx, req)
	if err != nil {
		if errors.Is(err, errRetrievalPolicyIdentity) {
			return nil, errors.New("policy_identity_failed: retrieval identity resolution failed")
		}
		return nil, errors.New("validation: invalid retrieval policy metadata")
	}
	response, err := retriever.RetrieveRequest(ctx, rag.RetrievalRequest{
		Query: question, K: topK, Policy: policy,
	})
	if err != nil {
		if safe := retrievalPolicyPromptError(response.Policy, err); safe != nil {
			return nil, safe
		}
		return nil, fmt.Errorf("rag: retrieve: %w", err)
	}
	results := flattenRetrievalResults(response.Results, true)

	contextText := "No relevant context found in the codebase."
	if len(results) > 0 {
		contextText = retriever.BuildContext(results, ragContextMaxTokens)
	}

	return &gomcp.GetPromptResult{
		Meta:        retrievalPolicyMeta(response.Policy),
		Description: "Question with RAG context",
		Messages: []*gomcp.PromptMessage{
			{Role: "assistant", Content: &gomcp.TextContent{Text: fmt.Sprintf("Use the following context from the codebase to answer the question. If the context is not relevant, say so.\n\n%s", contextText)}},
			{Role: "user", Content: &gomcp.TextContent{Text: question}},
		},
	}, nil
}

func retrievalPolicyPromptError(outcome rag.RetrievalPolicyOutcome, err error) error {
	code, message, ok := retrievalPolicyError(outcome, err)
	if !ok {
		return nil
	}
	return fmt.Errorf("%s: %s", code, message)
}

func (s *Server) handleRefactorPrompt(_ context.Context, req *gomcp.GetPromptRequest) (*gomcp.GetPromptResult, error) {
	code := req.Params.Arguments["code"]
	if code == "" {
		return nil, fmt.Errorf("code argument is required")
	}
	goal := req.Params.Arguments["goal"]
	if goal == "" {
		return nil, fmt.Errorf("goal argument is required")
	}

	lang := req.Params.Arguments["language"]

	systemText := "You are an expert software engineer specializing in refactoring. Suggest concrete refactoring changes with clear explanations. Show the refactored code."
	if lang != "" {
		systemText += fmt.Sprintf(" The code is written in %s.", lang)
	}

	return &gomcp.GetPromptResult{
		Description: "Refactoring prompt",
		Messages: []*gomcp.PromptMessage{
			{Role: "assistant", Content: &gomcp.TextContent{Text: systemText}},
			{Role: "user", Content: &gomcp.TextContent{Text: fmt.Sprintf("Please refactor this code with the goal: %s\n\n```\n%s\n```", goal, code)}},
		},
	}, nil
}
