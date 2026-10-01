package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/mcpclient"
)

// previewStub is a non-executing PlanningTool that previews its argv (exec
// stubs) or its path (the write stub). Invoke only counts.
type previewStub struct {
	name  string
	class agent.EffectClass
	runs  atomic.Int32
}

func (p *previewStub) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: p.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (p *previewStub) Effect() agent.Effect { return agent.Effect{Class: p.class} }
func (p *previewStub) Plan(_ context.Context, args json.RawMessage) (agent.ToolPlan, error) {
	var a struct {
		Argv []string `json:"argv"`
		Path string   `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	if p.name == "write_file" {
		// No trailing newline: the diff renderer appends its own.
		return agent.ToolPlan{Effect: p.Effect(), Preview: "write " + a.Path}, nil
	}
	return agent.ToolPlan{Effect: p.Effect(), Preview: "run command:\n  argv: " + strings.Join(a.Argv, " ") + "\n"}, nil
}
func (p *previewStub) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	p.runs.Add(1)
	return agent.ToolResult{Content: "ok"}, nil
}

// TestDefaultGuardsRiskLineIsCumulativeBadgeIsCurrent (#575): the risk line
// gates on the run's cumulative findings, the egress badge on the current
// call's. After one risky exec, a quiet exec and a write both show the
// cumulative line with no badge; a zero-risk interpreter finding still
// renders a line. Whole-output equality pins each prompt's preview, risk
// line and question together, so a badge leaking onto the quiet exec or the
// write prompt, or a line dropped from either, fails.
func TestDefaultGuardsRiskLineIsCumulativeBadgeIsCurrent(t *testing.T) {
	exec := &previewStub{name: "run_command", class: agent.Read | agent.Write | agent.Exec | agent.Network}
	write := &previewStub{name: "write_file", class: agent.Write}
	var out strings.Builder
	ap := newReplApprover(newScannerSource(strings.NewReader("y\ny\ny\n"), &out), &out, false)
	caller := &scriptCaller{responses: []agent.ModelResult{
		toolStep("c1", "run_command", `{"argv":["curl","https://x"]}`),
		toolStep("c2", "run_command", `{"argv":["echo","hi"]}`),
		toolStep("c3", "write_file", `{"path":"a.txt","content":"x"}`),
		answerStep("done"),
	}}
	res, err := newOrchestratorFactory(caller, flags{}, nil, nil)().Run(context.Background(),
		agent.Request{Goal: "q", Tools: []agent.Tool{exec, write}, Approver: ap}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	const want = "run command:\n  argv: curl https://x\ninterceptor risk 20 · egress: network (curl)\nRun this command? [y/N] " +
		"run command:\n  argv: echo hi\ninterceptor risk 20\nRun this command? [y/N] " +
		"write a.txt\ninterceptor risk 20\nApply this change? [y/N] "
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	requireRecords(t, res, 3)
	for i, rec := range res.ToolCalls {
		if rec.Denied || rec.Blocked || !rec.Invoked {
			t.Fatalf("record %d = %+v, want approved and invoked", i, rec)
		}
	}
	if exec.runs.Load() != 2 || write.runs.Load() != 1 {
		t.Fatalf("runs: exec = %d, write = %d, want 2 and 1", exec.runs.Load(), write.runs.Load())
	}
	if res.Risk == nil || res.Risk.Score != 20 || len(res.Risk.Findings) != 1 ||
		!slices.Equal(findingPairs(res.Risk.Findings), []findingPair{{"egress", "network"}}) || res.Risk.Findings[0].ToolCallID != "c1" {
		t.Fatalf("risk = %+v, want the single network finding from c1, score 20", res.Risk)
	}

	var zero strings.Builder
	ap = newReplApprover(newScannerSource(strings.NewReader("n\n"), &zero), &zero, false)
	caller = &scriptCaller{responses: []agent.ModelResult{toolStep("i1", "run_command", `{"argv":["python3","script.py"]}`), answerStep("done")}}
	res, err = newOrchestratorFactory(caller, flags{}, nil, nil)().Run(context.Background(),
		agent.Request{Goal: "q", Tools: []agent.Tool{&previewStub{name: "run_command", class: agent.Exec}}, Approver: ap}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	const wantZero = "run command:\n  argv: python3 script.py\ninterceptor risk 0 · egress: interpreter (python3)\nRun this command? [y/N] \n"
	if zero.String() != wantZero {
		t.Fatalf("zero-risk output = %q, want %q", zero.String(), wantZero)
	}
	if res.Risk == nil || res.Risk.Score != 0 || !slices.Equal(findingPairs(res.Risk.Findings), []findingPair{{"egress", "interpreter"}}) {
		t.Fatalf("zero-risk result risk = %+v, want one interpreter finding at score 0", res.Risk)
	}
}

// TestDefaultGuardsClassifyMCPArgv (#575): the real MCP adapter declares the
// full effect set, so an MCP call whose arguments carry a decodable argv gets
// an egress finding and a badge at its prompt even when it is then denied; one
// without argv gets none, though it still shows the cumulative line once an
// earlier call in the run produced a finding.
func TestDefaultGuardsClassifyMCPArgv(t *testing.T) {
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "guards", Version: "1"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "run", Description: "run", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{}, nil
		})
	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	t.Cleanup(httpServer.Close)
	// NewPinStore's argument is only the workspace namespace; the pin
	// storage lives under the user data dir (datadir.Base, which honors an
	// absolute XDG_DATA_HOME on every platform), so point that at a temp dir.
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pins, err := mcpclient.NewPinStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, _, err := mcpclient.Connect(t.Context(), mcpclient.Implementation{Name: "guards", Version: "1"},
		[]mcpclient.Server{mcpclient.HTTPServer("fs", httpServer.URL)}, mcpclient.ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	mcpTools := mgr.Tools()
	if len(mcpTools) != 1 || mcpTools[0].Spec().Name != "mcp__fs__run" || !mcpTools[0].Effect().Class.Has(agent.Exec) {
		t.Fatalf("mcp tools = %+v", mcpTools)
	}
	const (
		execBlock  = "run command:\n  argv: curl https://x\ninterceptor risk 20 · egress: network (curl)\nRun this command? [y/N] \n"
		mcpPrompt  = "Run this MCP tool? [y/N] \n"
		argvPrev   = "mcp tool call:\n  tool: mcp__fs__run\n  remote: run\n  args: {\"argv\":[\"curl\",\"https://x\"]}\n"
		noArgvPrev = "mcp tool call:\n  tool: mcp__fs__run\n  remote: run\n  args: {\"q\":\"x\"}\n"
	)
	for _, tc := range []struct {
		name      string
		steps     []agent.ModelResult
		answers   string
		preview   string // the MCP tool's own preview, printed before any risk line
		riskLine  string // the line between the MCP preview and its question
		prefix    string // output of an earlier prompt in the same run
		records   int
		wantRisk  bool
		wantID    string
		wantScore int
	}{
		{
			name:    "argv",
			steps:   []agent.ModelResult{toolStep("m1", "mcp__fs__run", `{"argv":["curl","https://x"]}`), answerStep("done")},
			answers: "n\n", preview: argvPrev, riskLine: "interceptor risk 20 · egress: network (curl)\n", records: 1, wantRisk: true, wantID: "m1", wantScore: 20,
		},
		{
			name:    "no argv",
			steps:   []agent.ModelResult{toolStep("m1", "mcp__fs__run", `{"q":"x"}`), answerStep("done")},
			answers: "n\n", preview: noArgvPrev, records: 1,
		},
		{
			name: "no argv after an earlier finding",
			steps: []agent.ModelResult{
				toolStep("c1", "run_command", `{"argv":["curl","https://x"]}`),
				toolStep("m1", "mcp__fs__run", `{"q":"x"}`),
				answerStep("done"),
			},
			answers: "n\nn\n", prefix: execBlock, preview: noArgvPrev, riskLine: "interceptor risk 20\n", records: 2, wantRisk: true, wantID: "c1", wantScore: 20,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			ap := newReplApprover(newScannerSource(strings.NewReader(tc.answers), &out), &out, false)
			tools := append([]agent.Tool{&previewStub{name: "run_command", class: agent.Read | agent.Write | agent.Exec | agent.Network}}, mcpTools...)
			res, err := newOrchestratorFactory(&scriptCaller{responses: tc.steps}, flags{}, nil, nil)().Run(t.Context(),
				agent.Request{Goal: "q", Tools: tools, Approver: ap}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			requireRecords(t, res, tc.records)
			for i, rec := range res.ToolCalls {
				if !rec.Denied || rec.Invoked || rec.Blocked {
					t.Fatalf("record %d = %+v, want denied at the prompt", i, rec)
				}
			}
			want := tc.prefix + tc.preview + tc.riskLine + mcpPrompt
			if out.String() != want {
				t.Fatalf("output = %q, want %q", out.String(), want)
			}
			if (res.Risk != nil) != tc.wantRisk {
				t.Fatalf("risk = %+v, want present = %v", res.Risk, tc.wantRisk)
			}
			if tc.wantRisk {
				if res.Risk.Score != tc.wantScore || !slices.Equal(findingPairs(res.Risk.Findings), []findingPair{{"egress", "network"}}) || res.Risk.Findings[0].ToolCallID != tc.wantID {
					t.Fatalf("risk = %+v, want the single network finding from %s, score %d", res.Risk, tc.wantID, tc.wantScore)
				}
			}
		})
	}
}
