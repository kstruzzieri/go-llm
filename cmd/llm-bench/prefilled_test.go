package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// prefilledTestTrace returns a valid prefilled-mode trace: a full assembled
// agent conversation (user, assistant-with-tool-call, tool result, assistant
// summary) ending in a user question.
func prefilledTestTrace(mode AssemblyMode) Trace {
	ae := &AssemblyEval{PairID: "p-a", Mode: mode}
	if mode == AssemblyLegacy || mode == AssemblyMixed {
		ae.Budget = 100
		ae.StateDigest = "sha256:d"
	}
	return Trace{
		ID:           "prefilled-" + string(mode),
		System:       "sys",
		AssemblyEval: ae,
		Tools: []json.RawMessage{
			json.RawMessage(`{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "u1"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}}},
			{Role: "tool", ToolCallID: "call-1", Name: "read_file", Content: "file body"},
			{Role: "assistant", Content: "I read it"},
			{Role: "user", Content: "final question?"},
		},
		Golden: Golden{FinalAnswerCriteria: "c"},
	}
}

func TestPrefilledTraceValidation(t *testing.T) {
	mutate := func(fn func(*Trace)) Trace {
		tr := prefilledTestTrace(AssemblyMixed)
		fn(&tr)
		return tr
	}
	cases := []struct {
		name    string
		trace   Trace
		wantErr string // empty = accept
	}{
		{name: "valid mixed", trace: prefilledTestTrace(AssemblyMixed), wantErr: ""},
		{name: "valid legacy", trace: prefilledTestTrace(AssemblyLegacy), wantErr: ""},
		{name: "valid topline", trace: prefilledTestTrace(AssemblyTopline), wantErr: ""},
		{
			name: "tool turn blank tool_call_id",
			trace: mutate(func(tr *Trace) {
				tr.Turns[2].ToolCallID = ""
			}),
			wantErr: "tool_call_id",
		},
		{
			name: "tool turn unknown tool_call_id",
			trace: mutate(func(tr *Trace) {
				tr.Turns[2].ToolCallID = "call-unknown"
			}),
			wantErr: "tool_call_id",
		},
		{
			name: "tool turn precedes its assistant call",
			trace: mutate(func(tr *Trace) {
				tr.Turns[1], tr.Turns[2] = tr.Turns[2], tr.Turns[1]
			}),
			wantErr: "tool_call_id",
		},
		{
			name: "double-answered assistant call",
			trace: mutate(func(tr *Trace) {
				tr.Turns = append(tr.Turns[:3], append([]Turn{{Role: "tool", ToolCallID: "call-1", Name: "read_file", Content: "again"}}, tr.Turns[3:]...)...)
			}),
			wantErr: "answered",
		},
		{
			name: "duplicate assistant tool-call id",
			trace: mutate(func(tr *Trace) {
				tr.Turns[1].ToolCalls = append(tr.Turns[1].ToolCalls, ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{}`)})
			}),
			wantErr: "duplicate",
		},
		{
			name: "unanswered assistant tool call",
			trace: mutate(func(tr *Trace) {
				tr.Turns = append(tr.Turns[:2], tr.Turns[3:]...) // drop the tool-result turn
			}),
			wantErr: "never answered",
		},
		{
			name: "blank assistant tool-call id",
			trace: mutate(func(tr *Trace) {
				tr.Turns[1].ToolCalls[0].ID = ""
			}),
			wantErr: "non-empty id",
		},
		{
			name: "final turn not user",
			trace: mutate(func(tr *Trace) {
				tr.Turns = tr.Turns[:4] // ends on assistant summary
			}),
			wantErr: "final turn",
		},
		{
			name: "final user turn empty content",
			trace: mutate(func(tr *Trace) {
				tr.Turns[4].Content = ""
			}),
			wantErr: "final turn",
		},
		{
			name: "unknown role",
			trace: mutate(func(tr *Trace) {
				tr.Turns[3].Role = "narrator"
			}),
			wantErr: "role",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTrace(tc.trace)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTrace = %v; want accept", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTrace = %v; want error containing %q", err, tc.wantErr)
			}
		})
	}

	// Non-assembly traces keep today's rules exactly: validateTrace stays
	// permissive about turn roles, and replay still rejects an assistant
	// top-level turn with errUnsupportedTurns before reaching the network.
	t.Run("non-assembly assistant top-level turn still rejected at replay", func(t *testing.T) {
		trace := Trace{
			ID:     "legacy-shape",
			System: "sys",
			Turns: []Turn{
				{Role: "assistant", Content: "hi"},
				{Role: "user", Content: "q"},
			},
			Golden: Golden{FinalAnswerCriteria: "c"},
		}
		if err := validateTrace(trace); err != nil {
			t.Fatalf("validateTrace = %v; want accept (non-assembly rules unchanged)", err)
		}
		srv, called := newBlockedServer(t)
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		_, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{})
		if !errors.Is(err, errUnsupportedTurns) {
			t.Fatalf("replayWith err = %v; want errUnsupportedTurns", err)
		}
		if *called {
			t.Error("replay reached the server despite the unsupported turn")
		}
	})
}

// rawRequestLog captures raw request bodies so tests can assert on the exact
// wire JSON (field presence/absence), not just the decoded struct.
type rawRequestLog struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (l *rawRequestLog) append(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bodies = append(l.bodies, b)
}

