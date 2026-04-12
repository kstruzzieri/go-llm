package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
)

// chatArgs are the parameters for the chat tool.
type chatArgs struct {
	Messages    []ollama.ChatMessage `json:"messages"`
	Model       string               `json:"model,omitempty"`
	UseRAG      bool                 `json:"use_rag,omitempty"`
	RAGTopK     int                  `json:"rag_top_k,omitempty"`
	Temperature *float64             `json:"temperature,omitempty"`
}

func (s *Server) registerChatTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "chat",
		Description: "Chat completion with optional RAG context. Returns the assistant's response.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{
					"type":        "array",
					"description": "Chat messages (each with role and content)",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":    map[string]any{"type": "string", "enum": []string{"system", "user", "assistant"}},
							"content": map[string]any{"type": "string"},
						},
						"required": []string{"role", "content"},
					},
				},
				"model":       map[string]any{"type": "string", "description": "Model name (uses configured default if omitted)"},
				"use_rag":     map[string]any{"type": "boolean", "description": "Prepend RAG context from the vector store"},
				"rag_top_k":   map[string]any{"type": "integer", "description": "Number of RAG results (default: 5)"},
				"temperature": map[string]any{"type": "number", "description": "Sampling temperature"},
			},
			"required": []string{"messages"},
		},
	}, s.handleChat)
}

func (s *Server) handleChat(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var args chatArgs
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if len(args.Messages) == 0 {
		return toolError("validation", "messages must not be empty"), nil
	}

	model, err := s.resolveModel(args.Model, "chat")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	messages := args.Messages

	// RAG orchestration: retrieve context and prepend as a system message.
	if args.UseRAG {
		// ragDisabled is immutable after NewServer; no lock needed.
		if s.ragDisabled {
			return toolError("rag", "RAG is disabled on this server"), nil
		}

		s.mu.RLock()
		retriever := s.retriever
		s.mu.RUnlock()

		if retriever == nil {
			return toolError("rag", "RAG index is empty; run rag_index_file or rag_index_directory first"), nil
		}

		topK := args.RAGTopK
		if topK <= 0 {
			topK = 5
		}

		// Extract query from last user message.
		query := lastUserMessage(messages)
		if query == "" {
			return toolError("validation", "use_rag requires at least one user message"), nil
		}

		results, err := retriever.Retrieve(ctx, query, topK)
		if err != nil {
			return toolError("rag", "retrieve: %v", err), nil
		}
		if len(results) == 0 {
			return toolError("rag", "RAG index is empty; run rag_index_file or rag_index_directory first"), nil
		}

		ragContext := retriever.BuildContext(results, 4096)
		systemMsg := ollama.ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Relevant context from the codebase:\n\n%s", ragContext),
		}
		messages = append([]ollama.ChatMessage{systemMsg}, messages...)
	}

	var opts *ollama.ModelOptions
	if args.Temperature != nil {
		opts = &ollama.ModelOptions{Temperature: *args.Temperature}
	}

	resp, err := s.client.Chat(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: messages,
		Options:  opts,
	})
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}

	return toolResult(resp.Message.Content), nil
}

// lastUserMessage returns the content of the last message with role "user".
func lastUserMessage(msgs []ollama.ChatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}
