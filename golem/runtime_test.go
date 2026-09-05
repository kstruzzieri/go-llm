package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

type scriptedCaller struct {
	result agent.ModelResult
}

func (s scriptedCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if err := onToken(provider.ChatResponse{Content: s.result.Response.Content}); err != nil {
		return agent.ModelResult{}, err
	}
	return s.result, nil
}

type toolCaller struct {
	calls int
}

func (c *toolCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls++
	if c.calls == 1 {
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID:   "call-1",
			Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "lookup",
				Arguments: json.RawMessage(`{"path":"file.txt"}`),
			},
		}}}}, nil
	}
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

type previewTool struct{}

func (previewTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:       "lookup",
		Parameters: json.RawMessage(`{"type":"object"}`),
	}
}

func (previewTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (previewTool) Plan(context.Context, json.RawMessage) (agent.ToolPlan, error) {
	return agent.ToolPlan{
		Effect:  agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever},
		Preview: "read: file.txt",
	}, nil
}

func (previewTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "contents", Preview: "1 match"}, nil
}

type largePreviewTool struct{}

func (largePreviewTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (largePreviewTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (largePreviewTool) Plan(context.Context, json.RawMessage) (agent.ToolPlan, error) {
	return agent.ToolPlan{
		Effect:  agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever},
		Preview: strings.Repeat("started", 2*1024),
	}, nil
}

func (largePreviewTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{
		Content: "contents",
		Preview: strings.Repeat("finished", 2*1024),
	}, nil
}

type failingTool struct{}

func (failingTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (failingTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (failingTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "lookup failed", Preview: "lookup failed", IsError: true}, nil
}

type namedTool string

func (n namedTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: string(n), Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (namedTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (namedTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

type sinkCancelCaller struct {
	canceled chan struct{}
}

func (c sinkCancelCaller) Chat(ctx context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if err := onToken(provider.ChatResponse{Content: "partial"}); err == nil {
		return agent.ModelResult{}, errors.New("test caller: sink unexpectedly accepted delta")
	}
	select {
	case <-ctx.Done():
		close(c.canceled)
		return agent.ModelResult{}, ctx.Err()
	case <-time.After(time.Second):
		return agent.ModelResult{}, errors.New("test caller: runtime did not cancel context")
	}
}

type errorCaller struct {
	err error
}

func (c errorCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{}, c.err
}

type contextCaller struct{}

func (contextCaller) Chat(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	<-ctx.Done()
	return agent.ModelResult{}, ctx.Err()
}

type barrierCaller struct {
	entered chan string
}

func (c barrierCaller) Chat(ctx context.Context, request provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.entered <- request.Messages[len(request.Messages)-1].Content
	<-ctx.Done()
	return agent.ModelResult{}, ctx.Err()
}

type countingCaller struct {
	calls int
}

func (c *countingCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls++
	return agent.ModelResult{Response: provider.ChatResponse{Content: "unexpected", Done: true}}, nil
}

type captureCaller struct {
	answer   string
	requests []provider.ChatRequest
}

func (c *captureCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if err := onToken(provider.ChatResponse{Content: c.answer}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: c.answer, Done: true}}, nil
}

type mapSessionStore struct {
	mu            sync.Mutex
	conversations map[string]conversation.Conversation
	loadErr       error
	saveErr       error
	compressedErr error
	loads         int
	saves         int
}

var _ golem.SessionStore = (*mapSessionStore)(nil)

func (s *mapSessionStore) Load(ctx context.Context, id string) (*conversation.Conversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	conv, ok := s.conversations[id]
	if !ok {
		return nil, fmt.Errorf("map session store: load %q: %w", id, conversation.ErrNotFound)
	}
	cloned := cloneConversation(conv)
	return &cloned, nil
}

func (s *mapSessionStore) Save(ctx context.Context, conv conversation.Conversation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.saveErr != nil {
		return s.saveErr
	}
	if conv.DurableSummary != nil && s.compressedErr != nil {
		return s.compressedErr
	}
	if s.conversations == nil {
		s.conversations = make(map[string]conversation.Conversation)
	}
	s.conversations[conv.ID] = cloneConversation(conv)
	return nil
}

func cloneConversation(conv conversation.Conversation) conversation.Conversation {
	cloned := conv
	cloned.Messages = append([]conversation.Message(nil), conv.Messages...)
	for i := range cloned.Messages {
		cloned.Messages[i].ToolCalls = append(json.RawMessage(nil), conv.Messages[i].ToolCalls...)
	}
	if conv.DurableSummary != nil {
		summary := *conv.DurableSummary
		cloned.DurableSummary = &summary
	}
	return cloned
}

type closeTrackingSessionStore struct {
	mapSessionStore
	closeCalls int
}

func (s *closeTrackingSessionStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	return nil
}

type blockingCompressionStore struct {
	mapSessionStore
	started chan struct{}
	once    sync.Once
}

func (s *blockingCompressionStore) Save(ctx context.Context, conv conversation.Conversation) error {
	if conv.DurableSummary == nil {
		return s.mapSessionStore.Save(ctx, conv)
	}
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

type malformedSessionStore struct {
	conversation *conversation.Conversation
}

func (s malformedSessionStore) Load(context.Context, string) (*conversation.Conversation, error) {
	return s.conversation, nil
}

func (malformedSessionStore) Save(context.Context, conversation.Conversation) error { return nil }

type thinkingCaller struct{}

func (thinkingCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if err := onToken(provider.ChatResponse{Thinking: "secret chain", Content: "answer"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{
		Thinking: "secret chain",
		Content:  "answer",
		Done:     true,
	}}, nil
}

type hostObserver struct {
	thinking string
	tokens   string
}

func (*hostObserver) OnStep(context.Context, agent.StepEvent) error { return nil }

func (*hostObserver) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }

func (o *hostObserver) OnToken(_ context.Context, event agent.TokenEvent) error {
	o.tokens += event.Content
	return nil
}

type failingHostObserver struct {
	err error
}

func (*failingHostObserver) OnStep(context.Context, agent.StepEvent) error { return nil }

func (*failingHostObserver) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }

func (o *failingHostObserver) OnToken(context.Context, agent.TokenEvent) error { return o.err }

type retrievalPresentationHost struct {
	events []agent.RetrievalPresentationEvent
	err    error
}

func (*retrievalPresentationHost) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*retrievalPresentationHost) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (*retrievalPresentationHost) OnToken(context.Context, agent.TokenEvent) error       { return nil }
func (h *retrievalPresentationHost) OnRetrievalPresentation(_ context.Context, e agent.RetrievalPresentationEvent) error {
	h.events = append(h.events, e)
	return h.err
}

func (o *hostObserver) OnThinking(_ context.Context, event agent.ThinkingEvent) error {
	o.thinking += event.Content
	return nil
}

func TestRunEmitsOrderedV1TextEvents(t *testing.T) {
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "test", Model: "model"}}
	orch := agent.New(scriptedCaller{
		result: agent.ModelResult{
			Response:     provider.ChatResponse{Content: "hello", Done: true},
			RouteOutcome: outcome,
		},
	}, agent.ContextManager{})
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: orch,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	result, err := runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-1",
		Message: "say hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Answer != "hello" {
		t.Fatalf("Answer = %q, want hello", result.Answer)
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v, want started, delta, finished", events)
	}

	wantTypes := []string{"run.started", "message.delta", "run.finished"}
	for i, event := range events {
		if event.Protocol != 1 || event.RunID != "run-1" || event.Seq != uint64(i+1) || event.Type != wantTypes[i] {
			t.Fatalf("event %d = %#v", i, event)
		}
	}
	var delta struct {
		MessageID string `json:"messageId"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(events[1].Payload, &delta); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if delta.MessageID == "" || delta.Text != "hello" {
		t.Fatalf("delta = %#v", delta)
	}
	var finished struct {
		StopReason string `json:"stopReason"`
		Model      string `json:"model"`
	}
	if err := json.Unmarshal(events[2].Payload, &finished); err != nil {
		t.Fatalf("decode finished: %v", err)
	}
	if finished.StopReason != "completed" || finished.Model != "test/model" {
		t.Fatalf("finished = %#v", finished)
	}
}

func TestRunEmitsToolEventsFromDisplayPreviews(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&toolCaller{}, agent.ContextManager{}),
		Tools:        []agent.Tool{previewTool{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-tool",
		Message: "look it up",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantTypes := []string{
		"run.started",
		"tool.started",
		"tool.finished",
		"message.delta",
		"run.finished",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event types = %v, want %v", eventTypes(events), wantTypes)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Fatalf("event types = %v, want %v", eventTypes(events), wantTypes)
		}
	}

	var started struct {
		ToolCallID string `json:"toolCallId"`
		Name       string `json:"name"`
		Preview    string `json:"preview"`
	}
	if err := json.Unmarshal(events[1].Payload, &started); err != nil {
		t.Fatalf("decode tool.started: %v", err)
	}
	if started.ToolCallID != "call-1" || started.Name != "lookup" || started.Preview != "read: file.txt" {
		t.Fatalf("tool.started = %#v", started)
	}
	var finished struct {
		ToolCallID string `json:"toolCallId"`
		Name       string `json:"name"`
		Preview    string `json:"preview"`
		IsError    bool   `json:"isError"`
	}
	if err := json.Unmarshal(events[2].Payload, &finished); err != nil {
		t.Fatalf("decode tool.finished: %v", err)
	}
	if finished.ToolCallID != "call-1" || finished.Name != "lookup" || finished.Preview != "1 match" || finished.IsError {
		t.Fatalf("tool.finished = %#v", finished)
	}
}

func TestRunReportsToolErrorInToolFinished(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&toolCaller{}, agent.ContextManager{}),
		Tools:        []agent.Tool{failingTool{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-tool-error",
		Message: "look it up",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var finished *struct {
		IsError bool   `json:"isError"`
		Preview string `json:"preview"`
	}
	for _, event := range events {
		if event.Type != "tool.finished" {
			continue
		}
		finished = &struct {
			IsError bool   `json:"isError"`
			Preview string `json:"preview"`
		}{}
		if err := json.Unmarshal(event.Payload, finished); err != nil {
			t.Fatalf("decode tool.finished: %v", err)
		}
	}
	if finished == nil || !finished.IsError {
		t.Fatalf("tool.finished = %+v, want isError true", finished)
	}
}

func TestMaxMessageBytesOptionRaisesTurnBound(t *testing.T) {
	message := strings.Repeat("m", 100*1024)
	defaultRuntime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{Response: provider.ChatResponse{Content: "ok", Done: true}}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = defaultRuntime.Close() })
	if _, err := defaultRuntime.Run(context.Background(), golem.Turn{RunID: "run-default-cap", Message: message},
		func(golem.Event) error { return nil }); !errors.Is(err, golem.ErrInvalidRequest) {
		t.Fatalf("default cap error = %v, want ErrInvalidRequest", err)
	}

	raised, err := golem.New(context.Background(), golem.Options{
		Root:            t.TempDir(),
		MaxMessageBytes: 1024 * 1024,
		Budget:          agent.Budget{InputCeiling: 256 * 1024},
		Orchestrator:    agent.New(scriptedCaller{result: agent.ModelResult{Response: provider.ChatResponse{Content: "ok", Done: true}}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = raised.Close() })
	if _, err := raised.Run(context.Background(), golem.Turn{RunID: "run-raised-cap", Message: message},
		func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("raised cap Run: %v", err)
	}
}

func TestRetainReasoningControlsThinkingScrub(t *testing.T) {
	caller := scriptedCaller{result: agent.ModelResult{Response: provider.ChatResponse{Content: "ok", Thinking: "chain of thought", Done: true}}}
	for _, tc := range []struct {
		name   string
		retain bool
	}{
		{name: "default scrubs", retain: false},
		{name: "trusted host retains", retain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:            t.TempDir(),
				RetainReasoning: tc.retain,
				Orchestrator:    agent.New(caller, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close() })
			result, err := runtime.Run(context.Background(), golem.Turn{RunID: "run-reasoning", Message: "q"},
				func(golem.Event) error { return nil })
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(result.Steps) == 0 {
				t.Fatal("no steps recorded")
			}
			got := result.Steps[len(result.Steps)-1].Response.Thinking
			if tc.retain && got != "chain of thought" {
				t.Fatalf("thinking = %q, want retained", got)
			}
			if !tc.retain && got != "" {
				t.Fatalf("thinking = %q, want scrubbed", got)
			}
		})
	}
}

func TestRunBoundsToolDisplayPreviews(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&toolCaller{}, agent.ContextManager{}),
		Tools:        []agent.Tool{largePreviewTool{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-preview-bounds",
		Message: "look it up",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, event := range events {
		if event.Type != "tool.started" && event.Type != "tool.finished" {
			continue
		}
		var payload struct {
			Preview string `json:"preview"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode %s: %v", event.Type, err)
		}
		if len(payload.Preview) > 8*1024 || !strings.HasSuffix(payload.Preview, "[truncated]") {
			t.Fatalf("%s preview bytes=%d suffix=%q", event.Type, len(payload.Preview), payload.Preview[len(payload.Preview)-min(len(payload.Preview), 32):])
		}
	}
}

