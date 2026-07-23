package analysis

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// ExplainWithChat returns an explanation of the provided code using the chat
// dependency. model may be empty; an empty model defers selection to the
// chat implementation (typically PreferredChain + Recommend in the Router).
func ExplainWithChat(ctx context.Context, chat ChatFunc, model, code string) (string, error) {
	if chat == nil {
		return "", fmt.Errorf("analysis: explain: chat is required")
	}
	if code == "" {
		return "", fmt.Errorf("analysis: explain: code is required")
	}

	prompt := "Explain the following code clearly and concisely. " +
		"Describe what it does, how it works, and any notable patterns or techniques used:\n\n```\n" +
		code + "\n```"

	resp, err := chat(ctx, "analysis", provider.ChatRequest{
		Model: model,
		Messages: []provider.ChatMessage{
			{Role: "system", Content: "You are an expert programmer. Explain code clearly for developers of all skill levels."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: explain: %w", err)
	}

	return resp.Content, nil
}

// Explain is the *ollama.Client-backed compat shim. New code should prefer
// ExplainWithChat.
func Explain(ctx context.Context, client *ollama.Client, model string, code string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("analysis: explain: client is required")
	}
	if model == "" {
		return "", fmt.Errorf("analysis: explain: model is required")
	}
	return ExplainWithChat(ctx, chatFuncFromOllamaClient(client), model, code)
}
