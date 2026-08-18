package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// openAICompatJudgeClient adapts an *openaicompat.Provider to the Ollama-typed
// judgeChatClient + judgeModelChecker seams the LLMJudgeScorer is built on. It
// is a judge-only transport: it translates the narrow judge ChatRequest
// (system + user messages, temperature, num_predict, think directive) to an
// OpenAI /v1/chat/completions call and maps the response content back —
// falling back to the server-separated reasoning when content is empty —
// leaving the judge prompt contract and parser untouched.
//
// JSON-mode note: the Ollama path sets ChatRequest.Format="json", but the
// openaicompat provider exposes no response_format field. The judge system
// prompt already mandates strict JSON and parseJudgeResponse extracts the
// first balanced JSON object, so a frontier judge stays robust on the prompt
// alone; Format is intentionally dropped here rather than widening the
// provider package for one caller.
type openAICompatJudgeClient struct {
	provider *openaicompat.Provider
	// disableThinking, when true, translates the judge's request-level
	// Think directive to the wire (chat_template_kwargs.enable_thinking).
	// Off by default because that kwarg is a llama.cpp/vLLM template
	// extension: api.openai.com — the documented frontier-judge endpoint —
	// rejects unrecognized top-level arguments with HTTP 400, so the wire
	// must stay byte-identical there. Set from -judge-disable-thinking.
	disableThinking bool
}

// newOpenAICompatJudge wraps an openaicompat provider as a judge transport.
func newOpenAICompatJudge(p *openaicompat.Provider) *openAICompatJudgeClient {
	return &openAICompatJudgeClient{provider: p}
}

// Chat implements judgeChatClient by translating the Ollama judge request to a
// provider.ChatRequest, issuing it through the openaicompat provider, and
// mapping provider.ChatResponse.Content back onto ollama.ChatResponse —
// substituting the server-separated reasoning when content is empty and the
// reply finished under the token budget — so the scorer's parser sees an
// unchanged shape.
func (j *openAICompatJudgeClient) Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	popts := toProviderJudgeOptions(req.Options)
	if j.disableThinking {
		// The judge contract sets Think=false; translating it lets
		// applyOptionsChat emit chat_template_kwargs.enable_thinking=false so
		// a thinking-tuned judge model doesn't reason its token budget away
		// before the verdict. Opt-in only — see the field doc.
		popts.Think = req.Think
	}
	presp, err := j.provider.Chat(ctx, provider.ChatRequest{
		Model:    req.Model,
		Messages: toProviderJudgeMessages(req.Messages),
		Options:  popts,
	})
	if err != nil {
		return nil, err
	}
	if presp == nil {
		return nil, fmt.Errorf("openai-compat judge: nil response")
	}
	content := presp.Content
	if strings.TrimSpace(content) == "" && strings.TrimSpace(presp.Thinking) != "" {
		// Templates that ignore enable_thinking (gemma4, measured 2026-08-16)
		// can leave content empty with the verdict in reasoning_content, which
		// the provider surfaces as Thinking. Non-empty content always wins;
		// this fires only where the alternative is a guaranteed hard failure.
		if popts.NumPredict > 0 && presp.Usage.CompletionTokens >= popts.NumPredict {
			// A reply that burned the whole budget with no content is a
			// deliberation truncated mid-reasoning, not a routed verdict. The
			// judge system prompt embeds an on-grid example object, so a
			// truncated echo of the schema would be minted into a real —
			// and cached — score if handed to the last-verdict-wins parser.
			// (Servers that omit usage report 0 tokens and skip this guard;
			// llama.cpp always reports usage.)
			return nil, fmt.Errorf("%w: judge burned its %d-token budget mid-reasoning with no verdict content; refusing reasoning-content fallback", errMalformedJudgeResponse, popts.NumPredict)
		}
		_, _ = fmt.Fprintf(os.Stderr, "llm-bench: judge content empty; substituting reasoning_content (model=%s, %d chars)\n", req.Model, len(presp.Thinking))
		content = presp.Thinking
	}
	return &ollama.ChatResponse{
		Model: presp.Model,
		Message: ollama.ChatMessage{
			Role:    "assistant",
			Content: content,
		},
		Done: true,
	}, nil
}

// toProviderJudgeMessages copies the role/content of each judge message. The
// judge only ever sends system + user turns, so tool fields are not carried.
func toProviderJudgeMessages(in []ollama.ChatMessage) []provider.ChatMessage {
	out := make([]provider.ChatMessage, len(in))
	for i, m := range in {
		out[i] = provider.ChatMessage{Role: m.Role, Content: m.Content}
	}
	return out
}

// toProviderJudgeOptions maps the judge's temperature + token budget. The
// temperature pointer is always set when options are present: the judge wants
// a specific low temperature, and a frontier endpoint accepts an explicit
// value (including 0) the same way Ollama does.
func toProviderJudgeOptions(opts *ollama.ModelOptions) provider.ModelOptions {
	if opts == nil {
		return provider.ModelOptions{}
	}
	return provider.ModelOptions{
		Temperature: opts.Temperature,
		NumPredict:  opts.NumPredict,
	}
}

// AvailableModels implements judgeModelChecker via /v1/models so judge-model
// validation works on this transport.
func (j *openAICompatJudgeClient) AvailableModels(ctx context.Context) ([]string, error) {
	models, err := j.provider.Models(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names, nil
}

// ShowModel implements judgeModelChecker. OpenAI-compatible servers expose no
// /api/show digest equivalent, so this returns (nil, nil); resolveJudgeDigest
// degrades to an empty digest. No network call is made.
func (j *openAICompatJudgeClient) ShowModel(_ context.Context, _ string) (*ollama.ModelInfo, error) {
	return nil, nil
}
