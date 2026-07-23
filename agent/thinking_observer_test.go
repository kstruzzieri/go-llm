package agent

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// thinkingThenAnswerCaller streams a fixed sequence of chunks (thinking
// deltas followed by content) through the onToken callback, then returns a
// final Done response with the full answer.
type thinkingThenAnswerCaller struct {
	chunks []provider.ChatResponse
	answer string
}

func (c *thinkingThenAnswerCaller) Chat(_ context.Context, _ provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {
	if onToken != nil {
		for _, chunk := range c.chunks {
			if err := onToken(chunk); err != nil {
				return ModelResult{}, err
			}
		}
	}
	return ModelResult{Response: provider.ChatResponse{Content: c.answer, Done: true}}, nil
}

func thinkingCaller() *thinkingThenAnswerCaller {
	return &thinkingThenAnswerCaller{
		chunks: []provider.ChatResponse{
			{Thinking: "step one"},
			{Thinking: "step two", Content: "answer"},
		},
		answer: "answer",
	}
}

// thinkingRec implements Observer AND ThinkingObserver, recording OnThinking
// and OnToken calls in ONE interleaved log so the cross-callback ordering
// contract (OnThinking before OnToken for the same chunk) is falsifiable.
// failAt (1-based) makes the Nth OnThinking return an error.
type thinkingRec struct {
	log    []string // "thinking:<step>:<content>" / "token:<step>:<content>" in call order
	failAt int
	n      int
}

func (r *thinkingRec) OnStep(context.Context, StepEvent) error         { return nil }
func (r *thinkingRec) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (r *thinkingRec) OnToken(_ context.Context, e TokenEvent) error {
	r.log = append(r.log, fmt.Sprintf("token:%d:%s", e.Step, e.Content))
	return nil
}
func (r *thinkingRec) OnThinking(_ context.Context, e ThinkingEvent) error {
	r.n++
	r.log = append(r.log, fmt.Sprintf("thinking:%d:%s", e.Step, e.Content))
	if r.failAt > 0 && r.n == r.failAt {
		return fmt.Errorf("boom")
	}
	return nil
}

// plainThinkingObs implements ONLY Observer (regression guard: thinking-only
// chunks must not reach it, and OnToken must still see answer content).
type plainThinkingObs struct {
	tokens []TokenEvent
}

func (p *plainThinkingObs) OnStep(context.Context, StepEvent) error         { return nil }
func (p *plainThinkingObs) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (p *plainThinkingObs) OnToken(_ context.Context, e TokenEvent) error {
	p.tokens = append(p.tokens, e)
	return nil
}

func TestThinkingForwardedToThinkingObserver(t *testing.T) {
	o := newTestOrchestrator(thinkingCaller())
	rec := &thinkingRec{}
	res, err := o.Run(context.Background(), Request{Goal: "q"}, rec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "answer" {
		t.Fatalf("res.Answer = %q, want %q", res.Answer, "answer")
	}
	// Exact interleaved sequence: the mixed chunk MUST produce its thinking
	// delta before its token delta.
	want := []string{
		"thinking:0:step one",
		"thinking:0:step two",
		"token:0:answer",
	}
	if !slices.Equal(rec.log, want) {
		t.Fatalf("event log = %q, want %q", rec.log, want)
	}
}

func TestPlainObserverUnaffectedByThinking(t *testing.T) {
	o := newTestOrchestrator(thinkingCaller())
	obs := &plainThinkingObs{}
	if _, ok := Observer(obs).(ThinkingObserver); ok {
		t.Fatal("plainThinkingObs must not satisfy ThinkingObserver")
	}
	res, err := o.Run(context.Background(), Request{Goal: "q"}, obs)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "answer" {
		t.Fatalf("res.Answer = %q, want %q", res.Answer, "answer")
	}
	if len(obs.tokens) != 1 || obs.tokens[0].Content != "answer" {
		t.Fatalf("tokens = %+v, want single %q (current behavior preserved)", obs.tokens, "answer")
	}
}

func TestThinkingObserverErrorAbortsRun(t *testing.T) {
	o := newTestOrchestrator(thinkingCaller())
	rec := &thinkingRec{failAt: 1}
	_, err := o.Run(context.Background(), Request{Goal: "q"}, rec)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("OnThinking error must abort Run, got %v", err)
	}
}
