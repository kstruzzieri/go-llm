package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// narrowRaw is the command every narrowing test plans; it never spawns.
const narrowRaw = `{"argv":["go","version"]}`

// seatbeltNet is a native policy that allows network, with the other fields
// set so a test can see the denied copy keep them.
var seatbeltNet = SandboxConfig{
	Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true, MemoryCapMB: 512, DropCaps: []string{"NET_RAW"},
}

// variantRunner records the policy of the backend it was bound into and
// counts its runs; it never spawns.
type variantRunner struct {
	cfg   SandboxConfig
	calls atomic.Int32
}

func (r *variantRunner) Run(context.Context, execSpec) (execResult, error) {
	r.calls.Add(1)
	return execResult{Stdout: []byte("ran")}, nil
}

// variantBuilds is a fake networkVariant.build: it binds a fresh
// variantRunner to the policy it is given, or fails with err.
type variantBuilds struct {
	mu     sync.Mutex
	err    error
	calls  int
	runner *variantRunner // the last backend built
}

func (b *variantBuilds) build(cfg SandboxConfig) (resolvedExecBackend, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.err != nil {
		return resolvedExecBackend{}, b.err
	}
	normalized, err := normalizeSandboxConfig(cfg)
	if err != nil {
		return resolvedExecBackend{}, err
	}
	b.runner = &variantRunner{cfg: normalized}
	return bindExecBackend(hostBackend{commandRunner: b.runner, backgroundStarter: starterOf()}, normalized), nil
}

func (b *variantBuilds) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// deniedCalls is the number of runs on the denied backend, 0 if none was built.
func (b *variantBuilds) deniedCalls() int32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runner == nil {
		return 0
	}
	return b.runner.calls.Load()
}

// narrowFixture is a run_command over a stub native backend, stamped as the
// sandboxed constructors stamp one. Its variant builds through builds and
// keeps the default suspension predicate until inject is called.
type narrowFixture struct {
	rc        *RunCommand
	original  *variantRunner
	builds    *variantBuilds
	variant   *networkVariant
	suspended bool // the injected predicate's answer
}

func newNarrowFixture(t *testing.T, cfg SandboxConfig, scratch bool) *narrowFixture {
	t.Helper()
	normalized := mustNormalizeSandbox(t, cfg)
	f := &narrowFixture{original: &variantRunner{cfg: normalized}, builds: &variantBuilds{}}
	backend := bindExecBackend(hostBackend{commandRunner: f.original, backgroundStarter: starterOf()}, normalized)
	if backend.variant == nil {
		t.Fatalf("bindExecBackend(%+v) bound no network variant", normalized)
	}
	f.variant = backend.variant
	f.variant.build = f.builds.build
	root := canonicalTempRoot(t)
	f.rc = NewRunCommand(mustWorkspace(t, root), backend)
	f.rc.sandbox = backend.approval
	if scratch {
		rt, err := newScratchRuntime(root, ScratchConfig{Enabled: true}, nil, normalized.Runtime)
		if err != nil {
			t.Fatal(err)
		}
		rt.tempBase = t.TempDir()
		f.rc.scratchRT = rt
	}
	return f
}

// inject replaces the default predicate with f.suspended.
func (f *narrowFixture) inject() {
	f.variant.suspended = func(context.Context) bool { return f.suspended }
}

