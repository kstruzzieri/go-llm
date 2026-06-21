package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