func TestRunSinkFailureCancelsAndReturnsSinkError(t *testing.T) {
	canceled := make(chan struct{})
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(sinkCancelCaller{canceled: canceled}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	sinkErr := errors.New("consumer disconnected")
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-sink",
		Message: "stream",
	}, func(event golem.Event) error {
		if event.Type == "message.delta" {
			return sinkErr
		}
		return nil
	})
	if !errors.Is(err, sinkErr) {
		t.Fatalf("Run error = %v, want sink error", err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("model context was not canceled")
	}
}

func TestRunModelFailureEmitsOneFailedTerminal(t *testing.T) {
	modelErr := errors.New("model exploded")
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(errorCaller{err: modelErr}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-fail",
		Message: "fail",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, modelErr) {
		t.Fatalf("Run error = %v, want model error", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(events[1].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "internal" || failed.Message != "model exploded" {
		t.Fatalf("run.failed = %#v", failed)
	}
}

func TestRunCancellationEmitsCanceledTerminal(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(contextCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var events []golem.Event
	_, err = runtime.Run(ctx, golem.Turn{
		RunID:   "run-cancel",
		Message: "wait",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.canceled"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if string(events[1].Payload) != "{}" {
		t.Fatalf("run.canceled payload = %s, want {}", events[1].Payload)
	}
}

func TestStatefulRunCancellationEmitsCanceledTerminal(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(contextCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var events []golem.Event
	_, err = runtime.Run(ctx, golem.Turn{
		ThreadID: "thread-canceled-before-load",
		RunID:    "run-canceled-before-load",
		Message:  "wait",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.canceled"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
}

func TestCancelDuringDeltaWinsOverSuccessfulModelReturn(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response: provider.ChatResponse{Content: "already computed", Done: true},
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-cancel-delta",
		Message: "answer",
	}, func(event golem.Event) error {
		events = append(events, event)
		if event.Type == "message.delta" && !runtime.Cancel("run-cancel-delta") {
			t.Fatal("Cancel did not find active run")
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "message.delta", "run.canceled"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
}

func TestCancelFromTerminalSinkReturnsFalse(t *testing.T) {
	modelErr := errors.New("model failed")
	tests := []struct {
		name     string
		caller   agent.ModelCaller
		context  func() context.Context
		terminal string
		wantErr  error
	}{
		{
			name: "finished",
			caller: scriptedCaller{result: agent.ModelResult{
				Response: provider.ChatResponse{Content: "done", Done: true},
			}},
			context:  context.Background,
			terminal: "run.finished",
		},
		{
			name:     "failed",
			caller:   errorCaller{err: modelErr},
			context:  context.Background,
			terminal: "run.failed",
			wantErr:  modelErr,
		},
		{
			name:   "canceled",
			caller: &countingCaller{},
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			terminal: "run.canceled",
			wantErr:  context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Orchestrator: agent.New(tt.caller, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close() })

			cancelFound := true
			_, runErr := runtime.Run(tt.context(), golem.Turn{
				RunID:   "run-terminal-commit",
				Message: "answer",
			}, func(event golem.Event) error {
				if event.Type == tt.terminal {
					cancelFound = runtime.Cancel("run-terminal-commit")
				}
				return nil
			})
			if !errors.Is(runErr, tt.wantErr) {
				t.Fatalf("Run error = %v, want %v", runErr, tt.wantErr)
			}
			if cancelFound {
				t.Fatalf("Cancel found a run after %s was committed", tt.terminal)
			}
		})
	}
}

func TestDuplicateRunIDStaysEventlessWhileCloseWaitsForTerminalSink(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response: provider.ChatResponse{Content: "done", Done: true},
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	terminalEntered := make(chan struct{})
	releaseTerminal := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			RunID:   "run-closing-terminal",
			Message: "answer",
		}, func(event golem.Event) error {
			if event.Type == "run.finished" {
				close(terminalEntered)
				<-releaseTerminal
			}
			return nil
		})
		runDone <- runErr
	}()
	<-terminalEntered

	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()

	for probe := 0; ; probe++ {
		_, probeErr := runtime.Run(context.Background(), golem.Turn{
			RunID:   fmt.Sprintf("run-close-probe-%d", probe),
			Message: "probe",
		}, func(golem.Event) error { return nil })
		if errors.Is(probeErr, golem.ErrClosed) {
			break
		}
		if probeErr != nil && !errors.Is(probeErr, context.Canceled) {
			t.Fatalf("close probe %d: %v", probe, probeErr)
		}
	}

	var duplicateEvents []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-closing-terminal",
		Message: "duplicate",
	}, func(event golem.Event) error {
		duplicateEvents = append(duplicateEvents, event)
		return nil
	})
	if !errors.Is(err, golem.ErrRunConflict) {
		t.Fatalf("duplicate Run error = %v, want ErrRunConflict", err)
	}
	if len(duplicateEvents) != 0 {
		t.Fatalf("duplicate Run events = %v, want none", eventTypes(duplicateEvents))
	}

	close(releaseTerminal)
	if err := <-runDone; err != nil {
		t.Fatalf("original Run: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCancelFindsActiveRunAndStopsIt(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(contextCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			RunID:   "run-active",
			Message: "wait",
		}, func(event golem.Event) error {
			if event.Type == "run.started" {
				close(started)
			}
			return nil
		})
		done <- runErr
	}()
	<-started
	if !runtime.Cancel("run-active") {
		t.Fatal("Cancel returned false for active run")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if runtime.Cancel("run-active") {
		t.Fatal("Cancel returned true after run exited")
	}
}

func TestRunRejectsConcurrentTurnForSameThread(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := &mapSessionStore{}
	entered := make(chan string, 1)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		SessionStore: store,
		Orchestrator: agent.New(barrierCaller{entered: entered}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			ThreadID: "thread-1",
			RunID:    "run-1",
			Message:  "wait",
		}, func(golem.Event) error { return nil })
		firstDone <- runErr
	}()
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var events []golem.Event
	_, err = runtime.Run(ctx, golem.Turn{
		ThreadID: "thread-1",
		RunID:    "run-2",
		Message:  "must not reach model",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, golem.ErrRunConflict) {
		t.Fatalf("second Run error = %v, want ErrRunConflict", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(events[1].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "run_conflict" {
		t.Fatalf("run.failed code = %q, want run_conflict", failed.Code)
	}

	runtime.Cancel("run-1")
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run error = %v, want context canceled", err)
	}
	store.mu.Lock()
	loads := store.loads
	store.mu.Unlock()
	if loads != 1 {
		t.Fatalf("store loads = %d, want only the reserved run to load", loads)
	}
}

func TestRunRejectsDuplicateActiveRunIDWithoutEvents(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(contextCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	started := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			RunID:   "run-duplicate",
			Message: "wait",
		}, func(event golem.Event) error {
			if event.Type == "run.started" {
				close(started)
			}
			return nil
		})
		firstDone <- runErr
	}()
	<-started

	var duplicateEvents []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-duplicate",
		Message: "must not create a second stream",
	}, func(event golem.Event) error {
		duplicateEvents = append(duplicateEvents, event)
		return nil
	})
	if !errors.Is(err, golem.ErrRunConflict) {
		t.Fatalf("duplicate Run error = %v, want ErrRunConflict", err)
	}
	if len(duplicateEvents) != 0 {
		t.Fatalf("duplicate Run events = %v, want none", eventTypes(duplicateEvents))
	}

	if !runtime.Cancel("run-duplicate") {
		t.Fatal("Cancel did not find original run")
	}
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("original Run error = %v, want context canceled", err)
	}
}

