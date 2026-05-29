package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// chatArgs are the parameters for the chat tool.
type chatArgs struct {
	Messages       []ollama.ChatMessage `json:"messages"`
	Model          string               `json:"model,omitempty"`
	UseRAG         bool                 `json:"use_rag,omitempty"`
	RAGTopK        int                  `json:"rag_top_k,omitempty"`
	Temperature    *float64             `json:"temperature,omitempty"`
	ConversationID string               `json:"conversation_id,omitempty"`
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
							"role":         map[string]any{"type": "string", "enum": []string{"system", "user", "assistant", "tool"}},
							"content":      map[string]any{"type": "string"},
							"tool_calls":   map[string]any{"type": "array"},
							"tool_name":    map[string]any{"type": "string"},
							"tool_call_id": map[string]any{"type": "string"},
						},
						"required": []string{"role", "content"},
					},
				},
				"model":           map[string]any{"type": "string", "description": "Model name (uses configured default if omitted)"},
				"use_rag":         map[string]any{"type": "boolean", "description": "Prepend RAG context from the vector store"},
				"rag_top_k":       map[string]any{"type": "integer", "description": "Number of RAG results (default: 5)"},
				"temperature":     map[string]any{"type": "number", "description": "Sampling temperature"},
				"conversation_id": map[string]any{"type": "string", "description": "Optional stable id to group calls of one conversation; omit to derive identity from message content"},
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

	router := s.routerSnapshot()
	if router == nil {
		return toolError("config", "router unavailable"), nil
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

		results, rerr := retriever.Retrieve(ctx, query, topK)
		if rerr != nil {
			return toolError("rag", "retrieve: %v", rerr), nil
		}
		if len(results) == 0 {
			return toolError("rag", "RAG index is empty; run rag_index_file or rag_index_directory first"), nil
		}

		ragContext := retriever.BuildContext(results, ragContextMaxTokens)
		systemMsg := ollama.ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Relevant context from the codebase:\n\n%s", ragContext),
		}
		messages = append([]ollama.ChatMessage{systemMsg}, messages...)
	}

	pmsgs := toProviderChatMessages(messages)

	opts := provider.ModelOptions{}
	if args.Temperature != nil {
		opts.Temperature = args.Temperature
	}
	model, err := s.routeModelSelector(ctx, args.Model, "chat")
	if err != nil {
		return toolError("config", "%v", err), nil
	}

	rr := provider.RoutingRequest{
		Model:          model,
		UseCase:        "chat",
		RequiredCaps:   provider.CapChat,
		Messages:       pmsgs,
		Options:        opts,
		ExpectedOutput: provider.DefaultExpectedOutput("chat"),
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
	resp, err := plan.ExecuteChat(ctx)
	if err != nil {
		return toolError("ollama", "%v", err), nil
	}
	return toolResult(resp.Content), nil
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

func toProviderChatMessages(in []ollama.ChatMessage) []provider.ChatMessage {
	out := make([]provider.ChatMessage, len(in))
	for i, m := range in {
		out[i] = provider.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  toProviderToolCalls(m.ToolCalls),
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
		}
	}
	return out
}

func toProviderToolCalls(in []ollama.ToolCall) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}

	out := make([]provider.ToolCall, len(in))
	for i, c := range in {
		var args json.RawMessage
		if c.Function.Arguments != nil {
			args, _ = json.Marshal(c.Function.Arguments)
		}
		out[i] = provider.ToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: provider.ToolCallFunction{
				Index:     c.Function.Index,
				Name:      c.Function.Name,
				Arguments: args,
			},
		}
	}
	return out
}