func (f *narrowFixture) plan(t *testing.T) agent.ToolPlan {
	t.Helper()
	plan, err := f.rc.Plan(context.Background(), json.RawMessage(narrowRaw))
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

func (f *narrowFixture) invoke(t *testing.T) agent.ToolResult {
	t.Helper()
	res, err := f.rc.Invoke(context.Background(), json.RawMessage(narrowRaw))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

// deniedApproval is the approval identity of cfg with network denied,
// derived without the variant under test.
func deniedApproval(t *testing.T, cfg SandboxConfig) sandboxApproval {
	t.Helper()
	cfg.AllowNetwork = false
	return approvalForSandbox(mustNormalizeSandbox(t, cfg))
}

func TestNetworkVariantNilForHostAndDenied(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     SandboxConfig
		variant bool
	}{
		{"zero host", SandboxConfig{}, false},
		{"explicit host", SandboxConfig{Runtime: SandboxRuntimeHost}, false},
		{"seatbelt denied", SandboxConfig{Runtime: SandboxRuntimeSeatbelt}, false},
		{"bwrap denied", SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 64}, false},
		{"seatbelt allowed", seatbeltNet, true},
		{"bwrap allowed", SandboxConfig{Runtime: SandboxRuntimeBwrap, AllowNetwork: true}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			normalized := mustNormalizeSandbox(t, tt.cfg)
			v := newNetworkVariant(normalized)
			if (v != nil) != tt.variant {
				t.Fatalf("newNetworkVariant(%+v) = %v, want variant %t", normalized, v, tt.variant)
			}
			bound := bindExecBackend(hostBackend{commandRunner: &markerRunner{}, backgroundStarter: starterOf()}, normalized)
			if (bound.variant != nil) != tt.variant {
				t.Fatalf("bindExecBackend(%+v).variant = %v, want variant %t", normalized, bound.variant, tt.variant)
			}
			if v == nil {
				return
			}
			want := normalized
			want.AllowNetwork = false
			if v.cfg.Runtime != want.Runtime || v.cfg.AllowNetwork || v.cfg.MemoryCapMB != want.MemoryCapMB ||
				v.cfg.CPULimit != want.CPULimit || !slices.Equal(v.cfg.DropCaps, want.DropCaps) {
				t.Fatalf("variant policy = %+v, want %+v", v.cfg, want)
			}
			if len(normalized.DropCaps) > 0 {
				normalized.DropCaps[0] = "MUTATED"
				if v.cfg.DropCaps[0] == "MUTATED" {
					t.Fatal("variant policy shares the original's DropCaps")
				}
			}
			if v.denied != nil {
				t.Fatal("variant built its backend at construction")
			}
		})
	}
}

func TestNetworkVariantNotBuiltWhenClean(t *testing.T) {
	f := newNarrowFixture(t, seatbeltNet, false) // default predicate: no run, so not suspended
	got, err := f.variant.forContext(context.Background())
	if got != nil || err != nil {
		t.Fatalf("forContext outside a run = (%v, %v), want (nil, nil)", got, err)
	}
	f.plan(t)
	if res := f.invoke(t); res.IsError {
		t.Fatalf("Invoke: %s", res.Content)
	}
	if n := f.builds.count(); n != 0 {
		t.Fatalf("backend constructor called %d times while clean", n)
	}
	if n := f.original.calls.Load(); n != 1 {
		t.Fatalf("original runner calls = %d, want 1", n)
	}
	var nilVariant *networkVariant
	if got, err := nilVariant.forContext(context.Background()); got != nil || err != nil {
		t.Fatalf("nil variant forContext = (%v, %v), want (nil, nil)", got, err)
	}
}

// TestNetworkVariantBuildsOnceAndRetriesAfterFailure pins the cache: a failed
// build is not cached and the next call tries again, and concurrent callers
// share one successful build.
func TestNetworkVariantBuildsOnceAndRetriesAfterFailure(t *testing.T) {
	f := newNarrowFixture(t, seatbeltNet, false)
	f.inject()
	f.suspended = true
	f.builds.err = errors.New("probe failed")
	if got, err := f.variant.forContext(context.Background()); got != nil || err == nil || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("failed build = (%v, %v), want the build error", got, err)
	}
	f.builds.mu.Lock()
	f.builds.err = nil
	f.builds.mu.Unlock()

	const callers = 8
	got := make([]*resolvedExecBackend, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, err := f.variant.forContext(context.Background())
			if err != nil {
				t.Errorf("forContext: %v", err)
			}
			got[i] = b
		}()
	}
	wg.Wait()
	if n := f.builds.count(); n != 2 {
		t.Fatalf("backend constructor calls = %d, want 2 (one failure, one success)", n)
	}
	for i, b := range got {
		if b == nil || b != got[0] {
			t.Fatalf("caller %d got backend %p, want the one cached %p", i, b, got[0])
		}
	}
	if got[0].sandbox.AllowNetwork {
		t.Fatal("cached backend allows network")
	}
}

