package interceptor

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/provider"
)

func syntheticSecret() string { return "sk-" + "aB3_dE7-fG9_" + "hJ2-kL4" }

func assertSecretFindings(t *testing.T, got, want []agent.Finding) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Secrets findings count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			// Never render a finding: a regression in Detail must not print values.
			t.Errorf("Secrets finding %d differs from expected kind/location metadata", i)
		}
	}
}

func TestSecretsInputBlocksAllKindsAndOrigins(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, text string }{
		{"openai_token", syntheticSecret()},
		{"github_token", "ghp_" + "aB3dE7fG9hJ2" + "kL4mN6"},
		{"gitlab_token", "glpat-" + "aB3_dE7-fG9_" + "hJ2-kL4"},
		{"slack_token", "xoxb-" + "aB3dE7-fG9" + "hJ2"},
		{"npm_token", "npm_" + "aB3dE7fG9hJ2" + "kL4mN6"},
		{"bearer_token", "Bearer " + "aB3dE7fG9hJ2" + "kL4mN6pQ8"},
		{"secret_assignment", "token=" + "aB3dE7fG9hJ2" + "kL4mN6pQ8"},
		{"private_key", "-----BEGIN " + "PRIVATE KEY-----\nplaceholder\n-----END " + "PRIVATE KEY-----"},
		{"payment_card", "45320198" + "7654321" + "5"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, origin := range []agent.Origin{agent.OriginUnknown, agent.OriginUser, agent.OriginSystem, agent.OriginModel, agent.OriginWorkspace, agent.OriginForeign, agent.Origin(99)} {
				got, err := (Secrets{}).InspectInput(t.Context(), inputOf(origin, tc.text))
				if err != nil {
					t.Fatal("Secrets input returned an error")
				}
				assertSecretFindings(t, got, msgFinding("sensitive_"+tc.kind, agent.VerdictBlock, 100, origin, "detected "+tc.kind))
				for _, finding := range got {
					if strings.Contains(finding.Detail, tc.text) {
						t.Error("Secrets finding disclosed matched content")
					}
				}
			}
		})
	}
}

