package analysis

import (
	"context"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// ChatFunc is the chat-shaped dependency analysis tools need. The useCase
// string flows to the Router (in router-aware callers) for per-tool weight
// profile selection (e.g. "code-review", "analysis"). For *ollama.Client-
// backed compat shims, useCase is ignored.
type ChatFunc func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error)

// ContextRetriever is the narrow retriever dependency code review needs.
// Satisfied directly by *rag.Retriever — no adapter required.
type ContextRetriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error)
	BuildContext(results []rag.SearchResult, maxTokens int) string
}

// chatFuncFromOllamaClient adapts an *ollama.Client to the analysis.ChatFunc
// signature. The useCase parameter is ignored — direct *ollama.Client calls
// have no router context. Translation mirrors provider.toOllamaChatRequest
// semantics for the subset of fields analysis tools use (no tool calls today).
func chatFuncFromOllamaClient(client *ollama.Client) ChatFunc {
	return func(ctx context.Context, _ string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		oMsgs := make([]ollama.ChatMessage, len(req.Messages))
		for i, m := range req.Messages {
			oMsgs[i] = ollama.ChatMessage{Role: m.Role, Content: m.Content}
		}
		oReq := ollama.ChatRequest{
			Model:    req.Model,
			Messages: oMsgs,
		}
		if req.Options.Temperature != nil || req.Options.NumPredict > 0 {
			oo := &ollama.ModelOptions{NumPredict: req.Options.NumPredict}
			if req.Options.Temperature != nil {
				oo.Temperature = req.Options.Temperature
			}
			oReq.Options = oo
		}
		oResp, err := client.Chat(ctx, oReq)
		if err != nil {
			return nil, err
		}
		return &provider.ChatResponse{
			Model:    oResp.Model,
			Provider: "ollama",
			Content:  oResp.Message.Content,
			Done:     true,
		}, nil
	}
}
