//go:build linux || darwin

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// stubVerifierBudget replaces buildVerifierFn with a stub that builds no
// verifier and records the budget it was given.
func stubVerifierBudget(t *testing.T) *[]time.Duration {
	t.Helper()
	orig := buildVerifierFn
	t.Cleanup(func() { buildVerifierFn = orig })
	var got []time.Duration
	buildVerifierFn = func(_ string, _ agenttools.SandboxConfig, budget time.Duration) (*agenttools.VerifyCommand, string, error) {
		got = append(got, budget)
		return nil, "", nil
	}
	return &got
}

// startCommandTimeout returns the effect timeout of the start_command tool in
// set, failing the row when there is none.
func startCommandTimeout(t *testing.T, label string, set []agent.Tool) time.Duration {
	t.Helper()
	for _, tool := range set {
		if tool.Spec().Name == "start_command" {
			return tool.Effect().Timeout
		}
	}
	t.Fatalf("%s: no start_command mounted", label)
	return 0
}

func TestCredentialScreenBudgetReachesTools(t *testing.T) {
	t.Run("late exec", func(t *testing.T) {
		sess := newMountSession(t, &captureCaller{answer: "ok"}, t.TempDir())
		sess.screenBudget = 45 * time.Second
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-exec")
		if !sess.allowExec {
			t.Fatal("late exec: exec not enabled")
		}
		if got := startCommandTimeout(t, "late exec", sess.tools); got != 55*time.Second {
			t.Fatalf("late exec: start_command timeout = %s, want 55s", got)
		}
	})
	t.Run("late verifier", func(t *testing.T) {
		got := stubVerifierBudget(t)
		sess := newMountSession(t, &captureCaller{answer: "ok"}, t.TempDir())
		sess.screenBudget = 45 * time.Second
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
		if len(*got) != 1 || (*got)[0] != 45*time.Second {
			t.Fatalf("late verifier: budgets received = %v, want [45s]", *got)
		}
	})
	t.Run("startup", func(t *testing.T) {
		got := stubVerifierBudget(t)
		configPath, root := startupPrepConfig(t, nil)
		sess, _ := runStartupPrep(t, configPath, root, "-credential-screen-budget", "45s", "-allow-exec", "-allow-write")
		if sess.screenBudget != 45*time.Second {
			t.Fatalf("startup: session budget = %s, want 45s", sess.screenBudget)
		}
		if tm := startCommandTimeout(t, "startup", sess.tools); tm != 55*time.Second {
			t.Fatalf("startup: start_command timeout = %s, want 55s", tm)
		}
		if len(*got) != 1 || (*got)[0] != 45*time.Second {
			t.Fatalf("startup: verifier budgets received = %v, want [45s]", *got)
		}
	})
}
