package analysis

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Explain generates a natural-language explanation of the provided code snippet
// using the specified LLM model.
func Explain(ctx context.Context, client *ollama.Client, model string, code string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("analysis: explain: client is required")
	}
	if model == "" {
		return "", fmt.Errorf("analysis: explain: model is required")
	}
	if code == "" {
		return "", fmt.Errorf("analysis: explain: code is required")
	}

	prompt := "Explain the following code clearly and concisely. " +
		"Describe what it does, how it works, and any notable patterns or techniques used:\n\n```\n" +
		code + "\n```"

	resp, err := client.Chat(ctx, ollama.ChatRequest{
		Model: model,
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "You are an expert programmer. Explain code clearly for developers of all skill levels."},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("analysis: explain: %w", err)
	}

	return resp.Message.Content, nil
}
