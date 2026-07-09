package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
)

// stubTool is a minimal agent.Tool to stand in for retrieve.
type stubTool struct{ name string }

func (s stubTool) Spec() agent.ToolSpec { return agent.ToolSpec{Name: s.name} }
func (s stubTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (s stubTool) Invoke(ctx context.Context, _ json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func names(tools []agent.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Spec().Name
	}
	return out
}

func TestBuildTools_FileOnly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := buildTools(root, nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	got := names(tools)
	want := []string{"read_file", "search", "glob", "list"}
	if len(got) != len(want) {
		t.Fatalf("tool names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildTools_WithRetrieve(t *testing.T) {
	root := t.TempDir()
	tools, err := buildTools(root, stubTool{name: "retrieve"})
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	got := names(tools)
	if len(got) != 5 || got[4] != "retrieve" {
		t.Errorf("tool names = %v, want file tools + retrieve last", got)
	}
}

func TestBuildTools_BadRoot(t *testing.T) {
	if _, err := buildTools(filepath.Join(t.TempDir(), "does-not-exist"), nil); err == nil {
		t.Fatal("buildTools on missing root err = nil, want error")
	}
}

func TestBuildTools_NoExecTool(t *testing.T) {
	tools, err := buildTools(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("buildTools: %v", err)
	}
	for _, tool := range tools {
		if tool.Spec().Name == "run_command" {
			t.Error("run_command must not be present in the read-only tool set (requires -allow-exec)")
		}
	}
}

func TestBuildSystemPrompt(t *testing.T) {
	ro := buildSystemPrompt(false, false)
	if !strings.Contains(ro, "Do not") || strings.Contains(ro, "run_command to") {
		t.Errorf("read-only prompt wrong:\n%s", ro)
	}
	wo := buildSystemPrompt(true, false)
	if !strings.Contains(wo, "write_file") {
		t.Error("write-only prompt missing write capability")
	}
	if !strings.Contains(wo, "Do not") {
		t.Error("write-only must still forbid running commands (!allowExec)")
	}
	eo := buildSystemPrompt(false, true)
	if !strings.Contains(eo, "run_command") {
		t.Error("exec prompt missing exec capability")
	}
	if strings.Contains(eo, "Do not claim to run") {
		t.Error("exec-enabled prompt must NOT forbid commands")
	}
	if !strings.Contains(eo, "authoritative") {
		t.Error("priority note dropped")
	}
}

func TestBuildExecTools(t *testing.T) {
	tools, err := buildExecTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Spec().Name != "run_command" {
		t.Fatalf("got %d tools", len(tools))
	}
}

func TestBuildDelegateTool_ResolvesCodingRole(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"coding": {Name: "coder", Provider: "local"},
		},
	}
	tool, chain, err := buildDelegateTool(cfg, nil, "coding", nil)
	if err != nil {
		t.Fatalf("buildDelegateTool: %v", err)
	}
	if tool == nil || tool.Spec().Name != "delegate_code" {
		t.Fatalf("expected delegate_code tool, got %+v", tool)
	}
	if len(chain) != 1 || chain[0] != "local/coder" {
		t.Fatalf("chain = %v", chain)
	}
}

func TestBuildDelegateTool_NilConfig(t *testing.T) {
	if _, _, err := buildDelegateTool(nil, nil, "coding", nil); err == nil {
		t.Fatal("nil config should fail loudly, not no-op")
	}
}

func TestBuildDelegateTool_UnknownRole(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	if _, _, err := buildDelegateTool(cfg, nil, "coding", nil); err == nil {
		t.Fatal("unknown role should error")
	}
}

func TestBuildDelegateTool_WithStreamSink(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"coding": {Name: "coder", Provider: "local"},
		},
	}
	var sink = func(string) {} // non-nil sink
	tool, _, err := buildDelegateTool(cfg, nil, "coding", sink)
	if err != nil {
		t.Fatalf("buildDelegateTool with sink: %v", err)
	}
	if tool == nil || tool.Spec().Name != "delegate_code" {
		t.Fatalf("expected delegate_code tool, got %+v", tool)
	}
}

func TestDelegateSystemFragment(t *testing.T) {
	if delegateSystemFragment(false, false) != "" || delegateSystemFragment(false, true) != "" {
		t.Fatal("fragment must be empty when delegation disabled, regardless of allowWrite")
	}
	withWrite := delegateSystemFragment(true, true)
	if !strings.Contains(withWrite, "delegate_code") || !strings.Contains(withWrite, "write_file") {
		t.Fatalf("write-enabled fragment should mention delegate_code and write_file: %q", withWrite)
	}
	noWrite := delegateSystemFragment(true, false)
	if !strings.Contains(noWrite, "delegate_code") {
		t.Fatalf("fragment should mention delegate_code: %q", noWrite)
	}
	if strings.Contains(noWrite, "write_file") || strings.Contains(noWrite, "edit_file") {
		t.Fatalf("write-disabled fragment must not instruct writing to disk: %q", noWrite)
	}
}

func TestEffectClassName(t *testing.T) {
	cases := []struct {
		in   agent.EffectClass
		want string
	}{
		{agent.Read, "read"},
		{agent.Read | agent.Write, "read|write"},
		{agent.Exec, "exec"},
		{agent.Network, "network"},
		{agent.Read | agent.Write | agent.Exec | agent.Network, "read|write|exec|network"},
		{agent.EffectClass(0), "none"},
	}
	for _, c := range cases {
		if got := effectClassName(c.in); got != c.want {
			t.Errorf("effectClassName(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
