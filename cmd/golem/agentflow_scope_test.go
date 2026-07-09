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

func TestStepScopeGuardAgentCaseInsensitive(t *testing.T) {
	plan := &agentflow.Plan{
		// Permissive allowed_files so only the .agent rule can deny a write.
		AllowedFiles: []string{"**"},
		Steps:        []agentflow.Step{{ID: "P1", Files: []string{".AGENT/proof-pack.json", ".Agent/x", ".agent", ".agent/receipts"}}},
	}
	g := stepScopeGuard(plan, "P1")

	// Proof state must stay opaque on a case-insensitive filesystem (macOS APFS):
	// deny for BOTH read and write regardless of the case the model supplies.
	denied := []string{".AGENT/proof-pack.json", ".Agent/x", ".agent", ".agent/receipts"}
	for _, rel := range denied {
		if err := g(rel, false); err == nil {
			t.Errorf("%q read must be denied by the .agent rule", rel)
		}
		if err := g(rel, true); err == nil {
			t.Errorf("%q write must be denied by the .agent rule", rel)
		}
	}

	// An unrelated top-level name that merely shares the .agent prefix is NOT
	// proof state: the read must pass (preserving the original prefix semantics).
	if err := g(".agentfoo", false); err != nil {
		t.Errorf(".agentfoo read must not be denied by the .agent rule: %v", err)
	}
}