func TestRunAllowsDifferentThreadsConcurrently(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	entered := make(chan string, 2)
	store := &mapSessionStore{}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		SessionStore: store,
		Orchestrator: agent.New(barrierCaller{entered: entered}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	done := make(chan error, 2)
	for i := 1; i <= 2; i++ {
		i := i
		go func() {
			_, runErr := runtime.Run(context.Background(), golem.Turn{
				ThreadID: fmt.Sprintf("thread-%d", i),
				RunID:    fmt.Sprintf("run-%d", i),
				Message:  fmt.Sprintf("message-%d", i),
			}, func(golem.Event) error { return nil })
			done <- runErr
		}()
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case message := <-entered:
			seen[message] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d model calls entered; different threads were serialized", len(seen))
		}
	}
	runtime.Cancel("run-1")
	runtime.Cancel("run-2")
	for i := 0; i < 2; i++ {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context canceled", err)
		}
	}
}

func TestCloseCancelsAndWaitsForActiveRuns(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(contextCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			RunID:   "run-close",
			Message: "wait",
		}, func(event golem.Event) error {
			if event.Type == "run.started" {
				close(started)
			}
			return nil
		})
		done <- runErr
	}()
	<-started
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned without stopping the active run")
	}
}

func TestRunAfterCloseIsNotReportedAsConflict(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-after-close",
		Message: "hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("Run error = %v, want ErrClosed", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(events[1].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "runtime_closed" {
		t.Fatalf("run.failed code = %q, want runtime_closed", failed.Code)
	}
}

