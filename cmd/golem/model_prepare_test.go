package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
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

// assertStartupBudget pins the budget startup PUBLISHED (the live runtime
// snapshot every REPL reader consults) and the frozen copy the AgentFlow
// consumers keep. The two agree at startup and diverge only after a /model
// set, so asserting both here keeps the 5b parity statement exact.
func assertStartupBudget(t *testing.T, sess *replSession, want agent.Budget) {
	t.Helper()
	if got := sess.runtime.Budget(); got != want {
		t.Errorf("runtime budget = %+v, want %+v", got, want)
	}
	if sess.startupBudget != want {
		t.Errorf("frozen startup budget = %+v, want %+v", sess.startupBudget, want)
	}
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
	assertStartupBudget(t, sess, want)
	if opts := sess.startupModelOptions; opts.Think != nil || opts.ThinkEffort != "" {
		t.Errorf("startup thinking controls = %+v, want them unset", opts)
	}
	if wantChain := []string{"test/agent-model"}; !reflect.DeepEqual(sess.selection.chain, wantChain) {
		t.Errorf("selection chain = %v, want %v", sess.selection.chain, wantChain)
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
	assertStartupBudget(t, sess, want)
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
	assertStartupBudget(t, sess, want)
	if wantChain := []string{"test/agent-model"}; !reflect.DeepEqual(sess.selection.chain, wantChain) {
		t.Errorf("selection chain = %v, want %v", sess.selection.chain, wantChain)
	}
}

// prepModelKey builds a key in the "test" provider used by every unit test
// below, matching the selector shape "test/<model>".
func prepModelKey(model string) provider.ModelKey {
	return provider.ModelKey{Provider: "test", Model: model}
}

// prepProfile is a tool-capable profile with the given context window.
func prepProfile(model string, window int) *provider.ModelProfile {
	return &provider.ModelProfile{Key: prepModelKey(model), ContextWindow: window, Caps: agent.ModelCallCapabilities(true)}
}

// prepRegistry collects profiles into the shared ceiling/preflight fake.
func prepRegistry(profiles ...*provider.ModelProfile) inputCeilingTestRegistry {
	byKey := make(map[provider.ModelKey]*provider.ModelProfile, len(profiles))
	for _, p := range profiles {
		byKey[p.Key] = p
	}
	return inputCeilingTestRegistry{profiles: byKey}
}

// agentPlan is the chainPlan a /model set candidate produces: a strict chain
// under use case "agent".
func agentPlan(chain ...string) chainPlan {
	return chainPlan{chain: chain, useCase: modelSetUseCase}
}

func TestPrepareModelResolvesCeilingWarningsAndBudget(t *testing.T) {
	tests := []struct {
		name          string
		reg           capChecker
		plan          chainPlan
		explicit      int
		outputReserve int
		wantCeiling   int
		wantSource    inputCeilingSource
		wantLine      string
		wantWarnings  []string
	}{
		{
			name:        "explicit ceiling overrides model metadata",
			reg:         prepRegistry(prepProfile("large", 65_536)),
			plan:        agentPlan("test/large"),
			explicit:    4_096,
			wantCeiling: 4_096,
			wantSource:  inputCeilingExplicit,
			wantLine:    "input ceiling: 4096 tokens (explicit)",
		},
		{
			name:        "chain minimum follows the small eligible fallback",
			reg:         prepRegistry(prepProfile("large", 65_536), prepProfile("small", 32_768)),
			plan:        agentPlan("test/large", "test/small"),
			wantCeiling: 30_720,
			wantSource:  inputCeilingChainMinimum,
			wantLine:    "input ceiling: 30720 tokens (chain minimum)",
		},
		{
			name:          "explicit output reserve leaves the full window",
			reg:           prepRegistry(prepProfile("small", 32_768)),
			plan:          agentPlan("test/small"),
			outputReserve: 512,
			wantCeiling:   32_768,
			wantSource:    inputCeilingChainMinimum,
			wantLine:      "input ceiling: 32768 tokens (chain minimum)",
		},
		{
			name:        "missing context metadata labels the safe fallback",
			reg:         prepRegistry(prepProfile("unknown", 0)),
			plan:        agentPlan("test/unknown"),
			wantCeiling: 8_192,
			wantSource:  inputCeilingSafeFallback,
			wantLine:    "input ceiling: 8192 tokens (safe fallback; model context metadata unavailable)",
		},
		{
			name: "a fallback without tool_call warns and is excluded from the ceiling",
			reg: prepRegistry(prepProfile("large", 65_536), &provider.ModelProfile{
				Key: prepModelKey("notool"), ContextWindow: 4_096, Caps: provider.CapChat | provider.CapStream,
			}),
			plan:        agentPlan("test/large", "test/notool"),
			wantCeiling: 63_488,
			wantSource:  inputCeilingChainMinimum,
			wantLine:    "input ceiling: 63488 tokens (chain minimum)",
			wantWarnings: []string{
				`agent fallback "test/notool" is not tool-capable (chat|stream|tool_call); if it supports function calling, add "capabilities": ["chat","generate","stream","tool_call"] to the model entry for "test/notool" in models.json`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prep, err := prepareModel(context.Background(), modelPreparation{
				models:        tt.reg,
				plan:          tt.plan,
				inputCeiling:  tt.explicit,
				outputReserve: tt.outputReserve,
			})
			if err != nil {
				t.Fatalf("prepareModel: %v", err)
			}
			if prep.ceiling.ceiling != tt.wantCeiling || prep.ceiling.source != tt.wantSource {
				t.Errorf("ceiling = %+v, want ceiling=%d source=%q", prep.ceiling, tt.wantCeiling, tt.wantSource)
			}
			if got := prep.ceiling.line(); got != tt.wantLine {
				t.Errorf("ceiling line = %q, want %q", got, tt.wantLine)
			}
			if !reflect.DeepEqual(prep.warnings, tt.wantWarnings) {
				t.Errorf("warnings = %#v, want %#v", prep.warnings, tt.wantWarnings)
			}
			wantBudget := agent.Budget{InputCeiling: tt.wantCeiling, OutputReserve: tt.outputReserve}
			if prep.budget != wantBudget {
				t.Errorf("budget = %+v, want %+v", prep.budget, wantBudget)
			}
			if !reflect.DeepEqual(prep.plan, tt.plan) {
				t.Errorf("plan = %+v, want %+v", prep.plan, tt.plan)
			}
		})
	}
}

func TestPrepareModelDerivesThePressureBandFromTheWarnPercent(t *testing.T) {
	prep, err := prepareModel(context.Background(), modelPreparation{
		models:        prepRegistry(prepProfile("small", 32_768)),
		plan:          agentPlan("test/small"),
		outputReserve: 1_024,
		pressureWarn:  60,
	})
	if err != nil {
		t.Fatalf("prepareModel: %v", err)
	}
	want := agent.Budget{
		InputCeiling:  32_768,
		OutputReserve: 1_024,
		Pressure:      agent.PressureThresholds{Watch: 0.60, Warn: 0.60, Critical: 0.90},
	}
	if prep.budget != want {
		t.Fatalf("budget = %+v, want %+v", prep.budget, want)
	}
	// A disabled warning leaves the band unset, exactly as startup does.
	off, err := prepareModel(context.Background(), modelPreparation{
		models: prepRegistry(prepProfile("small", 32_768)), plan: agentPlan("test/small"),
	})
	if err != nil {
		t.Fatalf("prepareModel: %v", err)
	}
	if off.budget.Pressure != (agent.PressureThresholds{}) {
		t.Fatalf("pressure band = %+v with -pressure-warn 0, want the zero value", off.budget.Pressure)
	}
}

func TestPrepareModelResolvesTheCeilingUnderThePlansUseCase(t *testing.T) {
	// "reasoning" honors QualityCtxCeiling and reserves 4096 by default;
	// "agent" honors neither. Resolving under anything but the plan's use
	// case therefore produces a different number.
	reg := prepRegistry(&provider.ModelProfile{
		Key:               prepModelKey("yarn"),
		ContextWindow:     100_000,
		QualityCtxCeiling: 32_768,
		Caps:              agent.ModelCallCapabilities(true),
	})
	for _, tt := range []struct {
		useCase string
		want    int
	}{{"agent", 97_952}, {"reasoning", 28_672}} {
		t.Run(tt.useCase, func(t *testing.T) {
			prep, err := prepareModel(context.Background(), modelPreparation{
				models: reg, plan: chainPlan{chain: []string{"test/yarn"}, useCase: tt.useCase},
			})
			if err != nil {
				t.Fatalf("prepareModel: %v", err)
			}
			if prep.ceiling.ceiling != tt.want {
				t.Fatalf("%s ceiling = %d, want %d", tt.useCase, prep.ceiling.ceiling, tt.want)
			}
		})
	}
}

func TestPrepareModelPreservesPreflightErrorClassification(t *testing.T) {
	tests := []struct {
		name           string
		reg            capChecker
		chain          []string
		wantErr        string
		wantCapability bool
	}{
		{
			// Every entry resolved and none is tool-capable: a deterministic
			// config gap, which startup reports as caller misuse.
			name: "resolved capability gap stays a capability error",
			reg: prepRegistry(&provider.ModelProfile{
				Key: prepModelKey("notool"), ContextWindow: 32_768, Caps: provider.CapChat | provider.CapStream,
			}),
			chain: []string{"test/notool"},
			wantErr: "golem: tool-capability preflight failed:\n" +
				`agent fallback "test/notool" is not tool-capable (chat|stream|tool_call); if it supports function calling, add "capabilities": ["chat","generate","stream","tool_call"] to the model entry for "test/notool" in models.json`,
			wantCapability: true,
		},
		{
			// A degenerate bare id qualified against the sole provider
			// (#376 M1 rule 3) reaches the registry as "ollama//qwen3" and
			// fails to resolve. The message must name that exact selector.
			name:  "degenerate selector reports an actionable lookup failure",
			reg:   prepRegistry(prepProfile("qwen3", 32_768)),
			chain: []string{"ollama//qwen3"},
			wantErr: "golem: tool-capability preflight failed:\n" +
				`agent fallback "ollama//qwen3": provider lookup failed: model not found`,
			wantCapability: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prep, err := prepareModel(context.Background(), modelPreparation{models: tt.reg, plan: agentPlan(tt.chain...)})
			if err == nil {
				t.Fatalf("prepareModel = %+v, want an error", prep)
			}
			if err.Error() != tt.wantErr {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
			}
			if got := isPreflightCapabilityError(err); got != tt.wantCapability {
				t.Errorf("isPreflightCapabilityError = %v, want %v", got, tt.wantCapability)
			}
			// The per-entry diagnostics still reach the caller so startup can
			// fold them into the backend warnings before returning.
			if len(prep.warnings) != 1 {
				t.Errorf("warnings = %#v, want exactly one diagnostic", prep.warnings)
			}
			if prep.ceiling != (inputCeilingResolution{}) || prep.caller != nil {
				t.Errorf("failed preflight produced ceiling %+v and caller %v, want neither", prep.ceiling, prep.caller)
			}
		})
	}
}

