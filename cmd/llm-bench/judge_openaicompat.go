package main

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// openAICompatJudgeClient adapts an *openaicompat.Provider to the Ollama-typed
// judgeChatClient + judgeModelChecker seams the LLMJudgeScorer is built on. It
// is a judge-only transport: it translates the narrow judge ChatRequest
// (system + user messages, temperature, num_predict) to an OpenAI
// /v1/chat/completions call and maps the response content back, leaving the
// judge prompt contract and parser untouched.
//
// JSON-mode note: the Ollama path sets ChatRequest.Format="json", but the
// openaicompat provider exposes no response_format field. The judge system
// prompt already mandates strict JSON and parseJudgeResponse extracts the
// first balanced JSON object, so a frontier judge stays robust on the prompt
// alone; Format is intentionally dropped here rather than widening the
// provider package for one caller.
type openAICompatJudgeClient struct {
	provider *openaicompat.Provider
}

// newOpenAICompatJudge wraps an openaicompat provider as a judge transport.
func newOpenAICompatJudge(p *openaicompat.Provider) *openAICompatJudgeClient {
	return &openAICompatJudgeClient{provider: p}
}

// Chat implements judgeChatClient by translating the Ollama judge request to a
// provider.ChatRequest, issuing it through the openaicompat provider, and
// mapping provider.ChatResponse.Content back onto ollama.ChatResponse so the
// scorer's parser sees an unchanged shape.
func (j *openAICompatJudgeClient) Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	presp, err := j.provider.Chat(ctx, provider.ChatRequest{
		Model:    req.Model,
		Messages: toProviderJudgeMessages(req.Messages),
		Options:  toProviderJudgeOptions(req.Options),
	})
	if err != nil {
		return nil, err
	}
	if presp == nil {
		return nil, fmt.Errorf("openai-compat judge: nil response")
	}
	return &ollama.ChatResponse{
		Model: presp.Model,
		Message: ollama.ChatMessage{
			Role:    "assistant",
			Content: presp.Content,
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
	temp := opts.Temperature
	return provider.ModelOptions{
		Temperature: &temp,
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
