package golem

// Package-internal tests for the Options.FailureMessage presentation seam: the
// private run.failed payload helper plus integration coverage of every
// reachable run.failed branch (reservation, session load, orchestrator /
// provider-or-observer, session save). turnGoal cannot currently fail (its
// JSON shape contains only strings), so its call site is covered by the
// helper unit test.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
)

func closeTestRuntime(t *testing.T, rt *Runtime) {
	t.Helper()
	if err := rt.Close(); err != nil {
		t.Errorf("Runtime.Close: %v", err)
	}
}

type stubSessionStore struct {
	loadErr error
	saveErr error
}

func (s *stubSessionStore) Load(_ context.Context, id string) (*conversation.Conversation, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return nil, fmt.Errorf("stub store: load %q: %w", id, conversation.ErrNotFound)
}

func (s *stubSessionStore) Save(context.Context, conversation.Conversation) error {
	return s.saveErr
}

type failingCaller struct{ err error }

func (c failingCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{}, c.err
}

type answeringCaller struct{}

func (answeringCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

type blockingChatCaller struct{ entered chan struct{} }

func (c blockingChatCaller) Chat(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	close(c.entered)
	<-ctx.Done()
	return agent.ModelResult{}, ctx.Err()
}

type tokenFailingObserver struct{ err error }

func (*tokenFailingObserver) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*tokenFailingObserver) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (o *tokenFailingObserver) OnToken(context.Context, agent.TokenEvent) error     { return o.err }

type recordingPresenter struct {
	mu    sync.Mutex
	codes []string
	errs  []error
}

func (p *recordingPresenter) present(code string, err error) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.codes = append(p.codes, code)
	p.errs = append(p.errs, err)
	return "presented: " + code
}

func (p *recordingPresenter) lastCode() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.codes) == 0 {
		return ""
	}
	return p.codes[len(p.codes)-1]
}

func decodeFailure(t *testing.T, events []Event) (failurePayload, bool) {
	t.Helper()
	for _, event := range events {
		if event.Type != "run.failed" {
			continue
		}
		var payload failurePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode run.failed: %v", err)
		}
		return payload, true
	}
	return failurePayload{}, false
}

func assertNoMarker(t *testing.T, events []Event, marker string) {
	t.Helper()
	for _, event := range events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		if strings.Contains(string(raw), marker) {
			t.Fatalf("event %s leaks marker %q: %s", event.Type, marker, raw)
		}
	}
}