func TestPrepareModelBuildsAStrictChainCallerForThePlan(t *testing.T) {
	plan := agentPlan("test/large", "test/small")
	prep, err := prepareModel(context.Background(), modelPreparation{
		models: prepRegistry(prepProfile("large", 65_536), prepProfile("small", 32_768)),
		plan:   plan,
	})
	if err != nil {
		t.Fatalf("prepareModel: %v", err)
	}
	caller, ok := prep.caller.(*chainModelCaller)
	if !ok {
		t.Fatalf("caller = %T, want *chainModelCaller", prep.caller)
	}
	if !reflect.DeepEqual(caller.chain, plan.chain) || caller.useCase != plan.useCase {
		t.Fatalf("caller = {chain:%v useCase:%q}, want {chain:%v useCase:%q}",
			caller.chain, caller.useCase, plan.chain, plan.useCase)
	}
	// Behavioral: the routing request pins the ordered chain strictly, so the
	// router appends no global-recommend tail.
	stub := errors.New("routing stopped")
	var got provider.RoutingRequest
	caller.route = func(_ context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
		got = rr
		return nil, stub
	}
	if _, err := caller.Chat(context.Background(), provider.ChatRequest{}, nil); !errors.Is(err, stub) {
		t.Fatalf("Chat error = %v, want the routing stub", err)
	}
	if !reflect.DeepEqual(got.PreferredChain, plan.chain) {
		t.Errorf("PreferredChain = %v, want %v", got.PreferredChain, plan.chain)
	}
	if !got.StrictChain {
		t.Error("StrictChain = false; a selected chain must never grow a recommendation tail")
	}
	if got.UseCase != modelSetUseCase {
		t.Errorf("UseCase = %q, want %q", got.UseCase, modelSetUseCase)
	}
}

