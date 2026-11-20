package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
)

func exposureAnswer() *scriptedCaller {
	return &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{Content: "ok", Done: true}}}}
}

func TestExposurePriorState(t *testing.T) {
	hist := []provider.ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}
	unknown := Exposure{Recorded: true, Unknown: true, First: "history"}
	for _, tc := range []struct {
		name string
		req  Request
		want Exposure
	}{
		{"clean", Request{}, Exposure{Recorded: true}},
		{"history", Request{History: hist}, unknown},
		{"summary", Request{HistorySummary: "s"}, unknown},
		{"unrecorded prior", Request{PriorExposure: &Exposure{}}, unknown},
		{"recorded prior", Request{PriorExposure: &Exposure{Recorded: true, Foreign: true, First: "tool:x"}, History: hist},
			Exposure{Recorded: true, Foreign: true, First: "tool:x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Goal = "g"
			res, err := newTestOrchestrator(exposureAnswer()).Run(context.Background(), tc.req, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Exposure != tc.want {
				t.Fatalf("Exposure = %+v, want %+v", res.Exposure, tc.want)
			}
		})
	}
}

func TestExposureReportedOnEveryReturnPath(t *testing.T) {
	o := newTestOrchestrator(exposureAnswer(), WithInterceptors(&stubInterceptor{name: "blk", input: func(in InputInspection) []Finding {
		for _, m := range in.Messages {
			if strings.Contains(m.Content, "BLOCKME") {
				return []Finding{{Rule: "r", Verdict: VerdictBlock, Risk: 1, Target: TargetMessage, StateIndex: m.StateIndex}}
			}
		}
		return nil
	}}))
	for _, goal := range []string{"", "BLOCKME"} {
		res, err := o.Run(context.Background(), Request{Goal: goal}, nil)
		if err == nil {
			t.Fatalf("goal %q: want error", goal)
		}
		if !res.Exposure.Recorded {
			t.Errorf("goal %q: Exposure not recorded: %+v", goal, res.Exposure)
		}
	}
}

// errAfterTool answers one tool call, then fails the next model call.
type errAfterTool struct{ calls int }

func (e *errAfterTool) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (ModelResult, error) {
	e.calls++
	if e.calls == 1 {
		return ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{tc("stub", `{}`)}}}, nil
	}
	return ModelResult{}, errors.New("model down")
}

func TestExposureKeptOnModelError(t *testing.T) {
	res, err := runAdmit(t, &errAfterTool{}, Request{Tools: []Tool{foreignStub()}}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:stub"}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestExposureFirstIsBounded(t *testing.T) {
	_, r := newExposureRun(context.Background(), Request{})
	r.admit(OriginForeign, strings.Repeat("é", 5000))
	r.admit(OriginUnknown, "later")
	got := r.snapshot()
	if !got.Foreign || !got.Unknown {
		t.Fatalf("snapshot = %+v", got)
	}
	if len(got.First) > conversation.MaxExposureFirstBytes || !utf8.ValidString(got.First) || !strings.HasPrefix(got.First, "tool:é") {
		t.Fatalf("First = %q (%d bytes)", got.First, len(got.First))
	}
}

func TestExposureNestedRunsShareAndNeverClear(t *testing.T) {
	ctx, parent := newExposureRun(context.Background(), Request{HistorySummary: "s"})
	parent.admitForeign("consult")
	_, _ = newExposureRun(ctx, Request{PriorExposure: &Exposure{Recorded: true}})
	want := Exposure{Recorded: true, Foreign: true, Unknown: true, First: "history"}
	if got := parent.snapshot(); got != want {
		t.Fatalf("parent = %+v, want %+v", got, want)
	}
	_, r := newExposureRun(context.Background(), Request{PriorExposure: &Exposure{Recorded: true, First: "junk"}})
	if got := r.snapshot(); got != (Exposure{Recorded: true}) {
		t.Fatalf("clean prior = %+v", got)
	}
	var nilRun *exposureRun
	nilRun.admit(OriginForeign, "x")
	nilRun.admitForeign("x")
	if exposureFrom(context.Background()) != nil {
		t.Fatal("no tracker expected")
	}
}

func TestExposurePriorFirstIsBounded(t *testing.T) {
	long := strings.Repeat("é", 5000)
	_, r := newExposureRun(context.Background(), Request{PriorExposure: &Exposure{Recorded: true, Foreign: true, First: long}})
	got := r.snapshot().First
	if len(got) > conversation.MaxExposureFirstBytes || !utf8.ValidString(got) || got == "" {
		t.Fatalf("First = %d bytes", len(got))
	}
}

func TestResultExposureNotSerialized(t *testing.T) {
	b, err := json.Marshal(Result{Exposure: Exposure{Recorded: true, Foreign: true}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := json.Marshal(Result{})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(empty) {
		t.Fatalf("serialized %s, want %s", b, empty)
	}
}
