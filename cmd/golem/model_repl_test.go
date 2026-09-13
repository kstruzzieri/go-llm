package main

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// This file pins the /model command surface (#376 M5/M6): the read-only
// status block, the usage line, the synchronous /model set sequence and its
// bookkeeping, and the REPL input sequences around both.

// newModelStatusRuntime builds a runtime whose current snapshot carries the
// exact budget and options the status block must read back.
func newModelStatusRuntime(t *testing.T, budget agent.Budget, opts provider.ModelOptions) *golemruntime.Runtime {
	t.Helper()
	rt, err := golemruntime.New(t.Context(), golemruntime.Options{
		Root:         t.TempDir(),
		System:       "system",
		Budget:       budget,
		ModelOptions: opts,
		Orchestrator: agent.New(&scriptCaller{}, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestModelStatusRendersTheLiteralBlock pins the M6 fixture byte for byte.
// Every value is read back from a DIFFERENT authority -- the selection for
// the selector/chain/ceiling source, the runtime snapshot for the ceiling
// value and thinking, the session for the last routed model -- so a status
// line sourced from a stale cache shows up as a wrong byte here.
func TestModelStatusRendersTheLiteralBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		sel  modelSelection
		last string
		opts provider.ModelOptions
		want string
	}{
		{
			name: "strict chain",
			sel: modelSelection{
				requested:     "coding",
				chain:         []string{"local/big", "local/small"},
				useCase:       "agent",
				ceilingSource: inputCeilingChainMinimum,
			},
			opts: provider.ModelOptions{ThinkEffort: "high"},
			want: "model: coding\n" +
				"chain: local/big -> local/small (strict; use case: agent)\n" +
				"input ceiling: 28672 tokens (chain minimum)\n" +
				"think: high\n" +
				"last routed: not yet routed\n",
		},
		{
			name: "recommend mode",
			sel: modelSelection{
				chain:         nil,
				useRecommend:  true,
				useCase:       "agent",
				ceilingSource: inputCeilingSafeFallback,
			},
			last: "test/agent-model",
			want: "model: none configured\n" +
				"chain: model recommendation (use case: agent)\n" +
				"input ceiling: 28672 tokens (safe fallback; model context metadata unavailable)\n" +
				"think: default (model decides)\n" +
				"last routed: test/agent-model\n",
		},
		{
			name: "explicit ceiling and thinking off",
			sel: modelSelection{
				requested:     "test/alt-model",
				chain:         []string{"test/alt-model"},
				useCase:       "agent",
				ceilingSource: inputCeilingExplicit,
			},
			last: "test/alt-model",
			opts: provider.ModelOptions{Think: boolPtr(false)},
			want: "model: test/alt-model\n" +
				"chain: test/alt-model (strict; use case: agent)\n" +
				"input ceiling: 28672 tokens (explicit)\n" +
				"think: off\n" +
				"last routed: test/alt-model\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &replSession{
				runtime:   newModelStatusRuntime(t, agent.Budget{InputCeiling: 28672}, tc.opts),
				selection: tc.sel,
				lastModel: tc.last,
			}
			var out strings.Builder
			if forced, exit := dispatchSlash(t.Context(), &out, sess, "/model"); forced != "" || exit {
				t.Fatalf("/model returned forced=%q exit=%v", forced, exit)
			}
			if out.String() != tc.want {
				t.Fatalf("/model =\n%q\nwant\n%q", out.String(), tc.want)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// TestModelUsageLineForInvalidForms pins the one usage line every malformed
// command prints, and that nothing else is written.
func TestModelUsageLineForInvalidForms(t *testing.T) {
	const usage = "usage: /model [set <role|name>]\n"
	for _, line := range []string{"/model show", "/model set", "/model set a b", "/model SET x", "/model set  "} {
		sess := &replSession{
			runtime:   newModelStatusRuntime(t, agent.Budget{InputCeiling: 100}, provider.ModelOptions{}),
			selection: modelSelection{requested: "coding", chain: []string{"local/big"}, useCase: "agent"},
		}
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, line)
		if out.String() != usage {
			t.Fatalf("%q = %q, want %q", line, out.String(), usage)
		}
	}
}