// prepThinkProfile is a tool-capable profile that also declares a think mode,
// so one registry can serve both the preflight and the thinking gate.
func prepThinkProfile(model string, window int, mode provider.ThinkMode) *provider.ModelProfile {
	p := prepProfile(model, window)
	p.ThinkMode = mode
	return p
}

// prepUnrelatedOptions carries every non-thinking field ModelOptions has, so
// a candidate copy that touches anything but Think/ThinkEffort is visible.
func prepUnrelatedOptions() provider.ModelOptions {
	return provider.ModelOptions{
		Temperature:   provider.Ptr(0.35),
		TopP:          provider.Ptr(0.9),
		TopK:          provider.Ptr(40),
		NumPredict:    512,
		NumCtx:        16_384,
		Stop:          []string{"</done>"},
		RepeatPenalty: provider.Ptr(1.1),
	}
}

// assertUnrelatedOptionsPreserved fails when anything but the thinking fields
// moved away from prepUnrelatedOptions.
func assertUnrelatedOptionsPreserved(t *testing.T, got provider.ModelOptions) {
	t.Helper()
	want := prepUnrelatedOptions()
	got.Think, got.ThinkEffort = nil, ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unrelated options = %+v, want %+v", got, want)
	}
}

func TestThinkFlagValueInvertsThinkModelOptions(t *testing.T) {
	for _, value := range []string{"", "off", "on", "low", "medium", "high"} {
		t.Run("value="+value, func(t *testing.T) {
			if got := thinkFlagValue(thinkModelOptions(value)); got != value {
				t.Errorf("thinkFlagValue(thinkModelOptions(%q)) = %q, want %q", value, got, value)
			}
		})
	}

	// Explicit false wins over an effort hint, exactly as formatThinkOptions
	// renders it -- without parsing that display string.
	off := provider.ModelOptions{Think: provider.Ptr(false), ThinkEffort: "high"}
	if got := thinkFlagValue(off); got != "off" {
		t.Errorf("thinkFlagValue(%+v) = %q, want %q", off, got, "off")
	}
	if got := formatThinkOptions(off); got != "think: off" {
		t.Errorf("formatThinkOptions disagrees: %q", got)
	}
	// An effort with no explicit toggle still inverts to that effort.
	if got := thinkFlagValue(provider.ModelOptions{ThinkEffort: "medium"}); got != "medium" {
		t.Errorf("thinkFlagValue(effort-only) = %q, want %q", got, "medium")
	}
	// Unrelated options never invent a thinking state.
	if got := thinkFlagValue(prepUnrelatedOptions()); got != "" {
		t.Errorf("thinkFlagValue(no thinking controls) = %q, want the empty default", got)
	}
}

