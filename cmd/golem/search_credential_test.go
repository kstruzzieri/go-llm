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

const allowedLine = "TOKEN=allowed-627"

// TestProductionSearchSkipsCredentialFiles (#627 T12): the search tool that
// buildTools returns, which the startup orchestrator and dispatch children
// share, skips .env while still answering from an allowed file, so the
// missing sentinel is not vacuous.
func TestProductionSearchSkipsCredentialFiles(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(allowedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tools, err := buildTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var search agent.Tool
	for _, tool := range tools {
		if tool.Spec().Name == "search" {
			search = tool
		}
	}
	if search == nil {
		t.Fatal("buildTools returned no search tool")
	}
	out, err := search.Invoke(context.Background(), json.RawMessage(`{"pattern":"TOKEN="}`))
	if err != nil || out.IsError {
		t.Fatalf("search = %+v, %v", out, err)
	}
	if !strings.Contains(out.Content, "notes.txt:1: "+allowedLine) || strings.Contains(out.Content, envSentinelToken) {
		t.Fatalf("search = %q, want the allowed line and no .env line", out.Content)
	}
}

// TestDefaultGuardsPath_DispatchChildSearch (#627 T12): a production dispatch
// child's search skips .env. dispatchOnce also fails if the sentinel reaches
// the parent-facing envelope.
func TestDefaultGuardsPath_DispatchChildSearch(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(allowedLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	child := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		routed(toolStep("s1", "search", `{"pattern":"TOKEN="}`)),
		routed(answerStep("done")),
	}}}
	out, env := dispatchOnce(t, child, root)
	if out.IsError || env.Results[0].Error != "" {
		t.Fatalf("dispatch = %s", out.Content)
	}
	obs := toolObservation(t, child.reqs[len(child.reqs)-1].Messages, "s1")
	if !strings.Contains(obs.Content, "notes.txt:1: "+allowedLine) || strings.Contains(obs.Content, envSentinelToken) {
		t.Fatalf("child search observation = %q, want the allowed line and no .env line", obs.Content)
	}
}
