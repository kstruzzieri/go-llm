package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

const (
	canaryNonce      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	canaryUpperNonce = "0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF"
	canaryMixedNonce = "0123456789AbCdEf0123456789aBcDeF0123456789ABcdef0123456789abCDef"
)

var (
	wantCanaryContent = agent.Finding{
		Rule: "canary_in_content", Verdict: agent.VerdictAbort, Risk: 100, Detail: "canary detected in content",
		Target: agent.TargetOutputContent, StateIndex: -1, Group: -1, Alternative: -1,
	}
	wantCanaryThinking = agent.Finding{
		Rule: "canary_in_thinking", Verdict: agent.VerdictAbort, Risk: 100, Detail: "canary detected in thinking",
		Target: agent.TargetOutputContent, StateIndex: -1, Group: -1, Alternative: -1,
	}
	wantCanaryArguments = agent.Finding{
		Rule: "canary_in_tool_arguments", Verdict: agent.VerdictAbort, Risk: 100, Detail: "canary detected in tool arguments",
		Target: agent.TargetNone, StateIndex: -1, Group: -1, Alternative: -1,
	}
	wantCanaryMetadata = agent.Finding{
		Rule: "canary_in_tool_metadata", Verdict: agent.VerdictAbort, Risk: 100, Detail: "canary detected in tool metadata",
		Target: agent.TargetNone, StateIndex: -1, Group: -1, Alternative: -1,
	}
	wantCanaryInput = agent.Finding{
		Rule: "canary_in_input", Verdict: agent.VerdictAbort, Risk: 100, Detail: "canary detected in input",
		Target: agent.TargetNone, StateIndex: -1, Group: -1, Alternative: -1,
	}
)

func newTestCanary(t *testing.T) Canary {
	t.Helper()
	c, err := NewCanary(canaryNonce)
	if err != nil {
		t.Fatalf("NewCanary(valid nonce) error = %v, want nil", err)
	}
	return c
}

func assertCanaryFindings(t *testing.T, got, want []agent.Finding) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Error("Canary findings differ from the literal expected fields")
	}
}

func TestCanaryConstructor(t *testing.T) {
	t.Parallel()
	const wantErr = "interceptor: canary nonce must be 64 lowercase hexadecimal characters"
	for _, tc := range []struct {
		name  string
		nonce string
	}{
		{name: "empty", nonce: ""},
		{name: "short", nonce: "0123456789abcdef"},
		{name: "nonhex", nonce: "g123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{name: "uppercase", nonce: canaryUpperNonce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewCanary(tc.nonce)
			if err == nil || err.Error() != wantErr {
				t.Errorf("NewCanary(%s nonce) error matched static validation message = %v, want true", tc.name, err != nil && err.Error() == wantErr)
			}
		})
	}

	c := newTestCanary(t)
	if c.Name() != "canary" {
		t.Errorf("Canary.Name() = %q, want %q", c.Name(), "canary")
	}
}

func TestCanaryZeroValueFailsClosed(t *testing.T) {
	t.Parallel()
	const wantErr = "interceptor: canary nonce must be 64 lowercase hexadecimal characters"
	checks := []struct {
		name string
		run  func() ([]agent.Finding, error)
	}{
		{name: "input", run: func() ([]agent.Finding, error) { return (Canary{}).InspectInput(t.Context(), agent.InputInspection{}) }},
		{name: "output", run: func() ([]agent.Finding, error) {
			return (Canary{}).InspectOutput(t.Context(), agent.OutputInspection{})
		}},
		{name: "tool call", run: func() ([]agent.Finding, error) {
			return (Canary{}).InspectToolCall(t.Context(), agent.ToolCallInspection{})
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			findings, err := check.run()
			if findings != nil || err == nil || err.Error() != wantErr {
				t.Errorf("zero Canary %s findings/error = nonnil/%v, want nil/static error", check.name, err != nil)
			}
		})
	}
}

