package compat

import (
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

// CompletionRequest is the OpenAI-shape POST /v1/completions body. Fields
// prefixed with x_ are go-llm-specific extensions; OpenAI SDKs ignore unknown
// fields so the wire remains compatible.
//
// The Prompt field accepts either a single JSON string or a JSON array of
// strings; see PromptUnion for the array-form policy. Suffix is optional and,
// when non-empty, triggers the Fill-in-the-Middle routing branch
// (CapGenerate|CapInsert required, priority elevated to PriorityHigh).
type CompletionRequest struct {
	Model       string        `json:"model"`
	Prompt      PromptUnion   `json:"prompt"`
	Suffix      string        `json:"suffix,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	Stop        StopSequences `json:"stop,omitempty"`

	// Extensions. All optional.
	Language    string `json:"x_language,omitempty"`
	FilePath    string `json:"x_file_path,omitempty"`
	UseCase     string `json:"x_use_case,omitempty"`
	Priority    *int   `json:"x_priority,omitempty"`
	AffinityKey string `json:"x_affinity_key,omitempty"`
	DryRun      bool   `json:"x_dry_run,omitempty"`
}

// CompletionResponse is the OpenAI-shape response body. The x_ fields extend
// it with go-llm routing metadata and confidence scoring.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"` // "text_completion"
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   UsageWire          `json:"usage"`

	// Extensions.
	CompletionID string        `json:"x_completion_id,omitempty"`
	Confidence   *float64      `json:"x_confidence,omitempty"`
	RouteInfo    *RouteInfoExt `json:"x_route_info,omitempty"`
}

// CompletionChoice is one completion in an OpenAI /v1/completions response.
type CompletionChoice struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// CompletionChunk is one frame of an OpenAI-shape streaming /v1/completions
// response. Each chunk carries exactly one CompletionChunkChoice today. Usage
// is not included on per-chunk frames — OpenAI emits usage only on the final
// frame when stream_options.include_usage is set, which we don't yet expose.
type CompletionChunk struct {
	ID      string                  `json:"id"`
	Object  string                  `json:"object"` // "text_completion"
	Created int64                   `json:"created"`
	Model   string                  `json:"model"`
	Choices []CompletionChunkChoice `json:"choices"`

	// Extensions. Populated only on the final (Done) chunk so interim frames
	// stay minimal; omitempty + nil pointer keep the wire unchanged for
	// non-final chunks. Mirrors CompletionResponse.CompletionID / Confidence
	// so clients that toggle stream:true see the same extension fields.
	CompletionID string   `json:"x_completion_id,omitempty"`
	Confidence   *float64 `json:"x_confidence,omitempty"`
}

// CompletionChunkChoice is one choice inside a streaming completion chunk.
// FinishReason is a pointer so interim chunks serialize as
// "finish_reason":null and only the final chunk carries a non-null reason,
// matching OpenAI's wire format.
type CompletionChunkChoice struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// PromptUnion decodes OpenAI's "prompt" field which may be either a single
// string or a JSON array of strings. Both shapes unmarshal to []string so the
// handler can treat the payload uniformly.
//
// Array form: OpenAI treats each element as an independent completion and
// returns a multi-choice response. go-llm returns a single-choice response,
// so the array form here is reduced to its first non-empty element — see
// String(). Joining the elements would silently reshape the prompt; picking
// the first non-empty element is unambiguous and matches typical IDE usage
// where clients pass a single string anyway.
type PromptUnion []string

// UnmarshalJSON accepts JSON null, a single string, or a []string. In the
// array form, empty strings are filtered out to match StopSequences semantics
// (an empty element is always a caller mistake). An unrecognized JSON kind
// (object, number, bool) is rejected so the client gets a precise 400 rather
// than a silent empty-prompt error downstream.
func (p *PromptUnion) UnmarshalJSON(data []byte) error {
	// JSON null → empty slice (same as an empty string).
	if string(data) == "null" {
		*p = nil
		return nil
	}
	// Try array first — canonical multi-prompt shape.
	var arr []string
	if err := json.Unmarshal(data, &arr); err == nil {
		filtered := arr[:0]
		for _, v := range arr {
			if v != "" {
				filtered = append(filtered, v)
			}
		}
		if len(filtered) == 0 {
			*p = nil
		} else {
			*p = filtered
		}
		return nil
	}
	// Fall back to single string.
	var single string
	if err := json.Unmarshal(data, &single); err != nil {
		return fmt.Errorf("compat: prompt must be string or []string: %w", err)
	}
	if single == "" {
		*p = nil
	} else {
		*p = []string{single}
	}
	return nil
}

// String returns the first non-empty element of the prompt, or "" if none.
// This collapses the array form to a single completion prompt; see the
// PromptUnion godoc for the rationale.
func (p PromptUnion) String() string {
	for _, v := range p {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// handleCompletions implements POST /v1/completions (non-streaming). Streaming
// requests currently return 501; Task 13 replaces the stub with the real SSE
// writer.
//
// Order of operations:
//  1. Decode body and validate required fields.
//  2. Resolve model alias -> provider.ModelKey.
//  3. Build provider.RoutingRequest:
//     - When Suffix is non-empty, take the FIM branch (CapGenerate|CapInsert,
//     PriorityHigh, UseCase="fim") regardless of caller-supplied x_priority.
//     - Otherwise take the generate branch (CapGenerate, default priority).
//  4. If streaming, dispatch to the 501 stub.
//  5. Acquire a concurrency slot using the resolved priority — AFTER the
//     FIM/generate branch sets rr.Priority so FIM gets its elevated slot.
//  6. Route via Router.Route. On error, map via writeCompatError.
//  7. If DryRun, render route metadata with empty choice text and return.
//     This runs BEFORE applyFIMBudget so the metadata describes the plan
//     as it would actually be routed, not a budget-mutated copy that never
//     executes.
//  8. FIM branch: apply the adaptive FIM budget to the routing request IN
//     PLACE before execution so the provider receives trimmed prefix/suffix.
//  9. Execute via RoutePlan.ExecuteGenerate and render the OpenAI response.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if s.router == nil {
		writeError(w, http.StatusServiceUnavailable, "no_router", "server has no router")
		return
	}

	var req CompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "decode_error", err.Error())
		return
	}
	key, err := resolveModel(req.Model, s.aliases)
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing_model", err.Error())
		return
	}
	// A request with no prompt and no suffix has nothing to complete. An empty
	// prompt with a non-empty suffix is a valid FIM pattern (completing at the
	// very start of a file), so we only reject when both are empty.
	prompt := req.Prompt.String()
	if prompt == "" && req.Suffix == "" {
		writeError(w, http.StatusBadRequest, "empty_prompt", "prompt and suffix are both empty")
		return
	}

	rr := provider.RoutingRequest{
		Model:       selectorFor(key),
		Prompt:      prompt,
		Suffix:      req.Suffix,
		Options:     toModelOptions(req.Temperature, req.TopP, req.MaxTokens, []string(req.Stop)),
		AffinityKey: req.AffinityKey,
		DryRun:      req.DryRun,
	}
	fim := req.Suffix != ""
	if fim {
		// FIM branch: Router.Generate mirrors these fields when Suffix is
		// non-empty, so we match them exactly for consistency.
		rr.UseCase = "fim"
		rr.RequiredCaps = provider.CapGenerate | provider.CapInsert
		rr.Priority = provider.PriorityHigh
	} else {
		rr.UseCase = firstNonEmpty(req.UseCase, "generate")
		rr.RequiredCaps = provider.CapGenerate
		rr.Priority = resolvePriority(req.Priority, provider.PriorityNormal)
	}

	if req.Stream {
		s.serveCompletionsStream(w, r, rr, req.FilePath, fim)
		return
	}

	// Acquire the HTTP concurrency slot AFTER FIM/generate branching so the
	// resolved priority (PriorityHigh for FIM, default for generate) drives
	// admission control.
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

	// Dry-run short-circuits BEFORE applyFIMBudget so the returned route
	// metadata reflects the plan that would actually be routed (unmutated
	// prefix/suffix). Reporting a budget-mutated plan that never executes
	// would be phantom state.
	if req.DryRun {
		writeCompletionDryRun(w, r, plan)
		return
	}

	// Apply the adaptive FIM budget in place on the plan's request snapshot so
	// buildGenerateRequest picks up the trimmed prefix/suffix. No-op for the
	// generate branch (suffix is empty).
	if req.Suffix != "" {
		applyFIMBudget(plan, req.Language, prompt, req.Suffix, req.MaxTokens)
	}

	resp, err := plan.ExecuteGenerate(r.Context())
	if err != nil {
		writeCompatError(w, err)
		return
	}

	// Derive finish_reason from usage: "length" when MaxTokens was set and the
	// response exhausted the budget, else "stop". We do not emit "tool_calls"
	// here — the completions endpoint is not tool-aware.
	// TODO(#48): Ollama's done_reason isn't surfaced by provider — finish_reason
	// may misreport "stop" when the upstream actually hit num_predict.
	finish := "stop"
	if req.MaxTokens != nil && *req.MaxTokens > 0 && resp.Usage.CompletionTokens >= *req.MaxTokens {
		finish = "length"
	}

	id := completionResponseID(r.Context())
	routeReason := ""
	if resp.RouteOutcome != nil {
		routeReason = resp.RouteOutcome.Reason
	}
	s.completionStore.put(CompletionRecord{
		ID:        id,
		Provider:  resp.Provider,
		Model:     plan.Profile.Key.String(),
		UseCase:   rr.UseCase,
		FilePath:  req.FilePath,
		RouteInfo: routeReason,
	})
	out := CompletionResponse{
		ID:      id,
		Object:  "text_completion",
		Created: time.Now().Unix(),
		// Qualified "provider/model" form taken from the plan so success and
		// dry-run responses have identical shape even when resp.Model omits
		// the provider prefix.
		Model: plan.Profile.Key.String(),
		Choices: []CompletionChoice{{
			Index:        0,
			Text:         resp.Response,
			FinishReason: finish,
		}},
		Usage: UsageWire{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		CompletionID: id,
		RouteInfo:    routeInfoFrom(resp.RouteOutcome),
	}
	if resp.Confidence != nil {
		score := resp.Confidence.Score
		out.Confidence = &score
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// serveCompletionsStream handles the streaming branch of POST /v1/completions.
// It emits an SSE (text/event-stream) body whose events each carry a JSON
// CompletionChunk, terminated by a "data: [DONE]" sentinel.
//
// Order of operations:
//  1. Broaden rr.RequiredCaps with CapStream so the router rejects any
//     provider that cannot stream before any bytes are written.
//  2. Acquire the HTTP concurrency slot using the resolved priority — the
//     slot is released when the stream completes.
//  3. Route via Router.Route. On error, writeCompatError still works
//     because no bytes have been written yet.
//  4. Construct a lazy-start sseWriter — it does NOT commit the 200 status
//     until the first chunk is written, so a pre-first-chunk
//     ExecuteGenerateStream failure can still be surfaced as a regular JSON
//     error envelope. Without this guard a failure that happens before the
//     first chunk is written as an empty 200 response, which is
//     indistinguishable from a successful zero-token stream.
//  5. For each chunk, emit a CompletionChunk with the qualified model ID and,
//     on the Done chunk, a derived finish_reason matching the non-streaming
//     branch's logic ("length" when NumPredict was set and usage reached the
//     cap, "stop" otherwise — completions don't emit tool_calls).
//  6. After ExecuteGenerateStream returns, emit "data: [DONE]" only on clean
//     completion. If the failure happened before the first chunk, surface a
//     JSON error envelope. If the failure happened mid-stream, skip the
//     [DONE] sentinel so clients can detect premature EOS via its absence;
//     context.Canceled (client disconnect — e.g. IDE cancels completion on
//     every keystroke) is treated as normal and its log is suppressed to
//     prevent spam.
//
// FIM budget is not applied on the streaming path — a streaming client
// selects num_predict via the provider's live back-pressure rather than a
// pre-truncation budget. Non-streaming callers still get applyFIMBudget.
// If maintainers later want streaming-FIM budgeting, they can wire
// req.MaxTokens through rr.Options.NumPredict.
func (s *Server) serveCompletionsStream(w http.ResponseWriter, r *http.Request, rr provider.RoutingRequest, filePath string, fim bool) {
	_ = fim // reserved for Task 14's completion store; currently unused

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
	// provider so every chunk carries stable identifiers. completionResponseID
	// falls back to a random suffix when no request-id middleware has run.
	id := completionResponseID(r.Context())
	created := time.Now().Unix()
	modelID := plan.Profile.Key.String()

	var lastConfidence *float64
	var sawDone bool

	streamErr := plan.ExecuteGenerateStream(r.Context(), func(chunk provider.GenerateResponse) error {
		// Derive finish_reason on the Done chunk. Completions are not
		// tool-aware, so the rule collapses to "length" vs "stop".
		// TODO(#48): Ollama's done_reason isn't surfaced by provider —
		// finish_reason may misreport "stop" when the upstream actually hit
		// num_predict but arrived with CompletionTokens==0. Same caveat as
		// the non-streaming branch; a future DoneReason plumbing will fix
		// both sites.
		var finish *string
		if chunk.Done {
			sawDone = true
			reason := "stop"
			if rr.Options.NumPredict > 0 && chunk.Usage.CompletionTokens >= rr.Options.NumPredict {
				reason = "length"
			}
			finish = &reason
			if chunk.Confidence != nil {
				v := chunk.Confidence.Score
				lastConfidence = &v
			}
			routeReason := ""
			if chunk.RouteOutcome != nil {
				routeReason = chunk.RouteOutcome.Reason
			}
			s.completionStore.put(CompletionRecord{
				ID:        id,
				Provider:  chunk.Provider,
				Model:     modelID,
				UseCase:   rr.UseCase,
				FilePath:  filePath,
				RouteInfo: routeReason,
			})
		}
		out := CompletionChunk{
			ID:      id,
			Object:  "text_completion",
			Created: created,
			Model:   modelID,
			Choices: []CompletionChunkChoice{{
				Index:        0,
				Text:         chunk.Response,
				FinishReason: finish,
			}},
		}
		// Surface x_completion_id / x_confidence only on the final frame so
		// interim chunks stay minimal (omitempty + nil pointer). This mirrors
		// the non-streaming CompletionResponse shape — toggling stream:true
		// must not silently drop those extensions.
		if chunk.Done {
			out.CompletionID = id
			out.Confidence = lastConfidence
		}
		return sw.writeEvent(out)
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
			log.Printf("compat: generate stream error rid=%s: %v", requestIDFrom(r.Context()), streamErr)
		}
		return
	}

	// A nil streamErr with no terminal Done chunk is a misbehaving provider
	// (or upstream wrapper that swallowed the terminator). Emitting [DONE]
	// here would tell the SDK the stream completed successfully even though
	// we never saw a Done frame. Skip the sentinel so clients detect the
	// premature EOS, and log one breadcrumb with the request ID so the
	// misbehavior is observable in logs.
	if !sawDone {
		log.Printf("compat: generate stream ended without Done chunk rid=%s", requestIDFrom(r.Context()))
		return
	}

	_ = sw.writeDone()
}

// writeCompletionDryRun renders the route decision without executing the
// request. The response carries a single empty-text choice and a populated
// x_route_info block so clients can observe the chosen model.
//
// Unlike the chat dry-run (which uses an empty Choices slice), the completions
// endpoint always emits one choice so OpenAI SDKs that iterate resp.choices[0]
// do not index out of bounds on dry-run responses. Text is empty — clients
// should treat dry-run as metadata-only.
func writeCompletionDryRun(w http.ResponseWriter, r *http.Request, plan *provider.RoutePlan) {
	id := completionResponseID(r.Context())
	out := CompletionResponse{
		ID:      id,
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   plan.Profile.Key.String(),
		Choices: []CompletionChoice{{
			Index:        0,
			Text:         "",
			FinishReason: "stop",
		}},
		CompletionID: id,
		RouteInfo: &RouteInfoExt{
			ActualModel:  plan.Profile.Key.String(),
			PlannedModel: plan.Profile.Key.String(),
			WasSticky:    plan.WasSticky(),
			Score:        plan.Score,
			Reason:       plan.Reason,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// applyFIMBudget mutates plan.Request.Prompt and plan.Request.Suffix to stay
// within the computed prefix/suffix token budget. It uses a 4-chars-per-token
// approximation — good enough for the edge-trimming policy below, and avoids
// pulling in a tokenizer. When maxTokens is nil or <= 0 the step is skipped
// (no caller-supplied budget to enforce).
//
// Truncation policy: preserve the TAIL of the prefix (the text closest to the
// cursor) and the HEAD of the suffix (the text immediately after the cursor).
// That keeps the span most meaningful to the model; the opposite end is the
// natural candidate for truncation.
func applyFIMBudget(plan *provider.RoutePlan, language, prompt, suffix string, maxTokens *int) {
	if maxTokens == nil || *maxTokens <= 0 || plan == nil || plan.Profile == nil {
		return
	}
	var overridePct int
	if plan.Profile.FIM != nil {
		overridePct = plan.Profile.FIM.PrefixBudgetPct
	}
	prefixTok := (len(prompt) / 4) + 1
	suffixTok := (len(suffix) / 4) + 1
	b := budgetForProfile(plan.Profile.Family, language, overridePct, prefixTok, suffixTok, *maxTokens)
	// Guard against zero/negative budgets (e.g. max_tokens=1): without this,
	// keepChars=0 would silently set Prompt/Suffix to "" and the provider
	// would see an empty FIM request. Preserve the original content instead
	// and log one breadcrumb per occurrence so the misconfiguration is
	// observable.
	if b.Prefix <= 0 {
		log.Printf("compat: applyFIMBudget: zero prefix budget (max_tokens=%d, family=%s, lang=%s); preserving original prefix",
			*maxTokens, plan.Profile.Family, language)
	} else if b.Prefix < prefixTok {
		keepChars := b.Prefix * 4
		if keepChars > 0 && keepChars < len(prompt) {
			plan.Request.Prompt = prompt[len(prompt)-keepChars:]
		}
	}
	if b.Suffix <= 0 {
		log.Printf("compat: applyFIMBudget: zero suffix budget (max_tokens=%d, family=%s, lang=%s); preserving original suffix",
			*maxTokens, plan.Profile.Family, language)
	} else if b.Suffix < suffixTok {
		keepChars := b.Suffix * 4
		if keepChars > 0 && keepChars < len(suffix) {
			plan.Request.Suffix = suffix[:keepChars]
		}
	}
}

// completionResponseID builds the "cmpl_<suffix>" response ID. It prefers the
// request ID attached by requestIDMiddleware (so HTTP access logs and response
// bodies correlate), but falls back to a freshly generated random suffix when
// the context carries none — e.g. when handlers are invoked directly via the
// mux for tests or embedded callers. Without the fallback, bypass paths would
// return "cmpl_" verbatim, which is indistinguishable from a bug.
//
// NOTE: we deliberately do NOT reuse fallbackRequestID() from chat.go — that
// helper prepends its own "cmpl_" prefix, which would yield "cmpl_cmpl_<hex>"
// when composed here.
func completionResponseID(ctx context.Context) string {
	rid := requestIDFrom(ctx)
	if rid == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		rid = hex.EncodeToString(b[:])
	}
	return "cmpl_" + rid
}
