package interceptor_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/provider"
)

// The expected findings are spelled out, not built from the package's
// constants, so a renamed rule or reworded detail fails here.
var (
	wantConfiguredOutput = agent.Finding{
		Rule: "configured_secret", Verdict: agent.VerdictBlock, Risk: 100, Detail: "configured credential in tool call",
		Target: agent.TargetOutputContent, StateIndex: -1, Group: -1, Alternative: -1,
	}
	wantConfiguredToolCall = agent.Finding{
		Rule: "configured_secret", Verdict: agent.VerdictBlock, Risk: 100, Detail: "configured credential in tool call",
		Target: agent.TargetToolCall, StateIndex: -1, Group: -1, Alternative: -1,
	}
)

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

// configuredFixture is a synthetic credential of no shape the token detectors
// know, generated per test.
func configuredFixture(t *testing.T) string { return "mule-orbit-" + randHex(t, 8) }

// containsMatcher is the plain-substring lookup the host's matcher is: the
// guard, not the matcher, owns JSON decoding.
func containsMatcher(fixture string) func(string) bool {
	return func(s string) bool { return strings.Contains(s, fixture) }
}

func newTestConfiguredSecrets(t *testing.T, fixture string) interceptor.ConfiguredSecrets {
	t.Helper()
	c, err := interceptor.NewConfiguredSecrets(containsMatcher(fixture))
	if err != nil {
		t.Fatalf("NewConfiguredSecrets(matcher) error = %v, want nil", err)
	}
	return c
}

// unicodeEscape writes every byte of s as a JSON \u00XX escape, so a document
// built from it holds s only after decoding.
func unicodeEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		_, _ = fmt.Fprintf(&b, `\u%04x`, s[i])
	}
	return b.String()
}

func outputOf(calls ...provider.ToolCall) agent.OutputInspection {
	return agent.OutputInspection{ToolCalls: calls}
}

func toolCallOf(args string) agent.ToolCallInspection {
	return agent.ToolCallInspection{Call: toolCall("call-1", "lookup", args)}
}

// assertBothHooks checks that args draw exactly want at the output hook and
// the tool-call hook, which repeat the same argument check.
func assertBothHooks(t *testing.T, c interceptor.ConfiguredSecrets, args string, wantHit bool) {
	t.Helper()
	var wantOut, wantCall []agent.Finding
	if wantHit {
		wantOut, wantCall = []agent.Finding{wantConfiguredOutput}, []agent.Finding{wantConfiguredToolCall}
	}
	got, err := c.InspectOutput(t.Context(), outputOf(toolCall("call-1", "lookup", args)))
	if err != nil {
		t.Fatalf("ConfiguredSecrets.InspectOutput error = %v, want nil", err)
	}
	if !slices.Equal(got, wantOut) {
		t.Errorf("ConfiguredSecrets.InspectOutput findings = %+v, want %+v", got, wantOut)
	}
	got, err = c.InspectToolCall(t.Context(), toolCallOf(args))
	if err != nil {
		t.Fatalf("ConfiguredSecrets.InspectToolCall error = %v, want nil", err)
	}
	if !slices.Equal(got, wantCall) {
		t.Errorf("ConfiguredSecrets.InspectToolCall findings = %+v, want %+v", got, wantCall)
	}
}

func TestConfiguredSecretsMatchesArguments(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	esc := unicodeEscape(fx)
	for _, tc := range []struct {
		name    string
		args    string
		literal bool // the document carries the fixture undecoded
	}{
		{name: "raw", args: `{"q":"` + fx + `"}`, literal: true},
		{name: "escaped value", args: `{"q":"` + esc + `"}`},
		{name: "escaped key", args: `{"` + esc + `":"safe"}`},
		{name: "nested", args: `{"a":[{"b":{"c":["safe","` + esc + `"]}}]}`},
		{name: "shadowed duplicate first member", args: `{"k":"` + esc + `","k":"safe"}`},
		{name: "shadowed duplicate last member", args: `{"k":"safe","k":"` + esc + `"}`},
		{name: "malformed", args: `{"q":"` + fx, literal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if strings.Contains(tc.args, fx) != tc.literal {
				t.Fatalf("fixture row %q: literal fixture present = %v, want %v", tc.name, !tc.literal, tc.literal)
			}
			assertBothHooks(t, newTestConfiguredSecrets(t, fx), tc.args, true)
		})
	}
}

