package main

import (
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

func TestStepScopeGuard(t *testing.T) {
	plan := &agentflow.Plan{
		AllowedFiles: []string{"src/*"},
		BlockedFiles: []string{"src/secret.go"},
		Steps:        []agentflow.Step{{ID: "P1", Files: []string{"src/a.go", "src/secret.go"}}},
	}
	g := stepScopeGuard(plan, "P1")

	if err := g("src/a.go", true); err != nil {
		t.Fatalf("in-scope write should pass: %v", err)
	}
	if err := g("src/secret.go", true); err == nil {
		t.Fatal("blocked file write must fail")
	}
	if err := g("docs/x.md", true); err == nil {
		t.Fatal("out-of-scope write must fail")
	}
	if err := g("src/a.go", false); err != nil {
		t.Fatalf("in-scope read should pass: %v", err)
	}
	// .agent is denied for BOTH read and write regardless of step scope.
	if err := g(".agent/plan.lock.json", false); err == nil {
		t.Fatal(".agent read must be denied")
	}
	if err := g(".agent/x", true); err == nil {
		t.Fatal(".agent write must be denied")
	}
}
