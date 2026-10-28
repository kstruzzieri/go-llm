package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/provider"
)

// sensitiveReadTool is the child's read_file: it returns fixed text, so the
// retained evidence a fallback would salvage is known.
type sensitiveReadTool struct{ content string }

func (sensitiveReadTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (sensitiveReadTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}
func (t sensitiveReadTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.content}, nil
}

// namedSearchBlocker blocks the output of any response that proposes a search
// call, under whatever interceptor name a row needs.
type namedSearchBlocker struct{ name string }

func (b namedSearchBlocker) Name() string { return b.name }
func (namedSearchBlocker) InspectInput(context.Context, agent.InputInspection) ([]agent.Finding, error) {
	return nil, nil
}
func (namedSearchBlocker) InspectOutput(_ context.Context, out agent.OutputInspection) ([]agent.Finding, error) {
	for _, call := range out.ToolCalls {
		if call.Function.Name == "search" {
			return []agent.Finding{{Rule: "test_block", Verdict: agent.VerdictBlock, Target: agent.TargetOutputContent, StateIndex: -1, Group: -1, Alternative: -1}}, nil
		}
	}
	return nil, nil
}
func (namedSearchBlocker) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}

// failingInterceptor errors on the output that proposes a search call, so the
// block on that same output is joined with its error.
type failingInterceptor struct{}

func (failingInterceptor) Name() string { return "failing" }
func (failingInterceptor) InspectInput(context.Context, agent.InputInspection) ([]agent.Finding, error) {
	return nil, nil
}
func (failingInterceptor) InspectOutput(_ context.Context, out agent.OutputInspection) ([]agent.Finding, error) {
	for _, call := range out.ToolCalls {
		if call.Function.Name == "search" {
			return nil, errors.New("inspection unavailable")
		}
	}
	return nil, nil
}
func (failingInterceptor) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}

// renamedGuard is ConfiguredSecrets under a host's own name: a sensitive
// guard that SensitiveBlocked cannot recognize by name, only by its mark.
type renamedGuard struct {
	interceptor.ConfiguredSecrets
	name string
}

func (g renamedGuard) Name() string { return g.name }

var _ agent.SensitiveTextInterceptor = renamedGuard{}

func sensitiveFixture(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return "mule-orbit-" + hex.EncodeToString(b)
}

func configuredGuard(t *testing.T, fixture string) interceptor.ConfiguredSecrets {
	t.Helper()
	g, err := interceptor.NewConfiguredSecrets(func(s string) bool { return strings.Contains(s, fixture) })
	if err != nil {
		t.Fatalf("NewConfiguredSecrets: %v", err)
	}
	return g
}

// sensitiveChild is one dispatch child: it reads a file (the retained
// evidence), then proposes a search call, which the chain blocks.
type sensitiveChild struct {
	chain      []agent.Interceptor
	readOutput string
	searchArgs string
	model      provider.ModelKey
}

// run invokes a one-task dispatch and returns the raw parent envelope and its
// only decoded result.
func (c sensitiveChild) run(t *testing.T) (string, dispatchResult) {
	t.Helper()
	var steps atomic.Int32
	caller := dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		out := agent.ModelResult{RouteOutcome: &provider.RouteOutcome{ActualModel: c.model}}
		switch steps.Add(1) {
		case 1:
			out.Response = provider.ChatResponse{ToolCalls: []provider.ToolCall{
				{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}},
			}}
		case 2:
			out.Response = provider.ChatResponse{ToolCalls: []provider.ToolCall{
				{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "search", Arguments: json.RawMessage(c.searchArgs)}},
			}}
		default:
			out.Response = provider.ChatResponse{Content: "done", Done: true}
		}
		return out, nil
	})
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		sensitiveReadTool{content: c.readOutput},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	d, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{MaxSteps: 4, MaxTasks: 1, MaxConcurrent: 1}, c.chain...)
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	res, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":["inspect notes"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := steps.Load(); got != 2 {
		t.Fatalf("model calls = %d, want 2 (the second response is the one blocked)", got)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(res.Content), &envelope); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\n%s", err, res.Content)
	}
	if len(envelope.Results) != 1 {
		t.Fatalf("results = %+v, want 1", envelope.Results)
	}
	return res.Content, envelope.Results[0]
}

// jsonStrings collects every object key and string value in a decoded document.
func jsonStrings(v any, out *[]string) {
	switch v := v.(type) {
	case string:
		*out = append(*out, v)
	case []any:
		for _, item := range v {
			jsonStrings(item, out)
		}
	case map[string]any:
		for key, item := range v {
			*out = append(*out, key)
			jsonStrings(item, out)
		}
	}
}

// envelopeLeak returns how secret appears in the envelope, or "" when it does
// not: as written, as JSON \u00XX escapes of every byte, or in any decoded key
// or string value (which catches any partial escape).
func envelopeLeak(content, secret string) string {
	if strings.Contains(content, secret) {
		return "literal"
	}
	var escaped strings.Builder
	for i := range len(secret) {
		_, _ = fmt.Fprintf(&escaped, `\u%04x`, secret[i])
	}
	if strings.Contains(strings.ToLower(content), escaped.String()) {
		return "escaped"
	}
	var doc any
	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		return "invalid JSON: " + err.Error()
	}
	var strs []string
	jsonStrings(doc, &strs)
	for _, s := range strs {
		if strings.Contains(s, secret) {
			return "decoded"
		}
	}
	return ""
}

func requireEnvelopeWithout(t *testing.T, content, secret string) {
	t.Helper()
	if how := envelopeLeak(content, secret); how != "" {
		t.Fatalf("envelope leaks %q (%s): %s", secret, how, content)
	}
}