func TestCanaryInspectOutput(t *testing.T) {
	t.Parallel()
	escapedValue := `{"value":"0123456789\u0061bcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	escapedKey := `{"0123456789\u0061bcdef0123456789abcdef0123456789abcdef0123456789abcdef":"safe"}`
	duplicateKeys := `{"0123456789\u0061bcdef0123456789abcdef0123456789abcdef0123456789abcdef":"first","0123456789\u0061bcdef0123456789abcdef0123456789abcdef0123456789abcdef":"second"}`
	cases := []struct {
		name string
		out  agent.OutputInspection
		want []agent.Finding
	}{
		{name: "clean", out: agent.OutputInspection{Content: "safe", Thinking: "also safe"}},
		{name: "lowercase content", out: agent.OutputInspection{Content: canaryNonce}, want: []agent.Finding{wantCanaryContent}},
		{name: "uppercase content with prefix and punctuation", out: agent.OutputInspection{Content: "[0x" + canaryUpperNonce + "]"}, want: []agent.Finding{wantCanaryContent}},
		{name: "mixed-case content", out: agent.OutputInspection{Content: canaryMixedNonce}, want: []agent.Finding{wantCanaryContent}},
		{name: "lowercase thinking", out: agent.OutputInspection{Thinking: canaryNonce}, want: []agent.Finding{wantCanaryThinking}},
		{name: "uppercase thinking", out: agent.OutputInspection{Thinking: canaryUpperNonce}, want: []agent.Finding{wantCanaryThinking}},
		{name: "mixed-case thinking", out: agent.OutputInspection{Thinking: canaryMixedNonce}, want: []agent.Finding{wantCanaryThinking}},
		{name: "content and thinking", out: agent.OutputInspection{Content: canaryNonce, Thinking: canaryUpperNonce}, want: []agent.Finding{wantCanaryContent, wantCanaryThinking}},
		{name: "raw arguments", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "known", `{"value":"(`+canaryNonce+`)"}`)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "decoded string arguments", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "known", escapedValue)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "decoded key arguments", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "known", escapedKey)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "duplicate decoded members", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "known", duplicateKeys)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "malformed arguments with literal", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "known", `{"value":"`+canaryNonce)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "unknown tool arguments", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", "missing-tool", `{"value":"`+canaryNonce+`"}`)}}, want: []agent.Finding{wantCanaryArguments}},
		{name: "call id metadata", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall(canaryNonce, "known", `{}`)}}, want: []agent.Finding{wantCanaryMetadata}},
		{name: "call type metadata", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{{ID: "safe-id", Type: canaryUpperNonce, Function: provider.ToolCallFunction{Name: "known", Arguments: json.RawMessage(`{}`)}}}}, want: []agent.Finding{wantCanaryMetadata}},
		{name: "function name metadata", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("safe-id", canaryMixedNonce, `{}`)}}, want: []agent.Finding{wantCanaryMetadata}},
		{name: "arguments and metadata deduplicate by category", out: agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall(canaryNonce, canaryNonce, `{"a":"`+canaryNonce+`"}`)}}, want: []agent.Finding{wantCanaryArguments, wantCanaryMetadata}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestCanary(t).InspectOutput(t.Context(), tc.out)
			if err != nil {
				t.Fatalf("Canary.InspectOutput(%s) error = %v, want nil", tc.name, err)
			}
			assertCanaryFindings(t, got, tc.want)
		})
	}
}

func TestCanaryOutputFindingsDoNotCopyToolMetadata(t *testing.T) {
	t.Parallel()
	call := provider.ToolCall{
		ID: canaryNonce, Type: "type-" + canaryNonce,
		Function: provider.ToolCallFunction{Name: "name-" + canaryNonce, Arguments: json.RawMessage(`{"` + canaryNonce + `":"` + canaryNonce + `"}`)},
	}
	got, err := newTestCanary(t).InspectOutput(t.Context(), agent.OutputInspection{ToolCalls: []provider.ToolCall{call}})
	if err != nil {
		t.Fatalf("Canary.InspectOutput(metadata) error = %v, want nil", err)
	}
	want := []agent.Finding{
		wantCanaryArguments,
		wantCanaryMetadata,
	}
	assertCanaryFindings(t, got, want)
	for _, finding := range got {
		for _, field := range []string{finding.Interceptor, finding.Rule, finding.Detail, finding.ToolCallID} {
			if strings.Contains(field, canaryNonce) {
				t.Error("Canary output finding copied model-controlled tool metadata")
			}
		}
	}
}