func TestSecretsInputPreservesEveryTarget(t *testing.T) {
	t.Parallel()
	value := syntheticSecret()
	in := agent.InputInspection{System: value, Summary: value, Messages: []agent.InspectedMessage{{
		StateIndex: 4, Role: "tool", Origin: agent.OriginWorkspace, Content: value + " " + value,
		Alternatives: []agent.InspectedAlternative{{Group: 1, Alternative: 2, Content: value}},
	}, {StateIndex: 7, Role: "user", Origin: agent.OriginUser, Content: value}}}
	got, err := (Secrets{}).InspectInput(context.Background(), in)
	if err != nil {
		t.Fatal("Secrets input returned an error")
	}
	assertSecretFindings(t, got, []agent.Finding{
		{Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected openai_token", Target: agent.TargetSystem, Origin: agent.OriginSystem, StateIndex: -1, Group: -1, Alternative: -1},
		{Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected openai_token", Target: agent.TargetSummary, Origin: agent.OriginModel, StateIndex: -1, Group: -1, Alternative: -1},
		{Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected openai_token", Target: agent.TargetMessage, Origin: agent.OriginWorkspace, StateIndex: 4, Group: -1, Alternative: -1},
		{Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected openai_token", Target: agent.TargetAlternative, Origin: agent.OriginWorkspace, StateIndex: 4, Group: 1, Alternative: 2},
		{Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected openai_token", Target: agent.TargetMessage, Origin: agent.OriginUser, StateIndex: 7, Group: -1, Alternative: -1},
	})
	if (Secrets{}).Name() != "secrets" {
		t.Error("Secrets name does not identify the policy")
	}
	clean, err := (Secrets{}).InspectInput(t.Context(), inputOf(agent.OriginForeign, "ordinary content"))
	if err != nil || clean != nil {
		t.Error("Secrets clean input must return nil findings and no error")
	}
}

func outputSecretFindings(target agent.TargetKind, id string, kinds ...string) []agent.Finding {
	var findings []agent.Finding
	for _, kind := range kinds {
		findings = append(findings, agent.Finding{
			Rule: "sensitive_" + kind, Verdict: agent.VerdictBlock, Risk: 100, Detail: "detected " + kind,
			Target: target, Origin: agent.OriginModel, ToolCallID: id, StateIndex: -1, Group: -1, Alternative: -1,
		})
	}
	return findings
}

func TestSecretsOutputScansContentAndThinkingSeparately(t *testing.T) {
	t.Parallel()
	value := syntheticSecret()
	for _, tc := range []struct {
		name, content, thinking string
		kinds                   []string
	}{
		{"clean", "ordinary content", "ordinary thinking", nil},
		{"content", value, "", []string{"openai_token"}},
		{"thinking", "", value, []string{"openai_token"}},
		{"shared target dedup", value + " " + value, value, []string{"openai_token"}},
		{"distinct kinds", value, "ghp_" + "aB3dE7fG9hJ2" + "kL4mN6", []string{"openai_token", "github_token"}},
		{"no cross-string match", value[:5], value[5:], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (Secrets{}).InspectOutput(t.Context(), agent.OutputInspection{Content: tc.content, Thinking: tc.thinking})
			if err != nil {
				t.Fatal("Secrets output returned an error")
			}
			assertSecretFindings(t, got, outputSecretFindings(agent.TargetOutputContent, "", tc.kinds...))
		})
	}
}

// secretArgumentCases is shared by both hooks so their decoded coverage cannot
// drift. All candidates are inert values assembled from fragments.
func secretArgumentCases() []struct {
	name, raw string
	kinds     []string
} {
	value := syntheticSecret()
	escaped := strings.ReplaceAll(value, "sk-", `\u0073k-`)
	generic := "aB3dE7fG9hJ2" + "kL4mN6pQ8"
	github := "ghp_" + "aB3dE7fG9hJ2" + "kL4mN6"
	return []struct {
		name, raw string
		kinds     []string
	}{
		{"clean", `{"p":{"q":["ordinary",null,12,true]}}`, nil},
		{"raw value", `{"p":"` + value + `"}`, []string{"openai_token"}},
		{"decoded value", `{"p":"` + escaped + `"}`, []string{"openai_token"}},
		{"decoded key", `{"` + escaped + `":"ordinary"}`, []string{"openai_token"}},
		{"top-level string", `"` + escaped + `"`, []string{"openai_token"}},
		{"nested array strings", `{"p":[{"q":["` + escaped + `"]}]}`, []string{"openai_token"}},
		{"invalid raw", "not json " + value, []string{"openai_token"}},
		{"invalid decoded only", `{"p":"` + escaped + `"`, nil},
		{"trailing invalid JSON", `{"p":"` + escaped + `"} {}`, nil},
		{"raw first and dedup", `{"p":"` + escaped + `","q":"` + github + `","r":"` + github + `"}`, []string{"github_token", "openai_token"}},
		{"raw and decoded dedup", `{"p":"` + value + `","q":"` + escaped + `"}`, []string{"openai_token"}},
		{"direct decoded assignment", `{"\u0074oken":"` + generic + `"}`, []string{"secret_assignment"}},
		{"normalized decoded key", `{"\u0041PI-Key":"` + generic + `"}`, []string{"secret_assignment"}},
		{"nested assignment", `{"p":[{"\u0074oken":"` + generic + `"}]}`, []string{"secret_assignment"}},
		{"duplicate member preserves first", `{"\u0074oken":"` + generic + `","\u0074oken":null}`, []string{"secret_assignment"}},
		{"duplicate member preserves last", `{"\u0074oken":null,"\u0074oken":"` + generic + `"}`, []string{"secret_assignment"}},
		{"siblings cannot supply value", `{"\u0074oken":"short","p":"` + generic + `"}`, nil},
		{"null consumes key", `{"\u0074oken":null,"p":"` + generic + `"}`, nil},
		{"number consumes key", `{"\u0074oken":1,"p":"` + generic + `"}`, nil},
		{"boolean consumes key", `{"\u0074oken":true,"p":"` + generic + `"}`, nil},
		{"array cannot inherit key", `{"\u0074oken":["` + generic + `"]}`, nil},
		{"nested object cannot inherit key", `{"\u0074oken":{"p":"` + generic + `"}}`, nil},
		{"container consumes key", `{"\u0074oken":{},"p":"` + generic + `"}`, nil},
		{"adjacent strings cannot join", `["` + escaped[:8] + `","` + escaped[8:] + `"]`, nil},
		{"decoded assignment supplements provider", `{"\u0074oken":"` + escaped + `"}`, []string{"openai_token", "secret_assignment"}},
		{"raw assignment dedup", `{"token":"` + generic + `"}`, []string{"secret_assignment"}},
		{"decoded placeholder", `{"\u0074oken":"${EXAMPLE_TOKEN_PLACEHOLDER}"}`, nil},
	}
}

func TestSecretsOutputToolArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range secretArgumentCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (Secrets{}).InspectOutput(t.Context(), agent.OutputInspection{ToolCalls: []provider.ToolCall{toolCall("original-id", "noop", tc.raw)}})
			if err != nil {
				t.Fatal("Secrets output arguments returned an error")
			}
			assertSecretFindings(t, got, outputSecretFindings(agent.TargetOutputToolCall, "original-id", tc.kinds...))
		})
	}
}

