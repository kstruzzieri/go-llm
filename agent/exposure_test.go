package agent

import (
	"context"
	"encoding/json"
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
	ctx, parent := newExposureRun(context.Background(), Request{})
	_, child := newExposureRun(ctx, Request{PriorExposure: &Exposure{}})
	if !parent.snapshot().Unknown {
		t.Fatal("child prior must reach parent")
	}
	child.admitForeign("consult")
	if s := parent.snapshot(); !s.Foreign || s.First != "history" {
		t.Fatalf("parent = %+v", s)
	}
	var nilRun *exposureRun
	nilRun.admit(OriginForeign, "x")
	nilRun.admitForeign("x")
	if exposureFrom(context.Background()) != nil {
		t.Fatal("no tracker expected")
	}
}

func TestResultExposureNotSerialized(t *testing.T) {
	b, err := json.Marshal(Result{Exposure: Exposure{Recorded: true, Foreign: true}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Exposure") {
		t.Fatalf("serialized: %s", b)
	}
}