func TestApplyThinkOptionsCopiesOnlyTheThinkingFields(t *testing.T) {
	current := prepUnrelatedOptions()
	current.Think = provider.Ptr(false)
	resolved := provider.ModelOptions{
		Think: provider.Ptr(true), ThinkEffort: "low",
		// Fields a whole-value copy would drag along.
		NumCtx: 999, Temperature: provider.Ptr(1.0),
	}
	got := applyThinkOptions(current, resolved)
	if got.Think == nil || !*got.Think || got.ThinkEffort != "low" {
		t.Fatalf("thinking fields = %+v, want Think=true effort=low", got)
	}
	assertUnrelatedOptionsPreserved(t, got)
}

func TestPrepareModelRoundTripsAcceptedThinkingOntoTheCandidate(t *testing.T) {
	reg := prepRegistry(prepThinkProfile("toggle", 32_768, provider.ThinkToggle))
	plan := agentPlan("test/toggle")
	for _, value := range []string{"", "off", "on", "low", "medium", "high"} {
		t.Run("accepted="+value, func(t *testing.T) {
			accepted := applyThinkOptions(prepUnrelatedOptions(), thinkModelOptions(value))
			prep, err := prepareModel(context.Background(), modelPreparation{
				models: reg, plan: plan, think: thinkFlagValue(accepted),
			})
			if err != nil {
				t.Fatalf("prepareModel: %v", err)
			}
			if prep.thinkNotice != "" {
				t.Errorf("notice = %q, want none for a thinking-capable chain", prep.thinkNotice)
			}
			candidate := applyThinkOptions(accepted, prep.thinkOpts)
			if got := thinkFlagValue(candidate); got != value {
				t.Errorf("round-tripped state = %q, want %q", got, value)
			}
			assertUnrelatedOptionsPreserved(t, candidate)
		})
	}
}

