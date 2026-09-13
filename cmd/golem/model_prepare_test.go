package main

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// This file pins the POST-ADMISSION half of model preparation (#376 M4 steps
// 3-5): tool-capability preflight, thinking resolution, input-ceiling
// derivation, and the caller/budget the orchestrator factory is fed. Startup
// and the future /model set handler run the same sequence, so the parity
// tests below assert the startup wiring's observable outputs with literal
// values and the unit tests assert the shared helper reproduces them.

// startupPrepConfig writes the shared lifecycle config, optionally patching
// the agent model entry. patch is applied to the raw JSON so tests can add
// think_mode or drop context_window without a second config fixture.
func startupPrepConfig(t *testing.T, patch func(string) string) (string, string) {
	t.Helper()
	configPath, root := writeRunLifecycleConfig(t)
	if patch == nil {
		return configPath, root
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := patch(string(raw))
	if patched == string(raw) {
		t.Fatal("config patch changed nothing; the fixture drifted")
	}
	if err := os.WriteFile(configPath, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root
}

// runStartupPrep drives run() to the point where the replSession exists and
// returns the built session plus everything written to stderr. The session is
// captured rather than used: the run is stopped immediately afterwards, so no
// turn, no auto-index, and no model call happens.
func runStartupPrep(t *testing.T, configPath, root string, extra ...string) (*replSession, string) {
	t.Helper()
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after session ready")
	var captured *replSession
	args := append([]string{"-config", configPath, "-root", root,
		"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context", "-no-auto-index"}, extra...)
	err := run(args, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(sess *replSession) error {
			captured = sess
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run error = %v, want the session-ready stop; stderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if captured == nil {
		t.Fatal("afterSessionReady never ran")
	}
	return captured, readRunTestFile(t, stderr)
}

// defaultPressureBand is the band -pressure-warn 75 (the flag default)
// derives. Spelled literally so dropping the derivation is visible.
var defaultPressureBand = agent.PressureThresholds{Watch: 0.60, Warn: 0.75, Critical: 0.90}

func TestStartupModelPreparationDerivesCeilingBudgetAndThinkDefaults(t *testing.T) {
	configPath, root := startupPrepConfig(t, nil)
	sess, stderrText := runStartupPrep(t, configPath, root)

	if !strings.Contains(stderrText, "input ceiling: 30720 tokens (chain minimum)") {
		t.Errorf("stderr missing the derived ceiling line:\n%s", stderrText)
	}
	if strings.Contains(stderrText, "think:") {
		t.Errorf("unset -think printed a thinking notice:\n%s", stderrText)
	}
	if strings.Contains(stderrText, "agent fallback") {
		t.Errorf("a tool-capable chain produced a preflight warning:\n%s", stderrText)
	}
	want := agent.Budget{InputCeiling: 30_720, OutputReserve: 0, Pressure: defaultPressureBand}
	if sess.budget != want {
		t.Errorf("session budget = %+v, want %+v", sess.budget, want)
	}
	if opts := sess.startupModelOptions; opts.Think != nil || opts.ThinkEffort != "" {
		t.Errorf("startup thinking controls = %+v, want them unset", opts)
	}
	if wantChain := []string{"test/agent-model"}; !reflect.DeepEqual(sess.thinkChain, wantChain) {
		t.Errorf("think chain = %v, want %v", sess.thinkChain, wantChain)
	}
}

func TestStartupModelPreparationHonorsExplicitCeilingReserveAndPressure(t *testing.T) {
	// think_mode "none" on the only chain entry is the all-ThinkNone case:
	// the controls are cleared and the explanation printed once.
	configPath, root := startupPrepConfig(t, func(raw string) string {
		return strings.Replace(raw, `"type": "dense", "context_window": 32768`,
			`"type": "dense", "context_window": 32768, "think_mode": "none"`, 1)
	})
	sess, stderrText := runStartupPrep(t, configPath, root,
		"-input-ceiling", "4096", "-output-reserve", "512", "-pressure-warn", "60", "-think", "high")

	if !strings.Contains(stderrText, "input ceiling: 4096 tokens (explicit)") {
		t.Errorf("stderr missing the explicit ceiling line:\n%s", stderrText)
	}
	if want := "think: model test/agent-model does not support thinking; -think ignored"; !strings.Contains(stderrText, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrText)
	}
	want := agent.Budget{
		InputCeiling:  4_096,
		OutputReserve: 512,
		Pressure:      agent.PressureThresholds{Watch: 0.60, Warn: 0.60, Critical: 0.90},
	}
	if sess.budget != want {
		t.Errorf("session budget = %+v, want %+v", sess.budget, want)
	}
	if opts := sess.startupModelOptions; opts.Think != nil || opts.ThinkEffort != "" {
		t.Errorf("suppressed thinking left controls %+v, want them unset", opts)
	}
}

func TestStartupModelPreparationCarriesAcceptedThinkingToTheRuntime(t *testing.T) {
	configPath, root := startupPrepConfig(t, func(raw string) string {
		return strings.Replace(raw, `"type": "dense", "context_window": 32768`,
			`"type": "dense", "context_window": 32768, "think_mode": "toggle"`, 1)
	})
	sess, stderrText := runStartupPrep(t, configPath, root, "-think", "high")

	if strings.Contains(stderrText, "does not support thinking") {
		t.Errorf("a think_mode toggle chain was reported as unsupported:\n%s", stderrText)
	}
	if sess.startupModelOptions.Think == nil || !*sess.startupModelOptions.Think {
		t.Fatalf("startup model options = %+v, want Think=true", sess.startupModelOptions)
	}
	if sess.startupModelOptions.ThinkEffort != "high" {
		t.Errorf("ThinkEffort = %q, want %q", sess.startupModelOptions.ThinkEffort, "high")
	}
	if got := sess.runtime.ModelOptions(); got.ThinkEffort != "high" {
		t.Errorf("runtime model options = %+v, want ThinkEffort high", got)
	}
}

func TestStartupModelPreparationUsesThePlanningUseCaseInGoalMode(t *testing.T) {
	configPath, root := startupPrepConfig(t, nil)
	sess, stderrText := runStartupPrep(t, configPath, root, "-goal", "ship the thing")

	// The planning route falls through to defaults.agent; preparation runs
	// under use case "planning", which is what the notice names.
	if want := "planning route: using defaults.agent"; !strings.Contains(stderrText, want) {
		t.Errorf("stderr missing %q:\n%s", want, stderrText)
	}
	if !strings.Contains(stderrText, "input ceiling: 30720 tokens (chain minimum)") {
		t.Errorf("stderr missing the derived ceiling line:\n%s", stderrText)
	}
	want := agent.Budget{InputCeiling: 30_720, Pressure: defaultPressureBand}
	if sess.budget != want {
		t.Errorf("session budget = %+v, want %+v", sess.budget, want)
	}
	if wantChain := []string{"test/agent-model"}; !reflect.DeepEqual(sess.thinkChain, wantChain) {
		t.Errorf("think chain = %v, want %v", sess.thinkChain, wantChain)
	}
}
