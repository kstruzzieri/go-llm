package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// replayInjection sends the frozen decision-point wire with tools exposed and
// returns exactly one candidate turn (spec §4.6). Model behavior (malformed
// calls, empty replies) is never an error; only transport failures are.
func replayInjection(ctx context.Context, client candidateChatClient, model string, trace Trace, opts replayOptions) (replayOutput, error) {
	tools, err := decodeTraceTools(trace.Tools)
	if err != nil {
		return replayOutput{}, fmt.Errorf("trace %q: %w", trace.ID, err)
	}
	messages := make([]ollama.ChatMessage, 0, len(trace.Turns)+1)
	messages = append(messages, ollama.ChatMessage{Role: "system", Content: trace.System})
	rawArgs := make([][]json.RawMessage, 1, len(trace.Turns)+1)
	for i, turn := range trace.Turns {
		msg, err := prefilledChatMessage(turn)
		if err != nil {
			return replayOutput{}, fmt.Errorf("trace %q turn %d: %w", trace.ID, i, err)
		}
		messages = append(messages, msg)
		rawArgs = append(rawArgs, prefilledRawToolArgs(turn))
	}
	req := candidateChatRequest{
		ChatRequest:     ollama.ChatRequest{Model: model, Messages: messages, Tools: tools, KeepAlive: benchKeepAlive},
		RawToolArgs:     rawArgs,
		KeepRawCalls:    true,
		ProviderOptions: opts.ProviderOptions,
		ParseThinkMode:  opts.ParseThinkMode,
		SessionID:       opts.SessionID,
	}
	turnCtx := ctx
	if opts.PerTurnTimeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, opts.PerTurnTimeout)
		defer cancel()
	}
	start := time.Now()
	resp, err := client.Chat(turnCtx, req)
	if err != nil {
		return replayOutput{}, err
	}
	turn, err := assistantTurnFromMessage(resp.Message)
	if err != nil {
		return replayOutput{}, err
	}
	turn.Thinking = resp.Thinking
	out := replayOutput{Transcript: []Turn{turn}, RawToolCalls: resp.RawToolCalls}
	out.TurnLatenciesMs = append(out.TurnLatenciesMs, time.Since(start).Milliseconds())
	out.addUsage(tokenUsage{PromptEval: resp.PromptEvalCount, Gen: resp.EvalCount, Thinking: resp.ThinkingTokens, ThinkingComputed: resp.ThinkingTokensComputed})
	return out, nil
}