func TestFailureMessagePresenterSanitizesReachableBranches(t *testing.T) {
	const (
		configMarker  = "/home/user/.config/golem/config.yaml-SECRET"
		bodyMarker    = "response body: SECRET-PROVIDER-BODY"
		keyMarker     = "sk-SECRET-API-KEY"
		consentMarker = "/home/user/.firn/consent.json-SECRET"
	)
	cases := []struct {
		name     string
		marker   string
		wantCode string
		options  func(*recordingPresenter) Options
		turn     Turn
	}{
		{
			name:     "session load failure",
			marker:   configMarker,
			wantCode: "internal",
			options: func(p *recordingPresenter) Options {
				return Options{
					SessionStore:   &stubSessionStore{loadErr: errors.New("load blew up reading " + configMarker)},
					Orchestrator:   agent.New(nil, agent.ContextManager{}),
					FailureMessage: p.present,
				}
			},
			turn: Turn{ThreadID: "thread-load", RunID: "run-load", Message: "go"},
		},
		{
			name:     "model caller failure",
			marker:   bodyMarker,
			wantCode: "internal",
			options: func(p *recordingPresenter) Options {
				return Options{
					Orchestrator:   agent.New(failingCaller{err: errors.New("provider rejected: " + bodyMarker)}, agent.ContextManager{}),
					FailureMessage: p.present,
				}
			},
			turn: Turn{RunID: "run-model", Message: "go"},
		},
		{
			name:     "host observer failure",
			marker:   keyMarker,
			wantCode: "observer_failed",
			options: func(p *recordingPresenter) Options {
				return Options{
					Orchestrator:   agent.New(answeringCaller{}, agent.ContextManager{}),
					FailureMessage: p.present,
				}
			},
			turn: Turn{
				RunID:    "run-observer",
				Message:  "go",
				Observer: &tokenFailingObserver{err: errors.New("host sink rejected token using " + keyMarker)},
			},
		},
		{
			name:     "session save failure",
			marker:   consentMarker,
			wantCode: "internal",
			options: func(p *recordingPresenter) Options {
				return Options{
					SessionStore:   &stubSessionStore{saveErr: errors.New("save denied by " + consentMarker)},
					Orchestrator:   agent.New(answeringCaller{}, agent.ContextManager{}),
					FailureMessage: p.present,
				}
			},
			turn: Turn{ThreadID: "thread-save", RunID: "run-save", Message: "go"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			presenter := &recordingPresenter{}
			opts := tc.options(presenter)
			opts.Root = t.TempDir()
			rt, err := New(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeTestRuntime(t, rt) })

			var events []Event
			_, runErr := rt.Run(context.Background(), tc.turn, func(event Event) error {
				events = append(events, event)
				return nil
			})
			if runErr == nil {
				t.Fatal("Run succeeded, want failure")
			}
			if !strings.Contains(runErr.Error(), tc.marker) {
				t.Fatalf("Run error %q lost the original cause marker %q", runErr, tc.marker)
			}
			payload, ok := decodeFailure(t, events)
			if !ok {
				t.Fatalf("no run.failed event in %v", eventTypeNames(events))
			}
			if payload.Code != tc.wantCode || payload.Message != "presented: "+tc.wantCode {
				t.Fatalf("run.failed = %+v, want code %q with presented message", payload, tc.wantCode)
			}
			assertNoMarker(t, events, tc.marker)
			if got := presenter.lastCode(); got != tc.wantCode {
				t.Fatalf("presenter code = %q, want %q", got, tc.wantCode)
			}
			presenter.mu.Lock()
			presented := presenter.errs[len(presenter.errs)-1]
			presenter.mu.Unlock()
			if presented == nil || !strings.Contains(presented.Error(), tc.marker) {
				t.Fatalf("presenter error = %v, want original cause with marker", presented)
			}
		})
	}
}

func TestFailureMessageReceivesRunConflictOverride(t *testing.T) {
	const rootMarker = "/tmp/golem-canonical-root-SECRET"
	presenter := &recordingPresenter{}
	entered := make(chan struct{})
	rt, err := New(context.Background(), Options{
		Root:           t.TempDir(),
		SessionStore:   &stubSessionStore{},
		Orchestrator:   agent.New(blockingChatCaller{entered: entered}, agent.ContextManager{}),
		FailureMessage: presenter.present,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })

	threadID := "thread-" + rootMarker
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, runErr := rt.Run(firstCtx, Turn{ThreadID: threadID, RunID: "run-first", Message: "go"},
			func(Event) error { return nil })
		firstDone <- runErr
	}()
	<-entered

	var events []Event
	_, runErr := rt.Run(context.Background(), Turn{ThreadID: threadID, RunID: "run-second", Message: "go"},
		func(event Event) error {
			events = append(events, event)
			return nil
		})
	cancelFirst()
	if firstErr := <-firstDone; !errors.Is(firstErr, context.Canceled) {
		t.Fatalf("first Run error = %v, want context.Canceled", firstErr)
	}

	if !errors.Is(runErr, ErrRunConflict) {
		t.Fatalf("Run error = %v, want ErrRunConflict", runErr)
	}
	if !strings.Contains(runErr.Error(), rootMarker) {
		t.Fatalf("Run error %q lost the thread marker for host logging", runErr)
	}
	payload, ok := decodeFailure(t, events)
	if !ok {
		t.Fatalf("no run.failed event in %v", eventTypeNames(events))
	}
	if payload.Code != "run_conflict" || payload.Message != "presented: run_conflict" {
		t.Fatalf("run.failed = %+v, want presented run_conflict", payload)
	}
	// The envelope's threadId legitimately echoes the host's own correlation
	// value; the presenter contract covers the failure payloads, so assert on
	// those rather than the whole envelope.
	for _, event := range events {
		if strings.Contains(string(event.Payload), rootMarker) {
			t.Fatalf("event %s payload leaks marker: %s", event.Type, event.Payload)
		}
	}
	if got := presenter.lastCode(); got != "run_conflict" {
		t.Fatalf("presenter code = %q, want run_conflict", got)
	}
}

