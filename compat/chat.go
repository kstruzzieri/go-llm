package compat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// ChatMessageParam is a single message in an OpenAI chat request or response.
//
// Fields:
//   - Role / Content: always populated on the wire.
//   - Name: honored only when Role=="tool" (it names the tool whose result
//     this message carries). On any other role, Name is silently dropped
//     during conversion because provider.ChatMessage has no general-purpose
//     Name field. This is intentional — OpenAI's Name on non-tool roles is
//     informational and has no provider-side effect today.
//   - ToolCallID: correlates a role=="tool" result with the prior assistant
//     tool_call that produced it. Required by the model to match the result
//     to the invocation.
//   - ToolCalls: populated on assistant turns (inbound when replaying a
//     tool-calling history, outbound when the model emits a new invocation).
type ChatMessageParam struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	Name       string             `json:"name,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCallWire `json:"tool_calls,omitempty"`
}

// ChatToolCallWire mirrors OpenAI's tool_calls[] entry on assistant messages.
// It carries the call ID, the fixed "function" type, and the nested function
// descriptor.
type ChatToolCallWire struct {
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type"` // "function"
	Function ChatToolCallFunctionWire `json:"function"`
}

// ChatToolCallFunctionWire is the function descriptor inside a tool_calls entry.
// Arguments is forwarded from the provider verbatim as raw JSON. Note that
// OpenAI's canonical API returns Arguments as a JSON-encoded string (a string
// literal whose value is a serialized JSON object). go-llm preserves the
// provider's own encoding instead of re-stringifying it; clients that compare
// bytes strictly may notice the difference. This is an intentional transparency
// tradeoff, not a lossy conversion — the semantic payload round-trips.
type ChatToolCallFunctionWire struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
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

// ChatCompletionChunk is one frame of an OpenAI-shape streaming response.
// Each chunk carries exactly one ChatChunkChoice today. Usage is not included
// on per-chunk frames — OpenAI emits usage only on the final frame when
// stream_options.include_usage is set, which we don't yet expose.
type ChatCompletionChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"` // "chat.completion.chunk"
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []ChatChunkChoice `json:"choices"`

	// Extensions.
	Thinking string `json:"x_thinking,omitempty"`
}

