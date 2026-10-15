package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	// httpClient, when non-nil, replaces the timeout-only client on the
	// openai-compat branch: -replay-attempt injects its recording client
	// (#687). Every legacy caller leaves it nil.
	httpClient *http.Client
}

// defaultOpenAICompatTimeout mirrors provider/openaicompat.NewClient's own
// default (5 minutes), so a zero -timeout keeps its hang guard.
const defaultOpenAICompatTimeout = 5 * time.Minute

// candidateHTTPClient returns the client the openai-compat branch wraps: a copy
// of opts.httpClient when set, else a timeout-bounded client (the provider's
// default when opts.timeout is not positive).
func candidateHTTPClient(opts candidateTransportOptions) *http.Client {
	if opts.httpClient != nil {
		c := *opts.httpClient
		return &c
	}
	if opts.timeout > 0 {
		return &http.Client{Timeout: opts.timeout}
	}
	return &http.Client{Timeout: defaultOpenAICompatTimeout}
}

type candidateTransport struct {
	chat         candidateChatClient
	providerName string
}

type candidateChatClient interface {
	Chat(ctx context.Context, req candidateChatRequest) (candidateChatResponse, error)
}

// candidateChatRequest wraps the ollama-shaped chat request with the
// prefilled fixtures' raw tool-call argument bytes (#331 W3 byte fidelity).
// RawToolArgs mirrors Messages: RawToolArgs[i][j] is the byte-exact fixture
// argument payload for Messages[i].ToolCalls[j]. A nil slice (whole,
// per-message, or per-call entry) means "no raw bytes — marshal the decoded
// map", the pre-W3 behavior. Only replayPrefilled populates it: fixture
// bytes are the thing being replayed verbatim there; candidate RESPONSES in
// the scripted tool loop were already decoded by the transport and carry no
// authoritative byte form.
type candidateChatRequest struct {
	ollama.ChatRequest
	RawToolArgs [][]json.RawMessage
	// KeepRawCalls (injection traces only): return tool calls through
	// RawToolCalls even when their arguments do not decode, instead of failing
	// the whole response (spec §4.6).
	KeepRawCalls bool
	// ProviderOptions, when set, is sent verbatim on the openai-compat wire in
	// place of the ollama-option conversion: the registered decoding (§4.9).
	ProviderOptions *provider.ModelOptions
	// ParseThinkMode, when set, overrides the openai-compat provider's inline
	// <think> parser mode for this request: the registered parser policy, so the
	// replay extracts exactly what Golem's configured think_mode would (#452).
	ParseThinkMode *provider.ThinkMode
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
	// RawToolCalls is the injection raw side channel, read from the HTTP body
	// before provider normalization. Set only when KeepRawCalls was requested.
	RawToolCalls []injectionRawCall
}

// rawResponseSlot receives the response body of the request whose context
// carries it. Injection replays attach one; every other request passes through.
type rawResponseSlot struct{ body []byte }

type rawSlotKey struct{}

type rawResponseRecorder struct{ next http.RoundTripper }

func (r rawResponseRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	slot, _ := req.Context().Value(rawSlotKey{}).(*rawResponseSlot)
	if err != nil || slot == nil || resp.Body == nil {
		return resp, err
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, rerr
	}
	slot.body = b
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}

// rawCallsFromResponseBody parses choices[0].message.tool_calls from the raw
// chat-completion body, preserving argument presence and exact text.
// DecodeOK is true only when arguments is a JSON object, or a JSON string
// whose content parses to a JSON object.
func rawCallsFromResponseBody(body []byte) ([]injectionRawCall, error) {
	var env struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("raw tool calls: %w", err)
	}
	if len(env.Choices) == 0 {
		return nil, nil
	}
	var out []injectionRawCall
	for _, tc := range env.Choices[0].Message.ToolCalls {
		raw := tc.Function.Arguments // nil when the key is absent
		c := injectionRawCall{ID: tc.ID, Name: tc.Function.Name, Arguments: string(raw), ArgumentsPresent: raw != nil}
		_, c.DecodeOK = decodeRawArgs(c.Arguments)
		out = append(out, c)
	}
	return out, nil
}