func TestCanaryInspectToolCall(t *testing.T) {
	t.Parallel()
	escaped := `{"value":"0123456789\u0061bcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`
	want := []agent.Finding{wantCanaryArguments}
	for _, tc := range []struct {
		name string
		raw  string
		want []agent.Finding
	}{
		{name: "clean", raw: `{"value":"safe"}`},
		{name: "raw", raw: `{"value":"` + canaryNonce + `"}`, want: want},
		{name: "decoded", raw: escaped, want: want},
		{name: "malformed literal", raw: `{"value":"` + canaryNonce, want: want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestCanary(t).InspectToolCall(t.Context(), agent.ToolCallInspection{Call: toolCall("unsafe-id-"+canaryNonce, "unsafe-name-"+canaryNonce, tc.raw)})
			if err != nil {
				t.Fatalf("Canary.InspectToolCall(%s) error = %v, want nil", tc.name, err)
			}
			assertCanaryFindings(t, got, tc.want)
			for _, finding := range got {
				if finding.ToolCallID != "" || strings.Contains(finding.Detail, canaryNonce) {
					t.Error("Canary tool-call finding copied model-controlled metadata")
				}
			}
		})
	}
}

func TestCanaryInspectInput(t *testing.T) {
	t.Parallel()
	want := []agent.Finding{wantCanaryInput}
	cases := []struct {
		name string
		in   agent.InputInspection
		want []agent.Finding
	}{
		{name: "authorized system and clean siblings", in: agent.InputInspection{System: "internal " + canaryNonce, Summary: "safe", Messages: []agent.InspectedMessage{{StateIndex: 0, Content: "safe", Alternatives: []agent.InspectedAlternative{{Content: "safe"}}}}}},
		{name: "lowercase summary", in: agent.InputInspection{Summary: canaryNonce}, want: want},
		{name: "uppercase message", in: agent.InputInspection{Messages: []agent.InspectedMessage{{StateIndex: 4, ToolCallID: "unsafe-" + canaryNonce, Content: canaryUpperNonce}}}, want: want},
		{name: "mixed-case alternative", in: agent.InputInspection{Messages: []agent.InspectedMessage{{StateIndex: 5, Alternatives: []agent.InspectedAlternative{{Group: 2, Alternative: 3, Content: canaryMixedNonce}}}}}, want: want},
		{name: "clean siblings and another session nonce", in: agent.InputInspection{Summary: "safe", Messages: []agent.InspectedMessage{{StateIndex: 0, Content: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", Alternatives: []agent.InspectedAlternative{{Content: "safe"}}}}}},
		{name: "incomplete", in: agent.InputInspection{Summary: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde"}},
		{name: "split", in: agent.InputInspection{Messages: []agent.InspectedMessage{{Content: "0123456789abcdef0123456789abcdef 0123456789abcdef0123456789abcdef"}}}},
		{name: "transformed", in: agent.InputInspection{Messages: []agent.InspectedMessage{{Alternatives: []agent.InspectedAlternative{{Content: "0123456789%61bcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestCanary(t).InspectInput(t.Context(), tc.in)
			if err != nil {
				t.Fatalf("Canary.InspectInput(%s) error = %v, want nil", tc.name, err)
			}
			assertCanaryFindings(t, got, tc.want)
			for _, finding := range got {
				if finding.ToolCallID != "" || strings.Contains(finding.Detail, canaryNonce) {
					t.Error("Canary input finding copied inspected metadata")
				}
			}
		})
	}
}

type canaryCaller struct {
	response provider.ChatResponse
	err      error
	calls    int
}

func (c *canaryCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls++
	if c.calls == 1 {
		return agent.ModelResult{Response: c.response}, c.err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

type canaryObserver struct {
	steps, calls, results int
}

func (o *canaryObserver) OnStep(context.Context, agent.StepEvent) error {
	o.steps++
	return nil
}
func (o *canaryObserver) OnToolCall(context.Context, agent.ToolCallEvent) error {
	o.calls++
	return nil
}
func (*canaryObserver) OnToken(context.Context, agent.TokenEvent) error { return nil }
func (o *canaryObserver) OnToolResult(context.Context, agent.ToolResultEvent) error {
	o.results++
	return nil
}

type canaryReadTool struct {
	name    string
	result  string
	invokes atomic.Int32
}

func (t *canaryReadTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (*canaryReadTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}
func (t *canaryReadTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	t.invokes.Add(1)
	return agent.ToolResult{Content: t.result, Origin: agent.OriginWorkspace}, nil
}

func assertCanaryLoopAbort(t *testing.T, caller *canaryCaller, observer *canaryObserver, res agent.Result, err error, wantFinding agent.Finding, wantError string, wantSteps int) {
	t.Helper()
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatal("Orchestrator.Run() error does not contain *agent.BlockedError")
	}
	wantFinding.Interceptor = "canary"
	wantFinding.Hook = agent.HookOutput
	wantFinding.Origin = agent.OriginModel
	assertCanaryFindings(t, blocked.Findings, []agent.Finding{wantFinding})
	if blocked.Error() != wantError {
		t.Error("BlockedError.Error() differs from the fixed canary output error")
	}
	if blocked.Hook != agent.HookOutput || blocked.Step != 0 {
		t.Errorf("BlockedError location = (%s, %d), want (output, 0)", blocked.Hook, blocked.Step)
	}
	if caller.calls != 1 || observer.steps != 0 || observer.calls != 0 || observer.results != 0 {
		t.Errorf("model/step/tool-call/tool-result calls = %d/%d/%d/%d, want 1/0/0/0", caller.calls, observer.steps, observer.calls, observer.results)
	}
	if res.Answer != "" || len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Messages[0].Content != "safe goal" {
		t.Error("Orchestrator.Run() retained tainted output or lost the safe user input")
	}
	if len(res.Steps) != wantSteps {
		t.Errorf("Orchestrator.Run() retained steps = %d, want %d", len(res.Steps), wantSteps)
	}
	for _, step := range res.Steps {
		if step.Response.Content != "" || step.Response.Thinking != "" || step.Response.ToolCalls != nil {
			t.Error("Orchestrator.Run() retained tainted response fields")
		}
	}
	if res.Risk == nil || res.Risk.Score != 100 {
		t.Error("Orchestrator.Run() did not retain the canary risk score")
	} else {
		assertCanaryFindings(t, res.Risk.Findings, []agent.Finding{wantFinding})
	}
}

func TestCanaryLoopAbortsTaintedFinalAndPartialOutput(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider failed")
	for _, tc := range []struct {
		name      string
		response  provider.ChatResponse
		err       error
		want      agent.Finding
		wantError string
		wantSteps int
	}{
		{name: "final content", response: provider.ChatResponse{Content: "unsafe " + canaryNonce, Done: true}, want: wantCanaryContent, wantError: "agent: output blocked by interceptor canary (canary_in_content)", wantSteps: 1},
		{name: "final thinking", response: provider.ChatResponse{Thinking: canaryUpperNonce, Done: true}, want: wantCanaryThinking, wantError: "agent: output blocked by interceptor canary (canary_in_thinking)", wantSteps: 1},
		{name: "partial content with provider error", response: provider.ChatResponse{Content: canaryMixedNonce, Partial: true}, err: providerErr, want: wantCanaryContent, wantError: "agent: output blocked by interceptor canary (canary_in_content)"},
		{name: "partial thinking with provider error", response: provider.ChatResponse{Thinking: canaryNonce, Partial: true}, err: providerErr, want: wantCanaryThinking, wantError: "agent: output blocked by interceptor canary (canary_in_thinking)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := &canaryCaller{response: tc.response, err: tc.err}
			observer := &canaryObserver{}
			o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(newTestCanary(t)))
			res, err := o.Run(t.Context(), agent.Request{Goal: "safe goal"}, observer)
			assertCanaryLoopAbort(t, caller, observer, res, err, tc.want, tc.wantError, tc.wantSteps)
			if tc.err != nil && !errors.Is(err, providerErr) {
				t.Error("Orchestrator.Run() lost the joined provider error")
			}
		})
	}
}

func TestCanaryLoopAbortsTaintedToolBatchesBeforeDispatch(t *testing.T) {
	t.Parallel()
	t.Run("serial", func(t *testing.T) {
		t.Parallel()
		clean := &guardedPlanTool{name: "write_a", class: agent.Write}
		tainted := &guardedPlanTool{name: "write_b", class: agent.Write}
		approver := &countingApprover{}
		caller := &canaryCaller{response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
			toolCall("safe-a", clean.name, `{"value":"safe"}`),
			toolCall("safe-b", tainted.name, `{"value":"`+canaryNonce+`"}`),
		}}}
		observer := &canaryObserver{}
		o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(newTestCanary(t)))
		res, err := o.Run(t.Context(), agent.Request{Goal: "safe goal", Tools: []agent.Tool{clean, tainted}, Approver: approver}, observer)
		assertCanaryLoopAbort(t, caller, observer, res, err, wantCanaryArguments, "agent: output blocked by interceptor canary (canary_in_tool_arguments)", 1)
		if clean.plans.Load() != 0 || tainted.plans.Load() != 0 || clean.invokes.Load() != 0 || tainted.invokes.Load() != 0 || approver.calls.Load() != 0 {
			t.Error("Canary serial output abort reached Plan, approval, or Invoke")
		}
	})
	t.Run("parallel", func(t *testing.T) {
		t.Parallel()
		clean := &canaryReadTool{name: "read_a", result: "safe"}
		tainted := &canaryReadTool{name: "read_b", result: "safe"}
		approver := &countingApprover{}
		caller := &canaryCaller{response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
			toolCall("safe-a", clean.name, `{"value":"safe"}`),
			toolCall("safe-b", tainted.name, `{"value":"`+canaryUpperNonce+`"}`),
		}}}
		observer := &canaryObserver{}
		o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(newTestCanary(t)))
		res, err := o.Run(t.Context(), agent.Request{Goal: "safe goal", Tools: []agent.Tool{clean, tainted}, Approver: approver}, observer)
		assertCanaryLoopAbort(t, caller, observer, res, err, wantCanaryArguments, "agent: output blocked by interceptor canary (canary_in_tool_arguments)", 1)
		if clean.invokes.Load() != 0 || tainted.invokes.Load() != 0 || approver.calls.Load() != 0 {
			t.Error("Canary parallel output abort reached approval or Invoke")
		}
	})
}

func TestCanaryLoopAbortsAtToolResultIngress(t *testing.T) {
	t.Parallel()
	tool := &canaryReadTool{name: "read", result: canaryNonce}
	caller := &canaryCaller{response: provider.ChatResponse{ToolCalls: []provider.ToolCall{toolCall("safe-id", tool.name, `{}`)}}}
	observer := &canaryObserver{}
	o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(newTestCanary(t)))
	res, err := o.Run(t.Context(), agent.Request{Goal: "safe goal", Tools: []agent.Tool{tool}}, observer)
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatal("Orchestrator.Run(tool ingress) error does not contain *agent.BlockedError")
	}
	wantFinding := wantCanaryInput
	wantFinding.Interceptor = "canary"
	wantFinding.Hook = agent.HookInput
	assertCanaryFindings(t, blocked.Findings, []agent.Finding{wantFinding})
	if blocked.Error() != "agent: input blocked by interceptor canary (canary_in_input)" {
		t.Error("BlockedError.Error() differs from the fixed canary input error")
	}
	if blocked.Hook != agent.HookInput || blocked.Step != 0 || caller.calls != 1 || tool.invokes.Load() != 1 {
		t.Errorf("tool-ingress abort hook/step/model/invokes = %s/%d/%d/%d, want input/0/1/1", blocked.Hook, blocked.Step, caller.calls, tool.invokes.Load())
	}
	if observer.steps != 1 || observer.calls != 1 || observer.results != 0 {
		t.Errorf("step/tool-call/tool-result events = %d/%d/%d, want 1/1/0", observer.steps, observer.calls, observer.results)
	}
	if res.Answer != "" || len(res.Messages) != 2 || res.Messages[0].Content != "safe goal" || len(res.Messages[1].ToolCalls) != 1 {
		t.Error("Orchestrator.Run(tool ingress) did not retain exactly the safe pre-ingress transcript")
	}
}
