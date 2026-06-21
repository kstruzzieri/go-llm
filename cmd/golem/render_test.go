package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestRenderer_StreamToolStepFinal(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock })

	ctx := context.Background()
	_ = r.OnToken(ctx, agent.TokenEvent{Step: 0, Content: "hello "})
	_ = r.OnToken(ctx, agent.TokenEvent{Step: 0, Content: "world"})
	_ = r.OnToolCall(ctx, agent.ToolCallEvent{
		Step: 0,
		Call: provider.ToolCall{Function: provider.ToolCallFunction{
			Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}},
	})
	clock = clock.Add(1500 * time.Millisecond)
	_ = r.OnStep(ctx, agent.StepEvent{
		Index:        0,
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "ollama", Model: "gemma4:31b"}},
		Pressure:     agent.Pressure{UsedPct: 0.45},
	})

	got := buf.String()
	want := "hello world" +
		"\n> read_file {\"path\":\"a.txt\"}\n" +
		"ollama/gemma4:31b · 1.5s · ctx 45% · step 1/16\n"
	if got != want {
		t.Errorf("output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderer_FinalFooter_Stopped(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock })
	clock = clock.Add(3 * time.Second)
	r.finalFooter(agent.Result{
		Steps:      make([]agent.StepRecord, 2),
		Usage:      provider.Usage{TotalTokens: 128},
		StopReason: agent.StepCapReached,
	})
	want := "done · 2 steps · 3.0s · 128 tok  stopped: step_cap_reached\n"
	if buf.String() != want {
		t.Errorf("final footer = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_OnStepStartsNewLineAfterUnterminatedToken(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock })

	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "answer"}); err != nil {
		t.Fatalf("OnToken: %v", err)
	}
	if err := r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}

	want := "answer\n? · 0.0s · ctx 0% · step 1/16\n"
	if buf.String() != want {
		t.Errorf("step footer after unterminated token = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_FinalFooter_Completed_NoStoppedSuffix(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock })
	clock = clock.Add(2 * time.Second)
	r.finalFooter(agent.Result{
		Steps:      make([]agent.StepRecord, 1),
		Usage:      provider.Usage{TotalTokens: 42},
		StopReason: agent.Completed,
	})
	want := "done · 1 steps · 2.0s · 42 tok\n"
	if buf.String() != want {
		t.Errorf("completed footer = %q, want %q (no stopped suffix)", buf.String(), want)
	}
}

func TestRenderer_OnStep_NilRouteOutcome_RendersQuestionMark(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock })
	if err := r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}
	want := "? · 0.0s · ctx 0% · step 1/16\n"
	if buf.String() != want {
		t.Errorf("nil-RouteOutcome footer = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_OnToolCall_NoArgs_OmitsTrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) })
	if err := r.OnToolCall(context.Background(), agent.ToolCallEvent{
		Call: provider.ToolCall{Function: provider.ToolCallFunction{Name: "list"}},
	}); err != nil {
		t.Fatalf("OnToolCall: %v", err)
	}
	if buf.String() != "\n> list\n" {
		t.Errorf("no-args tool line = %q, want %q", buf.String(), "\n> list\n")
	}
}

func TestRenderer_Color_WrapsFooter(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, true, 16, func() time.Time { return clock })
	_ = r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}})
	got := buf.String()
	if got[:4] != "\x1b[2m" || got[len(got)-5:] != "\x1b[0m\n" {
		t.Errorf("color footer not dim-wrapped: %q", got)
	}
}