func TestRunRejectsInvalidTurnBeforeStarting(t *testing.T) {
	tests := []struct {
		name string
		turn golem.Turn
	}{
		{
			name: "missing run ID",
			turn: golem.Turn{Message: "hello"},
		},
		{
			name: "empty message",
			turn: golem.Turn{RunID: "run-empty"},
		},
		{
			name: "run ID over 256 bytes",
			turn: golem.Turn{RunID: strings.Repeat("r", 257), Message: "hello"},
		},
		{
			name: "thread ID over 256 bytes",
			turn: golem.Turn{ThreadID: strings.Repeat("t", 257), RunID: "run-thread", Message: "hello"},
		},
		{
			name: "message over 64 KiB",
			turn: golem.Turn{RunID: "run-message", Message: strings.Repeat("x", 64*1024+1)},
		},
		{
			name: "context description over 256 bytes",
			turn: golem.Turn{
				RunID:   "run-description",
				Message: "hello",
				Context: []golem.ContextItem{{
					Description: strings.Repeat("x", 257),
					Value:       "value",
				}},
			},
		},
		{
			name: "context value over 64 KiB",
			turn: golem.Turn{
				RunID:   "run-value",
				Message: "hello",
				Context: []golem.ContextItem{{
					Description: "file",
					Value:       strings.Repeat("x", 64*1024+1),
				}},
			},
		},
		{
			name: "aggregate context over 256 KiB",
			turn: golem.Turn{
				RunID:   "run-context",
				Message: "hello",
				Context: []golem.ContextItem{
					{Description: "one", Value: strings.Repeat("x", 64*1024)},
					{Description: "two", Value: strings.Repeat("x", 64*1024)},
					{Description: "three", Value: strings.Repeat("x", 64*1024)},
					{Description: "four", Value: strings.Repeat("x", 64*1024)},
					{Description: "five", Value: "x"},
				},
			},
		},
		{
			name: "serialized aggregate context over 256 KiB",
			turn: golem.Turn{
				RunID:   "run-context-envelope",
				Message: "hello",
				Context: make([]golem.ContextItem, 10_000),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := &countingCaller{}
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Orchestrator: agent.New(caller, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if err := runtime.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			})

			var events []golem.Event
			_, err = runtime.Run(context.Background(), tt.turn, func(event golem.Event) error {
				events = append(events, event)
				return nil
			})
			if !errors.Is(err, golem.ErrInvalidRequest) {
				t.Fatalf("Run error = %v, want ErrInvalidRequest", err)
			}
			if caller.calls != 0 {
				t.Fatalf("model calls = %d, want 0", caller.calls)
			}
			if len(events) != 0 {
				t.Fatalf("events = %v, want none", eventTypes(events))
			}
		})
	}
}

func TestRunRejectsNilEventSink(t *testing.T) {
	caller := &countingCaller{}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-nil-sink",
		Message: "hello",
	}, nil)
	if !errors.Is(err, golem.ErrInvalidRequest) {
		t.Fatalf("Run error = %v, want ErrInvalidRequest", err)
	}
	if caller.calls != 0 {
		t.Fatalf("model calls = %d, want 0", caller.calls)
	}
}

func TestStatelessRunDoesNotCreateSessionDatabase(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response: provider.ChatResponse{Content: "answer", Done: true},
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-stateless",
		Message: "hello",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(dataDir, "golem", "sessions.db")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session database stat error = %v, want not exist", err)
	}
}

