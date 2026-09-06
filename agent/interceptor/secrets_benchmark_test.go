package interceptor

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// BenchmarkSecretsTurn64KB counts original payload bytes once, before JSON
// decoding, while timing every repeated inspection in a representative turn.
// Late matches intentionally continue through all hooks despite terminal blocks.
func BenchmarkSecretsTurn64KB(b *testing.B) {
	for _, name := range []string{"clean", "late_match"} {
		b.Run(name, func(b *testing.B) {
			var suffix string
			if name == "late_match" {
				suffix = "\n" + syntheticSecret()
			}
			text := func(size int) string {
				const source = "func example() { return true }\n"
				return strings.Repeat(source, size/len(source)+1)[:size-len(suffix)] + suffix
			}
			argument := func(escaped bool) json.RawMessage {
				const before = `{"nested":[{"te\u0078t":"`
				const after = `"}],"last":"line\n`
				value := strings.TrimPrefix(suffix, "\n")
				if escaped {
					value = strings.ReplaceAll(value, "sk-", `\u0073k-`)
				}
				padding := strings.Repeat("ordinary source; ", 128)
				raw := before + padding[:(2<<10)-len(before)-len(after)-len(value)-2] + after + value + `"}`
				if len(raw) != 2<<10 || !json.Valid([]byte(raw)) {
					b.Fatal("benchmark argument must contain exactly 2 KiB of valid JSON")
				}
				return json.RawMessage(raw)
			}
			initial := agent.InputInspection{System: text(4 << 10), Summary: text(4 << 10), Messages: []agent.InspectedMessage{{
				StateIndex: 0, Role: "user", Origin: agent.OriginUser, Content: text(8 << 10),
			}}}
			output := agent.OutputInspection{Content: text(4 << 10), Thinking: text(4 << 10), ToolCalls: []provider.ToolCall{
				{ID: "one", Function: provider.ToolCallFunction{Name: "noop", Arguments: argument(false)}},
				{ID: "two", Function: provider.ToolCallFunction{Name: "noop", Arguments: argument(true)}},
			}}
			observations := []agent.InputInspection{
				inputOf(agent.OriginWorkspace, text(18<<10)),
				inputOf(agent.OriginForeign, text(18<<10)),
			}
			calls := []agent.ToolCallInspection{{Call: output.ToolCalls[0]}, {Call: output.ToolCalls[1]}}
			b.SetBytes(64 << 10)
			b.ReportAllocs()
			ctx := b.Context()
			ic := Secrets{}
			for b.Loop() {
				if _, err := ic.InspectInput(ctx, initial); err != nil {
					b.Fatal("initial inspection failed")
				}
				if _, err := ic.InspectOutput(ctx, output); err != nil {
					b.Fatal("output inspection failed")
				}
				for _, call := range calls {
					if _, err := ic.InspectToolCall(ctx, call); err != nil {
						b.Fatal("tool-call inspection failed")
					}
				}
				for _, observation := range observations {
					if _, err := ic.InspectInput(ctx, observation); err != nil {
						b.Fatal("observation inspection failed")
					}
				}
			}
		})
	}
}