func (l *rawRequestLog) snapshot() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]byte, len(l.bodies))
	copy(out, l.bodies)
	return out
}

func newOllamaCaptureServer(t *testing.T, log *rawRequestLog, respond func() ollama.ChatResponse) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		log.append(body)
		if err := json.NewEncoder(w).Encode(respond()); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

// assertPrefilledMessages asserts the exact prefilled message array shape on
// a decoded wire body shared by both transports (roles, contents, assistant
// tool_calls ids, tool_call_id on tool messages).
func assertPrefilledMessages(t *testing.T, body map[string]any) {
	t.Helper()
	if _, ok := body["tools"]; ok {
		t.Fatalf("request body contains tools field: %v", body["tools"])
	}
	rawMsgs, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("request body messages = %T; want array", body["messages"])
	}
	if len(rawMsgs) != 6 {
		t.Fatalf("messages len = %d; want 6", len(rawMsgs))
	}
	msg := func(i int) map[string]any { return rawMsgs[i].(map[string]any) }
	wantRoles := []string{"system", "user", "assistant", "tool", "assistant", "user"}
	for i, want := range wantRoles {
		if got := msg(i)["role"]; got != want {
			t.Fatalf("messages[%d].role = %v; want %q", i, got, want)
		}
	}
	if got := msg(0)["content"]; got != "sys" {
		t.Fatalf("messages[0].content = %v; want sys", got)
	}
	if got := msg(1)["content"]; got != "u1" {
		t.Fatalf("messages[1].content = %v; want u1", got)
	}
	calls, ok := msg(2)["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("messages[2].tool_calls = %v; want 1 call", msg(2)["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if got := call["id"]; got != "call-1" {
		t.Fatalf("assistant tool_calls[0].id = %v; want call-1", got)
	}
	fn, ok := call["function"].(map[string]any)
	if !ok || fn["name"] != "read_file" {
		t.Fatalf("assistant tool_calls[0].function = %v; want read_file", call["function"])
	}
	if got := msg(3)["tool_call_id"]; got != "call-1" {
		t.Fatalf("messages[3].tool_call_id = %v; want call-1", got)
	}
	// Tool name rides as "tool_name" on the Ollama wire and "name" on the
	// OpenAI wire; either way the tool message must carry it.
	if msg(3)["tool_name"] != "read_file" && msg(3)["name"] != "read_file" {
		t.Fatalf("messages[3] missing tool name: %v", msg(3))
	}
	if got := msg(3)["content"]; got != "file body" {
		t.Fatalf("messages[3].content = %v; want file body", got)
	}
	if got := msg(4)["content"]; got != "I read it" {
		t.Fatalf("messages[4].content = %v; want I read it", got)
	}
	if got := msg(5)["content"]; got != "final question?" {
		t.Fatalf("messages[5].content = %v; want final question?", got)
	}
}

