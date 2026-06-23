package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
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

func TestBuildSystemPrompt(t *testing.T) {
	cases := []struct {
		name           string
		write, exec    bool
		mustContain    []string
		mustNotContain []string
	}{
		{"readonly", false, false,
			[]string{"do not", "run_command" /*not*/},
			nil},
	}
	_ = cases
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
