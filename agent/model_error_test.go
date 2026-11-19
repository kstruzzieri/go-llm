package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// statusCaller streams one chunk (when it has text) and then fails. A
// callback error is returned wrapped, as a real provider stream does;
// otherwise err is returned unchanged.
type statusCaller struct {
	content, thinking string
	err               error
}

func (c statusCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
	partial := provider.ChatResponse{Content: c.content, Thinking: c.thinking}
	if onToken != nil && (c.content != "" || c.thinking != "") {
		if cbErr := onToken(partial); cbErr != nil {
			return ModelResult{Response: partial}, fmt.Errorf("provider: chat stream: %w", cbErr)
		}
	}
	return ModelResult{Response: partial}, c.err
}

// callbackErrObserver fails the stream callback it is told to fail.
type callbackErrObserver struct {
	token, thinking error
}

func (callbackErrObserver) OnStep(context.Context, StepEvent) error         { return nil }
func (callbackErrObserver) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (o callbackErrObserver) OnToken(context.Context, TokenEvent) error     { return o.token }
func (o callbackErrObserver) OnThinking(context.Context, ThinkingEvent) error {
	return o.thinking
}

func TestRunRecordsModelError(t *testing.T) {
	status := &provider.HTTPStatusError{StatusCode: 400}
	o := newTestOrchestrator(statusCaller{err: status})
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != error(status) {
		t.Fatalf("err = %#v, want the model caller's error unchanged", err)
	}
	if res.ModelError != error(status) {
		t.Fatalf("res.ModelError = %#v, want the model caller's error", res.ModelError)
	}
}

func TestRunCallbackErrorIsNotModelError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller statusCaller
		obs    func(error) callbackErrObserver
	}{
		{"token", statusCaller{content: "partial"}, func(e error) callbackErrObserver { return callbackErrObserver{token: e} }},
		{"thinking", statusCaller{thinking: "hmm"}, func(e error) callbackErrObserver { return callbackErrObserver{thinking: e} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hostErr := &provider.HTTPStatusError{StatusCode: 422}
			o := newTestOrchestrator(tc.caller)
			res, err := o.Run(context.Background(), Request{Goal: "q"}, tc.obs(hostErr))
			if !errors.Is(err, hostErr) {
				t.Fatalf("err = %v, want the observer's error", err)
			}
			if res.ModelError != nil {
				t.Fatalf("res.ModelError = %v, want nil for the run's own callback error", res.ModelError)
			}
		})
	}
}

func TestRunInspectionJoinIsNotModelError(t *testing.T) {
	status := &provider.HTTPStatusError{StatusCode: 400}
	ic := &stubInterceptor{name: "guard", output: func(out OutputInspection) []Finding {
		if out.Content == "partial" {
			return []Finding{{Rule: "leak", Verdict: VerdictBlock, Risk: 60, Target: TargetOutputContent}}
		}
		return nil
	}}
	o := newTestOrchestrator(statusCaller{content: "partial", err: status}, WithInterceptors(ic))
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	var blocked *BlockedError
	if !errors.As(err, &blocked) || !errors.Is(err, status) {
		t.Fatalf("err = %v, want the block joined with the model error", err)
	}
	if res.ModelError != nil {
		t.Fatalf("res.ModelError = %v, want nil when a block is joined", res.ModelError)
	}
}

func TestResultModelErrorNotSerialized(t *testing.T) {
	b, err := json.Marshal(Result{ModelError: errors.New("x")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ModelError") {
		t.Fatalf("Result JSON = %s, want no ModelError key", b)
	}
}
