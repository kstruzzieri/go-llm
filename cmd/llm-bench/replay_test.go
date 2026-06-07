package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newBlockedServer returns a test server that flags any received request.
// Callers assert !called to prove replay refused the trace before reaching
// the network.
func newBlockedServer(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

func rolesAndContent(messages []ollama.ChatMessage) []string {
	result := make([]string, 0, len(messages))
	for _, msg := range messages {
		result = append(result, msg.Role+":"+msg.Content)
	}
	return result
}

// requestLog is a goroutine-safe accumulator for request payloads observed
// by an httptest handler. Tests read snapshots from the test goroutine
// while the handler appends from the server goroutine — without this
// indirection, `go test -race` would flag the shared-slice access even
// though HTTP round-tripping currently happens to establish happens-
// before.
type requestLog struct {
	mu       sync.Mutex
	requests []ollama.ChatRequest
}

func (r *requestLog) append(req ollama.ChatRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
}

func (r *requestLog) snapshot() []ollama.ChatRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ollama.ChatRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

func (r *requestLog) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func TestReplayRefusesTraceWithoutUserTurn(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{ID: "no-user", Turns: []Turn{{Role: "assistant", Content: "hi"}}}

	_, err := replay(context.Background(), client, "m", trace)
	if !errors.Is(err, errNoUserTurn) {
		t.Fatalf("err = %v, want errNoUserTurn", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}

// thinkMarkupResidueNote flags answers that still carry <think> reasoning
// markup after extraction (e.g. a serving backend that left tags inline or
// used non-default delimiters), so a reviewer can discount the score.
func TestThinkMarkupResidueNote(t *testing.T) {
	if note := thinkMarkupResidueNote("clean final answer"); note != "" {
		t.Fatalf("clean answer: note = %q; want empty", note)
	}
	if note := thinkMarkupResidueNote("<think>leaked reasoning</think>"); note == "" {
		t.Fatal("well-formed residual <think> markup: note = empty; want a divergence note")
	}
	if note := thinkMarkupResidueNote("<think>unclosed reasoning that extraction cannot strip"); note == "" {
		t.Fatal("unclosed <think> markup: note = empty; want a divergence note")
	}
}

// When the serving backend leaves reasoning markup inline (so think extraction
// can't strip it), replayWith should annotate the result so the residue is
// visible to reviewers rather than silently scored as the answer.
func TestReplayWith_NotesResidualThinkMarkup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Model:           "test-model",
			Done:            true,
			Message:         ollama.ChatMessage{Role: "assistant", Content: "<think>unclosed reasoning leaking into the final answer"},
			PromptEvalCount: 1,
			EvalCount:       1,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "think-residue",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
	}

	out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "test-model", trace, replayOptions{})
	if err != nil {
		t.Fatalf("replayWith: %v", err)
	}

	found := false
	for _, note := range out.Notes {
		if strings.Contains(note, "<think") {
			found = true
		}
	}
	if !found {
		t.Fatalf("out.Notes = %#v; want a note flagging residual <think> markup", out.Notes)
	}
}