func TestInjectedSessionStorePreservesNativeHistoryAndThreadIsolationAcrossRuntimes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	store := &mapSessionStore{}

	first, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(&captureCaller{answer: "first answer"}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := first.Run(context.Background(), golem.Turn{
		ThreadID: "thread-injected",
		RunID:    "run-injected-first",
		Message:  "first question",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	secondCaller := &captureCaller{answer: "second answer"}
	second, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(secondCaller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
	if _, err := second.Run(context.Background(), golem.Turn{
		ThreadID: "thread-other",
		RunID:    "run-injected-other",
		Message:  "unrelated question",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("other thread Run: %v", err)
	}
	isolated := []provider.ChatMessage{
		{Role: "system", Content: golem.SystemPrompt(false, false) + "\n\n" + agent.ToolTrustContract},
		{Role: "user", Content: "unrelated question"},
	}
	if got := secondCaller.requests[0].Messages; !reflect.DeepEqual(got, isolated) {
		t.Fatalf("other thread messages = %#v, want %#v", got, isolated)
	}

	if _, err := second.Run(context.Background(), golem.Turn{
		ThreadID: "thread-injected",
		RunID:    "run-injected-second",
		Message:  "second question",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	want := []provider.ChatMessage{
		{Role: "system", Content: golem.SystemPrompt(false, false) + "\n\n" + agent.ToolTrustContract},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
	}
	if got := secondCaller.requests[1].Messages; !reflect.DeepEqual(got, want) {
		t.Fatalf("second request messages = %#v, want %#v", got, want)
	}
}

func TestInjectedSessionStoreDoesNotOpenDefaultSessionDatabase(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: &mapSessionStore{},
		Orchestrator: agent.New(&captureCaller{answer: "answer"}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-no-default-db",
		RunID:    "run-no-default-db",
		Message:  "hello",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(root, "golem", "sessions.db")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session database stat error = %v, want not exist", err)
	}
}

func TestRuntimeCloseDoesNotCloseInjectedSessionStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	store := &closeTrackingSessionStore{}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(&captureCaller{answer: "answer"}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-owned-by-caller",
		RunID:    "run-owned-by-caller",
		Message:  "hello",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store.mu.Lock()
	closeCalls := store.closeCalls
	store.mu.Unlock()
	if closeCalls != 0 {
		t.Fatalf("store close calls = %d, want 0", closeCalls)
	}
	if _, err := store.Load(context.Background(), "thread-owned-by-caller"); err != nil {
		t.Fatalf("caller store unusable after Runtime.Close: %v", err)
	}
}

func TestInjectedSessionStoreLoadFailurePreservesRunFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	loadErr := errors.New("load failed")
	caller := &countingCaller{}
	store := &mapSessionStore{loadErr: loadErr}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-load-failure",
		RunID:    "run-load-failure",
		Message:  "hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, loadErr) || errors.Is(err, golem.ErrSessionPersistence) {
		t.Fatalf("Run error = %v, want load error without ErrSessionPersistence", err)
	}
	if caller.calls != 0 || store.saves != 0 {
		t.Fatalf("model calls = %d, store saves = %d, want 0, 0", caller.calls, store.saves)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(events[1].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "internal" {
		t.Fatalf("run.failed code = %q, want internal", failed.Code)
	}
}

func TestInjectedSessionStoreRejectsMalformedLoadResult(t *testing.T) {
	tests := []struct {
		name         string
		conversation *conversation.Conversation
	}{
		{name: "nil conversation"},
		{name: "wrong conversation ID", conversation: &conversation.Conversation{ID: "thread-other"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := &countingCaller{}
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				SessionStore: malformedSessionStore{conversation: tt.conversation},
				Orchestrator: agent.New(caller, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { _ = runtime.Close() })

			var events []golem.Event
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				_, err = runtime.Run(context.Background(), golem.Turn{
					ThreadID: "thread-requested",
					RunID:    "run-malformed-load",
					Message:  "hello",
				}, func(event golem.Event) error {
					events = append(events, event)
					return nil
				})
			}()
			if panicked != nil {
				t.Fatalf("Run panicked: %v", panicked)
			}
			if err == nil {
				t.Fatal("Run succeeded with malformed loaded conversation")
			}
			if caller.calls != 0 {
				t.Fatalf("model calls = %d, want 0", caller.calls)
			}
			if got, want := eventTypes(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
				t.Fatalf("event types = %v, want %v", got, want)
			}
		})
	}
}

func TestInjectedSessionStoreSaveFailurePreservesRunFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	saveErr := errors.New("save failed")
	store := &mapSessionStore{saveErr: saveErr}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(&captureCaller{answer: "answer"}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var events []golem.Event
	result, err := runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-save-failure",
		RunID:    "run-save-failure",
		Message:  "hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if result.Answer != "answer" {
		t.Fatalf("Run answer = %q, want answer", result.Answer)
	}
	if !errors.Is(err, saveErr) || !errors.Is(err, golem.ErrSessionPersistence) {
		t.Fatalf("Run error = %v, want save error and ErrSessionPersistence", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "message.delta", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(events[2].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "internal" {
		t.Fatalf("run.failed code = %q, want internal", failed.Code)
	}
}

func TestInjectedSessionStoreLoadCancellationPreservesCanceledTerminal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	caller := &countingCaller{}
	store := &mapSessionStore{loadErr: context.Canceled}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-load-canceled",
		RunID:    "run-load-canceled",
		Message:  "hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context canceled", err)
	}
	if caller.calls != 0 || store.saves != 0 {
		t.Fatalf("model calls = %d, store saves = %d, want 0, 0", caller.calls, store.saves)
	}
	if got, want := eventTypes(events), []string{"run.started", "run.canceled"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	if string(events[1].Payload) != "{}" {
		t.Fatalf("run.canceled payload = %s, want {}", events[1].Payload)
	}
}

func TestStatefulThreadPersistsAcrossRuntimeInstances(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := t.TempDir()

	firstCaller := &captureCaller{answer: "first answer"}
	first, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		Orchestrator: agent.New(firstCaller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := first.Run(context.Background(), golem.Turn{
		ThreadID: "thread-state",
		RunID:    "run-first",
		Message:  "first question",
		Context: []golem.ContextItem{{
			Description: "selection",
			Value:       "ephemeral context",
		}},
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	secondCaller := &captureCaller{answer: "second answer"}
	second, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		Orchestrator: agent.New(secondCaller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
	if _, err := second.Run(context.Background(), golem.Turn{
		ThreadID: "thread-state",
		RunID:    "run-second",
		Message:  "second question",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if len(secondCaller.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(secondCaller.requests))
	}
	got := secondCaller.requests[0].Messages
	want := []provider.ChatMessage{
		{Role: "system", Content: golem.SystemPrompt(false, false) + "\n\n" + agent.ToolTrustContract},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second question"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("second request messages = %#v, want %#v", got, want)
	}
}

func TestCanceledRunDoesNotPersistPartialTurn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := &mapSessionStore{}
	entered := make(chan string, 1)
	first, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(barrierCaller{entered: entered}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := first.Run(context.Background(), golem.Turn{
			ThreadID: "thread-canceled",
			RunID:    "run-canceled",
			Message:  "must not persist",
		}, func(golem.Event) error { return nil })
		done <- runErr
	}()
	<-entered
	first.Cancel("run-canceled")
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Run error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	store.mu.Lock()
	loads, saves := store.loads, store.saves
	store.mu.Unlock()
	if loads != 1 || saves != 0 {
		t.Fatalf("store loads, saves = %d, %d, want 1, 0", loads, saves)
	}

	caller := &captureCaller{answer: "done"}
	second, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		SessionStore: store,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("second New: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})
	if _, err := second.Run(context.Background(), golem.Turn{
		ThreadID: "thread-canceled",
		RunID:    "run-next",
		Message:  "next",
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("next Run: %v", err)
	}
	got := caller.requests[0].Messages
	if len(got) != 2 ||
		got[0].Role != "system" || got[0].Content != golem.SystemPrompt(false, false)+"\n\n"+agent.ToolTrustContract ||
		got[1].Role != "user" || got[1].Content != "next" {
		t.Fatalf("next request messages = %#v, canceled turn leaked into history", got)
	}
}

func TestUnansweredRunDoesNotPersistPartialTurn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	store := &mapSessionStore{}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		MaxSteps:     1,
		Tools:        []agent.Tool{previewTool{}},
		SessionStore: store,
		Orchestrator: agent.New(&toolCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-unanswered",
		RunID:    "run-unanswered",
		Message:  "look it up",
	}, func(golem.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Answer != "" || result.StopReason != agent.StepCapReached {
		t.Fatalf("result = %#v, want unanswered step-cap result", result)
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := store.Load(context.Background(), "thread-unanswered"); !errors.Is(err, conversation.ErrNotFound) {
		t.Fatalf("load unanswered thread error = %v, want ErrNotFound", err)
	}
	if store.saves != 0 {
		t.Fatalf("store saves = %d, want 0", store.saves)
	}
}

func TestStatefulThreadCompressesHistoryIntoDurableSummary(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	store := &mapSessionStore{}
	caller := &captureCaller{answer: strings.Repeat("a", 20*1024)}
	summaryCalls := 0
	summarize := func(_ context.Context, _ string, _ []conversation.Message) (string, error) {
		summaryCalls++
		return "compressed summary", nil
	}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		Budget:       agent.Budget{InputCeiling: 32 * 1024},
		Summarizer:   summarize,
		SessionStore: store,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	for i := 0; i < 5; i++ {
		_, err := runtime.Run(context.Background(), golem.Turn{
			ThreadID: "thread-compress",
			RunID:    fmt.Sprintf("run-compress-%d", i),
			Message:  fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024)),
		}, func(golem.Event) error { return nil })
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if summaryCalls == 0 {
		t.Fatal("summarizer was not called after history crossed the compression threshold")
	}

	caller.answer = "done"
	_, err = runtime.Run(context.Background(), golem.Turn{
		ThreadID: "thread-compress",
		RunID:    "run-after-compress",
		Message:  "next",
	}, func(golem.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run after compression: %v", err)
	}
	lastRequest := caller.requests[len(caller.requests)-1]
	wantSummary := agent.DurableSummaryPrompt("compressed summary")
	found := false
	for _, message := range lastRequest.Messages {
		if message.Role == "system" && message.Content == wantSummary {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("next model request did not contain durable summary %q", wantSummary)
	}
	saved, err := store.Load(context.Background(), "thread-compress")
	if err != nil {
		t.Fatalf("load injected compressed thread: %v", err)
	}
	if saved.DurableSummary == nil || saved.DurableSummary.Content != "compressed summary" {
		t.Fatalf("stored durable summary = %#v, want compressed summary", saved.DurableSummary)
	}
}

func TestInjectedSessionStoreCompressionSaveFailureKeepsSuccessfulTurn(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	compressionSaveErr := errors.New("compressed save failed")
	store := &mapSessionStore{compressedErr: compressionSaveErr}
	answer := strings.Repeat("a", 20*1024)
	var warnings []error
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:   root,
		Budget: agent.Budget{InputCeiling: 32 * 1024},
		Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
			return "compressed summary", nil
		},
		OnWarning:    func(err error) { warnings = append(warnings, err) },
		SessionStore: store,
		Orchestrator: agent.New(&captureCaller{answer: answer}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var lastQuestion string
	for i := 0; i < 5; i++ {
		lastQuestion = fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024))
		var events []golem.Event
		_, err = runtime.Run(context.Background(), golem.Turn{
			ThreadID: "thread-compression-error",
			RunID:    fmt.Sprintf("run-compression-error-%d", i),
			Message:  lastQuestion,
		}, func(event golem.Event) error {
			events = append(events, event)
			return nil
		})
		if err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
		if got := events[len(events)-1].Type; got != "run.finished" {
			t.Fatalf("terminal event %d = %q, want run.finished", i, got)
		}
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(warnings) == 0 || !errors.Is(warnings[len(warnings)-1], compressionSaveErr) {
		t.Fatalf("compression warnings = %v, want compressed save error", warnings)
	}
	saved, err := store.Load(context.Background(), "thread-compression-error")
	if err != nil {
		t.Fatalf("load saved thread: %v", err)
	}
	if !slices.ContainsFunc(saved.Messages, func(message conversation.Message) bool {
		return message.Role == "user" && message.Content == lastQuestion
	}) || !slices.ContainsFunc(saved.Messages, func(message conversation.Message) bool {
		return message.Role == "assistant" && message.Content == answer
	}) {
		t.Fatalf("raw turn missing after compressed save failure")
	}
	if saved.DurableSummary != nil {
		t.Fatalf("failed compressed snapshot was persisted: %#v", saved.DurableSummary)
	}
}

func TestCompressionProviderFailureWithoutWarningHandlerStaysSuccessful(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	answer := strings.Repeat("a", 20*1024)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:   t.TempDir(),
		Budget: agent.Budget{InputCeiling: 32 * 1024},
		Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
			return "", &provider.HTTPStatusError{StatusCode: http.StatusServiceUnavailable}
		},
		Orchestrator: agent.New(&captureCaller{answer: answer}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	for i := 0; i < 5; i++ {
		var events []golem.Event
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			ThreadID: "thread-compression-provider",
			RunID:    fmt.Sprintf("run-compression-provider-%d", i),
			Message:  fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024)),
		}, func(event golem.Event) error {
			events = append(events, event)
			return nil
		})
		if runErr != nil {
			t.Fatalf("Run %d: %v", i, runErr)
		}
		if got := events[len(events)-1].Type; got != "run.finished" {
			t.Fatalf("terminal event = %q, want run.finished", got)
		}
	}
}

// TestCancellationDuringCompressionKeepsCommittedTurn pins the durable-commit
// ordering: the turn saves before compression runs, so a cancellation arriving
// mid-compression loses only the compaction — never the answered turn — and the
// run still reports run.finished (Cancel finds nothing to cancel).
func TestCancellationDuringCompressionKeepsCommittedTurn(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	answer := strings.Repeat("a", 20*1024)
	var runtime *golem.Runtime
	var activeRunID string
	compressionSeen := false
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:   t.TempDir(),
		Budget: agent.Budget{InputCeiling: 32 * 1024},
		Summarizer: func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
			compressionSeen = true
			if runtime.Cancel(activeRunID) {
				return "", fmt.Errorf("compression must run after the terminal claim; Cancel(%q) succeeded", activeRunID)
			}
			return "", context.Canceled
		},
		Orchestrator: agent.New(&captureCaller{answer: answer}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var lastMessage string
	for i := 0; i < 8; i++ {
		activeRunID = fmt.Sprintf("run-compression-cancel-%d", i)
		lastMessage = fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024))
		var events []golem.Event
		_, runErr := runtime.Run(context.Background(), golem.Turn{
			ThreadID: "thread-compression-cancel",
			RunID:    activeRunID,
			Message:  lastMessage,
		}, func(event golem.Event) error {
			events = append(events, event)
			return nil
		})
		if runErr != nil {
			t.Fatalf("Run %d: %v", i, runErr)
		}
		if got := events[len(events)-1].Type; got != "run.finished" {
			t.Fatalf("terminal event %d = %q, want run.finished", i, got)
		}
		if compressionSeen {
			break
		}
	}
	if !compressionSeen {
		t.Fatal("compression did not occur")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db, err := memory.OpenHardenedDB(context.Background(), filepath.Join(os.Getenv("XDG_DATA_HOME"), "golem", "sessions.db"))
	if err != nil {
		t.Fatalf("open saved sessions: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := conversation.NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("open conversation store: %v", err)
	}
	saved, err := store.Load(context.Background(), "thread-compression-cancel")
	if err != nil {
		t.Fatalf("load saved thread: %v", err)
	}
	if !slices.ContainsFunc(saved.Messages, func(message conversation.Message) bool {
		return message.Role == "user" && message.Content == lastMessage
	}) {
		t.Fatal("turn interrupted during compression was not persisted")
	}
}

// TestCloseAbortsPostCommitCompression pins that Close cancels a run that is
// already terminal: only best-effort compression can still be running there,
// and shutdown must not wait out a summarizer model call it can abort.
func TestCloseAbortsPostCommitCompression(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	answer := strings.Repeat("a", 20*1024)
	inCompression := make(chan struct{})
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:   t.TempDir(),
		Budget: agent.Budget{InputCeiling: 32 * 1024},
		Summarizer: func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
			close(inCompression)
			<-ctx.Done()
			return "", ctx.Err()
		},
		Orchestrator: agent.New(&captureCaller{answer: answer}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		var runErr error
		for i := 0; i < 8; i++ {
			_, runErr = runtime.Run(context.Background(), golem.Turn{
				ThreadID: "thread-close-compression",
				RunID:    fmt.Sprintf("run-close-compression-%d", i),
				Message:  fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024)),
			}, func(golem.Event) error { return nil })
			if runErr != nil {
				break
			}
			select {
			case <-inCompression:
				runDone <- runErr
				return
			default:
			}
		}
		runDone <- runErr
	}()

	<-inCompression
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked on post-commit compression")
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestCloseAbortsInjectedCompressionSave(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)
	store := &blockingCompressionStore{started: make(chan struct{})}
	answer := strings.Repeat("a", 20*1024)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         root,
		Budget:       agent.Budget{InputCeiling: 32 * 1024},
		Summarizer:   func(context.Context, string, []conversation.Message) (string, error) { return "summary", nil },
		SessionStore: store,
		Orchestrator: agent.New(&captureCaller{answer: answer}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	runDone := make(chan error, 1)
	go func() {
		for i := 0; i < 8; i++ {
			_, runErr := runtime.Run(context.Background(), golem.Turn{
				ThreadID: "thread-close-injected-compression",
				RunID:    fmt.Sprintf("run-close-injected-compression-%d", i),
				Message:  fmt.Sprintf("%d-%s", i, strings.Repeat("q", 20*1024)),
			}, func(golem.Event) error { return nil })
			if runErr != nil {
				runDone <- runErr
				return
			}
			select {
			case <-store.started:
				runDone <- nil
				return
			default:
			}
		}
		runDone <- errors.New("compression save did not start")
	}()

	select {
	case <-store.started:
	case err := <-runDone:
		t.Fatalf("Run ended before compressed save: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("compressed save did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked on best-effort compressed save")
	}
	if err := <-runDone; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestNewDiscoversConfigAndBuildsRuntime(t *testing.T) {
	toolsSeen := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-coder-next:latest"}]}`))
		case "/v1/chat/completions":
			var body struct {
				Tools []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			names := make([]string, len(body.Tools))
			for i := range body.Tools {
				names[i] = body.Tools[i].Function.Name
			}
			toolsSeen <- names
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"model\":\"qwen3-coder-next:latest\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"from config\"},\"finish_reason\":\"stop\"}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"model\":\"qwen3-coder-next:latest\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
		"providers": {
			"test": {
				"base_url": %q,
				"api_format": "openai-compat",
				"timeout": "2s"
			}
		},
		"models": {
			"chat": {
				"name": "qwen3-coder-next:latest",
				"provider": "test",
				"type": "dense",
				"context_window": 32768,
				"capabilities": ["chat", "stream", "tool_call"]
			}
		},
		"defaults": {"agent": "chat"}
	}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("GO_LLM_CONFIG", configPath)

	runtime, err := golem.New(context.Background(), golem.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	result, err := runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-config",
		Message: "hello",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Answer != "from config" {
		t.Fatalf("Answer = %q, want from config", result.Answer)
	}
	if got, want := eventTypes(events), []string{"run.started", "message.delta", "run.finished"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	gotTools := <-toolsSeen
	slices.Sort(gotTools)
	if want := []string{"glob", "list", "read_file", "search"}; !slices.Equal(gotTools, want) {
		t.Fatalf("provider tools = %v, want %v", gotTools, want)
	}
}

func TestNewRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime, err := golem.New(ctx, golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}),
	})
	if runtime != nil {
		_ = runtime.Close()
		t.Fatal("New returned a runtime for canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("New error = %v, want context canceled", err)
	}
}

func TestNewReportsProviderBootstrapWarnings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
		"providers": {
			"test": {
				"base_url": %q,
				"api_format": "openai-compat",
				"timeout": "2s"
			}
		},
		"models": {
			"chat": {
				"name": "qwen3-coder-next:latest",
				"provider": "test",
				"type": "dense",
				"context_window": 32768,
				"capabilities": ["chat", "stream", "tool_call"]
			}
		},
		"defaults": {"agent": "chat"}
	}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var warnings []error
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:       t.TempDir(),
		ConfigPath: configPath,
		OnWarning:  func(err error) { warnings = append(warnings, err) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	if len(warnings) == 0 || !strings.Contains(warnings[0].Error(), "refresh") {
		t.Fatalf("bootstrap warnings = %v, want provider refresh warning", warnings)
	}
}

func TestNewRejectsInvalidConfigPath(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:       t.TempDir(),
		ConfigPath: filepath.Join(t.TempDir(), "missing.json"),
	})
	if runtime != nil {
		_ = runtime.Close()
		t.Fatal("New returned a runtime for a missing explicit config")
	}
	if err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("New error = %v, want config load error", err)
	}
}

func TestNewRejectsDuplicateToolNames(t *testing.T) {
	tests := []struct {
		name  string
		tools []agent.Tool
	}{
		{
			name:  "host duplicates built-in",
			tools: []agent.Tool{namedTool("read_file")},
		},
		{
			name:  "host duplicates host",
			tools: []agent.Tool{namedTool("custom"), namedTool("custom")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Tools:        tt.tools,
				Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}),
			})
			if err == nil {
				_ = runtime.Close()
				t.Fatal("New succeeded with duplicate tool names")
			}
			if !strings.Contains(err.Error(), "duplicate tool name") {
				t.Fatalf("New error = %v, want duplicate tool name", err)
			}
		})
	}
}