func newCandidateTransport(target ModelTarget, opts candidateTransportOptions) (candidateTransport, error) {
	switch normalizeModelSelector(target.Provider) {
	case defaultBenchProvider:
		var clientOpts []ollama.Option
		if opts.timeout > 0 {
			clientOpts = append(clientOpts, ollama.WithTimeout(opts.timeout))
		}
		client, err := newOllamaClient(opts.ollamaURL, clientOpts...)
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
		// The recorder is always installed; requests without a slot pass
		// through untouched.
		wrapped := candidateHTTPClient(opts)
		next := wrapped.Transport
		if next == nil {
			next = http.DefaultTransport
		}
		wrapped.Transport = rawResponseRecorder{next: next}
		clientOpts = append(clientOpts, openaicompat.WithHTTPClient(wrapped))
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

// Chat sends the request over the native Ollama transport. FROZEN BOUNDARY
// (#331 W3): ollama.ToolCallFunction.Arguments is map[string]any on the
// frozen wire struct itself, so req.RawToolArgs cannot reach this wire —
// argument bytes are re-encoded by the ollama client (sorted keys, canonical
// whitespace). Semantic equality is the ceiling here; byte-exact fixture
// args are achievable only on transports whose frozen types accept raw JSON
// (see openAICompatCandidateClient).
func (c ollamaCandidateClient) Chat(ctx context.Context, req candidateChatRequest) (candidateChatResponse, error) {
	if req.KeepRawCalls {
		// Native ollama decodes arguments server-side, so the raw evidence is
		// gone: fail closed.
		return candidateChatResponse{}, fmt.Errorf("injection traces require the openai-compat transport")
	}
	resp, err := c.client.Chat(ctx, req.ChatRequest)
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

func (c openAICompatCandidateClient) Chat(ctx context.Context, req candidateChatRequest) (candidateChatResponse, error) {
	opts := toProviderCandidateOptions(req.Options)
	if req.ProviderOptions != nil {
		opts = *req.ProviderOptions
	}
	var slot *rawResponseSlot
	if req.KeepRawCalls {
		slot = &rawResponseSlot{}
		ctx = context.WithValue(ctx, rawSlotKey{}, slot)
	}
	presp, err := c.provider.Chat(ctx, provider.ChatRequest{
		Model:          req.Model,
		Messages:       toProviderCandidateMessages(req.Messages, req.RawToolArgs),
		Tools:          toProviderCandidateTools(req.Tools),
		Options:        opts,
		ParseThinkMode: req.ParseThinkMode,
	})
	if err != nil {
		return candidateChatResponse{}, err
	}
	if presp == nil {
		return candidateChatResponse{}, fmt.Errorf("openai-compat candidate: nil response")
	}
	var raw []injectionRawCall
	if slot != nil {
		if raw, err = rawCallsFromResponseBody(slot.body); err != nil {
			return candidateChatResponse{}, err
		}
	}
	toolCalls, err := toOllamaCandidateToolCalls(presp.ToolCalls)
	if err != nil {
		if !req.KeepRawCalls {
			return candidateChatResponse{}, err
		}
		toolCalls = nil // malformed: the raw channel is the evidence
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
		RawToolCalls:           raw,
	}, nil
}

// toProviderCandidateMessages converts ollama-shaped history to provider
// messages. rawArgs (may be nil) mirrors in per candidateChatRequest: when a
// message carries raw fixture argument bytes they pass through verbatim
// instead of being re-marshaled from the decoded map (#331 W3).
func toProviderCandidateMessages(in []ollama.ChatMessage, rawArgs [][]json.RawMessage) []provider.ChatMessage {
	out := make([]provider.ChatMessage, len(in))
	for i, m := range in {
		var raw []json.RawMessage
		if i < len(rawArgs) {
			raw = rawArgs[i]
		}
		out[i] = provider.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toProviderCandidateToolCalls(m.ToolCalls, raw),
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
	repeatPenalty := opts.RepeatPenalty
	out := provider.ModelOptions{
		NumPredict: opts.NumPredict,
		NumCtx:     opts.NumCtx,
		Stop:       opts.Stop,
	}
	out.Temperature = opts.Temperature
	out.TopP = opts.TopP
	if opts.RepeatPenalty != 0 {
		out.RepeatPenalty = &repeatPenalty
	}
	return out
}

// toProviderCandidateToolCalls converts tool calls, preferring the byte-exact
// raw fixture arguments in rawArgs[i] over re-marshaling the decoded map.
// provider.ToolCallFunction.Arguments and the openai-compat wire's
// arguments envelope are both json.RawMessage, and the frozen
// encodeToolCallArguments string-wraps raw JSON verbatim (edge-trimmed), so
// the fixture bytes — key order and interior whitespace included — survive
// to the wire on this transport.
func toProviderCandidateToolCalls(in []ollama.ToolCall, rawArgs []json.RawMessage) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(in))
	for i, tc := range in {
		var args json.RawMessage
		if i < len(rawArgs) && len(rawArgs[i]) > 0 {
			args = rawArgs[i]
		} else {
			args, _ = json.Marshal(tc.Function.Arguments)
		}
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
				return nil, &toolArgumentsError{name: tc.Function.Name, err: err}
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

// toolArgumentsError is a candidate tool call whose arguments do not decode
// to a JSON object. Error() keeps the pre-#687 message byte for byte (legacy
// output is pinned by the legacy goldens); -replay-attempt finds it with
// errors.As and classifies it argument_decode.
type toolArgumentsError struct {
	name string
	err  error
}

func (e *toolArgumentsError) Error() string {
	return fmt.Sprintf("openai-compat candidate tool %q arguments: %v", e.name, e.err)
}

func (e *toolArgumentsError) Unwrap() error { return e.err }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
