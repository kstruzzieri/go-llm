package main

import (
	"bufio"
	"context"
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
	// FNV-1a 64 parities (computed independently of the implementation):
	//   "p-a" -> 8681626274009790885 (odd)  => mixed-first
	//   "p-b" -> 8681622975474906252 (even) => legacy-first
	// Expected order: plain traces in input order, then pairs sorted by
	// PairID with the parity-selected arm first, then topline sorted by ID.
	traces := []Trace{
		{ID: "zz-plain", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
		toplineCaptureTrace("top-b"),
		pairedCaptureTrace("pb-mixed", "p-b", AssemblyMixed),
		pairedCaptureTrace("pa-legacy", "p-a", AssemblyLegacy),
		pairedCaptureTrace("pa-mixed", "p-a", AssemblyMixed),
		pairedCaptureTrace("pb-legacy", "p-b", AssemblyLegacy),
		toplineCaptureTrace("top-a"),
	}
	wantOrder := []string{"zz-plain", "pa-mixed", "pa-legacy", "pb-legacy", "pb-mixed", "top-a", "top-b"}
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

func TestAssemblyCaptureProvenanceAndUsage(t *testing.T) {
	traces := []Trace{
		pairedCaptureTrace("arm-legacy", "p-b", AssemblyLegacy),
		{ID: "plain-t", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "m", Provider: "ollama", Model: "m"}}
	out := filepath.Join(t.TempDir(), "artifacts.jsonl")
	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: &orderRecordingRunner{}, Targets: targets, Traces: traces, OutputPath: out,
		ModelDigests: map[string]string{"m": "sha256:model-digest"},
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
	if prov.ModelDigest != "sha256:model-digest" {
		t.Fatalf("capture model_digest = %q; want sha256:model-digest", prov.ModelDigest)
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
	targets := []ModelTarget{
		{Display: "m1", Provider: "ollama", Model: "m1"},
		{Display: "openai-compat/m2", Provider: "openai-compat", Model: "m2"},
		{Display: "missing", Provider: "ollama", Model: "missing"},
	}
	got := resolveCandidateDigests(context.Background(), &fakeShowModeler{digests: map[string]string{"m1": "sha256:abc"}}, targets)
	if got["m1"] != "sha256:abc" {
		t.Fatalf("digest for m1 = %q; want sha256:abc", got["m1"])
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