func TestSecretsOutputKeepsCallPositionsIndependent(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"reused-id", ""} {
		call := toolCall(id, "noop", `{"p":"`+syntheticSecret()+`"}`)
		got, err := (Secrets{}).InspectOutput(t.Context(), agent.OutputInspection{Content: syntheticSecret(), ToolCalls: []provider.ToolCall{call, call}})
		if err != nil {
			t.Fatal("Secrets output returned an error")
		}
		want := outputSecretFindings(agent.TargetOutputContent, "", "openai_token")
		want = append(want, outputSecretFindings(agent.TargetOutputToolCall, id, "openai_token")...)
		want = append(want, outputSecretFindings(agent.TargetOutputToolCall, id, "openai_token")...)
		assertSecretFindings(t, got, want)
	}
}

func TestSecretsToolCallTextsRetainsSourceOrder(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"a":[{"b":"c"},"d"],"a":"e","f":1e999}`)
	want := []string{string(raw), "a", "b", "c", "d", "a", "e", "f"}
	if !slices.Equal(toolCallTexts(raw), want) {
		t.Error("toolCallTexts lost raw-first order, a duplicate key, or a decoded string")
	}
	raw = json.RawMessage(`{"a":"b"} {}`)
	if !slices.Equal(toolCallTexts(raw), []string{string(raw)}) {
		t.Error("toolCallTexts projected invalid JSON")
	}
}

func TestSecretsToolCallArguments(t *testing.T) {
	t.Parallel()
	for _, tc := range secretArgumentCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (Secrets{}).InspectToolCall(t.Context(), agent.ToolCallInspection{Call: toolCall("original-id", "noop", tc.raw)})
			if err != nil {
				t.Fatal("Secrets tool-call arguments returned an error")
			}
			assertSecretFindings(t, got, outputSecretFindings(agent.TargetToolCall, "original-id", tc.kinds...))
		})
	}
}

type secretCaller struct {
	response provider.ChatResponse
	err      error
	requests []provider.ChatRequest
}

func (c *secretCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if len(c.requests) == 1 {
		return agent.ModelResult{Response: c.response}, c.err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

type secretObserver struct {
	steps, calls int
	results      []agent.ToolResultEvent
}

func (o *secretObserver) OnStep(context.Context, agent.StepEvent) error {
	o.steps++
	return nil
}
func (o *secretObserver) OnToolCall(context.Context, agent.ToolCallEvent) error {
	o.calls++
	return nil
}
func (*secretObserver) OnToken(context.Context, agent.TokenEvent) error { return nil }
func (o *secretObserver) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	o.results = append(o.results, e)
	return nil
}

func TestSecretsPipelineInitialBlock(t *testing.T) {
	t.Parallel()
	for _, req := range []agent.Request{
		{Goal: syntheticSecret()},
		{Goal: "q", System: syntheticSecret()},
		{Goal: "q", HistorySummary: syntheticSecret()},
		{Goal: "q", History: []provider.ChatMessage{{Role: "assistant", Content: syntheticSecret()}}},
	} {
		caller := &secretCaller{}
		observer := &secretObserver{}
		o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(Defaults()...))
		res, err := o.Run(t.Context(), req, observer)
		var blocked *agent.BlockedError
		if !errors.As(err, &blocked) || blocked.Hook != agent.HookInput {
			t.Error("Secrets initial input did not terminally block at ingress")
		}
		if len(caller.requests) != 0 || observer.steps != 0 || len(res.Steps) != 0 {
			t.Error("Secrets initial block reached model or response recording")
		}
	}
}

func TestSecretsPipelineDecodedArgumentsBlockBeforeRecordingAndDispatch(t *testing.T) {
	t.Parallel()
	raw := `{"p":"` + strings.ReplaceAll(syntheticSecret(), "sk-", `\u0073k-`) + `"}`
	if len(secretscan.Scan(raw)) != 0 || !json.Valid([]byte(raw)) {
		t.Fatal("regression fixture must be valid JSON detectable only after decoding")
	}
	tool := &guardedPlanTool{name: "write_file", class: agent.Write}
	approver := &countingApprover{}
	observer := &secretObserver{}
	caller := &batchCaller{calls: []provider.ToolCall{toolCall("original-id", tool.name, raw)}}
	o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(Defaults()...))
	res, err := o.Run(t.Context(), agent.Request{Goal: "q", Tools: []agent.Tool{tool}, Approver: approver}, observer)
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) || blocked.Hook != agent.HookOutput {
		t.Fatal("Secrets decoded arguments did not terminally block model output")
	}
	if observer.steps != 0 || observer.calls != 0 || tool.plans.Load() != 0 || tool.invokes.Load() != 0 || approver.calls.Load() != 0 || caller.n != 1 {
		t.Error("Secrets decoded argument block reached a publication or dispatch side effect")
	}
	if len(res.Steps) != 1 || res.Steps[0].Response.ToolCalls != nil || res.Steps[0].Response.Content != "" || res.Steps[0].Response.Thinking != "" {
		t.Error("Secrets blocked response was retained in step recording")
	}
	if len(res.ToolCalls) != 0 || len(res.Messages) != 1 || res.Messages[0].Role != "user" || res.Answer != "" {
		t.Error("Secrets blocked response was retained in transcript or dispatch records")
	}
	if len(blocked.Findings) != 1 || blocked.Findings[0].ToolCallID != "original-id" || blocked.Findings[0].Target != agent.TargetOutputToolCall || blocked.Findings[0].Interceptor != "secrets" {
		t.Error("Secrets block lost original output call metadata")
	}
}

func TestSecretsPipelineBlocksOutputAndPartialProviderErrors(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider stopped")
	for _, tc := range []struct {
		name     string
		response provider.ChatResponse
		err      error
	}{
		{"content", provider.ChatResponse{Content: syntheticSecret()}, nil},
		{"thinking", provider.ChatResponse{Thinking: syntheticSecret()}, nil},
		{"partial content", provider.ChatResponse{Content: syntheticSecret()}, providerErr},
		{"partial thinking", provider.ChatResponse{Thinking: syntheticSecret()}, providerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &secretCaller{response: tc.response, err: tc.err}
			observer := &secretObserver{}
			o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(Defaults()...))
			res, err := o.Run(t.Context(), agent.Request{Goal: "q"}, observer)
			var blocked *agent.BlockedError
			if !errors.As(err, &blocked) || blocked.Hook != agent.HookOutput {
				t.Fatal("Secrets collected output did not terminally block")
			}
			if tc.err != nil && !errors.Is(err, providerErr) {
				t.Error("Secrets partial block lost the provider error")
			}
			if observer.steps != 0 || len(caller.requests) != 1 || res.Answer != "" || len(res.Messages) != 1 || res.Messages[0].Role != "user" {
				t.Error("Secrets blocked output reached a publication or transcript")
			}
			for _, step := range res.Steps {
				if step.Response.Content != "" || step.Response.Thinking != "" || step.Response.ToolCalls != nil {
					t.Error("Secrets blocked output remained in step recording")
				}
			}
		})
	}
}

type secretResultTool struct {
	name   string
	result agent.ToolResult
}

func (s secretResultTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s secretResultTool) Effect() agent.Effect {
	class := agent.Read
	if s.name == agent.WriteFileToolName {
		class = agent.Write
	}
	return agent.Effect{Class: class, Approval: agent.ApprovalNever}
}
func (secretResultTool) Origin() agent.Origin { return agent.OriginWorkspace }
func (s secretResultTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return s.result, nil
}

func secretContext(content string) *agent.ContextSet {
	return &agent.ContextSet{Groups: []agent.ContextGroup{{
		Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "synthetic"}},
		Alternatives: []agent.ContextAlternative{{
			Desc:    contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata}}},
			Content: content,
		}},
	}}}
}

func TestSecretsPipelineReplacesObservationsAndAlternatives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		mixed       bool
		content     string
		alternative string
	}{
		{"fallback legacy", false, syntheticSecret(), "safe alternative"},
		{"fallback mixed", true, syntheticSecret(), "safe alternative"},
		{"alternative mixed", true, "safe fallback", syntheticSecret()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := secretResultTool{name: "remote", result: agent.ToolResult{Content: tc.content, Context: secretContext(tc.alternative), Attrib: &agent.RetrievalAttribution{}}}
			caller := &secretCaller{response: provider.ChatResponse{ToolCalls: []provider.ToolCall{toolCall("read-id", tool.name, `{}`)}}}
			observer := &secretObserver{}
			o := agent.New(caller, agent.ContextManager{Mixed: tc.mixed}, agent.WithInterceptors(Defaults()...))
			res, err := o.Run(t.Context(), agent.Request{Goal: "q", Tools: []agent.Tool{tool}}, observer)
			if err != nil || len(caller.requests) != 2 || len(observer.results) != 1 || len(res.Messages) != 4 {
				t.Fatal("Secrets observation pipeline did not finish its safe second turn")
			}
			const want = "tool result blocked by interceptor secrets (sensitive_openai_token)"
			event := observer.results[0]
			if event.Result.Content != want || event.Result.Context != nil || event.Result.Attrib != nil || !event.Blocked || !event.Invoked || !event.Result.IsError {
				t.Error("Secrets observer received an unreplaced result or alternative")
			}
			if res.Messages[2].Content != want || !res.ToolCalls[0].Blocked || !res.ToolCalls[0].Invoked {
				t.Error("Secrets transcript did not retain the safe observation and invocation metadata")
			}
			var observation string
			for _, msg := range caller.requests[1].Messages {
				if msg.Role == "tool" {
					observation = msg.Content
				}
			}
			if !strings.Contains(observation, want) || strings.Contains(observation, syntheticSecret()) {
				t.Error("Secrets model request did not receive only the safe replacement")
			}
		})
	}
}

type secretVerifier string

func (s secretVerifier) Verify(context.Context, agent.Approver) (string, error) {
	return string(s), nil
}

func TestSecretsPipelineReplacesVerifierObservation(t *testing.T) {
	t.Parallel()
	tool := secretResultTool{name: agent.WriteFileToolName, result: agent.ToolResult{Content: "applied"}}
	caller := &secretCaller{response: provider.ChatResponse{ToolCalls: []provider.ToolCall{toolCall("write-id", tool.name, `{}`)}}}
	o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(Defaults()...), agent.WithVerifier(secretVerifier(syntheticSecret())))
	res, err := o.Run(t.Context(), agent.Request{Goal: "q", Tools: []agent.Tool{tool}}, nil)
	if err != nil || len(caller.requests) != 2 || len(res.Messages) != 4 {
		t.Fatal("Secrets verifier pipeline did not finish its safe second turn")
	}
	const want = "applied\ntool result blocked by interceptor secrets (sensitive_openai_token)"
	if res.Messages[2].Content != want {
		t.Error("Secrets verifier output was not safely replaced before transcript recording")
	}
	for _, msg := range caller.requests[1].Messages {
		if strings.Contains(msg.Content, syntheticSecret()) {
			t.Error("Secrets verifier content reached the model")
		}
	}
}