func TestConfiguredSecretsMetadata(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	clean := `{"q":"safe"}`
	cases := []struct {
		name  string
		calls []provider.ToolCall
	}{
		{name: "call id", calls: []provider.ToolCall{toolCall(fx, "lookup", clean)}},
		{name: "function name", calls: []provider.ToolCall{toolCall("call-1", fx, clean)}},
		{name: "call type", calls: []provider.ToolCall{{ID: "call-1", Type: fx, Function: provider.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(clean)}}}},
		{name: "second call in batch", calls: []provider.ToolCall{toolCall("call-1", "lookup", clean), toolCall(fx, "lookup", clean)}},
		{name: "three bad calls give one finding", calls: []provider.ToolCall{
			toolCall(fx, "lookup", clean),
			toolCall("call-2", fx, `{"q":"`+fx+`"}`),
			{ID: "call-3", Type: fx, Function: provider.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(`{"` + unicodeEscape(fx) + `":"x"}`)}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newTestConfiguredSecrets(t, fx).InspectOutput(t.Context(), outputOf(tc.calls...))
			if err != nil {
				t.Fatalf("ConfiguredSecrets.InspectOutput error = %v, want nil", err)
			}
			if want := []agent.Finding{wantConfiguredOutput}; !slices.Equal(got, want) {
				t.Errorf("ConfiguredSecrets.InspectOutput findings = %+v, want %+v", got, want)
			}
		})
	}
}

func TestConfiguredSecretsNoMatch(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	if strings.ToUpper(fx) == fx {
		t.Fatalf("fixture %q has no letters to change case", fx)
	}
	half := len(fx) / 2
	cases := []struct {
		name  string
		calls []provider.ToolCall
	}{
		{name: "case changed value", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"q":"`+strings.ToUpper(fx)+`"}`)}},
		{name: "case changed key", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"`+strings.ToUpper(fx)+`":"x"}`)}},
		{name: "split across values", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"a":"`+fx[:half]+`","b":"`+fx[half:]+`"}`)}},
		{name: "split across key and value", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"`+fx[:half]+`":"`+fx[half:]+`"}`)}},
		{name: "split across id and name", calls: []provider.ToolCall{toolCall(fx[:half], fx[half:], `{"q":"safe"}`)}},
		{name: "split across calls", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"q":"`+fx[:half]+`"}`), toolCall("call-2", "lookup", `{"q":"`+fx[half:]+`"}`)}},
		{name: "unrelated", calls: []provider.ToolCall{toolCall("call-1", "lookup", `{"q":"nothing to see","n":42,"ok":true,"z":null}`)}},
		{name: "no calls"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestConfiguredSecrets(t, fx)
			got, err := c.InspectOutput(t.Context(), outputOf(tc.calls...))
			if err != nil || len(got) != 0 {
				t.Errorf("ConfiguredSecrets.InspectOutput = %+v, %v; want no finding", got, err)
			}
			for _, call := range tc.calls {
				got, err := c.InspectToolCall(t.Context(), agent.ToolCallInspection{Call: call})
				if err != nil || len(got) != 0 {
					t.Errorf("ConfiguredSecrets.InspectToolCall = %+v, %v; want no finding", got, err)
				}
			}
		})
	}
	t.Run("empty set", func(t *testing.T) {
		t.Parallel()
		c, err := interceptor.NewConfiguredSecrets(func(string) bool { return false })
		if err != nil {
			t.Fatalf("NewConfiguredSecrets(always-false matcher) error = %v, want nil", err)
		}
		args := `{"q":"` + fx + `"}`
		out, err := c.InspectOutput(t.Context(), outputOf(toolCall(fx, fx, args)))
		if err != nil || len(out) != 0 {
			t.Errorf("empty-set InspectOutput = %+v, %v; want no finding", out, err)
		}
		call, err := c.InspectToolCall(t.Context(), toolCallOf(args))
		if err != nil || len(call) != 0 {
			t.Errorf("empty-set InspectToolCall = %+v, %v; want no finding", call, err)
		}
	})
}

// TestConfiguredSecretsMatchesEmbeddedValue (Review Focus 1): a credential
// inside a larger string is still a substring match, wherever the string sits.
func TestConfiguredSecretsMatchesEmbeddedValue(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	esc := unicodeEscape(fx)
	for _, tc := range []struct{ name, args string }{
		{name: "url query", args: `{"url":"https://h.example/x?token=` + fx + `&v=1"}`},
		{name: "bearer header", args: `{"headers":{"Authorization":"Bearer ` + fx + `"}}`},
		{name: "prefix and suffix", args: `{"q":"pre` + fx + `suf"}`},
		{name: "escaped inside text", args: `{"q":"Bearer ` + esc + ` end"}`},
		{name: "inside key", args: `{"Bearer ` + fx + `":"x"}`},
		{name: "embedded in malformed", args: `{"cmd":["curl","-H","Authorization: Bearer ` + fx},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertBothHooks(t, newTestConfiguredSecrets(t, fx), tc.args, true)
		})
	}
	t.Run("metadata", func(t *testing.T) {
		t.Parallel()
		got, err := newTestConfiguredSecrets(t, fx).InspectOutput(t.Context(), outputOf(toolCall("call-"+fx+"-1", "lookup", `{}`)))
		if err != nil || !slices.Equal(got, []agent.Finding{wantConfiguredOutput}) {
			t.Errorf("embedded call ID: InspectOutput = %+v, %v; want one finding", got, err)
		}
	})
}