func TestRunAppliesHostInstructionsContextAndRequestOptions(t *testing.T) {
	caller := &captureCaller{answer: "answer"}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		System:       "base system",
		MaxSteps:     3,
		Budget:       agent.Budget{OutputReserve: 321},
		ModelOptions: provider.ModelOptions{ThinkEffort: "high"},
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:        "run-context",
		Message:      "question",
		Instructions: "produce only JSON",
		Context: []golem.ContextItem{{
			Description: "active file",
			Value:       "main.go",
		}},
	}, func(golem.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(caller.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(caller.requests))
	}
	request := caller.requests[0]
	// Host text, turn instructions, then the #430 base contract agent.Run appends.
	wantSystem := "base system\n\n--- GOLEM TURN INSTRUCTIONS ---\nproduce only JSON\n\n" + agent.ToolTrustContract
	if request.Messages[0].Role != "system" || request.Messages[0].Content != wantSystem {
		t.Fatalf("system message = %#v, want %q", request.Messages[0], wantSystem)
	}
	user := request.Messages[len(request.Messages)-1]
	if user.Role != "user" ||
		!strings.Contains(user.Content, "question") ||
		!strings.Contains(user.Content, `"description":"active file"`) ||
		!strings.Contains(user.Content, `"value":"main.go"`) {
		t.Fatalf("user message = %#v", user)
	}
	if request.Options.NumPredict != 321 || request.Options.ThinkEffort != "high" {
		t.Fatalf("model options = %#v", request.Options)
	}
}