func TestPrefilledReplaySendsExactHistory(t *testing.T) {
	trace := prefilledTestTrace(AssemblyMixed)
	if err := validateTrace(trace); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}

	t.Run("ollama", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := newOllamaCaptureServer(t, log, func() ollama.ChatResponse {
			return ollama.ChatResponse{
				Model:           "m",
				Done:            true,
				Message:         ollama.ChatMessage{Role: "assistant", Content: "final answer"},
				PromptEvalCount: 5,
				EvalCount:       3,
			}
		})
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{})
		if err != nil {
			t.Fatalf("replayWith: %v", err)
		}
		bodies := log.snapshot()
		if len(bodies) != 1 {
			t.Fatalf("HTTP calls = %d; want exactly 1", len(bodies))
		}
		assertPrefilledMessages(t, decodeBody(t, bodies[0]))
		if len(out.Transcript) != 1 {
			t.Fatalf("transcript len = %d; want 1", len(out.Transcript))
		}
		if out.Transcript[0].Role != "assistant" || out.Transcript[0].Content != "final answer" {
			t.Fatalf("transcript[0] = %+v; want assistant final answer", out.Transcript[0])
		}
		if out.PromptEvalTokens != 5 || out.GenTokens != 3 || out.TotalTokens != 8 {
			t.Fatalf("usage = %d/%d/%d; want 5/3/8", out.PromptEvalTokens, out.GenTokens, out.TotalTokens)
		}
	})

	t.Run("openai-compat", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				t.Errorf("path = %q; want /v1/chat/completions", r.URL.Path)
			}
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read request body: %v", readErr)
			}
			log.append(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"chatcmpl-test","model":"m",
				"choices":[{"index":0,"message":{"role":"assistant","content":"final answer"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
			}`))
		}))
		t.Cleanup(srv.Close)
		tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}, candidateTransportOptions{
			openAICompatBaseURL: srv.URL,
			timeout:             5 * time.Second,
		})
		if err != nil {
			t.Fatalf("newCandidateTransport: %v", err)
		}
		out, err := replayWith(context.Background(), tr.chat, "m", trace, replayOptions{})
		if err != nil {
			t.Fatalf("replayWith: %v", err)
		}
		bodies := log.snapshot()
		if len(bodies) != 1 {
			t.Fatalf("HTTP calls = %d; want exactly 1", len(bodies))
		}
		assertPrefilledMessages(t, decodeBody(t, bodies[0]))
		if len(out.Transcript) != 1 || out.Transcript[0].Content != "final answer" {
			t.Fatalf("transcript = %+v; want single final-answer turn", out.Transcript)
		}
	})
}

// TestPrefilledToolArgByteFidelity (#331 W3): fixture tool-call argument
// bytes must reach the wire without decode/re-encode wherever the frozen
// types permit. The payload has DELIBERATELY unsorted keys and interior
// whitespace: any map[string]any round-trip would sort the keys and strip
// the spacing.
//
//   - openai-compat: provider.ToolCallFunction.Arguments and the wire's
//     chatToolCallFunction.Arguments are json.RawMessage; the frozen
//     encodeToolCallArguments wraps raw JSON verbatim (edge-trimmed) into
//     OpenAI's string envelope. Byte-exactness is REQUIRED end to end.
//   - ollama: ollama.ToolCallFunction.Arguments is map[string]any — the
//     FROZEN wire struct itself — so bytes cannot survive past the decode in
//     prefilledToolCalls. Semantic equality is the frozen-boundary ceiling,
//     asserted here and documented on ollamaCandidateClient.Chat.
func TestPrefilledToolArgByteFidelity(t *testing.T) {
	const rawArgs = `{"zeta": 1,  "alpha": {"b": 2, "a": 1}, "query": "beta  gate"}`
	trace := prefilledTestTrace(AssemblyMixed)
	trace.Turns[1].ToolCalls[0].Arguments = json.RawMessage(rawArgs)

	wireCall := func(t *testing.T, body map[string]any) map[string]any {
		t.Helper()
		msgs := body["messages"].([]any)
		calls, ok := msgs[2].(map[string]any)["tool_calls"].([]any)
		if !ok || len(calls) != 1 {
			t.Fatalf("assistant tool_calls = %v; want 1 call", msgs[2])
		}
		fn, ok := calls[0].(map[string]any)["function"].(map[string]any)
		if !ok {
			t.Fatalf("tool call function missing: %v", calls[0])
		}
		return fn
	}

	t.Run("openai-compat wire carries the exact bytes", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read request body: %v", readErr)
			}
			log.append(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ans"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		t.Cleanup(srv.Close)
		tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}, candidateTransportOptions{
			openAICompatBaseURL: srv.URL,
			timeout:             5 * time.Second,
		})
		if err != nil {
			t.Fatalf("newCandidateTransport: %v", err)
		}
		if _, err := replayWith(context.Background(), tr.chat, "m", trace, replayOptions{}); err != nil {
			t.Fatalf("replayWith: %v", err)
		}
		bodies := log.snapshot()
		if len(bodies) != 1 {
			t.Fatalf("HTTP calls = %d; want 1", len(bodies))
		}
		fn := wireCall(t, decodeBody(t, bodies[0]))
		// OpenAI's wire envelope is a JSON string whose decoded value must be
		// the fixture bytes EXACTLY: key order and interior whitespace intact.
		got, ok := fn["arguments"].(string)
		if !ok {
			t.Fatalf("wire arguments = %T (%v); want OpenAI's JSON-string envelope", fn["arguments"], fn["arguments"])
		}
		if got != rawArgs {
			t.Fatalf("wire arguments = %q; want the byte-exact fixture payload %q", got, rawArgs)
		}
	})

	t.Run("ollama wire is semantically equal (frozen map boundary)", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := newOllamaCaptureServer(t, log, func() ollama.ChatResponse {
			return ollama.ChatResponse{Model: "m", Done: true, Message: ollama.ChatMessage{Role: "assistant", Content: "ans"}}
		})
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		if _, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{}); err != nil {
			t.Fatalf("replayWith: %v", err)
		}
		bodies := log.snapshot()
		if len(bodies) != 1 {
			t.Fatalf("HTTP calls = %d; want 1", len(bodies))
		}
		fn := wireCall(t, decodeBody(t, bodies[0]))
		got, ok := fn["arguments"].(map[string]any)
		if !ok {
			t.Fatalf("ollama wire arguments = %T; the frozen type is a JSON object", fn["arguments"])
		}
		var want map[string]any
		if err := json.Unmarshal([]byte(rawArgs), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ollama wire arguments = %v; want the semantic payload %v", got, want)
		}
	})
}

func TestPrefilledReplayCandidateToolCallDivergence(t *testing.T) {
	trace := prefilledTestTrace(AssemblyMixed)
	respond := func(content string) func() ollama.ChatResponse {
		return func() ollama.ChatResponse {
			return ollama.ChatResponse{
				Model: "m",
				Done:  true,
				Message: ollama.ChatMessage{
					Role:    "assistant",
					Content: content,
					ToolCalls: []ollama.ToolCall{{
						ID:   "cand-1",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "read_file",
							Arguments: map[string]any{"path": "b.go"},
						},
					}},
				},
			}
		}
	}

	t.Run("empty content gets sentinel", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := newOllamaCaptureServer(t, log, respond(""))
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{})
		if err != nil {
			t.Fatalf("replayWith = %v; want nil (divergence is scored, not an error)", err)
		}
		if len(out.Transcript) != 1 {
			t.Fatalf("transcript len = %d; want 1", len(out.Transcript))
		}
		if out.Transcript[0].Content != plainChatToolDivergenceFinal {
			t.Fatalf("transcript content = %q; want divergence sentinel", out.Transcript[0].Content)
		}
		if len(out.Transcript[0].ToolCalls) != 1 {
			t.Fatalf("transcript tool calls = %d; want 1 (kept for forensics)", len(out.Transcript[0].ToolCalls))
		}
		if len(out.Notes) == 0 || !strings.Contains(strings.Join(out.Notes, ";"), "scored on content") {
			t.Fatalf("Notes = %v; want tool-call divergence note", out.Notes)
		}
	})

	t.Run("content preserved alongside tool calls", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := newOllamaCaptureServer(t, log, respond("partial answer"))
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{})
		if err != nil {
			t.Fatalf("replayWith = %v; want nil", err)
		}
		if out.Transcript[0].Content != "partial answer" {
			t.Fatalf("transcript content = %q; want partial answer preserved", out.Transcript[0].Content)
		}
		if len(out.Notes) == 0 || !strings.Contains(strings.Join(out.Notes, ";"), "scored on content") {
			t.Fatalf("Notes = %v; want tool-call divergence note", out.Notes)
		}
		if len(log.snapshot()) != 1 {
			t.Fatalf("HTTP calls = %d; want exactly 1 (no tool loop)", len(log.snapshot()))
		}
	})
}

func TestPrefilledReplayErrorPaths(t *testing.T) {
	trace := prefilledTestTrace(AssemblyMixed)

	t.Run("whitespace-only empty reply", func(t *testing.T) {
		srv := newOllamaCaptureServer(t, &rawRequestLog{}, func() ollama.ChatResponse {
			return ollama.ChatResponse{Model: "m", Done: true, Message: ollama.ChatMessage{Role: "assistant", Content: "  \n"}}
		})
		client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		_, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "m", trace, replayOptions{})
		if !errors.Is(err, errEmptyAssistantReply) {
			t.Fatalf("err = %v; want errEmptyAssistantReply", err)
		}
	})

	t.Run("transport error surfaces as Result.Err", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		runner := &Runner{OllamaURL: srv.URL, Timeout: 5 * time.Second, Scorer: &CaptureScorer{}}
		results, err := runner.RunAll(context.Background(),
			[]ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}}, []Trace{trace})
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("results = %d; want 1", len(results))
		}
		if results[0].Err == nil {
			t.Fatal("Result.Err = nil; want the transport error surfaced per-result")
		}
	})
}

func TestAssemblyCaptureDecodingOptions(t *testing.T) {
	assembly := prefilledTestTrace(AssemblyMixed)
	plain := Trace{
		ID:     "plain-1",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "c"},
	}

	t.Run("ollama", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := newOllamaCaptureServer(t, log, func() ollama.ChatResponse {
			return ollama.ChatResponse{Model: "m", Done: true, Message: ollama.ChatMessage{Role: "assistant", Content: "ans"}}
		})
		runner := &Runner{OllamaURL: srv.URL, Timeout: 5 * time.Second, Scorer: &CaptureScorer{}}
		targets := []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}}
		results, err := runner.RunAll(context.Background(), targets, []Trace{assembly, plain})
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		for _, r := range results {
			if r.Err != nil {
				t.Fatalf("result %s err = %v", r.TraceID, r.Err)
			}
		}
		bodies := log.snapshot()
		if len(bodies) != 2 {
			t.Fatalf("HTTP calls = %d; want 2", len(bodies))
		}
		assemblyBody := decodeBody(t, bodies[0])
		plainBody := decodeBody(t, bodies[1])
		opts, ok := assemblyBody["options"].(map[string]any)
		if !ok {
			t.Fatalf("assembly request options = %v; want object with temperature", assemblyBody["options"])
		}
		if temp, ok := opts["temperature"].(float64); !ok || temp != 0 {
			t.Fatalf("assembly temperature = %v; want 0", opts["temperature"])
		}
		if _, ok := opts["seed"]; ok {
			t.Fatalf("assembly request carries seed = %v; ollama.ModelOptions has no seed support", opts["seed"])
		}
		if popts, ok := plainBody["options"].(map[string]any); ok {
			if _, ok := popts["temperature"]; ok {
				t.Fatalf("plain request carries temperature: %v", popts["temperature"])
			}
		}
	})

	t.Run("openai-compat", func(t *testing.T) {
		log := &rawRequestLog{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read request body: %v", readErr)
			}
			log.append(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ans"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		t.Cleanup(srv.Close)
		runner := &Runner{OllamaURL: "http://unused", OpenAICompatBaseURL: srv.URL, Timeout: 5 * time.Second, Scorer: &CaptureScorer{}}
		targets := []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}}
		results, err := runner.RunAll(context.Background(), targets, []Trace{assembly, plain})
		if err != nil {
			t.Fatalf("RunAll: %v", err)
		}
		for _, r := range results {
			if r.Err != nil {
				t.Fatalf("result %s err = %v", r.TraceID, r.Err)
			}
		}
		bodies := log.snapshot()
		if len(bodies) != 2 {
			t.Fatalf("HTTP calls = %d; want 2", len(bodies))
		}
		assemblyBody := decodeBody(t, bodies[0])
		plainBody := decodeBody(t, bodies[1])
		if temp, ok := assemblyBody["temperature"].(float64); !ok || temp != 0 {
			t.Fatalf("assembly temperature = %v; want 0", assemblyBody["temperature"])
		}
		if _, ok := assemblyBody["seed"]; ok {
			t.Fatalf("assembly request carries seed; openai-compat request shape has no seed support")
		}
		if _, ok := plainBody["temperature"]; ok {
			t.Fatalf("plain request carries temperature: %v", plainBody["temperature"])
		}
	})
}

// orderRecordingRunner records the trace order RunAll receives and
// synthesizes one successful Result per (target, trace) in that order.
type orderRecordingRunner struct {
	gotTraceIDs [][]string // one entry per RunAll call
}

func (f *orderRecordingRunner) RunAll(_ context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	ids := make([]string, 0, len(traces))
	for _, tr := range traces {
		ids = append(ids, tr.ID)
	}
	f.gotTraceIDs = append(f.gotTraceIDs, ids)
	var results []Result
	for _, target := range targets {
		for _, tr := range traces {
			results = append(results, Result{
				Model:             target.Display,
				TraceID:           tr.ID,
				CandidateProvider: target.Provider,
				Transcript:        []Turn{{Role: "assistant", Content: "ans " + tr.ID}},
				Score:             Score{PromptEvalTokens: 10, GenTokens: 2},
			})
		}
	}
	return results, nil
}

func pairedCaptureTrace(id, pairID string, mode AssemblyMode) Trace {
	return Trace{
		ID:     id,
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "c"},
		AssemblyEval: &AssemblyEval{
			PairID: pairID, Mode: mode, Budget: 100, StateDigest: "sha256:d",
		},
	}
}

func toplineCaptureTrace(id string) Trace {
	return Trace{
		ID:     id,
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "c"},
		AssemblyEval: &AssemblyEval{
			PairID: "tp-" + id, Mode: AssemblyTopline,
		},
	}
}

func readArtifactsFile(t *testing.T, path string) []Artifact {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []Artifact
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var a Artifact
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil {
			t.Fatalf("decode artifact: %v", err)
		}
		out = append(out, a)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan artifacts: %v", err)
	}
	return out
}

func TestAssemblyCaptureCounterbalance(t *testing.T) {
	// FNV-1a 64 hashes (computed independently of the implementation):
	//   "p-b" -> 8681622975474906252
	//   "p-a" -> 8681626274009790885
	// Registered W3 scheme: pairs sort by hash ASCENDING — p-b (…622…) before
	// p-a (…626…) — then the first arm ALTERNATES by position: position 0
	// (p-b, even) => legacy-first, position 1 (p-a, odd) => mixed-first.
	// Expected order: plain traces in input order, then the hash-ordered
	// pairs with the alternation-selected arm first, then topline sorted by ID.
	traces := []Trace{
		{ID: "zz-plain", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
		toplineCaptureTrace("top-b"),
		pairedCaptureTrace("pb-mixed", "p-b", AssemblyMixed),
		pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy),
		pairedCaptureTrace("pa-mixed", "p-a", AssemblyMixed),
		pairedCaptureTrace("pb-legacy", "p-b", AssemblyLegacy),
		toplineCaptureTrace("top-a"),
	}
	wantOrder := []string{"zz-plain", "pb-legacy", "pb-mixed", "pa-mixed", "pa-legacy", "top-a", "top-b"}
	// Two targets: the same trace must get the SAME capture order index
	// under every target (the index is the per-target replay position).
	targets := []ModelTarget{
		{Display: "m", Provider: "ollama", Model: "m"},
		{Display: "m2", Provider: "ollama", Model: "m2"},
	}

	runOnce := func(t *testing.T) ([]string, []Artifact) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "artifacts.jsonl")
		runner := &orderRecordingRunner{}
		if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
			Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
		}); err != nil {
			t.Fatalf("runCalibrateCapture: %v", err)
		}
		if len(runner.gotTraceIDs) != 1 {
			t.Fatalf("RunAll calls = %d; want 1", len(runner.gotTraceIDs))
		}
		return runner.gotTraceIDs[0], readArtifactsFile(t, out)
	}

	gotOrder, artifacts := runOnce(t)
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("capture order = %v; want %v", gotOrder, wantOrder)
	}

	if want := len(wantOrder) * len(targets); len(artifacts) != want {
		t.Fatalf("artifacts = %d; want %d (traces x targets)", len(artifacts), want)
	}
	byID := map[string][]Artifact{}
	for _, a := range artifacts {
		byID[a.TraceID] = append(byID[a.TraceID], a)
	}
	for _, a := range byID["zz-plain"] {
		if a.Capture != nil {
			t.Fatalf("plain artifact (%s) has capture provenance: %+v", a.CandidateModel, a.Capture)
		}
	}
	wantIndex := map[string]int{}
	for i, id := range wantOrder {
		wantIndex[id] = i
	}
	wantCapturedOrder := map[string]string{
		"pa-legacy": "mixed-first", "pa-mixed": "mixed-first",
		"pb-legacy": "legacy-first", "pb-mixed": "legacy-first",
		"top-a": "", "top-b": "",
	}
	for id, want := range wantCapturedOrder {
		group := byID[id]
		if len(group) != len(targets) {
			t.Fatalf("artifacts for %s = %d; want one per target", id, len(group))
		}
		for _, a := range group {
			if a.Capture == nil {
				t.Fatalf("artifact %s/%s missing capture provenance", id, a.CandidateModel)
			}
			if a.Capture.OrderIndex != wantIndex[id] {
				t.Fatalf("artifact %s/%s order_index = %d; want %d (same index across targets)",
					id, a.CandidateModel, a.Capture.OrderIndex, wantIndex[id])
			}
			if a.Capture.CapturedOrder != want {
				t.Fatalf("artifact %s/%s captured_order = %q; want %q", id, a.CandidateModel, a.Capture.CapturedOrder, want)
			}
		}
	}

	gotOrder2, _ := runOnce(t)
	if !reflect.DeepEqual(gotOrder, gotOrder2) {
		t.Fatalf("capture order not deterministic: %v vs %v", gotOrder, gotOrder2)
	}
}

// TestCounterbalanceIncompletePairUnlabeled: a pair with only one arm
// present is neither counterbalanced nor labeled — captured_order exists to
// name which arm ran first, which is meaningless with one arm — and it must
// not consume an alternation slot from the complete pairs.
func TestCounterbalanceIncompletePairUnlabeled(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("solo-legacy", "p-solo", AssemblyLegacy), // mixed arm missing
		pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy),
		pairedCaptureTrace("pa-mixed", "p-a", AssemblyMixed),
	}
	_, labels := counterbalanceCaptureTraces(traces)
	if got, ok := labels["p-solo"]; ok {
		t.Errorf("incomplete pair labeled %q; want no captured_order label", got)
	}
	// p-a is the ONLY complete pair, so it sits at alternation position 0
	// regardless of p-solo's hash.
	if labels["p-a"] != "legacy-first" {
		t.Errorf("complete pair label = %q; want legacy-first (position 0)", labels["p-a"])
	}

	// End to end: the single-arm artifact's provenance omits captured_order.
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:  &orderRecordingRunner{},
		Targets: []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:  traces, OutputPath: out, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	for _, a := range readArtifactsFile(t, out) {
		if a.Capture == nil {
			t.Fatalf("artifact %s missing capture provenance", a.TraceID)
		}
		want := "legacy-first"
		if a.TraceID == "solo-legacy" {
			want = ""
		}
		if a.Capture.CapturedOrder != want {
			t.Errorf("artifact %s captured_order = %q; want %q", a.TraceID, a.Capture.CapturedOrder, want)
		}
	}
}

// TestCounterbalanceAlternationBalance proves the W3 counterbalance is
// balanced BY CONSTRUCTION, not by hash luck: the five pair IDs below all
// have ODD FNV-1a-64 hashes, so the retired parity rule would have run every
// one of them mixed-first (5/0). Alternation over the hash-sorted order must
// come out 3/2. Hashes (computed independently):
//
//	"p-3" -> 8681571298428380335   position 0 => legacy-first
//	"p-1" -> 8681573497451636757   position 1 => mixed-first
//	"p-e" -> 8681621875963278041   position 2 => legacy-first
//	"p-c" -> 8681624074986534463   position 3 => mixed-first
//	"p-a" -> 8681626274009790885   position 4 => legacy-first
func TestCounterbalanceAlternationBalance(t *testing.T) {
	pairIDs := []string{"p-a", "p-c", "p-e", "p-1", "p-3"}
	var traces []Trace
	for _, id := range pairIDs {
		traces = append(traces,
			pairedCaptureTrace(id+"-legacy", id, AssemblyLegacy),
			pairedCaptureTrace(id+"-mixed", id, AssemblyMixed),
		)
	}
	_, labels := counterbalanceCaptureTraces(traces)
	want := map[string]string{
		"p-3": "legacy-first", "p-1": "mixed-first", "p-e": "legacy-first",
		"p-c": "mixed-first", "p-a": "legacy-first",
	}
	counts := map[string]int{}
	for id, wantLabel := range want {
		if got := labels[id]; got != wantLabel {
			t.Errorf("pair %s captured order = %q; want %q (hash-sorted alternation)", id, got, wantLabel)
		}
		counts[labels[id]]++
	}
	if diff := counts["legacy-first"] - counts["mixed-first"]; diff < -1 || diff > 1 {
		t.Errorf("first-arm counts legacy=%d mixed=%d; alternation must balance within 1",
			counts["legacy-first"], counts["mixed-first"])
	}
}

func TestAssemblyCaptureProvenanceAndUsage(t *testing.T) {
	const modelDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	traces := []Trace{
		pairedCaptureTrace("arm-legacy", "p-b", AssemblyLegacy),
		{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: targets, Traces: traces, OutputPath: out,
		ModelDigests: map[string]string{"m": modelDigest},
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	lines := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var a Artifact
		line := scanner.Text()
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatalf("decode: %v", err)
		}
		lines[a.TraceID] = line
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan artifacts: %v", err)
	}

	var assemblyArtifact Artifact
	if err := json.Unmarshal([]byte(lines["arm-legacy"]), &assemblyArtifact); err != nil {
		t.Fatalf("decode assembly artifact: %v", err)
	}
	prov := assemblyArtifact.Capture
	if prov == nil {
		t.Fatal("assembly artifact missing capture provenance")
	}
	if prov.Temperature == nil || *prov.Temperature != 0 {
		t.Fatalf("capture temperature = %v; want 0", prov.Temperature)
	}
	if prov.Seed != nil {
		t.Fatalf("capture seed = %v; want nil (no transport supports seed)", prov.Seed)
	}
	if prov.Transport != "ollama" {
		t.Fatalf("capture transport = %q; want ollama", prov.Transport)
	}
	if prov.Model != "m" {
		t.Fatalf("capture model = %q; want m", prov.Model)
	}
	if prov.ModelDigest != modelDigest {
		t.Fatalf("capture model_digest = %q; want %s", prov.ModelDigest, modelDigest)
	}
	if prov.PromptTokens != 10 || prov.GenTokens != 2 {
		t.Fatalf("capture usage = %d/%d; want 10/2 from replay usage", prov.PromptTokens, prov.GenTokens)
	}

	// Non-assembly artifacts must marshal byte-identical to today: no
	// capture key may appear on the wire at all.
	plainLine, ok := lines["plain-t"]
	if !ok {
		t.Fatal("plain artifact missing")
	}
	if strings.Contains(plainLine, `"capture"`) {
		t.Fatalf("plain artifact JSON carries capture field: %s", plainLine)
	}
}

func TestArtifactHashIgnoresCaptureProvenance(t *testing.T) {
	base := Artifact{
		TraceID:           "t1",
		CandidateModel:    "m",
		Trace:             prefilledTestTrace(AssemblyMixed),
		ActualFinalAnswer: "ans",
		ActualToolCalls:   []string{},
		ActualTranscript:  []Turn{{Role: "assistant", Content: "ans"}},
	}
	without := artifactHash(base)
	temp := 0.0
	base.Capture = &CaptureProvenance{
		OrderIndex:   3,
		Temperature:  &temp,
		Transport:    "ollama",
		Model:        "m",
		PromptTokens: 10,
		GenTokens:    2,
	}
	with := artifactHash(base)
	if without != with {
		t.Fatalf("artifactHash changed with capture provenance: %s vs %s", without, with)
	}
}

func TestCaptureManifestWritten(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy),
		pairedCaptureTrace("pa-mixed", "p-a", AssemblyMixed),
		{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	clock := func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	var stdout bytes.Buffer
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: targets, Traces: traces, OutputPath: out,
		Clock: clock, OpenAICompatBaseURL: "http://openai-compat.test", Stdout: &stdout,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}

	raw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("manifest missing at sibling path: %v", err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	for _, path := range []string{out, out + ".manifest.json"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v; want 0600", path, info.Mode().Perm())
		}
	}
	if m.SchemaVersion != "mixed-capture-manifest/v2" {
		t.Errorf("schema_version = %q; want mixed-capture-manifest/v2 (new captures write the run ledger)", m.SchemaVersion)
	}
	// v2 run ledger: an all-success capture records one captured row per
	// (trace x model); the failure shapes live in TestCaptureManifestV2ExpectedLedger.
	if m.ExpectedCount != 3 || len(m.Expected) != 3 {
		t.Errorf("expected_count/len(expected) = %d/%d; want 3/3", m.ExpectedCount, len(m.Expected))
	}
	for _, row := range m.Expected {
		if row.Status != "captured" || row.Attempts != 1 {
			t.Errorf("ledger row %+v; want status captured, attempts 1", row)
		}
	}
	if !m.CreatedAt.Equal(clock()) {
		t.Errorf("created_at = %v; want the injected clock %v", m.CreatedAt, clock())
	}
	if m.Endpoint != "http://openai-compat.test" || m.Transport != "openai-compat" {
		t.Errorf("endpoint/transport = %q/%q; want http://openai-compat.test/openai-compat", m.Endpoint, m.Transport)
	}
	if len(m.ModelTargets) != 1 || m.ModelTargets[0].Selector != "openai-compat/m" || m.ModelTargets[0].ResolvedDigest != "" {
		t.Errorf("model_targets = %+v; want the one openai-compat selector with empty digest", m.ModelTargets)
	}
	if len(m.ServerProbe.OllamaDigests) != 0 {
		t.Errorf("server_probe = %+v; want no digests for openai-compat", m.ServerProbe)
	}
	if m.Decoding.Temperature != 0 || m.Decoding.SeedSupported {
		t.Errorf("decoding = %+v; want temperature 0, seed_supported false", m.Decoding)
	}
	if !strings.Contains(m.CounterbalanceScheme, "fnv1a64") || !strings.Contains(m.CounterbalanceScheme, "alternates") {
		t.Errorf("counterbalance_scheme = %q; want the registered hash-order alternation description", m.CounterbalanceScheme)
	}
	if m.ArtifactCount != 3 || len(m.PerArtifact) != 3 {
		t.Fatalf("artifact_count/per_artifact = %d/%d; want 3/3 (plain artifacts are listed too)", m.ArtifactCount, len(m.PerArtifact))
	}
	arts := readArtifactsFile(t, out)
	hashByID := map[string]string{}
	for _, a := range arts {
		hashByID[a.TraceID] = a.ArtifactHash
	}
	for _, row := range m.PerArtifact {
		if row.ArtifactHash != hashByID[row.TraceID] {
			t.Errorf("row %s hash = %q; want the written artifact's %q", row.TraceID, row.ArtifactHash, hashByID[row.TraceID])
		}
		if !row.UsagePresent {
			t.Errorf("row %s usage_present = false; the fake replay reports 10/2 tokens", row.TraceID)
		}
	}

	// The manifest digest line reaches stdout and matches the file bytes.
	sum := sha256.Sum256(raw)
	wantLine := "manifest_digest sha256:" + hex.EncodeToString(sum[:])
	if !strings.Contains(stdout.String(), wantLine) {
		t.Errorf("stdout = %q; want it to carry %q", stdout.String(), wantLine)
	}
}

// zeroUsageRunner is orderRecordingRunner minus token usage: every Result
// reports 0/0, the "provider omitted usage" shape.
type zeroUsageRunner struct{ inner orderRecordingRunner }

func (f *zeroUsageRunner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	results, err := f.inner.RunAll(ctx, targets, traces)
	for i := range results {
		results[i].Score.PromptEvalTokens = 0
		results[i].Score.GenTokens = 0
	}
	return results, err
}

func TestCaptureManifestUsageAbsentRecordedFalse(t *testing.T) {
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     &zeroUsageRunner{},
		Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:     []Trace{pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy)},
		OutputPath: out, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	raw, err := os.ReadFile(out + ".manifest.json")
	if err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	var m captureManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.PerArtifact) != 1 || m.PerArtifact[0].UsagePresent {
		t.Fatalf("per_artifact = %+v; zero token counts must record usage_present false", m.PerArtifact)
	}
}

func TestCaptureManifestWriteFailureFailsCapture(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "artifacts.jsonl")
	if err := os.WriteFile(out, []byte("prior artifacts\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Occupy the manifest's sibling path with a DIRECTORY so the write fails.
	if err := os.Mkdir(out+".manifest.json", 0o755); err != nil {
		t.Fatal(err)
	}
	stderrFile, err := os.Create(filepath.Join(dir, "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	defer func() { os.Stderr = oldStderr }()
	var stdout bytes.Buffer
	err = runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:  &flakyRunner{failAlways: map[string]bool{"pa-mixed": true}},
		Targets: []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces: []Trace{
			pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy),
			pairedCaptureTrace("pa-mixed", "p-a", AssemblyMixed),
		},
		OutputPath: out, Stdout: &stdout,
	})
	os.Stderr = oldStderr
	if closeErr := stderrFile.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("runCalibrateCapture = %v; want a loud manifest-write failure", err)
	}
	stderr, readErr := os.ReadFile(filepath.Join(dir, "stderr.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if stdout.Len() != 0 || strings.Contains(string(stderr), "WARNING partial capture") {
		t.Fatalf("failed publication emitted success output: stdout=%q stderr=%q", stdout.String(), stderr)
	}
	assertFileState(t, out, "prior artifacts\n", 0o640)
	assertNoPublicationDebris(t, dir)
}

func TestCaptureManifestSkippedForNonAssemblyCaptures(t *testing.T) {
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     &orderRecordingRunner{},
		Targets:    []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}},
		Traces:     []Trace{{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}}},
		OutputPath: out, Stdout: io.Discard,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}
	if _, err := os.Stat(out + ".manifest.json"); !os.IsNotExist(err) {
		t.Fatalf("manifest stat err = %v; a non-assembly capture must not write one", err)
	}
}

// resolveCandidateDigests resolves ollama digests via ShowModel and leaves
// openai-compat targets absent (no digest endpoint on that transport).
type fakeShowModeler struct {
	digests map[string]string
	err     error
}

func (f *fakeShowModeler) ShowModel(_ context.Context, name string) (*ollama.ModelInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.digests[name]
	if !ok {
		return nil, fmt.Errorf("unknown model %q", name)
	}
	return &ollama.ModelInfo{Name: name, Digest: d}, nil
}

func TestResolveCandidateDigests(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	targets := []ModelTarget{
		{Display: "m1", Provider: "ollama", Model: "m1"},
		{Display: "openai-compat/m2", Provider: "openai-compat", Model: "m2"},
		{Display: "missing", Provider: "ollama", Model: "missing"},
	}
	got := resolveCandidateDigests(context.Background(), &fakeShowModeler{digests: map[string]string{"m1": digest}}, targets)
	if got["m1"] != digest {
		t.Fatalf("digest for m1 = %q; want %s", got["m1"], digest)
	}
	if _, ok := got["openai-compat/m2"]; ok {
		t.Fatal("openai-compat target must not get a digest (no digest endpoint)")
	}
	if d, ok := got["missing"]; ok && d != "" {
		t.Fatalf("failed ShowModel produced digest %q; want absent/empty (errors swallowed)", d)
	}

	// A resolver that fails outright degrades to no digests, never an error.
	down := resolveCandidateDigests(context.Background(), &fakeShowModeler{err: fmt.Errorf("ollama down")}, targets)
	if len(down) != 0 {
		t.Fatalf("digests from failing resolver = %v; want empty", down)
	}
}

func TestCanonicalSHA256Digest(t *testing.T) {
	hexLower := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hexUpper := strings.ToUpper(hexLower)
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"bare", hexLower, "sha256:" + hexLower},
		{"prefixed", "sha256:" + hexLower, "sha256:" + hexLower},
		{"uppercase", "SHA256:" + hexUpper, "sha256:" + hexLower},
		{"short", "sha256:abc", ""},
		{"long", "sha256:" + hexLower + "0", ""},
		{"nonhex", "sha256:" + strings.Repeat("g", 64), ""},
		{"leading whitespace", " " + hexLower, ""},
		{"trailing whitespace", hexLower + "\n", ""},
		{"control", hexLower[:32] + "\x00" + hexLower[33:], ""},
		{"oversized", strings.Repeat("a", 4096), ""},
		{"secret marker", "sk-FAKE-DIGEST-SECRET-0000", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalSHA256Digest(tc.raw); got != tc.want {
				t.Fatalf("canonicalSHA256Digest() = %q; want %q", got, tc.want)
			}
		})
	}
}