func TestRunCommandPlanSelectsDeniedWhenSuspended(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cfg   SandboxConfig
		label string
	}{
		{"seatbelt", seatbeltNet, "network=denied"},
		{"bwrap", SandboxConfig{Runtime: SandboxRuntimeBwrap, AllowNetwork: true, MemoryCapMB: 512, DropCaps: []string{"NET_RAW"}}, "network=ip-denied"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newNarrowFixture(t, tt.cfg, false)
			f.inject()
			f.suspended = true
			plan := f.plan(t)
			want := deniedApproval(t, tt.cfg)
			if !strings.Contains(plan.Preview, "  sandbox: "+want.preview+"\n") ||
				!strings.Contains(want.preview, " "+tt.label+" ") || strings.Contains(plan.Preview, "network=allowed") {
				t.Fatalf("preview does not show the denied policy %q:\n%s", want.preview, plan.Preview)
			}
			if !strings.HasPrefix(plan.ApprovalKey, execApprovalKeyPrefix+want.keyComponent) {
				t.Fatalf("key = %q, want the denied digest %q", plan.ApprovalKey, want.keyComponent)
			}
			if res := f.invoke(t); res.IsError {
				t.Fatalf("Invoke: %s", res.Content)
			}
			if n := f.original.calls.Load(); n != 0 {
				t.Fatalf("original runner ran %d times", n)
			}
			if n := f.builds.deniedCalls(); n != 1 {
				t.Fatalf("denied runner calls = %d, want 1", n)
			}
			ran := f.builds.runner.cfg
			if ran.AllowNetwork || ran.Runtime != tt.cfg.Runtime || ran.MemoryCapMB != 512 || !slices.Equal(ran.DropCaps, []string{"NET_RAW"}) {
				t.Fatalf("runner policy = %+v, want the original with AllowNetwork=false", ran)
			}
		})
	}
}

func TestRunCommandCleanKeepsOriginalBackend(t *testing.T) {
	f := newNarrowFixture(t, seatbeltNet, false)
	f.inject() // suspended stays false
	backend := f.rc.runner.(resolvedExecBackend)
	backend.variant = nil
	baseline := NewRunCommand(f.rc.ws, backend)
	baseline.sandbox = f.rc.sandbox
	want, err := baseline.Plan(context.Background(), json.RawMessage(narrowRaw))
	if err != nil {
		t.Fatal(err)
	}
	got := f.plan(t)
	if got.Preview != want.Preview || got.ApprovalKey != want.ApprovalKey {
		t.Fatalf("clean plan changed:\n got %q %q\nwant %q %q", got.ApprovalKey, got.Preview, want.ApprovalKey, want.Preview)
	}
	if !strings.HasPrefix(got.ApprovalKey, execApprovalKeyPrefix+backend.approval.keyComponent) ||
		!strings.Contains(got.Preview, "network=allowed") {
		t.Fatalf("clean plan does not carry the original policy: %q\n%s", got.ApprovalKey, got.Preview)
	}
	if res := f.invoke(t); res.IsError {
		t.Fatalf("Invoke: %s", res.Content)
	}
	if n := f.original.calls.Load(); n != 1 {
		t.Fatalf("original runner calls = %d, want 1", n)
	}
	if n := f.builds.count(); n != 0 {
		t.Fatalf("backend constructor called %d times while clean", n)
	}
}