func TestRunUsesDefaultGolemSystemPrompt(t *testing.T) {
	caller := &captureCaller{answer: "answer"}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-default-system",
		Message: "question",
	}, func(golem.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := caller.requests[0].Messages[0].Content; got != golem.SystemPrompt(false, false)+"\n\n"+agent.ToolTrustContract {
		t.Fatalf("system message = %q, want default Golem prompt plus the base contract", got)
	}
}

func TestRunDoesNotExposeRawReasoning(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(thinkingCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	result, err := runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-thinking",
		Message: "think",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshal events: %v", err)
	}
	if strings.Contains(string(raw), "secret chain") {
		t.Fatalf("events exposed reasoning: %s", raw)
	}
	for i, step := range result.Steps {
		if step.Response.Thinking != "" {
			t.Fatalf("result step %d exposed reasoning %q", i, step.Response.Thinking)
		}
	}
}

func TestRunForwardsReasoningOnlyToTrustedObserver(t *testing.T) {
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: agent.New(thinkingCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	host := &hostObserver{}
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:    "run-host-observer",
		Message:  "think",
		Observer: host,
	}, func(golem.Event) error { return nil })
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if host.thinking != "secret chain" || host.tokens != "answer" {
		t.Fatalf("host observer got thinking=%q tokens=%q", host.thinking, host.tokens)
	}
}

