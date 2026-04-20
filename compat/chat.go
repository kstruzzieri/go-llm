package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// ---------------------------------------------------------------------------
// Request / response wire types
// ---------------------------------------------------------------------------

// ChatCompletionRequest is the OpenAI-shape POST /v1/chat/completions body.
// Fields prefixed with x_ are go-llm-specific extensions; standard OpenAI
// SDKs ignore unknown fields so the wire remains compatible.
type ChatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []ChatMessageParam `json:"messages"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	MaxTokens   *int               `json:"max_tokens,omitempty"`
	Stop        StopSequences      `json:"stop,omitempty"` // string or []string
	Tools       []OpenAIToolParam  `json:"tools,omitempty"`
	ToolChoice  any                `json:"tool_choice,omitempty"`

	// Extensions (amendments #6, #10). All optional.
	UseCase     string `json:"x_use_case,omitempty"`
	Priority    *int   `json:"x_priority,omitempty"`
	AffinityKey string `json:"x_affinity_key,omitempty"`
	DryRun      bool   `json:"x_dry_run,omitempty"`
}

// ChatMessageParam is a single message in an OpenAI chat request.
// Role and Content are always populated; Name is optional and currently
// dropped during conversion because provider.ChatMessage has no Name field.
type ChatMessageParam struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// ChatCompletionResponse is the OpenAI-shape response body. The x_ fields
// extend it with go-llm routing metadata and extended reasoning output.
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // "chat.completion"
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   UsageWire    `json:"usage"`

	// Extensions.
	Thinking  string        `json:"x_thinking,omitempty"`
	RouteInfo *RouteInfoExt `json:"x_route_info,omitempty"`
}

// ChatChoice is one completion choice in an OpenAI chat response. The
// non-streaming handler currently emits exactly one choice.
type ChatChoice struct {
	Index        int              `json:"index"`
	Message      ChatMessageParam `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

// UsageWire is OpenAI's token accounting shape.
type UsageWire struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// RouteInfoExt surfaces the Router's decision metadata for a request. It is
// populated from provider.RouteOutcome on executed requests and from
// plan.Profile on dry-run requests.
type RouteInfoExt struct {
	ActualModel  string  `json:"actual_model"`
	PlannedModel string  `json:"planned_model,omitempty"`
	WasSticky    bool    `json:"was_sticky,omitempty"`
	Score        float64 `json:"score,omitempty"`
	Reason       string  `json:"reason,omitempty"`
}

// OpenAIToolParam mirrors OpenAI's function-calling tool descriptor.
type OpenAIToolParam struct {
	Type     string            `json:"type"` // "function"
	Function OpenAIToolFnParam `json:"function"`
}

// OpenAIToolFnParam is the function descriptor nested under a tool entry.
// Parameters is a raw JSON Schema document that go-llm forwards unmodified.
type OpenAIToolFnParam struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// StopSequences accepts OpenAI's "stop" field as either a single string or a
// list of strings. Both shapes unmarshal to []string internally so downstream
// provider.ModelOptions can consume a single type.
type StopSequences []string