func TestRunCommandScratchUsesNarrowedBackend(t *testing.T) {
	f := newNarrowFixture(t, seatbeltNet, true)
	f.inject()
	f.suspended = true
	plan := f.plan(t)
	if !strings.Contains(plan.Preview, "  sandbox: "+deniedApproval(t, seatbeltNet).preview+"\n") ||
		!strings.Contains(plan.Preview, "  scratch: ") {
		t.Fatalf("scratched preview does not show the denied policy:\n%s", plan.Preview)
	}
	res := f.invoke(t)
	if res.IsError || !strings.HasPrefix(res.Content, "scratch: id=") {
		t.Fatalf("scratched Invoke = %+v", res)
	}
	if n := f.original.calls.Load(); n != 0 {
		t.Fatalf("original runner ran %d times", n)
	}
	if n := f.builds.deniedCalls(); n != 1 || f.builds.runner.cfg.AllowNetwork {
		t.Fatalf("denied runner calls = %d (policy %+v), want 1 with network denied", n, f.builds.runner.cfg)
	}
}

func TestRunCommandVariantFailureFailsClosed(t *testing.T) {
	for _, scratch := range []bool{false, true} {
		t.Run(fmt.Sprintf("scratch=%t", scratch), func(t *testing.T) {
			f := newNarrowFixture(t, seatbeltNet, scratch)
			f.inject()
			f.suspended = true
			f.builds.err = errors.New("probe failed")
			plan, err := f.rc.Plan(context.Background(), json.RawMessage(narrowRaw))
			if err == nil || !strings.Contains(err.Error(), "probe failed") {
				t.Fatalf("Plan error = %v, want the build failure", err)
			}
			if plan.ApprovalKey != "" || plan.Preview != "" {
				t.Fatalf("failed Plan offered an approval: %q\n%s", plan.ApprovalKey, plan.Preview)
			}
			if res := f.invoke(t); !res.IsError || !strings.Contains(res.Content, "exec preview missing") {
				t.Fatalf("Invoke after a failed Plan = %+v", res)
			}
			if n := f.original.calls.Load(); n != 0 {
				t.Fatalf("original runner ran %d times after the variant failed", n)
			}
		})
	}
}

func TestRunCommandVariantStateMismatchInterleaved(t *testing.T) {
	for _, scratch := range []bool{false, true} {
		for _, tt := range []struct {
			name           string
			plan, invokeIn bool
		}{
			{"approved denied, invoked clean", true, false},
			{"approved original, invoked suspended", false, true},
		} {
			t.Run(fmt.Sprintf("%s/scratch=%t", tt.name, scratch), func(t *testing.T) {
				f := newNarrowFixture(t, seatbeltNet, scratch)
				f.inject()
				f.suspended = tt.plan
				f.plan(t)
				f.suspended = tt.invokeIn
				res := f.invoke(t)
				if !res.IsError || res.Content != "approval state changed; retry" {
					t.Fatalf("Invoke = %+v, want the state-change refusal", res)
				}
				if n := f.original.calls.Load(); n != 0 {
					t.Fatalf("original runner ran %d times", n)
				}
				if n := f.builds.deniedCalls(); n != 0 {
					t.Fatalf("denied runner ran %d times", n)
				}
			})
		}
	}
}

// freshYes answers every call, fresh or not, with !deny and records the
// previews it was shown. onApprove runs inside a reusable-path Approve.
type freshYes struct {
	deny      bool
	fresh     int
	previews  []string
	onApprove func(ctx context.Context)
}

func (a *freshYes) Approve(ctx context.Context, _ provider.ToolCall, preview string) (bool, error) {
	a.previews = append(a.previews, preview)
	if a.onApprove != nil {
		a.onApprove(ctx)
	}
	return !a.deny, nil
}

