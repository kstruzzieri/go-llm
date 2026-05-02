// Package analysis provides domain-specific LLM analysis helpers for code review,
// code explanation, ML training metrics, and trading strategy analysis.
package analysis

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// reviewConfig holds optional configuration for a code review request.
type reviewConfig struct {
	language string
	focus    string
}

// ReviewOption configures a code review request.
type ReviewOption func(*reviewConfig)

// WithLanguage sets the programming language for more targeted review feedback.
func WithLanguage(lang string) ReviewOption {
	return func(cfg *reviewConfig) {
		cfg.language = lang
	}
}

// WithFocus narrows the review to a specific area such as "security", "performance",
// or "error handling".
func WithFocus(focus string) ReviewOption {
	return func(cfg *reviewConfig) {
		cfg.focus = focus
	}
}

// CodeReviewer generates code reviews using an LLM with optional RAG context.
type CodeReviewer struct {
	chat      ChatFunc
	retriever ContextRetriever
	model     string
}

// NewCodeReviewer is the *ollama.Client-backed compat shim. Existing callers
// (Firn IDE, Flux ML, Quantum Trader) continue to use this constructor with
// no behavior change. New code should prefer NewCodeReviewerWithChat.
func NewCodeReviewer(client *ollama.Client, retriever *rag.Retriever, model string) (*CodeReviewer, error) {
	if client == nil {
		return nil, fmt.Errorf("analysis: new code reviewer: client is required")
	}
	if model == "" {
		return nil, fmt.Errorf("analysis: new code reviewer: model is required")
	}
	// Pass retriever as ContextRetriever; *rag.Retriever satisfies it
	// structurally. nil *rag.Retriever stays nil through the conversion.
	var r ContextRetriever
	if retriever != nil {
		r = retriever
	}
	return NewCodeReviewerWithChat(chatFuncFromOllamaClient(client), r, model)
}

// NewCodeReviewerWithChat builds a CodeReviewer that routes through chat.
// The model parameter may be empty; an empty model defers selection to the
// chat implementation (typically PreferredChain + Recommend in the Router).
func NewCodeReviewerWithChat(chat ChatFunc, retriever ContextRetriever, model string) (*CodeReviewer, error) {
	if chat == nil {
		return nil, fmt.Errorf("analysis: new code reviewer: chat is required")
	}
	return &CodeReviewer{chat: chat, retriever: retriever, model: model}, nil
}

// Review performs a code review on the provided code and returns the review as text.
// Options can specify language and focus area. If a retriever is configured, relevant
// codebase context is included in the prompt.
func (cr *CodeReviewer) Review(ctx context.Context, code string, opts ...ReviewOption) (string, error) {
	if code == "" {
		return "", fmt.Errorf("analysis: review code: code is required")
	}

	cfg := &reviewConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	prompt := cr.buildReviewPrompt(ctx, code, cfg)

	resp, err := cr.chat(ctx, "code-review", provider.ChatRequest{
		Model: cr.model,
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "You are an expert code reviewer. Provide clear, actionable feedback."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: review code: %w", err)
	}

	return resp.Content, nil
}

// buildReviewPrompt constructs the review prompt with optional language, focus, and RAG context.
func (cr *CodeReviewer) buildReviewPrompt(ctx context.Context, code string, cfg *reviewConfig) string {
	var b strings.Builder

	// Include RAG context if available
	if cr.retriever != nil {
		results, err := cr.retriever.Retrieve(ctx, code, 5)
		if err == nil && len(results) > 0 {
			ragContext := cr.retriever.BuildContext(results, 2048)
			if ragContext != "" {
				b.WriteString(ragContext)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("Please review the following code")
	if cfg.language != "" {
		fmt.Fprintf(&b, " (language: %s)", cfg.language)
	}
	if cfg.focus != "" {
		fmt.Fprintf(&b, " with a focus on %s", cfg.focus)
	}
	b.WriteString(":\n\n```\n")
	b.WriteString(code)
	b.WriteString("\n```\n\nProvide:\n1. Issues found (bugs, style, potential problems)\n2. Suggestions for improvement\n3. Positive aspects of the code")

	return b.String()
}
