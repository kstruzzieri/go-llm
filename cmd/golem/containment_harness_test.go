package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

func hostHasCapability(c capability) bool {
	switch c {
	case capBash:
		_, err := exec.LookPath("bash")
		return err == nil
	case capSymlink, capUnix, capExec:
		return os.PathSeparator == '/' // exec runner is unsupported on Windows (exec_other.go:13)
	}
	return false
}

type fixtureOpts struct {
	interceptors bool // full chain + canary (D2/D3)
	autoEdits    bool // seed the /auto-edits file grant
	verifier     agent.Verifier
	background   bool
}

type fixture struct {
	Root, Outside string // workspace root and a sibling outside-root directory
	Grants        *approvalGrants
	Out           *strings.Builder
	tools         []agent.Tool
	opts          fixtureOpts
	t             *testing.T
	mgr           *agenttools.BackgroundManager
}

func newFixture(t *testing.T, opts fixtureOpts) *fixture {
	t.Helper()
	base := t.TempDir()
	f := &fixture{Root: filepath.Join(base, "ws"), Outside: filepath.Join(base, "outside"), Grants: newApprovalGrants(), Out: &strings.Builder{}, opts: opts, t: t}
	for _, d := range []string{f.Root, f.Outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := agenttools.NewWorkspace(f.Root)
	if err != nil {
		t.Fatal(err)
	}
	f.tools = append(f.tools, agenttools.NewMutatingTools(ws, nil)...)
	if opts.background {
		f.mgr = agenttools.NewBackgroundManager()
		t.Cleanup(f.mgr.Shutdown)
		exe, err := agenttools.NewExecToolsWithBackground(f.Root, f.mgr)
		if err != nil {
			t.Fatal(err)
		}
		f.tools = append(f.tools, exe...)
	} else {
		exe, err := agenttools.NewExecTools(f.Root)
		if err != nil {
			t.Fatal(err)
		}
		f.tools = append(f.tools, exe...)
	}
	if opts.autoEdits {
		f.Grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
	}
	return f
}

// approver returns the REPL approver reading answers ("y\n", "N\n", "a\n").
func (f *fixture) approver(answers string) *replApprover {
	ap := newReplApprover(newScannerSource(strings.NewReader(answers), f.Out), f.Out, false)
	ap.grants = f.Grants
	return ap
}

// orchestrator is the ONE construction path every runner uses, so the
// fixture's interceptor chain, canary binding and verifier are always
// honored (a second construction path silently dropped Secrets in review).
func (f *fixture) orchestrator(t *testing.T, caller agent.ModelCaller) *agent.Orchestrator {
	t.Helper()
	var canary *canaryBinding
	if f.opts.interceptors {
		canary = testCanaryBinding(t)
	}
	return newOrchestratorFactory(caller, flags{allowWrite: true, allowExec: true, interceptors: f.opts.interceptors}, f.opts.verifier, canary)()
}

// runErr drives the real Golem factory with a scripted compliant caller and
// returns the run error (D3 expects an *agent.BlockedError).
func (f *fixture) runErr(t *testing.T, caller agent.ModelCaller, maxSteps int, answers string) (agent.Result, error) {
	t.Helper()
	return f.orchestrator(t, caller).Run(context.Background(), agent.Request{Goal: "containment", Tools: f.tools, Approver: f.approver(answers), MaxSteps: maxSteps}, nil)
}

// run is runErr with a scripted list of calls; any run error fails the test.
func (f *fixture) run(t *testing.T, calls []agent.ModelResult, answers string) agent.Result {
	t.Helper()
	caller := &scriptCaller{responses: append(calls, agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}})}
	res, err := f.runErr(t, caller, len(calls)+1, answers)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

var callSeq int

// call scripts one compliant tool call. callID returns the same call with its
// ID so validity checks can read that call's observation via toolObservation.
func call(name string, args any) agent.ModelResult {
	r, _ := callID(name, args)
	return r
}

func callID(name string, args any) (agent.ModelResult, string) {
	callSeq++
	id := "c" + strconv.Itoa(callSeq)
	raw, _ := json.Marshal(args)
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function",
		Function: provider.ToolCallFunction{Name: name, Arguments: raw},
	}}}}, id
}

// prompts counts approval questions rendered so far.
func (f *fixture) prompts() int { return strings.Count(f.Out.String(), "? [y/N") }

// writeScript writes an executable bash script into the workspace.
func (f *fixture) writeScript(t *testing.T, rel, body string) {
	t.Helper()
	p := filepath.Join(f.Root, rel)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}