// ChatChunkChoice is one choice inside a streaming chunk. FinishReason is a
// pointer so interim chunks serialize as "finish_reason":null and only the
// final chunk carries a non-null reason, matching OpenAI's wire format.
type ChatChunkChoice struct {
	Index        int              `json:"index"`
	Delta        ChatMessageParam `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
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
// Empty strings are filtered out in both shapes so downstream code sees a
// clean list: [""] / "" become nil, ["", "b", ""] becomes []string{"b"}.
// Providers treat an empty stop sequence as a no-op at best and an always-fire
// sentinel at worst, so normalizing at the edge is strictly safer.
func (s *StopSequences) UnmarshalJSON(data []byte) error {
	// Try array first — this is the OpenAI canonical shape.
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		filtered := arr[:0]
		for _, v := range arr {
			if v != "" {
				filtered = append(filtered, v)
			}
		}
		if len(filtered) == 0 {
			*s = nil
		} else {
			*s = filtered
		}
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
		tools, err := toProviderTools(req.Tools)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_tool_parameters", err.Error())
			return
		}
		rr.RequiredCaps |= provider.CapToolCall
		rr.Tools = tools
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

	// Derive finish_reason from response state. OpenAI's ordering is:
	// "tool_calls" when the model emitted any function invocations; else
	// "length" when MaxTokens was set and the response exhausted the budget;
	// else "stop".
	finish := "stop"
	if len(resp.ToolCalls) > 0 {
		finish = "tool_calls"
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 && resp.Usage.CompletionTokens >= *req.MaxTokens {
		finish = "length"
	}

	out := ChatCompletionResponse{
		ID:      chatResponseID(r.Context()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		// Model is the qualified "provider/model" form taken from the plan
		// so success and dry-run responses have identical shape even when
		// resp.Model (provider-reported) omits the provider prefix.
		Model: plan.Profile.Key.String(),
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessageParam{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: toolCallsToWire(resp.ToolCalls),
			},
			FinishReason: finish,
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

// serveChatStream handles the streaming branch of POST /v1/chat/completions.
// It emits an SSE (text/event-stream) body whose events each carry a JSON
// ChatCompletionChunk, terminated by a "data: [DONE]" sentinel.
//
// Order of operations:
//  1. Broaden rr.RequiredCaps with CapStream so the router rejects any
//     provider that cannot stream before any bytes are written.
//  2. Acquire the HTTP concurrency slot using the resolved priority — the
//     slot is released when the stream completes. Placement AFTER route
//     parsing (already done by the caller) means x_priority influences
//     admission control on streaming requests too.
//  3. Route via Router.Route. On error, writeCompatError still works
//     because no bytes have been written yet.
//  4. Construct a lazy-start sseWriter — it does NOT commit the 200 status
//     until the first chunk is written, so a pre-first-chunk
//     ExecuteChatStream failure (e.g. provider returns 5xx before any chunk)
//     can still be surfaced as a regular JSON error envelope.
//  5. For each chunk, emit a ChatCompletionChunk with the qualified model
//     ID and, on the Done chunk, a derived finish_reason matching the
//     non-streaming branch's logic (tool_calls > length > stop).
//  6. After ExecuteChatStream returns, emit "data: [DONE]" only on clean
//     completion. If the failure happened before the first chunk, surface
//     a JSON error envelope. If the failure happened mid-stream, skip the
//     [DONE] sentinel so clients can detect premature EOS via its absence;
//     context.Canceled (client disconnect) is treated as normal and its
//     log is suppressed to prevent spam.
func (s *Server) serveChatStream(w http.ResponseWriter, r *http.Request, rr provider.RoutingRequest) {
	rr.RequiredCaps |= provider.CapStream

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

	sw, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "no_flusher", err.Error())
		return
	}

	// Capture the response ID and qualified model ID BEFORE invoking the
	// provider so every chunk carries stable identifiers. chatResponseID
	// falls back to a random suffix when no request-id middleware has run —
	// this matches the non-streaming branch's fix (commit 9f04342).
	id := chatResponseID(r.Context())
	created := time.Now().Unix()
	modelID := plan.Profile.Key.String()

	streamErr := plan.ExecuteChatStream(r.Context(), func(chunk provider.ChatResponse) error {
		delta := ChatMessageParam{
			Role:      "assistant",
			Content:   chunk.Content,
			ToolCalls: toolCallsToWire(chunk.ToolCalls),
		}
		var finish *string
		if chunk.Done {
			reason := deriveStreamFinishReason(chunk, rr.Options.NumPredict)
			finish = &reason
		}
		return sw.writeEvent(ChatCompletionChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   modelID,
			Choices: []ChatChunkChoice{{
				Index:        0,
				Delta:        delta,
				FinishReason: finish,
			}},
			Thinking: chunk.Thinking,
		})
	})

	if streamErr != nil {
		// If the failure happened BEFORE any chunk was delivered, the SSE
		// writer never committed the 200 status — so we can still surface a
		// JSON error envelope. Otherwise the stream is already live on the
		// wire and we must NOT emit "data: [DONE]" — that is OpenAI's
		// success sentinel, and emitting it under error conditions silently
		// signals success. Skipping it lets SDKs detect premature EOS via
		// the missing terminator.
		if !sw.started {
			writeCompatError(w, streamErr)
			return
		}
		// Client disconnect is normal (e.g., IDE cancels completion on every
		// keystroke) — suppress the log to prevent spam.
		if !errors.Is(streamErr, context.Canceled) {
			log.Printf("compat: chat stream error rid=%s: %v", requestIDFrom(r.Context()), streamErr)
		}
		return
	}

	_ = sw.writeDone()
}

// deriveStreamFinishReason mirrors the non-streaming branch's finish_reason
// logic for the Done chunk of a stream. Priority:
//   - "tool_calls" when the final chunk carried any tool invocations.
//   - "length" when MaxTokens was set and usage reached the cap.
//   - "stop" otherwise.
//
// numPredict is 0 when MaxTokens was unset, in which case we cannot infer a
// length stop and default to "stop".
func deriveStreamFinishReason(chunk provider.ChatResponse, numPredict int) string {
	if len(chunk.ToolCalls) > 0 {
		return "tool_calls"
	}
	if numPredict > 0 && chunk.Usage.CompletionTokens >= numPredict {
		return "length"
	}
	return "stop"
}

// writeChatDryRun renders the route decision without executing the request.
// The response carries an empty Choices slice (nothing was generated) and a
// populated x_route_info block so clients can observe the chosen model.
//
// On a dry-run the plan was not executed, so plan.Profile.Key is both the
// planned and the actual model — we do not walk the fallback chain.
func writeChatDryRun(w http.ResponseWriter, r *http.Request, plan *provider.RoutePlan) {
	out := ChatCompletionResponse{
		ID:      chatResponseID(r.Context()),
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
//
// Field handling:
//   - Role=="tool": Name populates provider.ChatMessage.ToolName and
//     ToolCallID populates provider.ChatMessage.ToolCallID. Both are
//     required for the model to correlate a tool result with the assistant
//     invocation that produced it; dropping them would break multi-turn
//     tool use.
//   - Role=="assistant" with inbound ToolCalls: converted to
//     provider.ToolCall entries on the provider message so history replay
//     carries the original invocations.
//   - Other roles: Name is intentionally dropped. OpenAI allows Name on
//     user/system messages for "human name" tagging, but provider.ChatMessage
//     has no general-purpose Name field today and adding one would require
//     provider-side changes outside the scope of the compat façade.
func toProviderMessages(in []ChatMessageParam) []provider.ChatMessage {
	out := make([]provider.ChatMessage, 0, len(in))
	for _, m := range in {
		msg := provider.ChatMessage{
			Role:    m.Role,
			Content: m.Content,
		}
		if m.Role == "tool" {
			msg.ToolName = m.Name
			msg.ToolCallID = m.ToolCallID
		}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = toolCallsFromWire(m.ToolCalls)
		}
		out = append(out, msg)
	}
	return out
}

// toProviderTools converts OpenAI tool descriptors to provider.Tool entries.
// Parameters is passed through as raw JSON so we do not canonicalize or
// re-encode the schema, but we reject payloads that are clearly not a JSON
// object. Forwarding, e.g., a JSON string or array to the provider would
// cause the upstream to return an opaque 400 that surfaces here as a 502.
// Catching the mismatch at the edge gives the client a precise
// invalid_tool_parameters signal instead.
func toProviderTools(in []OpenAIToolParam) ([]provider.Tool, error) {
	out := make([]provider.Tool, 0, len(in))
	for i, t := range in {
		if err := validateToolParameters(t.Function.Parameters); err != nil {
			return nil, fmt.Errorf("tools[%d].function.parameters: %w", i, err)
		}
		out = append(out, provider.Tool{
			Type: firstNonEmpty(t.Type, "function"),
			Function: provider.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		})
	}
	return out, nil
}

// validateToolParameters checks that a tool-function parameters payload is
// either empty (omitted) or a JSON object. We inspect the first
// non-whitespace byte rather than fully unmarshaling — JSON Schema documents
// can be arbitrarily nested and we do not want to pay the allocation cost on
// every request. Rejecting obvious mismatches (strings, numbers, arrays) is
// enough; malformed JSON inside a valid-looking object will still be caught
// by the provider.
func validateToolParameters(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("must be a JSON object")
	}
	return nil
}

// toolCallsFromWire converts inbound ChatToolCallWire entries (replayed on
// assistant messages) to provider.ToolCall. Arguments is forwarded as raw
// JSON; provider.ToolCallFunction.Index defaults to zero which matches
// OpenAI's behavior on non-streaming calls.
func toolCallsFromWire(in []ChatToolCallWire) []provider.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, 0, len(in))
	for _, c := range in {
		out = append(out, provider.ToolCall{
			ID:   c.ID,
			Type: firstNonEmpty(c.Type, "function"),
			Function: provider.ToolCallFunction{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
			},
		})
	}
	return out
}

// toolCallsToWire converts outbound provider.ToolCall entries to
// ChatToolCallWire for the response payload. Arguments is forwarded
// verbatim — see the ChatToolCallFunctionWire godoc for the
// OpenAI-canonical-string caveat.
func toolCallsToWire(in []provider.ToolCall) []ChatToolCallWire {
	if len(in) == 0 {
		return nil
	}
	out := make([]ChatToolCallWire, 0, len(in))
	for _, c := range in {
		out = append(out, ChatToolCallWire{
			ID:   c.ID,
			Type: firstNonEmpty(c.Type, "function"),
			Function: ChatToolCallFunctionWire{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
			},
		})
	}
	return out
}

// chatResponseID builds the "chatcmpl_" response ID. It prefers the request
// ID attached by requestIDMiddleware (so HTTP access logs and response
// bodies correlate), but falls back to a freshly generated random suffix
// when the context carries none — e.g. when handlers are invoked directly
// via the mux for tests or embedded callers. Without the fallback, bypass
// paths would return "chatcmpl_" verbatim, which is indistinguishable from
// a bug.
func chatResponseID(ctx context.Context) string {
	rid := requestIDFrom(ctx)
	if rid == "" {
		rid = fallbackRequestID()
	}
	return "chatcmpl_" + rid
}

// fallbackRequestID generates a short, random hex suffix for use when no
// request-ID middleware has run. 8 random bytes (16 hex chars) is well
// beyond collision-hazard for response-body correlation.
func fallbackRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "cmpl_" + hex.EncodeToString(b[:])
}

// toModelOptions maps the OpenAI sampling fields onto provider.ModelOptions.
// Pointer inputs pass through unchanged. maxTokens is stored as NumPredict
// (Ollama's convention); nil leaves NumPredict at zero so the provider picks
// a default. Empty stop slices are normalized to nil. Empty-string stop
// entries are filtered out: Ollama treats "" as "stop immediately" which
// would truncate generation to zero/one token with finish_reason="stop",
// silently masking a caller mistake ({"stop": ""} or {"stop": [""]}).
func toModelOptions(temperature, topP *float64, maxTokens *int, stop []string) provider.ModelOptions {
	opts := provider.ModelOptions{
		Temperature: temperature,
		TopP:        topP,
	}
	if maxTokens != nil {
		opts.NumPredict = *maxTokens
	}
	if len(stop) > 0 {
		filtered := make([]string, 0, len(stop))
		for _, s := range stop {
			if s != "" {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) > 0 {
			opts.Stop = filtered
		}
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
