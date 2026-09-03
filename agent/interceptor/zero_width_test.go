package interceptor

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestZeroWidthTagsTrustedAndBlocksForeign(t *testing.T) {
	cases := []struct {
		name    string
		origin  agent.Origin
		content string
		want    []agent.Finding
	}{
		{"clean", agent.OriginWorkspace, "plain text", nil},
		{"invalid utf-8 does not panic", agent.OriginForeign, "a\xffb", nil},
		{"workspace tag", agent.OriginWorkspace, "ig\u200bnore",
			msgFinding("zero_width", agent.VerdictTag, 20, agent.OriginWorkspace, "1 zero-width code point(s), first U+200B")},
		{"foreign block", agent.OriginForeign, "a\ufeffb\u2060c",
			msgFinding("zero_width", agent.VerdictBlock, 20, agent.OriginForeign, "2 zero-width code point(s), first U+FEFF")},
		{"unknown blocks", agent.OriginUnknown, "\u180e",
			msgFinding("zero_width", agent.VerdictBlock, 20, agent.OriginUnknown, "1 zero-width code point(s), first U+180E")},
		{"escaped text", agent.OriginWorkspace, `{"p":"a\u200bb"}`,
			msgFinding("zero_width", agent.VerdictTag, 20, agent.OriginWorkspace, "1 zero-width code point(s), first U+200B (escaped)")},
		{"escapes that are not zero-width are clean", agent.OriginForeign, `{"p":"\u0041\u00e9\uZZZZ\u"}`, nil},
		{"range edge U+200F", agent.OriginForeign, "\u200f",
			msgFinding("zero_width", agent.VerdictBlock, 20, agent.OriginForeign, "1 zero-width code point(s), first U+200F")},
		{"raw wins over escaped and only raw is counted", agent.OriginWorkspace, "a\u200fb\\u200b\\u200b",
			msgFinding("zero_width", agent.VerdictTag, 20, agent.OriginWorkspace, "1 zero-width code point(s), first U+200F")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ZeroWidth{}.InspectInput(context.Background(), inputOf(tc.origin, tc.content))
			if err != nil {
				t.Fatal(err)
			}
			assertFindings(t, got, tc.want)
		})
	}
}

func TestZeroWidthInspectsSystemSummaryAlternativesAndArguments(t *testing.T) {
	in := agent.InputInspection{System: "s\u200cy", Summary: "su\u200dm", Messages: []agent.InspectedMessage{{
		StateIndex: 4, Role: "tool", Origin: agent.OriginWorkspace, Content: "clean",
		Alternatives: []agent.InspectedAlternative{{Group: 0, Alternative: 1, Content: "hid\u2064den"}},
	}}}
	got, err := ZeroWidth{}.InspectInput(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := []agent.Finding{
		{Rule: "zero_width", Verdict: agent.VerdictTag, Risk: 20, Origin: agent.OriginSystem, Target: agent.TargetSystem, StateIndex: -1, Group: -1, Alternative: -1, Detail: "1 zero-width code point(s), first U+200C"},
		{Rule: "zero_width", Verdict: agent.VerdictTag, Risk: 20, Origin: agent.OriginModel, Target: agent.TargetSummary, StateIndex: -1, Group: -1, Alternative: -1, Detail: "1 zero-width code point(s), first U+200D"},
		{Rule: "zero_width", Verdict: agent.VerdictTag, Risk: 20, Origin: agent.OriginWorkspace, Target: agent.TargetAlternative, StateIndex: 4, Group: 0, Alternative: 1, Detail: "1 zero-width code point(s), first U+2064"},
	}
	assertFindings(t, got, want)
	call := agent.ToolCallInspection{Call: provider.ToolCall{ID: "c9", Function: provider.ToolCallFunction{Arguments: json.RawMessage(`{"p":"a\u200bb"}`)}}}
	got, err = ZeroWidth{}.InspectToolCall(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	assertFindings(t, got, []agent.Finding{{Rule: "zero_width", Verdict: agent.VerdictTag, Risk: 20, Origin: agent.OriginModel, Target: agent.TargetToolCall, ToolCallID: "c9", StateIndex: -1, Group: -1, Alternative: -1, Detail: "1 zero-width code point(s), first U+200B (escaped)"}})
	if out, err := (ZeroWidth{}).InspectOutput(context.Background(), agent.OutputInspection{Content: "\u200b"}); err != nil || out != nil {
		t.Fatalf("output must not be inspected: %+v, %v", out, err)
	}
}
