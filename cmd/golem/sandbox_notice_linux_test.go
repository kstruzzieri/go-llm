package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// A model that says only "done" must not suppress the operator's recovery
// instructions. Match the actual held name on disk in both output channels.
func TestBwrapREPLShowsRecoveryWithoutModelRelay(t *testing.T) {
	root := canonicalDir(t, t.TempDir())
	policy := nativePolicyForTest(t, root)
	execTools, err := tools.NewSandboxedExecToolsWithOptions(root, policy, tools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	call := provider.ToolCall{ID: "create", Type: "function", Function: provider.ToolCallFunction{
		Name: "run_command", Arguments: json.RawMessage(`{"argv":["mkdir","-p","example/.git"]}`),
	}}
	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{call}}},
		{Response: provider.ChatResponse{Content: "done"}},
	}}
	orch := agent.New(caller, agent.ContextManager{})
	var notices []string
	sess := &replSession{
		runtime:   newTestRuntime(t, root, buildSystemPrompt(false, true), orch, execTools),
		allowExec: true, sandbox: policy, maxSteps: 4, clock: time.Now,
		notice: func(line string) { notices = append(notices, line) },
	}
	var out strings.Builder
	res, err := runOnce(context.Background(), &out, nil, sess, "create a fixture", newScannerSource(strings.NewReader("y\n"), &out))
	if err != nil || res.Answer != "done" {
		t.Fatalf("answer %q, err %v, terminal %s", res.Answer, err, out.String())
	}
	entries, err := os.ReadDir(filepath.Join(root, "example"))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".git.golem-held-") {
		t.Fatalf("held entries %v, err %v", entries, err)
	}
	want := "note: held back new git metadata: example/.git → example/" + entries[0].Name() + " (rename it back to restore)"
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("operator notices %q, want %q", notices, want)
	}
	for _, msg := range caller.lastRequest.Messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, want) {
			return
		}
	}
	t.Fatal("model did not receive the recovery note")
}