func TestEnvelopeLeakDetector(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"clean", `{"results":[{"summary":"ok","model":""}]}`, ""},
		{"literal", `{"results":[{"summary":"xx abcdefgh yy"}]}`, "literal"},
		{"every byte escaped", `{"results":[{"summary":"\u0061\u0062\u0063\u0064\u0065\u0066\u0067\u0068"}]}`, "escaped"},
		{"partly escaped", `{"results":[{"summary":"\u0061bcdefgh"}]}`, "decoded"},
		{"in a key", `{"results":[{"\u0061bcdefgh":"x"}]}`, "decoded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := envelopeLeak(tc.content, "abcdefgh"); got != tc.want {
				t.Fatalf("envelopeLeak = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDispatchSensitiveBlockSkipsPartialSummary(t *testing.T) {
	fixture := sensitiveFixture(t)
	secretShaped := "sk-" + "aB3_dE7-fG9_" + "hJ2-kL4"
	evidence := "retained evidence " + sensitiveFixture(t)
	cases := []struct {
		name        string
		child       sensitiveChild
		wantSummary string
		wantBlocked string
		absent      []string
	}{
		{
			name: "configured secret read then proposed",
			child: sensitiveChild{
				chain:      []agent.Interceptor{configuredGuard(t, fixture)},
				readOutput: "file body " + fixture,
				searchArgs: `{"pattern":"` + fixture + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantSummary: dispatchSensitiveSummary,
			wantBlocked: "configured_secrets",
			absent:      []string{fixture},
		},
		{
			name: "secrets block",
			child: sensitiveChild{
				chain:      []agent.Interceptor{interceptor.Secrets{}},
				readOutput: evidence,
				searchArgs: `{"pattern":"` + secretShaped + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantSummary: dispatchSensitiveSummary,
			wantBlocked: "secrets",
			absent:      []string{evidence, secretShaped},
		},
		{
			name: "sensitive block joined with an interceptor error",
			child: sensitiveChild{
				chain:      []agent.Interceptor{failingInterceptor{}, configuredGuard(t, fixture)},
				readOutput: "file body " + fixture,
				searchArgs: `{"pattern":"` + fixture + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantSummary: dispatchSensitiveSummary,
			wantBlocked: "configured_secrets",
			absent:      []string{fixture},
		},
		{
			name: "host sensitive guard under its own name",
			child: sensitiveChild{
				chain:      []agent.Interceptor{renamedGuard{ConfiguredSecrets: configuredGuard(t, fixture), name: "vault"}},
				readOutput: "file body " + fixture,
				searchArgs: `{"pattern":"` + fixture + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantSummary: dispatchSensitiveSummary,
			wantBlocked: "vault",
			absent:      []string{fixture},
		},
		{
			name: "invariants block keeps the partial summary",
			child: sensitiveChild{
				chain:      []agent.Interceptor{namedSearchBlocker{name: "invariants"}},
				readOutput: evidence,
				searchArgs: `{"pattern":"x"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantSummary: "Partial result before error: " + evidence,
			wantBlocked: "invariants",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, got := tc.child.run(t)
			if got.Summary != tc.wantSummary {
				t.Fatalf("summary = %q, want %q", got.Summary, tc.wantSummary)
			}
			if got.StopReason != "error" {
				t.Fatalf("stop_reason = %q, want error", got.StopReason)
			}
			if !strings.Contains(got.Error, "blocked by interceptor "+tc.wantBlocked+" (") {
				t.Fatalf("error = %q, want a block by %s", got.Error, tc.wantBlocked)
			}
			for _, secret := range tc.absent {
				requireEnvelopeWithout(t, content, secret)
			}
		})
	}
}

func TestDispatchSensitiveBlockDropsModel(t *testing.T) {
	fixture := sensitiveFixture(t)
	secretShaped := "sk-" + "aB3_dE7-fG9_" + "hJ2-kL4"
	cases := []struct {
		name      string
		child     sensitiveChild
		wantModel string
		absent    []string
	}{
		{
			name: "configured secret in the model name",
			child: sensitiveChild{
				chain:      []agent.Interceptor{configuredGuard(t, fixture)},
				readOutput: "file body",
				searchArgs: `{"pattern":"` + fixture + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: fixture},
			},
			absent: []string{fixture},
		},
		{
			name: "secrets block drops an innocuous model too",
			child: sensitiveChild{
				chain:      []agent.Interceptor{interceptor.Secrets{}},
				readOutput: "file body",
				searchArgs: `{"pattern":"` + secretShaped + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			absent: []string{secretShaped},
		},
		{
			name: "host sensitive guard under its own name drops the model",
			child: sensitiveChild{
				chain:      []agent.Interceptor{renamedGuard{ConfiguredSecrets: configuredGuard(t, fixture), name: "vault"}},
				readOutput: "file body",
				searchArgs: `{"pattern":"` + fixture + `"}`,
				model:      provider.ModelKey{Provider: "local", Model: fixture},
			},
			absent: []string{fixture},
		},
		{
			name: "invariants block keeps the model",
			child: sensitiveChild{
				chain:      []agent.Interceptor{namedSearchBlocker{name: "invariants"}},
				readOutput: "file body",
				searchArgs: `{"pattern":"x"}`,
				model:      provider.ModelKey{Provider: "local", Model: "fast"},
			},
			wantModel: "local/fast",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content, got := tc.child.run(t)
			if got.Model != tc.wantModel {
				t.Fatalf("model = %q, want %q", got.Model, tc.wantModel)
			}
			for _, secret := range tc.absent {
				requireEnvelopeWithout(t, content, secret)
			}
		})
	}
}
