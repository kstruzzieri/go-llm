package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

type candidateTransportOptions struct {
	ollamaURL           string
	openAICompatBaseURL string
	openAICompatAPIKey  string
	timeout             time.Duration
}

type candidateTransport struct {
	chat         candidateChatClient
	providerName string
}

type candidateChatClient interface {
	Chat(ctx context.Context, req ollama.ChatRequest) (candidateChatResponse, error)
}

type candidateChatResponse struct {
	Message ollama.ChatMessage
	// Thinking is the reasoning text a backend separated from the answer
	// (Ollama message.thinking / openai-compat reasoning_content). It is kept
	// out of Message deliberately: Message feeds back into conversation history
	// and must stay free of reasoning to preserve replay determinism. The
	// runner maps this onto the transcript's Turn.Thinking. See #160.
	Thinking               string
	PromptEvalCount        int
	EvalCount              int
	ThinkingTokens         int
	ThinkingTokensComputed bool
}

func newCandidateTransport(target ModelTarget, opts candidateTransportOptions) (candidateTransport, error) {
	switch normalizeModelSelector(target.Provider) {
	case defaultBenchProvider:
		client, err := newOllamaClient(opts.ollamaURL)
		if err != nil {
			return candidateTransport{}, fmt.Errorf("candidate %q client: %w", target.Display, err)
		}
		return candidateTransport{chat: ollamaCandidateClient{client: client}, providerName: defaultBenchProvider}, nil
	case openAICompatTransport:
		baseURL := strings.TrimSpace(opts.openAICompatBaseURL)
		if baseURL == "" {
			return candidateTransport{}, fmt.Errorf("candidate %s transport requires -candidate-base-url", openAICompatTransport)
		}
		clientOpts := []openaicompat.ClientOption{}
		if opts.timeout > 0 {
			clientOpts = append(clientOpts, openaicompat.WithHTTPClient(&http.Client{Timeout: opts.timeout}))
		}
		if key := strings.TrimSpace(opts.openAICompatAPIKey); key != "" {
			clientOpts = append(clientOpts, openaicompat.WithAPIKey(key))
		}
		providerName := openAICompatCandidateProviderName(baseURL)
		// Leave ThinkMode at its default (extract). A reasoning model served via
		// llama.cpp can emit <think>...</think> inline in content; extraction
		// strips it so the scored transcript holds the final answer rather than
		// serving-stack-specific reasoning formatting. The stripped reasoning is
		// carried separately on candidateChatResponse.Thinking so replay history
		// stays reasoning-free while transcripts retain the reasoning text.
		prov := openaicompat.NewProvider(
			openaicompat.NewClient(baseURL, clientOpts...),
			openaicompat.WithProviderName(providerName),
		)
		return candidateTransport{chat: openAICompatCandidateClient{provider: prov}, providerName: prov.Name()}, nil
	default:
		return candidateTransport{}, fmt.Errorf("%w: %q", errUnsupportedProv, target.Provider)
	}
}

type ollamaCandidateClient struct {
	client *ollama.Client
}

func (c ollamaCandidateClient) Chat(ctx context.Context, req ollama.ChatRequest) (candidateChatResponse, error) {
	resp, err := c.client.Chat(ctx, req)
	if err != nil {
		return candidateChatResponse{}, err
	}
	if resp == nil {
		return candidateChatResponse{}, fmt.Errorf("ollama candidate: nil response")
	}
	// Lift Ollama's separate reasoning text out of the reply message so it is
	// captured for the transcript without leaking back into conversation
	// history when the message is replayed in later turns.
	msg := resp.Message
	thinking := msg.Thinking
	msg.Thinking = ""
	return candidateChatResponse{
		Message:         msg,
		Thinking:        thinking,
		PromptEvalCount: resp.PromptEvalCount,
		EvalCount:       resp.EvalCount,
	}, nil
}

type openAICompatCandidateClient struct {
	provider *openaicompat.Provider
}