func (a *freshYes) ApproveFresh(_ context.Context, _ provider.ToolCall, preview string, _ bool, _ agent.RiskReport, _ agent.Exposure) (bool, error) {
	a.fresh++
	a.previews = append(a.previews, preview)
	return !a.deny, nil
}

var _ agent.FreshApprover = (*freshYes)(nil)

// planCapture records the plan its run_command returns inside a run and
// whether approvals were suspended when it planned.
type planCapture struct {
	*RunCommand
	plan      agent.ToolPlan
	suspended bool
}

func (p *planCapture) Plan(ctx context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	p.suspended = agent.ApprovalsSuspended(ctx)
	plan, err := p.RunCommand.Plan(ctx, raw)
	p.plan = plan
	return plan, err
}

// foreignFetch is a read-only tool whose output is foreign content.
type foreignFetch struct{}

func (foreignFetch) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "fetch", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (foreignFetch) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}
func (foreignFetch) Origin() agent.Origin { return agent.OriginForeign }
func (foreignFetch) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "page"}, nil
}

func exposedPrior() *agent.Exposure {
	return &agent.Exposure{Recorded: true, Foreign: true, First: "tool:stub"}
}

func callScript(name, args string) []agent.ModelResult {
	return []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: name + "-1", Type: "function",
			Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}
}

// runStrictCommand runs one strict orchestrator whose scripted model calls
// run_command once, from prior, and returns the run's single tool record and
// the run_command result the model saw.
func runStrictCommand(t *testing.T, tool agent.Tool, prior *agent.Exposure, ap agent.Approver) (agent.ToolCallRecord, string) {
	t.Helper()
	res, err := agent.New(&proposalRunCaller{responses: callScript("run_command", narrowRaw)}, agent.ContextManager{}).Run(
		context.Background(), agent.Request{
			Goal: "run it", Tools: []agent.Tool{tool}, Approver: ap,
			StrictApproval: true, PriorExposure: prior,
		}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", res.ToolCalls)
	}
	var seen string
	for _, m := range res.Messages {
		if m.Role == "tool" {
			seen = m.Content
		}
	}
	return res.ToolCalls[0], seen
}

func TestHostPreviewUnchangedWhenSuspended(t *testing.T) {
	root := canonicalTempRoot(t)
	m := NewBackgroundManager()
	t.Cleanup(m.Shutdown)
	raw, err := NewExecTools(root)
	if err != nil {
		t.Fatal(err)
	}
	bg, err := NewExecToolsWithBackground(root, m)
	if err != nil {
		t.Fatal(err)
	}
	for name, tool := range map[string]agent.Tool{"raw runner": raw[0], "host backend": bg[0]} {
		t.Run(name, func(t *testing.T) {
			rc := tool.(*RunCommand)
			clean, err := rc.Plan(context.Background(), json.RawMessage(narrowRaw))
			if err != nil {
				t.Fatal(err)
			}
			capture := &planCapture{RunCommand: rc}
			// The approver says no, so nothing spawns.
			runStrictCommand(t, capture, exposedPrior(), &freshYes{deny: true})
			if !capture.suspended {
				t.Fatal("run_command did not plan while approvals were suspended")
			}
			if capture.plan.Preview != clean.Preview || capture.plan.ApprovalKey != clean.ApprovalKey {
				t.Fatalf("suspended host plan changed:\n got %q %q\nwant %q %q",
					capture.plan.ApprovalKey, capture.plan.Preview, clean.ApprovalKey, clean.Preview)
			}
			if strings.Contains(clean.Preview, "sandbox:") {
				t.Fatalf("host preview has a sandbox line:\n%s", clean.Preview)
			}
		})
	}
}

// TestRunCommandVariantRealRunForeground and its scratch twin drive real
// strict orchestrators with the default suspension predicate.
func TestRunCommandVariantRealRunForeground(t *testing.T) { testVariantRealRun(t, false) }

