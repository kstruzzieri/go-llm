package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// approvalTool is a write tool, so a trusted profile asks the turn's approver.
type approvalTool struct{}

func (approvalTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "put_note", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (approvalTool) Effect() agent.Effect { return agent.Effect{Class: agent.Write} }
func (approvalTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

// failingApprover fails every approval with err and counts the asks.
type failingApprover struct {
	err   error
	calls int
}

func (a *failingApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	a.calls++
	return false, a.err
}

// runFailure runs one turn and returns the terminal event type, its decoded
// run.failed payload, and the run error.
func runFailure(t *testing.T, opts golem.Options, turn golem.Turn) (string, map[string]json.RawMessage, error) {
	t.Helper()
	opts.Root = t.TempDir()
	rt, err := golem.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if turn.RunID == "" {
		turn.RunID = "run-740"
	}
	if turn.Message == "" {
		turn.Message = "go"
	}
	var events []golem.Event
	_, runErr := rt.Run(context.Background(), turn, func(e golem.Event) error {
		events = append(events, e)
		return nil
	})
	if len(events) == 0 {
		t.Fatalf("no events; run error %v", runErr)
	}
	last := events[len(events)-1]
	var payload map[string]json.RawMessage
	if last.Type == "run.failed" {
		if err := json.Unmarshal(last.Payload, &payload); err != nil {
			t.Fatalf("decode %s: %v", last.Type, err)
		}
	}
	return last.Type, payload, runErr
}

func codeOf(t *testing.T, payload map[string]json.RawMessage) string {
	t.Helper()
	var code string
	if err := json.Unmarshal(payload["code"], &code); err != nil {
		t.Fatalf("decode code: %v", err)
	}
	return code
}

// A model caller can return validation errors by value, directly or inside
// a value wrapper whose error interface holds the slice.
type modelValidationError []string

func (e modelValidationError) Error() string     { return strings.Join(e, ", ") }
func (modelValidationError) HTTPStatusCode() int { return 422 }

type wrappedModelError struct{ error }

func (e wrappedModelError) Unwrap() error { return e.error }

func TestRunProviderRejectionWithUncomparableError(t *testing.T) {
	rejection := modelValidationError{"prompt too large"}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"slice", rejection},
		{"slice in interface field", wrappedModelError{rejection}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ, payload, runErr := runFailure(t, golem.Options{
				Orchestrator: agent.New(errorCaller{err: tc.err}, agent.ContextManager{}),
			}, golem.Turn{})
			var got modelValidationError
			if !errors.As(runErr, &got) || !slices.Equal(got, rejection) {
				t.Fatalf("Run error = %v, want model validation error %v", runErr, rejection)
			}
			if typ != "run.failed" {
				t.Fatalf("terminal event = %q, want run.failed", typ)
			}
			assertTwoKeyPayload(t, payload, "provider_rejected", rejection.Error())
		})
	}
}

func TestRunApproverStatusErrorIsNotProviderRejected(t *testing.T) {
	// The approver's error carries a 400, either its own or one forwarded
	// from another run's provider reply. Neither is this run's model reply.
	_, _, other := runFailure(t, golem.Options{Orchestrator: agent.New(errorCaller{err: &provider.HTTPStatusError{StatusCode: 400}}, agent.ContextManager{})}, golem.Turn{})
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"own status", &provider.HTTPStatusError{StatusCode: 400}},
		{"forwarded provider error", other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			approver := &failingApprover{err: tc.err}
			typ, payload, runErr := runFailure(t, golem.Options{
				Profile:      golem.ProfileTrusted,
				Tools:        []agent.Tool{approvalTool{}},
				Orchestrator: agent.New(&exposureCaller{tool: "put_note", answer: "x"}, agent.ContextManager{}),
			}, golem.Turn{Approver: approver})
			if approver.calls != 1 {
				t.Fatalf("approvals = %d, want 1", approver.calls)
			}
			if !errors.Is(runErr, tc.err) {
				t.Fatalf("run error = %v, want the approver's error", runErr)
			}
			if typ != "run.failed" || codeOf(t, payload) != "internal" {
				t.Fatalf("terminal = %s %v, want run.failed internal", typ, payload)
			}
		})
	}
}

func TestRunCanceledJoinedWithRejectionIsCanceled(t *testing.T) {
	err := errors.Join(context.Canceled, &provider.HTTPStatusError{StatusCode: 400})
	typ, _, _ := runFailure(t, golem.Options{Orchestrator: agent.New(errorCaller{err: err}, agent.ContextManager{})}, golem.Turn{})
	if typ != "run.canceled" {
		t.Fatalf("terminal = %s, want run.canceled", typ)
	}
}

func TestRunFailedPresenterReadsProviderStatus(t *testing.T) {
	newStatus := func() error { return &provider.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"} }
	t.Run("presenter", func(t *testing.T) {
		var gotCode string
		var gotStatus int
		present := func(code string, err error) string {
			gotCode = code
			var hs interface{ HTTPStatusCode() int }
			if errors.As(err, &hs) {
				gotStatus = hs.HTTPStatusCode()
			}
			return "presented"
		}
		typ, payload, _ := runFailure(t, golem.Options{
			FailureMessage: present,
			Orchestrator:   agent.New(errorCaller{err: newStatus()}, agent.ContextManager{}),
		}, golem.Turn{})
		if typ != "run.failed" || gotCode != "provider_rejected" || gotStatus != 404 {
			t.Fatalf("terminal %s, presenter saw code %q status %d; want run.failed, provider_rejected, 404", typ, gotCode, gotStatus)
		}
		assertTwoKeyPayload(t, payload, "provider_rejected", "presented")
	})
	t.Run("default message", func(t *testing.T) {
		_, payload, _ := runFailure(t, golem.Options{Orchestrator: agent.New(errorCaller{err: newStatus()}, agent.ContextManager{})}, golem.Turn{})
		assertTwoKeyPayload(t, payload, "provider_rejected", "404 Not Found")
	})
}

func assertTwoKeyPayload(t *testing.T, payload map[string]json.RawMessage, code, message string) {
	t.Helper()
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"code", "message"}) {
		t.Fatalf("payload keys = %v, want only code and message", keys)
	}
	var gotMessage string
	if err := json.Unmarshal(payload["message"], &gotMessage); err != nil {
		t.Fatal(err)
	}
	if codeOf(t, payload) != code || gotMessage != message {
		t.Fatalf("payload = %s / %q, want %s / %q", payload["code"], gotMessage, code, message)
	}
}