func (c openAICompatCandidateClient) Chat(ctx context.Context, req ollama.ChatRequest) (candidateChatResponse, error) {
	presp, err := c.provider.Chat(ctx, provider.ChatRequest{
		Model:    req.Model,
		Messages: toProviderCandidateMessages(req.Messages),
		Tools:    toProviderCandidateTools(req.Tools),
		Options:  toProviderCandidateOptions(req.Options),
	})
	if err != nil {
		return candidateChatResponse{}, err
	}
	if presp == nil {
		return candidateChatResponse{}, fmt.Errorf("openai-compat candidate: nil response")
	}
	toolCalls, err := toOllamaCandidateToolCalls(presp.ToolCalls)
	if err != nil {
		return candidateChatResponse{}, err
	}
	// Map provider reasoning tokens onto the bench's thinking-token accounting.
	// ThinkingTokensComputed gates whether downstream treats the count as a
	// measured value, so preserve the provider's reported/presence bit rather
	// than inferring availability from a positive token count.
	reasoning := presp.Usage.ReasoningTokens
	return candidateChatResponse{
		Message: ollama.ChatMessage{
			Role:      "assistant",
			Content:   presp.Content,
			ToolCalls: toolCalls,
		},
		// presp.Thinking holds reasoning_content (native) or, failing that,
		// inline-extracted <think> text — captured separately from Message so it
		// reaches the transcript without polluting replayed history.
		Thinking:               presp.Thinking,
		PromptEvalCount:        presp.Usage.PromptTokens,
		EvalCount:              presp.Usage.CompletionTokens,
		ThinkingTokens:         reasoning,
		ThinkingTokensComputed: presp.Usage.ReasoningTokensReported,
	}, nil
}

func toProviderCandidateMessages(in []ollama.ChatMessage) []provider.ChatMessage {
	out := make([]provider.ChatMessage, len(in))
	for i, m := range in {
		out[i] = provider.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toProviderCandidateToolCalls(m.ToolCalls),
		}
	}
	return out
}

func toProviderCandidateTools(in []ollama.Tool) []provider.Tool {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.Tool, len(in))
	for i, t := range in {
		out[i] = provider.Tool{
			Type: t.Type,
			Function: provider.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		}
	}
	return out
}

func toProviderCandidateOptions(opts *ollama.ModelOptions) provider.ModelOptions {
	if opts == nil {
		return provider.ModelOptions{}
	}
	temp := opts.Temperature
	topP := opts.TopP
	repeatPenalty := opts.RepeatPenalty
	out := provider.ModelOptions{
		NumPredict: opts.NumPredict,
		NumCtx:     opts.NumCtx,
		Stop:       opts.Stop,
	}
	if opts.Temperature != 0 {
		out.Temperature = &temp
	}
	if opts.TopP != 0 {
		out.TopP = &topP
	}
	if opts.RepeatPenalty != 0 {
		out.RepeatPenalty = &repeatPenalty
	}
	return out
}

func toProviderCandidateToolCalls(in []ollama.ToolCall) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(in))
	for i, tc := range in {
		args, _ := json.Marshal(tc.Function.Arguments)
		out[i] = provider.ToolCall{
			ID:   tc.ID,
			Type: firstNonEmpty(tc.Type, "function"),
			Function: provider.ToolCallFunction{
				Index:     tc.Function.Index,
				Name:      tc.Function.Name,
				Arguments: args,
			},
		}
	}
	return out
}

func toOllamaCandidateToolCalls(in []provider.ToolCall) ([]ollama.ToolCall, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]ollama.ToolCall, len(in))
	for i, tc := range in {
		args := map[string]any{}
		if len(tc.Function.Arguments) > 0 {
			if err := json.Unmarshal(tc.Function.Arguments, &args); err != nil {
				return nil, fmt.Errorf("openai-compat candidate tool %q arguments: %w", tc.Function.Name, err)
			}
		}
		out[i] = ollama.ToolCall{
			ID:   tc.ID,
			Type: firstNonEmpty(tc.Type, "function"),
			Function: ollama.ToolCallFunction{
				Index:     tc.Function.Index,
				Name:      tc.Function.Name,
				Arguments: args,
			},
		}
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