// TestConfiguredSecretsKeyWithJSONSpecialBytes (Review Focus 2): json.Marshal
// writes the quote and backslash escaped and <, >, & as \u00XX, so a check on
// the raw argument bytes alone never sees such a credential.
func TestConfiguredSecretsKeyWithJSONSpecialBytes(t *testing.T) {
	t.Parallel()
	const key = `k"e\y<&>-0001`
	for _, tc := range []struct {
		name string
		doc  any
	}{
		{name: "as value", doc: map[string]string{"q": key}},
		{name: "as key", doc: map[string]string{key: "x"}},
		{name: "nested", doc: map[string]any{"a": []any{map[string]string{"b": "pre" + key + "suf"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if strings.Contains(string(raw), key) {
				t.Fatalf("encoded document %s carries the key undecoded", raw)
			}
			c, err := interceptor.NewConfiguredSecrets(containsMatcher(key))
			if err != nil {
				t.Fatalf("NewConfiguredSecrets error = %v, want nil", err)
			}
			assertBothHooks(t, c, string(raw), true)
		})
	}
}

// TestConfiguredSecretsNoTruncation (Review Focus 3): neither the document nor
// the candidate is clipped, so a credential in the last bytes of a 1 MiB value
// matches.
func TestConfiguredSecretsNoTruncation(t *testing.T) {
	t.Parallel()
	const size = 1 << 20
	fx := configuredFixture(t)
	for _, tc := range []struct{ name, tail string }{
		{name: "raw", tail: fx + "-end"},
		{name: "escaped", tail: unicodeEscape(fx) + "-end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// -end keeps the credential inside the last 32 bytes of the value.
			value := strings.Repeat("a", size-len(fx)-len("-end")) + tc.tail
			args := `{"q":"` + value + `"}`
			if len(args) < size {
				t.Fatalf("argument document is %d bytes, want at least %d", len(args), size)
			}
			assertBothHooks(t, newTestConfiguredSecrets(t, fx), args, true)
		})
	}
}

func TestConfiguredSecretsNameInputAndSensitiveText(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	c := newTestConfiguredSecrets(t, fx)
	if c.Name() != "configured_secrets" {
		t.Errorf("ConfiguredSecrets.Name() = %q, want %q", c.Name(), "configured_secrets")
	}
	// The guard adds no input scanning: existing Secrets and canary coverage
	// there is unchanged.
	got, err := c.InspectInput(t.Context(), agent.InputInspection{
		System: fx, Summary: fx, Messages: []agent.InspectedMessage{{Content: fx}},
	})
	if err != nil || got != nil {
		t.Errorf("ConfiguredSecrets.InspectInput = %+v, %v; want nil, nil", got, err)
	}
	var _ agent.SensitiveTextInterceptor = c
	for _, tc := range []struct {
		text string
		want bool
	}{{"x" + fx + "y", true}, {fx, true}, {strings.ToUpper(fx), false}, {"unrelated", false}, {"", false}} {
		if got := c.SensitiveText(tc.text); got != tc.want {
			t.Errorf("ConfiguredSecrets.SensitiveText(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestNewConfiguredSecretsRejectsNil(t *testing.T) {
	t.Parallel()
	c, err := interceptor.NewConfiguredSecrets(nil)
	if err == nil || err.Error() != "interceptor: configured secrets matcher must not be nil" {
		t.Errorf("NewConfiguredSecrets(nil) error = %v, want the static nil-matcher message", err)
	}
	if c.SensitiveText("anything") {
		t.Error("ConfiguredSecrets from a rejected constructor matched text")
	}
}

// TestConfiguredSecretsZeroValueFailsClosed: a zero value has no matcher, so
// it errors rather than letting calls through unchecked.
func TestConfiguredSecretsZeroValueFailsClosed(t *testing.T) {
	t.Parallel()
	const wantErr = "interceptor: configured secrets matcher must not be nil"
	var zero interceptor.ConfiguredSecrets
	out, err := zero.InspectOutput(t.Context(), outputOf(toolCall("call-1", "lookup", `{}`)))
	if out != nil || err == nil || err.Error() != wantErr {
		t.Errorf("zero InspectOutput = %+v, %v; want nil and the static error", out, err)
	}
	call, err := zero.InspectToolCall(t.Context(), toolCallOf(`{}`))
	if call != nil || err == nil || err.Error() != wantErr {
		t.Errorf("zero InspectToolCall = %+v, %v; want nil and the static error", call, err)
	}
	if zero.SensitiveText("anything") {
		t.Error("zero ConfiguredSecrets.SensitiveText matched text")
	}
}

// runGuarded runs one batch of calls against a tool-bearing orchestrator with
// the given chain and returns the pieces the loop tests assert on.
func runGuarded(t *testing.T, chain []agent.Interceptor, calls []provider.ToolCall, tools ...agent.Tool) (*canaryCaller, *canaryObserver, *canaryApprover, agent.Result, error) {
	t.Helper()
	caller := &canaryCaller{response: provider.ChatResponse{ToolCalls: calls}}
	observer := &canaryObserver{}
	approver := &canaryApprover{}
	o := agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(chain...))
	res, err := o.Run(t.Context(), agent.Request{Goal: "safe goal", Tools: tools, Approver: approver}, observer)
	return caller, observer, approver, res, err
}

func requireOutputBlock(t *testing.T, err error) *agent.BlockedError {
	t.Helper()
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("Orchestrator.Run error = %v, want a *agent.BlockedError", err)
	}
	if blocked.Hook != agent.HookOutput || blocked.Step != 0 {
		t.Errorf("BlockedError location = (%s, %d), want (output, 0)", blocked.Hook, blocked.Step)
	}
	return blocked
}

// TestConfiguredSecretsCentralRegression is the defect #424 closes: a
// credential of no known shape rides in tool arguments, the shape detector
// and the canary both pass it, and only the exact guard stops the call.
func TestConfiguredSecretsCentralRegression(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	call := toolCall("call-1", "lookup", `{"q":"`+fx+`"}`)

	if got, err := (interceptor.Secrets{}).InspectOutput(t.Context(), outputOf(call)); err != nil || len(got) != 0 {
		t.Fatalf("Secrets.InspectOutput(fixture) = %+v, %v; the fixture must be a shape Secrets does not know", got, err)
	}
	if got, err := newTestCanary(t).InspectOutput(t.Context(), outputOf(call)); err != nil || len(got) != 0 {
		t.Fatalf("Canary.InspectOutput(fixture) = %+v, %v; the fixture must not be a canary", got, err)
	}

	t.Run("with the guard", func(t *testing.T) {
		t.Parallel()
		tool := &canaryReadTool{name: "lookup", result: "safe"}
		chain := slices.Concat([]agent.Interceptor{newTestConfiguredSecrets(t, fx)}, interceptor.Defaults())
		_, _, _, _, err := runGuarded(t, chain, []provider.ToolCall{call}, tool)
		blocked := requireOutputBlock(t, err)
		if len(blocked.Findings) != 1 || blocked.Findings[0].Interceptor != "configured_secrets" || blocked.Findings[0].Rule != "configured_secret" {
			t.Errorf("BlockedError findings = %+v, want the one configured_secret finding", blocked.Findings)
		}
		if n := tool.invokes.Load(); n != 0 {
			t.Errorf("tool Invoke count = %d, want 0", n)
		}
	})
	t.Run("without the guard", func(t *testing.T) {
		t.Parallel()
		tool := &canaryReadTool{name: "lookup", result: "safe"}
		_, _, _, _, err := runGuarded(t, interceptor.Defaults(), []provider.ToolCall{call}, tool)
		if err != nil {
			t.Fatalf("Orchestrator.Run with Defaults() error = %v, want nil", err)
		}
		if n := tool.invokes.Load(); n != 1 {
			t.Errorf("tool Invoke count = %d, want 1", n)
		}
	})
}

// TestShapeAndCanaryUnchangedWithConfiguredSecrets: installing the guard,
// empty or not, changes nothing the existing detectors decide.
func TestShapeAndCanaryUnchangedWithConfiguredSecrets(t *testing.T) {
	t.Parallel()
	const shapeSecret = "sk-" + "aB3_dE7-fG9_" + "hJ2-kL4"
	other := configuredFixture(t)
	empty, err := interceptor.NewConfiguredSecrets(func(string) bool { return false })
	if err != nil {
		t.Fatalf("NewConfiguredSecrets(always-false matcher) error = %v, want nil", err)
	}
	canary := newTestCanary(t)
	chains := []struct {
		name  string
		chain []agent.Interceptor
	}{
		{name: "absent", chain: []agent.Interceptor{interceptor.Secrets{}, canary}},
		{name: "empty set", chain: []agent.Interceptor{empty, interceptor.Secrets{}, canary}},
		{name: "nonempty set", chain: []agent.Interceptor{newTestConfiguredSecrets(t, other), interceptor.Secrets{}, canary}},
	}
	type decision struct {
		Interceptor, Rule string
		Verdict           agent.Verdict
		Risk              int
	}
	for _, tc := range []struct {
		name string
		args string
		want []decision
	}{
		{name: "shape secret", args: `{"p":"` + shapeSecret + `"}`, want: []decision{{"secrets", "sensitive_openai_token", agent.VerdictBlock, 100}}},
		{name: "canary", args: `{"p":"` + canaryNonce + `"}`, want: []decision{{"canary", "canary_in_tool_arguments", agent.VerdictAbort, 100}}},
		{name: "both", args: `{"p":"` + shapeSecret + `","q":"` + canaryNonce + `"}`, want: []decision{
			{"secrets", "sensitive_openai_token", agent.VerdictBlock, 100}, {"canary", "canary_in_tool_arguments", agent.VerdictAbort, 100},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var first []agent.Finding
			for i, ch := range chains {
				tool := &canaryReadTool{name: "lookup", result: "safe"}
				_, _, _, _, err := runGuarded(t, ch.chain, []provider.ToolCall{toolCall("call-1", "lookup", tc.args)}, tool)
				blocked := requireOutputBlock(t, err)
				var got []decision
				for _, f := range blocked.Findings {
					got = append(got, decision{f.Interceptor, f.Rule, f.Verdict, f.Risk})
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("%s chain decisions = %+v, want %+v", ch.name, got, tc.want)
				}
				if i == 0 {
					first = blocked.Findings
				} else if !slices.Equal(blocked.Findings, first) {
					t.Errorf("%s chain findings = %+v, differ from the absent chain's %+v", ch.name, blocked.Findings, first)
				}
				if n := tool.invokes.Load(); n != 0 {
					t.Errorf("%s chain tool Invoke count = %d, want 0", ch.name, n)
				}
			}
		})
	}
}

// TestConfiguredSecretsWithCanaryKeepsAbort [plan-consult 424-B11]: a call
// that carries both the canary and a configured credential still aborts, in
// either chain order (spec D6, Existing behavior).
func TestConfiguredSecretsWithCanaryKeepsAbort(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	canary := newTestCanary(t)
	const wantErr = "agent: output blocked by interceptor canary (canary_in_tool_arguments)"
	for _, tc := range []struct {
		name  string
		chain []agent.Interceptor
	}{
		{name: "guard first", chain: []agent.Interceptor{newTestConfiguredSecrets(t, fx), canary}},
		{name: "canary first", chain: []agent.Interceptor{canary, newTestConfiguredSecrets(t, fx)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tool := &canaryReadTool{name: "lookup", result: "safe"}
			args := `{"a":"` + canaryNonce + `","b":"` + fx + `"}`
			_, _, _, _, err := runGuarded(t, tc.chain, []provider.ToolCall{toolCall("call-1", "lookup", args)}, tool)
			blocked := requireOutputBlock(t, err)
			if blocked.Error() != wantErr {
				t.Errorf("BlockedError = %q, want %q (the cause verdict must stay abort)", blocked.Error(), wantErr)
			}
			verdicts := map[string]agent.Verdict{}
			for _, f := range blocked.Findings {
				verdicts[f.Interceptor] = f.Verdict
			}
			if verdicts["canary"] != agent.VerdictAbort || verdicts["configured_secrets"] != agent.VerdictBlock || len(blocked.Findings) != 2 {
				t.Errorf("BlockedError findings = %+v, want canary abort and configured_secrets block", blocked.Findings)
			}
			if n := tool.invokes.Load(); n != 0 {
				t.Errorf("tool Invoke count = %d, want 0", n)
			}
		})
	}
}

// TestConfiguredSecretsBlocksBeforePublication [plan-consult 424-B8]: the real
// guard stops a credential-bearing batch at the output hook, before OnStep,
// OnToolCall, Plan, approval or Invoke, clean siblings included. Moving the
// exact check to InspectToolCall alone fails every row here; a metadata-only
// row fails if the ID, name or type check is skipped.
func TestConfiguredSecretsBlocksBeforePublication(t *testing.T) {
	t.Parallel()
	fx := configuredFixture(t)
	clean := `{"value":"safe"}`
	cases := []struct {
		name  string
		calls []provider.ToolCall
	}{
		{name: "raw arguments", calls: []provider.ToolCall{toolCall("call-1", "write_b", `{"value":"`+fx+`"}`)}},
		{name: "escaped arguments", calls: []provider.ToolCall{toolCall("call-1", "write_b", `{"value":"`+unicodeEscape(fx)+`"}`)}},
		{name: "clean sibling first", calls: []provider.ToolCall{toolCall("call-1", "write_a", clean), toolCall("call-2", "write_b", `{"value":"`+fx+`"}`)}},
		{name: "call id only", calls: []provider.ToolCall{toolCall("id-"+fx, "write_b", clean)}},
		{name: "function name only", calls: []provider.ToolCall{toolCall("call-1", "name-"+fx, clean)}},
		{name: "call type only", calls: []provider.ToolCall{{ID: "call-1", Type: "type-" + fx, Function: provider.ToolCallFunction{Name: "write_b", Arguments: json.RawMessage(clean)}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := &canaryPlanningTool{name: "write_a"}, &canaryPlanningTool{name: "write_b"}
			caller, observer, approver, res, err := runGuarded(t, []agent.Interceptor{newTestConfiguredSecrets(t, fx)}, tc.calls, a, b)
			blocked := requireOutputBlock(t, err)
			if len(blocked.Findings) != 1 || blocked.Findings[0].Interceptor != "configured_secrets" || blocked.Findings[0].Rule != "configured_secret" || blocked.Findings[0].Verdict != agent.VerdictBlock {
				t.Fatalf("BlockedError findings = %+v, want the one configured_secrets block", blocked.Findings)
			}
			if f := blocked.Findings[0]; f.Detail != "" || f.ToolCallID != "" {
				t.Errorf("published finding keeps free text: Detail %q, ToolCallID %q", f.Detail, f.ToolCallID)
			}
			if caller.calls != 1 {
				t.Errorf("model calls = %d, want 1", caller.calls)
			}
			if observer.steps != 0 || observer.calls != 0 || observer.results != 0 {
				t.Errorf("OnStep/OnToolCall/OnToolResult = %d/%d/%d, want 0/0/0", observer.steps, observer.calls, observer.results)
			}
			if a.plans.Load() != 0 || b.plans.Load() != 0 || approver.calls.Load() != 0 || a.invokes.Load() != 0 || b.invokes.Load() != 0 {
				t.Errorf("Plan/approval/Invoke counts = %d+%d/%d/%d+%d, want 0", a.plans.Load(), b.plans.Load(), approver.calls.Load(), a.invokes.Load(), b.invokes.Load())
			}
			if strings.Contains(err.Error(), fx) || strings.Contains(fmt.Sprintf("%+v", res.Messages), fx) {
				t.Error("the rejection carries the credential")
			}
		})
	}
}