func TestRunHostObserverFailureUsesStableCode(t *testing.T) {
	observerErr := errors.New("renderer failed")
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response: provider.ChatResponse{Content: "answer", Done: true},
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:    "run-observer",
		Message:  "hello",
		Observer: &failingHostObserver{err: observerErr},
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if !errors.Is(err, observerErr) {
		t.Fatalf("Run error = %v, want observer error", err)
	}
	if got, want := eventTypes(events), []string{"run.started", "message.delta", "run.failed"}; !slices.Equal(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(events[2].Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "observer_failed" {
		t.Fatalf("run.failed code = %q, want observer_failed", failed.Code)
	}
}

func TestRunFailureCodesAreStable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "no viable provider", err: provider.ErrNoViableCandidate, code: "provider_unavailable"},
		{name: "breakers open", err: provider.ErrAllBreakersOpen, code: "provider_unavailable"},
		{name: "router closed", err: provider.ErrRouterClosed, code: "provider_unavailable"},
		{name: "provider HTTP 503", err: &provider.HTTPStatusError{StatusCode: 503}, code: "provider_unavailable"},
		{name: "context exhausted", err: agent.ErrContextExhausted, code: "invalid_request"},
		{name: "budget exceeded", err: provider.ErrBudgetExceeded, code: "invalid_request"},
		{name: "provider mismatch", err: provider.ErrProviderMismatch, code: "invalid_request"},
		{name: "unknown", err: errors.New("boom"), code: "internal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Orchestrator: agent.New(errorCaller{err: tt.err}, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if err := runtime.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			})

			var events []golem.Event
			_, err = runtime.Run(context.Background(), golem.Turn{
				RunID:   "run-code",
				Message: "fail",
			}, func(event golem.Event) error {
				events = append(events, event)
				return nil
			})
			if !errors.Is(err, tt.err) {
				t.Fatalf("Run error = %v, want %v", err, tt.err)
			}
			var failed struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(events[len(events)-1].Payload, &failed); err != nil {
				t.Fatalf("decode run.failed: %v", err)
			}
			if failed.Code != tt.code {
				t.Fatalf("run.failed code = %q, want %q", failed.Code, tt.code)
			}
		})
	}
}

func TestRunSplitsOversizedDeltaWithinEventLimit(t *testing.T) {
	answer := strings.Repeat("\"\\\n界", 40*1024)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response: provider.ChatResponse{Content: answer, Done: true},
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   strings.Repeat("run", 32),
		Message: "stream",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var joined strings.Builder
	deltas := 0
	for i, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event %d: %v", i, err)
		}
		if len(raw) > 128*1024 {
			t.Fatalf("event %d bytes = %d, want at most 131072", i, len(raw))
		}
		if event.Seq != uint64(i+1) {
			t.Fatalf("event %d seq = %d, want %d", i, event.Seq, i+1)
		}
		if event.Type == "message.delta" {
			deltas++
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("decode delta %d: %v", i, err)
			}
			joined.WriteString(payload.Text)
		}
	}
	if deltas < 2 {
		t.Fatalf("delta events = %d, want split delivery", deltas)
	}
	if joined.String() != answer {
		t.Fatalf("joined delta bytes = %d, want %d", joined.Len(), len(answer))
	}
}

func TestRunBoundsFinishedModelMetadata(t *testing.T) {
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{
		Provider: strings.Repeat("p", 128*1024),
		Model:    "model",
	}}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root: t.TempDir(),
		Orchestrator: agent.New(scriptedCaller{result: agent.ModelResult{
			Response:     provider.ChatResponse{Content: "done", Done: true},
			RouteOutcome: outcome,
		}}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })

	var events []golem.Event
	_, err = runtime.Run(context.Background(), golem.Turn{
		RunID:   "run-large-model",
		Message: "answer",
	}, func(event golem.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := events[len(events)-1].Type; got != "run.finished" {
		t.Fatalf("terminal event = %q, want run.finished", got)
	}
	var finished struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(events[len(events)-1].Payload, &finished); err != nil {
		t.Fatalf("decode run.finished: %v", err)
	}
	if len(finished.Model) > 8*1024 || !strings.Contains(finished.Model, "[truncated]") {
		t.Fatalf("bounded model metadata length = %d, value suffix missing truncation marker", len(finished.Model))
	}
}

func eventTypes(events []golem.Event) []string {
	types := make([]string, len(events))
	for i := range events {
		types[i] = events[i].Type
	}
	return types
}

// toolWireCaller issues one lookup call per turn, then answers, recording
// every request.
type toolWireCaller struct {
	requests []provider.ChatRequest
}

func (c *toolWireCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if len(c.requests)%2 == 1 {
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(`{"path":"file.txt"}`)},
		}}}}, nil
	}
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

// TestRuntimeFramesObservationsPerRenderAcrossThreads (#430): one Runtime
// serving two threads frames each render under its own key, the host's
// custom System gets the base contract, and the stored observation is raw.
func TestRuntimeFramesObservationsPerRenderAcrossThreads(t *testing.T) {
	caller := &toolWireCaller{}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{}}
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		System:       "HOST PROMPT",
		Tools:        []agent.Tool{previewTool{}},
		Orchestrator: agent.New(caller, agent.ContextManager{}),
		SessionStore: store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	for _, thread := range []string{"thread-a", "thread-b"} {
		if _, err := runtime.Run(context.Background(), golem.Turn{ThreadID: thread, RunID: "run-" + thread, Message: "q"}, func(golem.Event) error { return nil }); err != nil {
			t.Fatalf("%s: %v", thread, err)
		}
	}
	if len(caller.requests) != 4 {
		t.Fatalf("requests = %d, want 2 per turn", len(caller.requests))
	}
	if got := caller.requests[0].Messages[0].Content; got != "HOST PROMPT\n\n"+agent.ToolTrustContract {
		t.Errorf("custom System = %q, want the host prompt plus the base contract", got)
	}
	var keys []string
	for _, i := range []int{1, 3} {
		var tool *provider.ChatMessage
		for j := range caller.requests[i].Messages {
			if caller.requests[i].Messages[j].Role == "tool" {
				tool = &caller.requests[i].Messages[j]
			}
		}
		if tool == nil {
			t.Fatalf("request %d has no tool message", i)
		}
		k := golem.ToolFrameKey(t, tool.Content)
		if tool.Content != golem.FramedToolResult(k, "contents") {
			t.Errorf("request %d tool message = %q, want %q", i, tool.Content, golem.FramedToolResult(k, "contents"))
		}
		keys = append(keys, k)
	}
	if keys[0] == keys[1] {
		t.Errorf("two renders share key %q", keys[0])
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, thread := range []string{"thread-a", "thread-b"} {
		conv, ok := store.conversations[thread]
		if !ok {
			t.Fatalf("%s not saved", thread)
		}
		found := false
		for _, m := range conv.Messages {
			if m.Role == "tool" {
				found = true
				if m.Content != "contents" {
					t.Errorf("%s stored tool observation = %q, want raw %q", thread, m.Content, "contents")
				}
			}
		}
		if !found {
			t.Errorf("%s: no tool observation stored", thread)
		}
	}
}
