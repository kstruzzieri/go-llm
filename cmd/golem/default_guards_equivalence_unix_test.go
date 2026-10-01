//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// normalizedRequests marshals each request and replaces its one per-render
// fence id after checking the id occurs only in the open and close marker of
// each tool message (one fence per render, #430). It fails when no request
// carried a tool message, so a normalizer that never saw a fence cannot make
// two runs look equal.
func normalizedRequests(t *testing.T, reqs []provider.ChatRequest) []string {
	t.Helper()
	out := make([]string, len(reqs))
	fenced := 0
	for i, req := range reqs {
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		key, tools := "", 0
		for _, m := range req.Messages {
			if m.Role != "tool" {
				continue
			}
			k := toolFrameKey(t, m.Content)
			if key != "" && k != key {
				t.Fatalf("request %d: two fence ids %q and %q in one render", i, key, k)
			}
			key = k
			tools++
		}
		if key != "" {
			if n := strings.Count(s, key); n != 2*tools {
				t.Fatalf("request %d: fence id occurs %d times, want %d (markers only)", i, n, 2*tools)
			}
			s = strings.ReplaceAll(s, key, "FENCEID")
			fenced++
		}
		// json:"-" fields still reach the provider (SessionID becomes a
		// request header), so compare them too.
		hidden, err := json.Marshal([]any{req.SessionID, req.ParseThinkMode, req.ParseThinkTags})
		if err != nil {
			t.Fatal(err)
		}
		out[i] = s + string(hidden)
	}
	if fenced == 0 {
		t.Fatalf("no request carried a fenced tool message in %d requests", len(reqs))
	}
	return out
}

// TestDefaultGuardsLeaveAllowedCallsEquivalent (#575): for these fixtures, a
// guarded run and an unguarded run execute the same calls, record the same
// raw observations and send the same provider requests modulo the per-render
// fence id. The guards still saw the exec calls (uname scores unknown 10),
// which is the only allowed difference. Both run_command calls are approved
// through the real -allow-tool approver, so equivalence holds under the same
// authorization path a headless run uses.
func TestDefaultGuardsLeaveAllowedCallsEquivalent(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello guards\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allow, err := newAllowToolSet([]string{"run_command"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(opts ...agent.Option) (agent.Result, []string) {
		t.Helper()
		readers, err := buildTools(root, nil)
		if err != nil {
			t.Fatal(err)
		}
		execs, err := agenttools.NewExecTools(root)
		if err != nil {
			t.Fatal(err)
		}
		caller := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{
			toolStep("a1", "read_file", `{"path":"hello.txt"}`),
			toolStep("a2", "search", `{"pattern":"hello"}`),
			toolStep("a3", "run_command", `{"argv":["echo","quiet"]}`),
			toolStep("a4", "run_command", `{"argv":["uname"]}`),
			answerStep("equivalent"),
		}}}
		res, err := agent.New(caller, agent.ContextManager{}, opts...).Run(context.Background(),
			agent.Request{Goal: "equivalence", SessionID: "equivalence", Tools: append(readers, execs...), Approver: newHeadlessApprover(allow)}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res, normalizedRequests(t, caller.reqs)
	}
	guarded, guardedReqs := run(agent.WithInterceptors(interceptorsFor(flags{}, nil)...))
	bare, bareReqs := run()
	// Real-output oracle: both runs invoke all four calls and get the tools'
	// actual outputs, so two equally broken runs cannot pass. The formats are
	// the search tool's "path:line: text" and formatExecResult's exit/stdout/
	// stderr frame.
	for name, res := range map[string]agent.Result{"guarded": guarded, "bare": bare} {
		if len(res.ToolCalls) != 4 || res.Answer != "equivalent" {
			t.Fatalf("%s run = %d calls, answer %q", name, len(res.ToolCalls), res.Answer)
		}
		for i, rec := range res.ToolCalls {
			if !rec.Invoked || rec.IsError || rec.Blocked {
				t.Fatalf("%s call %d = %+v, want a real successful invocation", name, i, rec)
			}
		}
		var outs []string
		for _, m := range res.Messages {
			if m.Role == "tool" {
				outs = append(outs, m.Content)
			}
		}
		if len(outs) != 4 ||
			!strings.Contains(outs[0], "hello guards") ||
			!strings.Contains(outs[1], "hello.txt:1: hello guards") ||
			outs[2] != "exit code: 0\n--- stdout ---\nquiet\n\n--- stderr ---\n" ||
			!strings.HasPrefix(outs[3], "exit code: 0\n--- stdout ---\n") ||
			!strings.HasSuffix(outs[3], "\n\n--- stderr ---\n") ||
			len(outs[3]) <= len("exit code: 0\n--- stdout ---\n\n--- stderr ---\n") {
			t.Fatalf("%s outputs = %q", name, outs)
		}
	}
	// Whole results, not just Messages: records (AutoApproved, Provenance...),
	// events and stop reason must match too. Latency is wall-clock and Risk
	// is the one intended difference, checked below.
	norm := func(r agent.Result) agent.Result {
		r.ToolCalls, r.Steps, r.Risk = slices.Clone(r.ToolCalls), slices.Clone(r.Steps), nil
		for i := range r.ToolCalls {
			r.ToolCalls[i].Latency = 0
		}
		for i := range r.Steps {
			r.Steps[i].Latency = 0
		}
		return r
	}
	if g, b := norm(guarded), norm(bare); !reflect.DeepEqual(g, b) {
		t.Fatalf("results differ beyond risk:\nguarded %+v\nbare    %+v", g, b)
	}
	if len(guardedReqs) != 5 || len(bareReqs) != 5 {
		t.Fatalf("request counts = %d guarded, %d bare, want 5 (four tool steps and the answer)", len(guardedReqs), len(bareReqs))
	}
	if !reflect.DeepEqual(guardedReqs, bareReqs) {
		for i := range guardedReqs {
			if guardedReqs[i] != bareReqs[i] {
				t.Fatalf("request %d differs:\nguarded %s\nbare    %s", i, guardedReqs[i], bareReqs[i])
			}
		}
	}
	// The guards inspected every call but flagged exactly the non-quiet exec:
	// the one allowed difference between the runs.
	if guarded.Risk == nil || guarded.Risk.Score != 10 || bare.Risk != nil {
		t.Fatalf("risk guarded = %+v bare = %+v, want 10 (uname unknown) and nil", guarded.Risk, bare.Risk)
	}
	if got := findingPairs(guarded.Risk.Findings); !slices.Equal(got, []findingPair{{"egress", "unknown"}}) {
		t.Fatalf("guarded findings = %+v, want exactly one egress unknown", guarded.Risk.Findings)
	}
	if id := guarded.Risk.Findings[0].ToolCallID; id != "a4" {
		t.Fatalf("finding is for call %q, want a4 (the non-quiet uname)", id)
	}
}
