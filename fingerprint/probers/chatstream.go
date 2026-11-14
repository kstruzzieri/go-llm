package probers

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/provider"
)

// Compile-time check: the prober runs the streamed recipes.
var _ fingerprint.CapabilityProber = (*OpenAICompatProber)(nil)

// ProbeCapabilities runs a streamed recipe, chat-stream or chat-stream-tools,
// and judges the reply by the recipe's contract. Every request is a bounded
// request (responsebound) under its own recipe timeout: a byte limit, no
// redirect, and a rejection whose body cannot be read is a failure.
//
// A completed stream (a final chunk and usage) proves chat and stream. For
// chat-stream-tools it also decides tool_call: yes when every emitted call
// meets completeToolCall, otherwise an inconclusive attempt with one of the
// Detail codes. A 400 or 422 on the first tools request sends a second without
// tool_choice; on the second it records request_rejected. Any other failure,
// and a cancelled caller, returns an error and no answers: no failure yields a
// no. result.Requests counts the requests sent, on failure too.
func (p *OpenAICompatProber) ProbeCapabilities(ctx context.Context, model string, req fingerprint.CapabilityProbeRequest) (fingerprint.CapabilityProbeResult, error) {
	if p == nil || p.prov == nil {
		return fingerprint.CapabilityProbeResult{}, fmt.Errorf("fingerprint: openaicompat probe %q: provider is required", model)
	}
	recipe, err := fingerprint.RecipeForRequest(req, fingerprint.AdapterOpenAICompat)
	if err != nil {
		return fingerprint.CapabilityProbeResult{}, fmt.Errorf("fingerprint: openaicompat probe %q: %w", model, err)
	}
	if model == "" {
		return fingerprint.CapabilityProbeResult{}, errors.New("fingerprint: openaicompat probe: model name is required")
	}
	wrap := func(err error) error {
		return fmt.Errorf("fingerprint: openaicompat %s probe %q: %w", recipe.Method, model, err)
	}
	tools := recipe.ToolName != ""
	result := fingerprint.CapabilityProbeResult{Method: recipe.Method}
	for i := range recipe.MaxRequests {
		// Checked again here so a caller who cancels just after a clean
		// rejection gets no second request.
		if err := ctx.Err(); err != nil {
			return result, wrap(err)
		}
		final, err := p.streamOnce(ctx, model, recipe, i == 0)
		// Requests counts what was sent. Two refusals send nothing: a body
		// the client's content guard refuses, and a destination denial from
		// the guarded transport or gate, which is a *DestinationDeniedError.
		// A refused redirect also matches ErrDestinationDenied but is not
		// that type, and follows a request that was sent, so it counts.
		var denied *provider.DestinationDeniedError
		if !errors.Is(err, provider.ErrContentDenied) && !errors.As(err, &denied) {
			result.Requests++
		}
		if err == nil {
			p.observe(model, final.Usage)
			result.Answers = streamAnswers(final, recipe, i > 0)
			return result, nil
		}
		// The order of these checks is the contract.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, wrap(ctxErr)
		}
		var unreadable *responsebound.RejectionReadError
		if errors.As(err, &unreadable) {
			return result, wrap(err)
		}
		if tools && rejected(err) {
			continue
		}
		if errors.Is(err, bufio.ErrTooLong) {
			// One line over the reader's cap reads as one body over the limit.
			err = responsebound.ErrExceeded
		}
		return result, wrap(err)
	}
	// Only a tools recipe gets here: every request was rejected.
	result.Answers = []fingerprint.ProbeAnswer{toolAttempt(fingerprint.DetailRequestRejected)}
	return result, nil
}

// rejected reports whether err carries status 400 or 422.
func rejected(err error) bool {
	var hs interface{ HTTPStatusCode() int }
	if !errors.As(err, &hs) {
		return false
	}
	code := hs.HTTPStatusCode()
	return code == 400 || code == 422
}