func TestRunCommandVariantRealRunScratch(t *testing.T) { testVariantRealRun(t, true) }

func testVariantRealRun(t *testing.T, scratch bool) {
	t.Run("exposed", func(t *testing.T) {
		f := newNarrowFixture(t, seatbeltNet, scratch)
		ap := &freshYes{}
		rec, seen := runStrictCommand(t, f.rc, exposedPrior(), ap)
		if rec.IsError || !rec.Invoked {
			t.Fatalf("run_command record = %+v, result %q", rec, seen)
		}
		if scratch && !strings.Contains(seen, "scratch: id=") {
			t.Fatalf("scratched result = %q", seen)
		}
		if ap.fresh != 1 || len(ap.previews) != 1 || !strings.Contains(ap.previews[0], " network=denied ") {
			t.Fatalf("approver asked fresh %d times, previews %q; want one fresh denied-network preview", ap.fresh, ap.previews)
		}
		if n := f.original.calls.Load(); n != 0 {
			t.Fatalf("original runner ran %d times", n)
		}
		if n := f.builds.deniedCalls(); n != 1 || f.builds.runner.cfg.AllowNetwork {
			t.Fatalf("denied runner calls = %d (policy %+v), want 1 with network denied", n, f.builds.runner.cfg)
		}
	})
	t.Run("clean", func(t *testing.T) {
		f := newNarrowFixture(t, seatbeltNet, scratch)
		ap := &freshYes{}
		rec, seen := runStrictCommand(t, f.rc, nil, ap)
		if rec.IsError || !rec.Invoked {
			t.Fatalf("run_command record = %+v, result %q", rec, seen)
		}
		if ap.fresh != 0 || len(ap.previews) != 1 || !strings.Contains(ap.previews[0], " network=allowed ") {
			t.Fatalf("approver asked fresh %d times, previews %q; want one reusable allowed-network preview", ap.fresh, ap.previews)
		}
		if n := f.original.calls.Load(); n != 1 {
			t.Fatalf("original runner calls = %d, want 1", n)
		}
		if n := f.builds.count(); n != 0 {
			t.Fatalf("backend constructor called %d times while clean", n)
		}
	})
}

// TestRunCommandVariantStateMismatchRealRun approves a call while the run is
// clean, from an approver that exposes the run by running a nested
// orchestrator whose tool returns foreign content: the call must be refused,
// not run on the original backend.
func TestRunCommandVariantStateMismatchRealRun(t *testing.T) {
	f := newNarrowFixture(t, seatbeltNet, false)
	var exposedInApproval bool
	ap := &freshYes{onApprove: func(ctx context.Context) {
		nested := agent.New(&proposalRunCaller{responses: callScript("fetch", `{}`)}, agent.ContextManager{})
		if _, err := nested.Run(ctx, agent.Request{Goal: "fetch", Tools: []agent.Tool{foreignFetch{}}}, nil); err != nil {
			t.Errorf("nested Run: %v", err)
		}
		exposedInApproval = agent.ApprovalsSuspended(ctx)
	}}
	rec, seen := runStrictCommand(t, f.rc, nil, ap)
	if !exposedInApproval {
		t.Fatal("the nested run did not expose the run")
	}
	if ap.fresh != 0 || len(ap.previews) != 1 || !strings.Contains(ap.previews[0], " network=allowed ") {
		t.Fatalf("approver asked fresh %d times, previews %q; want one clean approval", ap.fresh, ap.previews)
	}
	if !rec.IsError || !strings.Contains(seen, "approval state changed; retry") {
		t.Fatalf("run_command record = %+v, result %q; want the state-change refusal", rec, seen)
	}
	if n := f.original.calls.Load(); n != 0 {
		t.Fatalf("original runner ran %d times after the run was exposed", n)
	}
	if n := f.builds.deniedCalls(); n != 0 {
		t.Fatalf("denied runner ran %d times", n)
	}
}