// UnmarshalJSON decodes either a JSON string or array of strings into s.
// An empty string unmarshals to nil so downstream code sees no stop
// sequences rather than a single empty sentinel.
func (s *StopSequences) UnmarshalJSON(data []byte) error {
	// Try array first — this is the OpenAI canonical shape.
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		*s = arr
		return nil
	}
	// Fall back to single string.
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return fmt.Errorf("compat: stop must be string or []string: %w", err)
	}
	if single == "" {
		*s = nil
	} else {
		*s = []string{single}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleChatCompletions implements POST /v1/chat/completions (non-streaming).
// Streaming requests return 501 until Task 10 wires in the SSE writer.
//
// Order of operations:
//  1. Decode body and validate required fields.
//  2. Resolve model alias -> provider.ModelKey.
//  3. Build provider.RoutingRequest from wire fields + x_ extensions.
//  4. If streaming, dispatch to the SSE branch (currently stubbed).
//  5. Acquire a concurrency slot using the resolved priority — the slot is
//     released when the handler returns. This is deliberately AFTER request
//     parsing so x_priority influences admission control.
//  6. Route via Router.Route. On error, map via writeCompatError.
//  7. If DryRun, render route metadata with empty choices and return.
//  8. Execute via RoutePlan.ExecuteChat and render the OpenAI response.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeError(w, http.StatusServiceUnavailable, "no_router", "server has no router")
		return
	}

	var req ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode_error", err.Error())
		return
	}
	key, err := resolveModel(req.Model, s.aliases)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_model", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "missing_messages", "messages is required")
		return
	}

	rr := provider.RoutingRequest{
		Model:        selectorFor(key),
		UseCase:      firstNonEmpty(req.UseCase, "chat"),
		Messages:     toProviderMessages(req.Messages),
		Options:      toModelOptions(req.Temperature, req.TopP, req.MaxTokens, []string(req.Stop)),
		RequiredCaps: provider.CapChat,
		Priority:     resolvePriority(req.Priority, provider.PriorityNormal),
		AffinityKey:  req.AffinityKey,
		DryRun:       req.DryRun,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps |= provider.CapToolCall
		rr.Tools = toProviderTools(req.Tools)
	}
	if req.Stream {
		s.serveChatStream(w, r, rr)
		return
	}

	// Acquire the HTTP concurrency slot only after body parsing so x_priority
	// influences both admission control and routing.
	release, ok := s.semaphore.acquire(rr.Priority)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "capacity", "server is at capacity")
		return
	}
	defer release()

	plan, err := s.router.Route(r.Context(), rr)
	if err != nil {
		writeCompatError(w, err)
		return
	}
	if req.DryRun {
		writeChatDryRun(w, r, plan)
		return
	}
	resp, err := plan.ExecuteChat(r.Context())
	if err != nil {
		writeCompatError(w, err)
		return
	}

	out := ChatCompletionResponse{
		ID:      "chatcmpl_" + requestIDFrom(r.Context()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessageParam{
				Role:    "assistant",
				Content: resp.Content,
			},
			FinishReason: "stop",
		}},
		Usage: UsageWire{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		Thinking:  resp.Thinking,
		RouteInfo: routeInfoFrom(resp.RouteOutcome),
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// serveChatStream handles the streaming branch. Task 10 replaces this stub
// with a lazy-start SSE writer so pre-first-chunk failures still return a
// JSON error envelope.
func (s *Server) serveChatStream(w http.ResponseWriter, r *http.Request, rr provider.RoutingRequest) {
	_ = rr // consumed by the SSE implementation in a follow-up task.
	writeError(w, http.StatusNotImplemented, "not_implemented", "streaming arrives in a follow-up commit")
}

// writeChatDryRun renders the route decision without executing the request.
// The response carries an empty Choices slice (nothing was generated) and a
// populated x_route_info block so clients can observe the chosen model.
//
// On a dry-run the plan was not executed, so plan.Profile.Key is both the
// planned and the actual model — we do not walk the fallback chain.
func writeChatDryRun(w http.ResponseWriter, r *http.Request, plan *provider.RoutePlan) {
	out := ChatCompletionResponse{
		ID:      "chatcmpl_" + requestIDFrom(r.Context()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   plan.Profile.Key.String(),
		Choices: []ChatChoice{}, // empty — nothing was executed
		Usage:   UsageWire{},
		RouteInfo: &RouteInfoExt{
			ActualModel:  plan.Profile.Key.String(),
			PlannedModel: plan.Profile.Key.String(),
			Score:        plan.Score,
			Reason:       plan.Reason,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------------------
// Conversion helpers
// ---------------------------------------------------------------------------

// selectorFor formats a ModelKey for provider.RoutingRequest.Model. When the
// key is qualified it returns "provider/model"; unqualified keys return just
// the bare model name so the router performs a cross-provider lookup.
func selectorFor(k provider.ModelKey) string {
	if k.Provider == "" {
		return k.Model
	}
	return k.String()
}

// firstNonEmpty returns the first non-empty string from its arguments.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// resolvePriority maps the wire x_priority field (a *int) to a
// provider.Priority. nil falls back to def. Values outside the
// [PriorityBackground, PriorityCritical] range are clamped so callers sending
// garbage integers do not bypass admission control.
func resolvePriority(explicit *int, def provider.Priority) provider.Priority {
	if explicit == nil {
		return def
	}
	p := provider.Priority(*explicit)
	if p < provider.PriorityBackground {
		return provider.PriorityBackground
	}
	if p > provider.PriorityCritical {
		return provider.PriorityCritical
	}
	return p
}

// toProviderMessages converts OpenAI chat messages to provider.ChatMessage.
// Role/Content only — the Name field on ChatMessageParam is silently dropped
// because provider.ChatMessage has no corresponding field today.
func toProviderMessages(in []ChatMessageParam) []provider.ChatMessage {
	out := make([]provider.ChatMessage, 0, len(in))
	for _, m := range in {
		out = append(out, provider.ChatMessage{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	return out
}

// toProviderTools converts OpenAI tool descriptors to provider.Tool entries.
// Parameters is passed through as raw JSON so we do not canonicalize or
// re-encode the schema.
func toProviderTools(in []OpenAIToolParam) []provider.Tool {
	out := make([]provider.Tool, 0, len(in))
	for _, t := range in {
		out = append(out, provider.Tool{
			Type: firstNonEmpty(t.Type, "function"),
			Function: provider.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}
	return out
}

// toModelOptions maps the OpenAI sampling fields onto provider.ModelOptions.
// Pointer inputs pass through unchanged. maxTokens is stored as NumPredict
// (Ollama's convention); nil leaves NumPredict at zero so the provider picks
// a default. Empty stop slices are normalized to nil.
func toModelOptions(temperature, topP *float64, maxTokens *int, stop []string) provider.ModelOptions {
	opts := provider.ModelOptions{
		Temperature: temperature,
		TopP:        topP,
	}
	if maxTokens != nil {
		opts.NumPredict = *maxTokens
	}
	if len(stop) > 0 {
		opts.Stop = stop
	}
	return opts
}

// routeInfoFrom lifts a provider.RouteOutcome into the wire RouteInfoExt.
// Returns nil when the outcome is nil so the x_route_info field omits
// cleanly. ActualModel/PlannedModel use ModelKey.String() which always
// includes the provider prefix for routed responses.
func routeInfoFrom(outcome *provider.RouteOutcome) *RouteInfoExt {
	if outcome == nil {
		return nil
	}
	return &RouteInfoExt{
		ActualModel:  outcome.ActualModel.String(),
		PlannedModel: outcome.PlannedModel.String(),
		WasSticky:    outcome.WasSticky,
		Score:        outcome.Score,
		Reason:       outcome.Reason,
	}
}