func TestPrepareModelClearsThinkingForAnAllThinkNoneCandidate(t *testing.T) {
	prep, err := prepareModel(context.Background(), modelPreparation{
		models: prepRegistry(
			prepThinkProfile("none-a", 32_768, provider.ThinkNone),
			prepThinkProfile("none-b", 65_536, provider.ThinkNone),
		),
		plan:  agentPlan("test/none-a", "test/none-b"),
		think: "high",
	})
	if err != nil {
		t.Fatalf("prepareModel: %v", err)
	}
	want := "think: model test/none-a does not support thinking; -think ignored"
	if prep.thinkNotice != want {
		t.Errorf("notice = %q, want %q", prep.thinkNotice, want)
	}
	if prep.thinkOpts.Think != nil || prep.thinkOpts.ThinkEffort != "" {
		t.Errorf("resolved options = %+v, want the thinking controls cleared", prep.thinkOpts)
	}
	// Applying the cleared resolution onto a candidate that HAD thinking on
	// clears it there too, and leaves everything else alone.
	accepted := applyThinkOptions(prepUnrelatedOptions(), thinkModelOptions("high"))
	candidate := applyThinkOptions(accepted, prep.thinkOpts)
	if candidate.Think != nil || candidate.ThinkEffort != "" {
		t.Errorf("candidate = %+v, want the thinking controls cleared", candidate)
	}
	assertUnrelatedOptionsPreserved(t, candidate)
}

func TestPrepareModelKeepsThinkingOnAMixedCandidateChain(t *testing.T) {
	// One entry without thinking and one with: routing may still land on the
	// thinking model, so the gate fails open and prints nothing.
	prep, err := prepareModel(context.Background(), modelPreparation{
		models: prepRegistry(
			prepThinkProfile("none", 32_768, provider.ThinkNone),
			prepThinkProfile("toggle", 65_536, provider.ThinkToggle),
		),
		plan:  agentPlan("test/none", "test/toggle"),
		think: "high",
	})
	if err != nil {
		t.Fatalf("prepareModel: %v", err)
	}
	if prep.thinkNotice != "" {
		t.Errorf("notice = %q, want none for a mixed chain", prep.thinkNotice)
	}
	if prep.thinkOpts.Think == nil || !*prep.thinkOpts.Think || prep.thinkOpts.ThinkEffort != "high" {
		t.Errorf("resolved options = %+v, want Think=true effort=high", prep.thinkOpts)
	}
}

