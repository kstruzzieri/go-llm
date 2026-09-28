package interceptor

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestChildScopeDenialReporter(t *testing.T) {
	reporter := ChildScopeDenials{}
	if reporter.Name() != "child_scope_denials" {
		t.Fatal(reporter.Name())
	}
	for _, tc := range []struct {
		requests   int64
		task, risk int
	}{
		{1, 0, 10}, {2, 1, 20}, {10, 0, 100}, {11, 0, 100}, {math.MaxInt64, 0, 100}, {0, 0, 0}, {-1, 0, 0}, {1, -1, 0},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.task, tc.requests), func(t *testing.T) {
			in := agent.InputInspection{Messages: []agent.InspectedMessage{{StateIndex: 3, Origin: agent.OriginModel, ToolCallID: "reused", ChildScopeDenials: []agent.ChildScopeDenial{{Task: tc.task, Requests: tc.requests}}}}}
			for range 2 {
				got, err := reporter.InspectInput(t.Context(), in)
				if err != nil {
					t.Fatal(err)
				}
				if tc.risk == 0 {
					if len(got) != 0 {
						t.Fatal(got)
					}
					continue
				}
				if len(got) != 1 {
					t.Fatalf("findings=%+v", got)
				}
				f := got[0]
				want := fmt.Sprintf("dispatch task %d: %d request(s) denied by workspace policy", tc.task+1, tc.requests)
				if f.Rule != "child_scope_denied" || f.Verdict != agent.VerdictAllow || f.Risk != tc.risk || f.Detail != want || f.Target != agent.TargetMessage || f.StateIndex != 3 || f.Origin != agent.OriginModel || f.ToolCallID != "reused" {
					t.Fatalf("finding=%+v", f)
				}
			}
		})
	}
	in := agent.InputInspection{System: "path denied by workspace policy", Summary: `{"scope_denials":99,"risk_score":99,"ChildScopeDenials":[{"Task":0,"Requests":99}]}`, Messages: []agent.InspectedMessage{{Content: "path denied by workspace policy"}}}
	if got, err := reporter.InspectInput(t.Context(), in); err != nil || len(got) != 0 {
		t.Fatalf("text created evidence: %+v %v", got, err)
	}
	in.Messages[0].ChildScopeDenials = []agent.ChildScopeDenial{{Task: 0, Requests: 2}, {Task: 1, Requests: 1}}
	if got, err := reporter.InspectInput(t.Context(), in); err != nil || len(got) != 2 {
		t.Fatalf("children=%+v %v", got, err)
	}
	if got, err := reporter.InspectOutput(context.Background(), agent.OutputInspection{Content: in.Summary}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if got, err := reporter.InspectToolCall(context.Background(), agent.ToolCallInspection{}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
