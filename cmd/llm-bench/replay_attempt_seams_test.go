package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestToolArgumentsErrorKeepsLegacyText(t *testing.T) {
	_, err := toOllamaCandidateToolCalls([]provider.ToolCall{{
		ID:       "c1",
		Type:     "function",
		Function: provider.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(`"not json"`)},
	}})
	if err == nil {
		t.Fatal("want an argument decode error")
	}
	var m map[string]any
	inner := json.Unmarshal([]byte(`"not json"`), &m)
	want := fmt.Sprintf("openai-compat candidate tool %q arguments: %v", "lookup", inner)
	if err.Error() != want {
		t.Fatalf("error text changed:\n got %q\nwant %q", err.Error(), want)
	}
	var argErr *toolArgumentsError
	if !errors.As(err, &argErr) {
		t.Fatalf("errors.As(*toolArgumentsError) = false for %T", err)
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("inner *json.UnmarshalTypeError no longer reachable: %v", err)
	}
}

func TestNewCandidateTransportUsesInjectedHTTPClient(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: attemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(compatText("ok", ""))),
			Request:    r,
		}, nil
	})}
	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/cand", Provider: openAICompatTransport, Model: "cand"}, candidateTransportOptions{
		openAICompatBaseURL: "http://127.0.0.1:1", // unroutable: only the injected client can answer
		timeout:             time.Second,
		httpClient:          client,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}
	resp, err := tr.chat.Chat(context.Background(), candidateChatRequest{ChatRequest: ollama.ChatRequest{
		Model:    "cand",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "q"}},
	}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if calls.Load() != 1 || resp.Message.Content != "ok" {
		t.Fatalf("calls = %d, content = %q; want 1 call answered by the injected client", calls.Load(), resp.Message.Content)
	}
}

// divergenceChat answers each Chat call with the next scripted reply.
type divergenceChat struct {
	replies []candidateChatResponse
	calls   int
}

func (d *divergenceChat) Chat(context.Context, candidateChatRequest) (candidateChatResponse, error) {
	r := d.replies[d.calls]
	d.calls++
	return r, nil
}

func divergenceText(content string) candidateChatResponse {
	return candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant", Content: content}}
}

func divergenceToolCall(content string) candidateChatResponse {
	return candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant", Content: content, ToolCalls: []ollama.ToolCall{
		{Function: ollama.ToolCallFunction{Name: "lookup", Arguments: map[string]any{"q": "x"}}},
	}}}
}

func TestReplayRecordsDivergenceKinds(t *testing.T) {
	lookupTool := json.RawMessage(`{"type":"function","function":{"name":"lookup","description":"d","parameters":{"type":"object"}}}`)
	plain := Trace{ID: "plain", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}}
	routed := Trace{ID: "routed", System: "s", Tools: []json.RawMessage{lookupTool}, Turns: []Turn{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`)}}},
		{Role: "tool", Name: "lookup", Content: "result"},
		{Role: "assistant", Content: "final"},
	}, Golden: Golden{ToolCalls: []string{"lookup"}}}
	prefilled := Trace{ID: "pre", System: "s", Turns: []Turn{{Role: "user", Content: "q"}}, AssemblyEval: &AssemblyEval{PairID: "p", Mode: AssemblyTopline}}

	cases := []struct {
		name         string
		trace        Trace
		reply        candidateChatResponse
		wantKinds    []string
		wantSentinel bool
		wantContent  string
	}{
		{"scripted bypass keeps prose", routed, divergenceText("answer"), []string{divergenceToolRouteBypassed}, false, "answer"},
		{"plain-chat tool call substitutes", plain, divergenceToolCall("prose"), []string{divergencePlainChatToolCall}, true, plainChatToolDivergenceFinal},
		{"prefilled tool call with content keeps it", prefilled, divergenceToolCall("kept"), []string{divergencePrefilledToolCall}, false, "kept"},
		{"prefilled tool call without content substitutes", prefilled, divergenceToolCall(""), []string{divergencePrefilledToolCall}, true, plainChatToolDivergenceFinal},
		{"candidate-emitted sentinel is not a divergence", plain, divergenceText(plainChatToolDivergenceFinal), nil, false, plainChatToolDivergenceFinal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := replayWith(context.Background(), &divergenceChat{replies: []candidateChatResponse{tc.reply}}, "m", tc.trace, replayOptions{})
			if err != nil {
				t.Fatalf("replayWith: %v", err)
			}
			if !reflect.DeepEqual(out.Divergences, tc.wantKinds) {
				t.Fatalf("Divergences = %v, want %v", out.Divergences, tc.wantKinds)
			}
			if out.SentinelSubstituted != tc.wantSentinel {
				t.Fatalf("SentinelSubstituted = %v, want %v", out.SentinelSubstituted, tc.wantSentinel)
			}
			if got := lastAssistantContent(out.Transcript); got != tc.wantContent {
				t.Fatalf("final content = %q, want %q", got, tc.wantContent)
			}
		})
	}
}