func TestPrepareModelNeverResurrectsARejectedThinkValue(t *testing.T) {
	// Startup asked for -think high against an all-ThinkNone chain, so the
	// value was never accepted: the runtime's options carry no thinking.
	startupReg := prepRegistry(prepThinkProfile("none", 32_768, provider.ThinkNone))
	startup, err := prepareModel(context.Background(), modelPreparation{
		models: startupReg, plan: agentPlan("test/none"), think: "high",
	})
	if err != nil {
		t.Fatalf("startup prepareModel: %v", err)
	}
	accepted := applyThinkOptions(prepUnrelatedOptions(), startup.thinkOpts)

	// Switching to a chain that DOES support thinking must re-resolve from
	// those accepted options, not from the stale flag.
	candidatePrep, err := prepareModel(context.Background(), modelPreparation{
		models: prepRegistry(prepThinkProfile("toggle", 65_536, provider.ThinkToggle)),
		plan:   agentPlan("test/toggle"),
		think:  thinkFlagValue(accepted),
	})
	if err != nil {
		t.Fatalf("candidate prepareModel: %v", err)
	}
	if candidatePrep.thinkOpts.Think != nil || candidatePrep.thinkOpts.ThinkEffort != "" {
		t.Fatalf("candidate options = %+v, want no thinking: \"high\" was never accepted", candidatePrep.thinkOpts)
	}
	candidate := applyThinkOptions(accepted, candidatePrep.thinkOpts)
	if got := thinkFlagValue(candidate); got != "" {
		t.Errorf("candidate thinking state = %q, want the empty default", got)
	}
	assertUnrelatedOptionsPreserved(t, candidate)
}

// writeFallbackWarningConfig writes a config whose agent role falls back to a
// model the provider lists but that declares no tool_call. Preflight passes
// (the head is capable) and emits exactly one per-entry warning, which is the
// only startup-observable proof that the shared preparation's diagnostics are
// folded into the startup notices.
func writeFallbackWarningConfig(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"},{"id":"weak-model"}]}`)
	}))
	t.Cleanup(server.Close)
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"test": {"base_url": %q, "api_format": "openai-compat", "timeout": "2s"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "stream", "tool_call"], "fallbacks": ["weak"]},
    "weak": {"name": "weak-model", "provider": "test", "type": "dense", "context_window": 65536,
      "capabilities": ["chat", "stream"]}
  },
  "defaults": {"agent": "agent"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, t.TempDir()
}

func TestStartupModelPreparationFoldsPreflightWarningsIntoTheNotices(t *testing.T) {
	configPath, root := writeFallbackWarningConfig(t)
	sess, stderrText := runStartupPrep(t, configPath, root, "-no-rag")

	want := `warning: agent fallback "test/weak-model" is not tool-capable (chat|stream|tool_call); ` +
		`if it supports function calling, add "capabilities": ["chat","generate","stream","tool_call"] ` +
		`to the model entry for "test/weak-model" in models.json`
	if !strings.Contains(stderrText, want) {
		t.Errorf("stderr missing the preflight warning\nwant: %s\ngot:\n%s", want, stderrText)
	}
	// The incapable fallback is excluded from the ceiling, so the 32768 head
	// sets it rather than the larger 65536 fallback.
	if !strings.Contains(stderrText, "input ceiling: 30720 tokens (chain minimum)") {
		t.Errorf("stderr missing the derived ceiling line:\n%s", stderrText)
	}
	if wantChain := []string{"test/agent-model", "test/weak-model"}; !reflect.DeepEqual(sess.selection.chain, wantChain) {
		t.Errorf("selection chain = %v, want %v", sess.selection.chain, wantChain)
	}
}