func TestFailureMessageNilPreservesCurrentFailureText(t *testing.T) {
	const marker = "/home/user/.config/golem/config.yaml-SECRET"
	rt, err := New(context.Background(), Options{
		Root:         t.TempDir(),
		SessionStore: &stubSessionStore{loadErr: errors.New("load blew up reading " + marker)},
		Orchestrator: agent.New(nil, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })

	var events []Event
	_, runErr := rt.Run(context.Background(), Turn{ThreadID: "thread-nil", RunID: "run-nil", Message: "go"},
		func(event Event) error {
			events = append(events, event)
			return nil
		})
	if runErr == nil {
		t.Fatal("Run succeeded, want failure")
	}
	payload, ok := decodeFailure(t, events)
	if !ok {
		t.Fatalf("no run.failed event in %v", eventTypeNames(events))
	}
	if payload.Code != "internal" || payload.Message != truncateErrorMessage(runErr.Error()) {
		t.Fatalf("run.failed = %+v, want existing truncated err.Error() text", payload)
	}
	if !strings.Contains(payload.Message, marker) {
		t.Fatalf("nil presenter must preserve current failure text, got %q", payload.Message)
	}
}

func TestFailureMessagePayloadHelper(t *testing.T) {
	cause := errors.New("boom at /secret/root")

	nilPresenter := &Runtime{}
	got := nilPresenter.runFailedPayload(cause)
	if got.Code != "internal" || got.Message != cause.Error() {
		t.Fatalf("nil presenter payload = %+v", got)
	}

	var seenCode string
	var seenErr error
	custom := &Runtime{failureMessage: func(code string, err error) string {
		seenCode, seenErr = code, err
		return "clean"
	}}
	got = custom.runFailedPayload(fmt.Errorf("%w: thread %q is active", ErrRunConflict, "/secret/thread"))
	if got.Code != "run_conflict" || got.Message != "clean" {
		t.Fatalf("custom presenter payload = %+v", got)
	}
	if seenCode != "run_conflict" || seenErr == nil || !strings.Contains(seenErr.Error(), "/secret/thread") {
		t.Fatalf("presenter saw code %q err %v, want final code and original error", seenCode, seenErr)
	}

	long := &Runtime{failureMessage: func(string, error) string {
		return strings.Repeat("m", 2*maxDisplayBytes)
	}}
	got = long.runFailedPayload(cause)
	if len(got.Message) > maxDisplayBytes || !strings.HasSuffix(got.Message, truncatedMarker) {
		t.Fatalf("presented message escaped the display cap: len=%d", len(got.Message))
	}
}

func TestFailureMessageNotInvokedForCancellation(t *testing.T) {
	presenter := &recordingPresenter{}
	entered := make(chan struct{})
	rt, err := New(context.Background(), Options{
		Root:           t.TempDir(),
		Orchestrator:   agent.New(blockingChatCaller{entered: entered}, agent.ContextManager{}),
		FailureMessage: presenter.present,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })

	var events []Event
	done := make(chan error, 1)
	go func() {
		_, runErr := rt.Run(context.Background(), Turn{RunID: "run-cancel", Message: "go"},
			func(event Event) error {
				events = append(events, event)
				return nil
			})
		done <- runErr
	}()
	<-entered
	if !rt.Cancel("run-cancel") {
		t.Fatal("Cancel did not find the active run")
	}
	runErr := <-done

	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", runErr)
	}
	if len(events) == 0 || events[len(events)-1].Type != "run.canceled" {
		t.Fatalf("terminal event = %v, want run.canceled", eventTypeNames(events))
	}
	presenter.mu.Lock()
	codes := append([]string(nil), presenter.codes...)
	presenter.mu.Unlock()
	if len(codes) != 0 {
		t.Fatalf("presenter invoked for a cancellation: codes = %v", codes)
	}
}

func eventTypeNames(events []Event) []string {
	names := make([]string, 0, len(events))
	for _, event := range events {
		names = append(names, event.Type)
	}
	return names
}