// streamOnce sends one recipe request under its own timeout and returns the
// stream's final response: metadata, usage, finish reason and tool calls.
// Content and thinking deltas are discarded.
func (p *OpenAICompatProber) streamOnce(ctx context.Context, model string, recipe fingerprint.ProbeRecipe, first bool) (provider.ChatResponse, error) {
	ctx, cancel := context.WithTimeout(responsebound.WithLimit(ctx, recipe.MaxResponseBytes), cmp.Or(p.requestTimeout, recipe.RequestTimeout))
	defer cancel()
	req := provider.ChatRequest{
		Model:    model,
		Messages: []provider.ChatMessage{{Role: "user", Content: recipe.Message}},
		Options:  provider.ModelOptions{NumPredict: recipe.MaxOutputTokensPerRequest},
	}
	if recipe.ToolName != "" {
		req.Tools = []provider.Tool{{Type: "function", Function: provider.ToolFunction{
			Name: recipe.ToolName, Description: recipe.ToolDescription, Parameters: json.RawMessage(recipe.ToolParameters),
		}}}
		if first && recipe.RequiredToolChoiceFirst {
			req.ToolChoice = "required"
		}
	}
	send := p.streamChat
	if send == nil {
		send = p.prov.ChatStream
	}
	var final provider.ChatResponse
	var done bool
	err := send(ctx, req, func(r provider.ChatResponse) error {
		if r.Done { // a partial final comes only with an error, which discards it
			final, done = r, true
		}
		return nil
	})
	if err == nil && !done {
		err = errors.New("stream ended without a final response")
	}
	return final, err
}

// streamAnswers classifies a completed stream. retried is true for the second
// request.
func streamAnswers(final provider.ChatResponse, recipe fingerprint.ProbeRecipe, retried bool) []fingerprint.ProbeAnswer {
	answers := []fingerprint.ProbeAnswer{probeYes("chat", ""), probeYes("stream", "")}
	if recipe.ToolName == "" {
		return answers
	}
	complete, misidentified := 0, false
	for _, c := range final.ToolCalls {
		if completeToolCall(c, recipe.ToolName) {
			complete++
		}
		if c.Function.Name != recipe.ToolName || c.ID == "" || c.Type != "function" {
			misidentified = true
		}
	}
	calls := len(final.ToolCalls)
	var tool fingerprint.ProbeAnswer
	switch {
	case calls > 0 && complete == calls:
		tool = probeYes("tool_call", fingerprint.DetailToolCalled)
	case misidentified || complete > 0:
		tool = toolAttempt(fingerprint.DetailToolCallInvalid)
	case final.FinishReason == "length":
		tool = toolAttempt(fingerprint.DetailCutOff)
	case calls > 0:
		tool = toolAttempt(fingerprint.DetailToolCallInvalid)
	case retried:
		tool = toolAttempt(fingerprint.DetailToolNotCalled)
	default:
		tool = toolAttempt(fingerprint.DetailToolIgnored)
	}
	return append(answers, tool)
}

func probeYes(capability, detail string) fingerprint.ProbeAnswer {
	return fingerprint.ProbeAnswer{
		Capability: capability, Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer,
		State: fingerprint.CapProbeYes, Detail: detail,
	}
}

func toolAttempt(detail string) fingerprint.ProbeAnswer {
	return fingerprint.ProbeAnswer{
		Capability: "tool_call", Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAttempt,
		State: fingerprint.CapProbeInconclusive, Detail: detail, TTL: fingerprint.CapProbeInconclusiveTTL,
	}
}

// completeToolCall reports whether c meets the probe's certification rule:
// the expected name, a call ID, function type, and arguments that, trimmed,
// are a JSON object. The dispatcher checks only the name and json.Valid, so
// this is the stricter of the two on purpose.
func completeToolCall(c provider.ToolCall, expectedName string) bool {
	args := bytes.TrimSpace(c.Function.Arguments)
	return c.Function.Name == expectedName && c.ID != "" && c.Type == "function" &&
		len(args) > 0 && args[0] == '{' && json.Valid(args)
}