// Companion to TestReplayWith_NotesResidualThinkMarkup: a well-formed <think>
// block on the openai-compat path is stripped by default extraction, so the
// scored answer is clean and no residue note is recorded.
func TestReplayWith_OpenAICompatStripsThinkNoResidueNote(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-test",
			"model":"served",
			"choices":[{"index":0,"message":{"role":"assistant","content":"<think>private reasoning</think>final"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
		}`))
	}))
	t.Cleanup(srv.Close)

	tr, err := newCandidateTransport(ModelTarget{Display: "openai-compat/fake", Provider: "openai-compat", Model: "fake"}, candidateTransportOptions{
		openAICompatBaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("newCandidateTransport: %v", err)
	}

	trace := Trace{
		ID:     "think-clean",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
	}
	out, err := replayWith(context.Background(), tr.chat, "fake", trace, replayOptions{})
	if err != nil {
		t.Fatalf("replayWith: %v", err)
	}
	if got := lastAssistantContent(out.Transcript); got != "final" {
		t.Fatalf("final answer = %q; want %q (think block must be stripped before scoring)", got, "final")
	}
	for _, note := range out.Notes {
		if strings.Contains(note, "<think") {
			t.Fatalf("unexpected residue note for a well-formed think block: %q", note)
		}
	}
}

func TestReplaySupportsMultipleUserTurns(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)

		resp := ollama.ChatResponse{
			Model: "test-model",
			Done:  true,
		}
		switch log.len() {
		case 1:
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "model ack"}
		case 2:
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "final answer"}
		default:
			t.Fatalf("unexpected request %d", log.len())
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "multi-turn",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "captured ack"},
			{Role: "user", Content: "second"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 2 {
		t.Fatalf("transcript len = %d, want 2: %#v", len(transcript), transcript)
	}
	if transcript[0].Content != "model ack" || transcript[1].Content != "final answer" {
		t.Fatalf("transcript = %#v, want candidate assistant turns", transcript)
	}
	requests := log.snapshot()
	if len(requests) != 2 {
		t.Fatalf("requests len = %d, want 2", len(requests))
	}
	gotRoles := rolesAndContent(requests[1].Messages)
	wantRoles := []string{
		"system:sys",
		"user:first",
		"assistant:model ack",
		"user:second",
	}
	if !reflect.DeepEqual(gotRoles, wantRoles) {
		t.Fatalf("second request messages = %#v, want %#v", gotRoles, wantRoles)
	}
}

// replayWith must isolate prompt-eval and generation tokens (not just a
// combined total) and accumulate each across turns; TotalTokens stays the sum
// for back-compat.
func TestReplayWith_IsolatesGenAndPromptEvalTokens(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)
		resp := ollama.ChatResponse{
			Model:           "test-model",
			Done:            true,
			Message:         ollama.ChatMessage{Role: "assistant", Content: "ans"},
			PromptEvalCount: 10,
			EvalCount:       20,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "two-turn",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "captured ack"},
			{Role: "user", Content: "second"},
		},
	}
	out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "test-model", trace, replayOptions{})
	if err != nil {
		t.Fatalf("replayWith: %v", err)
	}
	if out.PromptEvalTokens != 20 {
		t.Errorf("PromptEvalTokens = %d; want 20 (10 x 2 turns)", out.PromptEvalTokens)
	}
	if out.GenTokens != 40 {
		t.Errorf("GenTokens = %d; want 40 (20 x 2 turns)", out.GenTokens)
	}
	if out.TotalTokens != 60 {
		t.Errorf("TotalTokens = %d; want 60 (prompt+gen, back-compat)", out.TotalTokens)
	}
}

func TestReplayInjectsFrozenToolResults(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)

		resp := ollama.ChatResponse{
			Model: "test-model",
			Done:  true,
		}
		switch log.len() {
		case 1:
			if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read_file" {
				t.Fatalf("tools = %#v, want read_file", req.Tools)
			}
			resp.Message = ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:   "candidate-call",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "read_file",
							Arguments: map[string]any{"path": "provider/router.go"},
						},
					},
				},
			}
		case 2:
			if len(req.Messages) != 4 {
				t.Fatalf("second request messages len = %d, want 4: %#v", len(req.Messages), req.Messages)
			}
			tool := req.Messages[3]
			if tool.Role != "tool" || tool.ToolName != "read_file" {
				t.Fatalf("tool message = %#v, want read_file result", tool)
			}
			if tool.ToolCallID != "candidate-call" {
				t.Fatalf("tool_call_id = %q, want candidate-call", tool.ToolCallID)
			}
			if tool.Content != "package provider" {
				t.Fatalf("tool content = %q, want frozen result", tool.Content)
			}
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "router summary"}
		default:
			t.Fatalf("unexpected request %d", log.len())
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-loop",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{
				Role:      "assistant",
				ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}},
			},
			{Role: "tool", Name: "read_file", ToolCallID: "captured-call", Content: "package provider"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 3 {
		t.Fatalf("transcript len = %d, want 3: %#v", len(transcript), transcript)
	}
	if got := extractToolNames(transcript); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("extractToolNames() = %v, want [read_file]", got)
	}
	if transcript[1].Role != "tool" || transcript[1].Name != "read_file" {
		t.Fatalf("tool transcript turn = %#v, want read_file result", transcript[1])
	}
	if transcript[1].ToolCallID != "candidate-call" {
		t.Fatalf("transcript tool_call_id = %q, want candidate-call", transcript[1].ToolCallID)
	}
	if transcript[2].Content != "router summary" {
		t.Fatalf("final transcript = %#v, want router summary", transcript[2])
	}
}

// TestReplayLeavesToolCallIDEmptyWhenCandidateOmitsIt verifies the
// regression fix for the captured-ID fallback: when the candidate omits
// an `id` on the tool call, the injected tool-result message must not
// borrow the captured trace's `ToolCallID` (which belonged to a different
// conversation and would mislead any model that routes by ID).
func TestReplayLeavesToolCallIDEmptyWhenCandidateOmitsIt(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)

		resp := ollama.ChatResponse{Model: "test-model", Done: true}
		switch log.len() {
		case 1:
			resp.Message = ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "read_file",
							Arguments: map[string]any{"path": "provider/router.go"},
						},
					},
				},
			}
		case 2:
			tool := req.Messages[3]
			if tool.ToolCallID != "" {
				t.Fatalf("tool_call_id = %q, want empty (candidate omitted id; must not borrow captured-call)", tool.ToolCallID)
			}
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "ok"}
		default:
			t.Fatalf("unexpected request %d", log.len())
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "no-id-fallback",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}}},
			{Role: "tool", Name: "read_file", ToolCallID: "captured-call", Content: "package provider"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if transcript[1].ToolCallID != "" {
		t.Fatalf("transcript tool_call_id = %q, want empty", transcript[1].ToolCallID)
	}
}

func TestReplayErrorsWhenCandidateToolCallDoesNotMatchFrozenResult(t *testing.T) {
	var requests = struct {
		sync.Mutex
		n int
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Lock()
		requests.n++
		requests.Unlock()
		resp := ollama.ChatResponse{
			Model: "test-model",
			Message: ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:   "candidate-call",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "write_file",
							Arguments: map[string]any{"path": "provider/router.go"},
						},
					},
				},
			},
			Done: true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-mismatch",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}}},
			{Role: "tool", Name: "read_file", Content: "package provider"},
		},
	}

	_, err := replay(context.Background(), client, "test-model", trace)
	if !errors.Is(err, errToolCallMismatch) {
		t.Fatalf("err = %v, want errToolCallMismatch", err)
	}
	requests.Lock()
	defer requests.Unlock()
	if requests.n != 1 {
		t.Fatalf("requests = %d, want 1", requests.n)
	}
}

// TestReplayErrorsWhenCandidateCallsToolForPlainTextScript guards the
// sentinel correction: a candidate that emits tool calls against a
// scripted plain-text assistant turn is a *mismatch*, not a missing
// tool result.
func TestReplayErrorsWhenCandidateCallsToolForPlainTextScript(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)

		resp := ollama.ChatResponse{
			Model: "test-model",
			Message: ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:       "candidate-call",
						Type:     "function",
						Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{}},
					},
				},
			},
			Done: true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "plain-text-scripted",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file","inputSchema":{"type":"object"}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "What is 2+2?"},
			{Role: "assistant", Content: "4"},
		},
	}

	_, err := replay(context.Background(), client, "test-model", trace)
	if !errors.Is(err, errToolCallMismatch) {
		t.Fatalf("err = %v, want errToolCallMismatch", err)
	}
	if errors.Is(err, errMissingToolResult) {
		t.Fatalf("err = %v, must not be errMissingToolResult", err)
	}
}

// TestReplayErrorsWhenCandidateCallsToolWithNoScriptedAssistant guards
// the second sentinel-taxonomy fix: when the trace has no scripted
// assistant turn after the user turn, a candidate tool call yields
// errMissingScriptedAssistant, not errMissingToolResult.
func TestReplayErrorsWhenCandidateCallsToolWithNoScriptedAssistant(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Model: "test-model",
			Message: ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:       "candidate-call",
						Type:     "function",
						Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{}},
					},
				},
			},
			Done: true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "no-scripted-assistant",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file","inputSchema":{"type":"object"}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read it"},
		},
	}

	_, err := replay(context.Background(), client, "test-model", trace)
	if !errors.Is(err, errMissingScriptedAssistant) {
		t.Fatalf("err = %v, want errMissingScriptedAssistant", err)
	}
}

// TestReplayRecordsBypassNoteWhenCandidateSkipsScriptedTools verifies
// that silently fast-forwarding past a scripted tool loop records a
// Notes annotation on the resulting Score — preserving the historical
// "refuse rather than mislead" intent in observability form.
func TestReplayRecordsBypassNoteWhenCandidateSkipsScriptedTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant", Content: "I already know the answer"},
			Done:    true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "skip-tool-route",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read","inputSchema":{"type":"object"}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}}},
			{Role: "tool", Name: "read_file", Content: "package provider"},
			{Role: "assistant", Content: "scripted summary"},
		},
	}

	out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "test-model", trace, replayOptions{})
	if err != nil {
		t.Fatalf("replayWith() error: %v", err)
	}
	if len(out.Notes) == 0 {
		t.Fatalf("expected divergence Notes, got none")
	}
	if len(out.Transcript) != 1 || out.Transcript[0].Content != "I already know the answer" {
		t.Fatalf("transcript = %#v, want single bypass assistant turn", out.Transcript)
	}
}

// TestReplayRejectsEmptyAssistantReply guards the empty-reply sentinel:
// a candidate that produces no content AND no tool calls fails the
// replay rather than being scored as a valid empty answer.
func TestReplayRejectsEmptyAssistantReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant"},
			Done:    true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "empty-reply",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "hi"}},
	}

	_, err := replay(context.Background(), client, "test-model", trace)
	if !errors.Is(err, errEmptyAssistantReply) {
		t.Fatalf("err = %v, want errEmptyAssistantReply", err)
	}
}

// TestReplaySupportsMultipleToolRoundsPerUserTurn drives the inner-loop's
// second iteration: the candidate calls tool A, gets a frozen result,
// then calls tool B, gets another result, then emits a final text
// answer — all within a single user turn.
func TestReplaySupportsMultipleToolRoundsPerUserTurn(t *testing.T) {
	log := &requestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		log.append(req)

		resp := ollama.ChatResponse{Model: "test-model", Done: true}
		switch log.len() {
		case 1:
			resp.Message = ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID: "call-a", Type: "function",
						Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{"path": "a"}},
					},
				},
			}
		case 2:
			resp.Message = ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID: "call-b", Type: "function",
						Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{"path": "b"}},
					},
				},
			}
		case 3:
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "summary of a and b"}
		default:
			t.Fatalf("unexpected request %d", log.len())
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "two-rounds",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file","inputSchema":{"type":"object"}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "summarize files a and b"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)}}},
			{Role: "tool", Name: "read_file", Content: "contents-a"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"b"}`)}}},
			{Role: "tool", Name: "read_file", Content: "contents-b"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if log.len() != 3 {
		t.Fatalf("requests = %d, want 3 (two tool rounds + final answer)", log.len())
	}
	// Transcript shape: [assistant-call-a, tool-a, assistant-call-b, tool-b, assistant-text]
	if len(transcript) != 5 {
		t.Fatalf("transcript len = %d, want 5: %#v", len(transcript), transcript)
	}
	if got := extractToolNames(transcript); !reflect.DeepEqual(got, []string{"read_file", "read_file"}) {
		t.Fatalf("extractToolNames() = %v, want [read_file read_file]", got)
	}
	if transcript[4].Role != "assistant" || transcript[4].Content != "summary of a and b" {
		t.Fatalf("final transcript = %#v, want assistant summary", transcript[4])
	}
}

func TestReplayForwardsTraceTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(req.Tools))
		}
		if req.Tools[0].Type != "function" {
			t.Fatalf("tool type = %q, want function", req.Tools[0].Type)
		}
		if req.Tools[0].Function.Name != "read_file" {
			t.Fatalf("tool name = %q, want read_file", req.Tools[0].Function.Name)
		}

		var schema map[string]any
		if err := json.Unmarshal(req.Tools[0].Function.Parameters, &schema); err != nil {
			t.Fatalf("unmarshal schema: %v", err)
		}
		if schema["type"] != "object" {
			t.Fatalf("schema type = %v, want object", schema["type"])
		}

		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant", Content: "done"},
			Done:    true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-trace",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{{Role: "user", Content: "Read provider/router.go"}},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 1 || transcript[0].Content != "done" {
		t.Fatalf("transcript = %#v, want single assistant turn with content", transcript)
	}
}

// TestReplayPropagatesNumCtxAndPerTurnTimeout verifies the new Runner
// knobs reach Ollama: NumCtx is set on req.Options, and PerTurnTimeout
// bounds an individual chat round-trip even when the parent context has
// a much larger budget.
func TestReplayPropagatesNumCtxOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Options == nil {
			t.Fatalf("Options = nil, want NumCtx propagated")
		}
		if req.Options.NumCtx != 32768 {
			t.Fatalf("NumCtx = %d, want 32768", req.Options.NumCtx)
		}
		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant", Content: "ok"},
			Done:    true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "numctx",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "hi"}},
	}

	out, err := replayWith(context.Background(), ollamaCandidateClient{client: client}, "test-model", trace, replayOptions{NumCtx: 32768})
	if err != nil {
		t.Fatalf("replayWith() error: %v", err)
	}
	if len(out.TurnLatenciesMs) != 1 {
		t.Fatalf("TurnLatenciesMs len = %d, want 1", len(out.TurnLatenciesMs))
	}
}

func TestReplayRejectsInvalidTraceTool(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{
		ID:     "bad-tool",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file","inputSchema":123}`),
		},
		Turns: []Turn{{Role: "user", Content: "hi"}},
	}

	_, err := replay(context.Background(), client, "m", trace)
	if !errors.Is(err, errInvalidTraceTool) {
		t.Fatalf("err = %v, want errInvalidTraceTool", err)
	}
	if *called {
		t.Error("replay reached Ollama despite invalid tool definition")
	}
}
