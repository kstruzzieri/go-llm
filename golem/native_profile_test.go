package golem

// These tests live in package golem so they can replace validateSandbox, the
// one seam between a native session and this host's backend: with it stubbed,
// every native policy constructs on every OS. Tests that need tools built by a
// real native backend use nativePolicyOrSkip instead.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

var (
	seatbeltPolicy = agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}
	bwrapPolicy    = agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap}
)

// stubValidateSandbox makes every policy available for the rest of the test.
func stubValidateSandbox(t *testing.T) {
	t.Helper()
	saved := validateSandbox
	validateSandbox = func(agenttools.SandboxConfig, string) error { return nil }
	t.Cleanup(func() { validateSandbox = saved })
}

// nativePolicyOrSkip returns this OS's native policy when its backend works
// here. GO_LLM_REQUIRE_SEATBELT or GO_LLM_REQUIRE_BWRAP=1 turns an
// unavailable backend into a failure instead of a skip.
func nativePolicyOrSkip(t *testing.T) agenttools.SandboxConfig {
	t.Helper()
	cfg, require := seatbeltPolicy, "GO_LLM_REQUIRE_SEATBELT"
	switch runtime.GOOS {
	case "darwin":
	case "linux":
		cfg, require = bwrapPolicy, "GO_LLM_REQUIRE_BWRAP"
	default:
		t.Skip("no native backend on " + runtime.GOOS)
	}
	if err := agenttools.ValidateSandbox(cfg, t.TempDir()); err != nil {
		if os.Getenv(require) == "1" {
			t.Fatalf("%s=1 but %s is unavailable: %v", require, cfg.Runtime, err)
		}
		t.Skipf("%s unavailable on this host: %v", cfg.Runtime, err)
	}
	return cfg
}

// countingCaller answers every turn and counts how many model calls it got.
type countingCaller struct{ calls atomic.Int32 }

func (c *countingCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls.Add(1)
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

// opaqueVerifier is a host verifier: any agent.Verifier but the library slot.
type opaqueVerifier struct{}

func (opaqueVerifier) Verify(context.Context, agent.Approver) (string, error) { return "", nil }

// canonicalDir returns a fresh directory and its canonical spelling, derived
// from the standard library rather than from the code under test.
func canonicalDir(t *testing.T) (dir, canonical string) {
	t.Helper()
	dir = t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir, canonical
}

func mustSlot(t *testing.T, root string, policy agenttools.SandboxConfig) *agenttools.VerifierSlot {
	t.Helper()
	slot, err := agenttools.NewVerifierSlot(root, policy)
	if err != nil {
		t.Fatalf("NewVerifierSlot: %v", err)
	}
	return slot
}

// newNative builds a Trusted runtime over root with the given policy, tools and
// orchestrator options, closing it at cleanup.
func newNative(t *testing.T, root string, policy agenttools.SandboxConfig, caller agent.ModelCaller, tools []agent.Tool, orchOpts ...agent.Option) (*Runtime, error) {
	t.Helper()
	rt, err := New(context.Background(), Options{
		Root:         root,
		Profile:      ProfileTrusted,
		Sandbox:      policy,
		Tools:        tools,
		Orchestrator: agent.New(caller, agent.ContextManager{}, orchOpts...),
	})
	if err == nil {
		t.Cleanup(func() { closeTestRuntime(t, rt) })
	}
	return rt, err
}

// hostExecSet is the library's background exec set over the host backend.
func hostExecSet(t *testing.T, root string, opts agenttools.ExecToolsOptions) []agent.Tool {
	t.Helper()
	mgr := agenttools.NewBackgroundManager()
	t.Cleanup(mgr.Shutdown)
	set, err := agenttools.NewExecToolsWithBackgroundOptions(root, mgr, opts)
	if err != nil {
		t.Fatalf("NewExecToolsWithBackgroundOptions: %v", err)
	}
	return set
}

// nativeExecSet is the same set over a real native backend for policy.
func nativeExecSet(t *testing.T, root string, policy agenttools.SandboxConfig, scratch bool) []agent.Tool {
	t.Helper()
	mgr, err := agenttools.NewSandboxedBackgroundManager(policy)
	if err != nil {
		t.Fatalf("NewSandboxedBackgroundManager: %v", err)
	}
	t.Cleanup(mgr.Shutdown)
	var opts agenttools.ExecToolsOptions
	if scratch {
		opts.Scratch = agenttools.ScratchConfig{Enabled: true}
	}
	set, err := agenttools.NewExecToolsWithBackgroundOptions(root, mgr, opts)
	if err != nil {
		t.Fatalf("NewExecToolsWithBackgroundOptions: %v", err)
	}
	return set
}

func runNative(t *testing.T, rt *Runtime, runID string) ([]Event, error) {
	t.Helper()
	var events []Event
	_, err := rt.Run(context.Background(), Turn{RunID: runID, Message: "go"}, func(e Event) error {
		events = append(events, e)
		return nil
	})
	return events, err
}

var replacePaths = map[string]func(rt *Runtime, caller agent.ModelCaller, tools []agent.Tool) error{
	"Replace": func(rt *Runtime, _ agent.ModelCaller, tools []agent.Tool) error {
		return rt.Replace("", tools)
	},
	"ReplaceConfiguration": func(rt *Runtime, caller agent.ModelCaller, tools []agent.Tool) error {
		return rt.ReplaceConfiguration(context.Background(), Configuration{Tools: tools, Orchestrator: agent.New(caller, agent.ContextManager{})})
	},
}

func TestNativeRequiresTrusted(t *testing.T) {
	var probes atomic.Int32
	saved := validateSandbox
	validateSandbox = func(agenttools.SandboxConfig, string) error { probes.Add(1); return nil }
	t.Cleanup(func() { validateSandbox = saved })

	for _, policy := range []agenttools.SandboxConfig{seatbeltPolicy, bwrapPolicy} {
		_, err := New(context.Background(), Options{
			Root:         t.TempDir(),
			Sandbox:      policy, // Profile is unset: the zero value is Inspect.
			Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}),
		})
		if !errors.Is(err, ErrProfileIncompatible) {
			t.Fatalf("Inspect + %s = %v, want ErrProfileIncompatible", policy.Runtime, err)
		}
	}
	if n := probes.Load(); n != 0 {
		t.Fatalf("validateSandbox ran %d time(s) for a profile that cannot be native, want 0", n)
	}
}

func TestNativeAdmissionAllPaths(t *testing.T) {
	t.Run("host-bound exec tools", func(t *testing.T) {
		stubValidateSandbox(t)
		dir := t.TempDir()
		_, err := newNative(t, dir, seatbeltPolicy, &countingCaller{}, hostExecSet(t, dir, agenttools.ExecToolsOptions{}))
		if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), `"run_command" is not admitted in a native sandbox session`) {
			t.Fatalf("New = %v, want run_command rejected with ErrProfileIncompatible", err)
		}
		for _, path := range []string{"Replace", "ReplaceConfiguration"} {
			t.Run(path, func(t *testing.T) {
				dir := t.TempDir()
				caller := &countingCaller{}
				rt, err := newNative(t, dir, seatbeltPolicy, caller, nil)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				before := rt.EffectivePermissions().Tools
				err = replacePaths[path](rt, caller, hostExecSet(t, dir, agenttools.ExecToolsOptions{}))
				if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), `"run_command" is not admitted`) {
					t.Fatalf("%s = %v, want run_command rejected with ErrProfileIncompatible", path, err)
				}
				if after := rt.EffectivePermissions().Tools; !slices.Equal(after, before) {
					t.Fatalf("a rejected %s published %+v, want %+v", path, after, before)
				}
			})
		}
	})

	t.Run("opaque read tool", func(t *testing.T) {
		stubValidateSandbox(t)
		_, err := newNative(t, t.TempDir(), seatbeltPolicy, &countingCaller{}, []agent.Tool{&floorStub{name: "lookup", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}})
		if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), "is not admitted in a native sandbox session") {
			t.Fatalf("New = %v, want the opaque tool rejected", err)
		}
	})

	t.Run("dispatch child ceiling", func(t *testing.T) {
		stubValidateSandbox(t)
		dir := t.TempDir()
		reads, err := agenttools.NewFileTools(dir)
		if err != nil {
			t.Fatalf("NewFileTools: %v", err)
		}
		d, err := agenttools.NewDispatch(&countingCaller{}, agent.ContextManager{}, append(reads, agenttools.Retrieve{}), agenttools.DispatchLimits{})
		if err != nil {
			t.Fatalf("NewDispatch: %v", err)
		}
		_, err = New(context.Background(), Options{
			Root:          dir,
			Profile:       ProfileTrusted,
			Sandbox:       seatbeltPolicy,
			Tools:         []agent.Tool{d},
			ToolAllowlist: []string{"read_file", "search", "glob", "list", "dispatch"},
			Orchestrator:  agent.New(&countingCaller{}, agent.ContextManager{}),
		})
		if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), `child tool "retrieve" is outside the frozen allowlist`) {
			t.Fatalf("New = %v, want the dispatch child-ceiling rule to report retrieve", err)
		}
	})
}

// TestNativeBackendScratchFrozenAcrossReplacement runs golem's frozen-scratch
// and unwrapped-publication rules against command tools a real native backend
// built, through the real binder. Both native CI jobs select the
// TestNativeBackend prefix with GO_LLM_REQUIRE_SEATBELT or
// GO_LLM_REQUIRE_BWRAP=1, where an unavailable backend fails instead of
// skipping. TestNativeScratchFrozenAtNew pins the same rules on every OS over a
// stubbed binder.
func TestNativeBackendScratchFrozenAcrossReplacement(t *testing.T) {
	policy := nativePolicyOrSkip(t)
	for _, path := range []string{"Replace", "ReplaceConfiguration"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			caller := &countingCaller{}
			// New mounts no command tools, so the frozen scratch identity is none.
			rt, err := newNative(t, dir, policy, caller, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = replacePaths[path](rt, caller, nativeExecSet(t, dir, policy, true))
			if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), "scratch policy is frozen") {
				t.Fatalf("%s with a scratch set = %v, want the frozen scratch policy to refuse it", path, err)
			}
			// The same set without scratch is admissible, so the refusal was
			// the scratch change alone. It is published unwrapped.
			plain := nativeExecSet(t, dir, policy, false)
			if err := replacePaths[path](rt, caller, plain); err != nil {
				t.Fatalf("%s with the plain native set: %v", path, err)
			}
			rt.mu.Lock()
			published := rt.snap.tools[len(rt.fileTools):]
			rt.mu.Unlock()
			if !slices.Equal(published, plain) {
				t.Fatalf("published %v, want the caller's own tools %v unwrapped", published, plain)
			}
		})
	}

	// A session frozen with scratch refuses command tools without it.
	dir := t.TempDir()
	caller := &countingCaller{}
	rt, err := newNative(t, dir, policy, caller, nativeExecSet(t, dir, policy, true))
	if err != nil {
		t.Fatalf("New with a scratch set: %v", err)
	}
	if err := rt.Replace("", nativeExecSet(t, dir, policy, false)); !errors.Is(err, ErrProfileIncompatible) {
		t.Fatalf("Replace dropping scratch = %v, want ErrProfileIncompatible", err)
	}
	if err := rt.Replace("", nil); err != nil {
		t.Fatalf("Replace unmounting every command tool: %v", err)
	}
}

// stubBinder replaces checkSandboxBindings for the rest of the test: it admits
// every set, records the arguments golem passed, and returns *key.
type stubBinder struct {
	key     string // returned scratch identity
	gotKey  string // last scratchKey argument
	gotRoot string
	gotCfg  agenttools.SandboxConfig
}

func installStubBinder(t *testing.T) *stubBinder {
	t.Helper()
	b := &stubBinder{}
	saved := checkSandboxBindings
	checkSandboxBindings = func(_ []agent.Tool, cfg agenttools.SandboxConfig, root, scratchKey string) (string, error) {
		b.gotKey, b.gotRoot, b.gotCfg = scratchKey, root, cfg
		return b.key, nil
	}
	t.Cleanup(func() { checkSandboxBindings = saved })
	return b
}

// TestNativeScratchFrozenAtNew pins golem's own native rules on every OS, over
// a binder stub: New freezes the scratch identity the binder returns and hands
// it back on every replacement, a replacement whose identity differs (including
// from a frozen "") is refused and publishes nothing, and admitted extras are
// published unwrapped.
func TestNativeScratchFrozenAtNew(t *testing.T) {
	stubValidateSandbox(t)
	for _, path := range []string{"Replace", "ReplaceConfiguration"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct{ frozen, other string }{
				{frozen: "scr:frozen:", other: ""},
				{frozen: "", other: "scr:other:"},
			} {
				dir, root := canonicalDir(t)
				binder := installStubBinder(t)
				binder.key = tc.frozen
				caller := &countingCaller{}
				rt, err := newNative(t, dir, seatbeltPolicy, caller, nil)
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				if binder.gotKey != "" || binder.gotRoot != root || binder.gotCfg.Runtime != agenttools.SandboxRuntimeSeatbelt {
					t.Fatalf("New passed key %q, root %q, cfg %+v; want \"\", %q, seatbelt", binder.gotKey, binder.gotRoot, binder.gotCfg, root)
				}
				before := rt.EffectivePermissions()

				binder.key = tc.other
				err = replacePaths[path](rt, caller, nil)
				if !errors.Is(err, ErrProfileIncompatible) || !strings.Contains(err.Error(), "scratch policy is frozen") {
					t.Fatalf("frozen %q, %s returning %q = %v, want the frozen scratch policy to refuse it", tc.frozen, path, tc.other, err)
				}
				if binder.gotKey != tc.frozen {
					t.Fatalf("%s passed scratch key %q, want the frozen %q", path, binder.gotKey, tc.frozen)
				}
				if after := rt.EffectivePermissions(); !reflect.DeepEqual(after, before) {
					t.Fatalf("a refused %s published %+v, want %+v", path, after, before)
				}

				binder.key = tc.frozen
				opaque := &floorStub{name: "lookup", effect: agent.Effect{Class: agent.Exec, Approval: agent.ApprovalNever}}
				if err := replacePaths[path](rt, caller, []agent.Tool{opaque}); err != nil {
					t.Fatalf("frozen %q, %s returning it: %v", tc.frozen, path, err)
				}
				rt.mu.Lock()
				published := rt.snap.tools[len(rt.fileTools)]
				rt.mu.Unlock()
				if published != agent.Tool(opaque) {
					t.Fatalf("published %T, want the admitted tool itself, unwrapped", published)
				}
			}
		})
	}
}

func TestNativeVerifierBinding(t *testing.T) {
	stubValidateSandbox(t)
	dir, root := canonicalDir(t)
	otherRoot := t.TempDir()
	allowed := agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt, AllowNetwork: true}

	rejected := []struct {
		name     string
		verifier agent.Verifier
	}{
		{"slot bound to another root", mustSlot(t, otherRoot, seatbeltPolicy)},
		{"slot bound to another policy", mustSlot(t, root, allowed)},
		{"host slot", mustSlot(t, root, agenttools.SandboxConfig{})},
		{"typed-nil slot", (*agenttools.VerifierSlot)(nil)},
		{"opaque verifier", opaqueVerifier{}},
	}
	for _, tc := range rejected {
		t.Run("New/"+tc.name, func(t *testing.T) {
			_, err := newNative(t, dir, seatbeltPolicy, &countingCaller{}, nil, agent.WithVerifier(tc.verifier))
			if !errors.Is(err, ErrProfileIncompatible) {
				t.Fatalf("New = %v, want ErrProfileIncompatible", err)
			}
		})
	}

	t.Run("bound slot", func(t *testing.T) {
		caller := &countingCaller{}
		rt, err := newNative(t, dir, seatbeltPolicy, caller, nil, agent.WithVerifier(mustSlot(t, dir, seatbeltPolicy)))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := runNative(t, rt, "bound"); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if n := caller.calls.Load(); n != 1 {
			t.Fatalf("model calls = %d, want 1", n)
		}
	})

	t.Run("swap after admission", func(t *testing.T) {
		caller := &countingCaller{}
		orch := agent.New(caller, agent.ContextManager{}, agent.WithVerifier(mustSlot(t, dir, seatbeltPolicy)))
		rt, err := New(context.Background(), Options{Root: dir, Profile: ProfileTrusted, Sandbox: seatbeltPolicy, Orchestrator: orch})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { closeTestRuntime(t, rt) })
		agent.WithVerifier(mustSlot(t, otherRoot, seatbeltPolicy))(orch)
		events, err := runNative(t, rt, "swapped")
		if !errors.Is(err, ErrProfileIncompatible) {
			t.Fatalf("Run after the swap = %v, want ErrProfileIncompatible", err)
		}
		if got, want := eventTypeNames(events), []string{"run.started", "run.failed"}; !slices.Equal(got, want) {
			t.Fatalf("events = %v, want %v", got, want)
		}
		if n := caller.calls.Load(); n != 0 {
			t.Fatalf("model calls = %d, want 0", n)
		}
	})

	t.Run("ReplaceConfiguration", func(t *testing.T) {
		caller := &countingCaller{}
		rt, err := newNative(t, dir, seatbeltPolicy, caller, nil)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		rt.mu.Lock()
		snap := rt.snap
		rt.mu.Unlock()
		before := rt.EffectivePermissions()
		mismatched := agent.New(caller, agent.ContextManager{}, agent.WithVerifier(mustSlot(t, otherRoot, seatbeltPolicy)))
		if err := rt.ReplaceConfiguration(context.Background(), Configuration{Orchestrator: mismatched}); !errors.Is(err, ErrProfileIncompatible) {
			t.Fatalf("ReplaceConfiguration with a mismatched slot = %v, want ErrProfileIncompatible", err)
		}
		rt.mu.Lock()
		unchanged := rt.snap == snap
		rt.mu.Unlock()
		if after := rt.EffectivePermissions(); !unchanged || !reflect.DeepEqual(after, before) {
			t.Fatalf("a refused ReplaceConfiguration published (snapshot unchanged %v): %+v, want %+v", unchanged, after, before)
		}
		bound := agent.New(caller, agent.ContextManager{}, agent.WithVerifier(mustSlot(t, dir, seatbeltPolicy)))
		if err := rt.ReplaceConfiguration(context.Background(), Configuration{Orchestrator: bound}); err != nil {
			t.Fatalf("ReplaceConfiguration with a bound slot: %v", err)
		}
	})

	t.Run("Trusted host keeps any verifier", func(t *testing.T) {
		caller := &countingCaller{}
		rt, err := newNative(t, dir, agenttools.SandboxConfig{}, caller, nil, agent.WithVerifier(opaqueVerifier{}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err := runNative(t, rt, "host"); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
}

func TestNativePermissionsCopy(t *testing.T) {
	stubValidateSandbox(t)
	const (
		seatbeltProfile = "deny-default profile with reviewed system reads, allowed workspace and private temp"
		noQuota         = "no memory/CPU quota"
		netAllowed      = "networking explicitly allowed by policy; filesystem limits remain"
		bwrapMounts     = "reviewed read-only system mounts, workspace/snapshot and private temp"
		bwrapCaps       = "all capabilities dropped by the backend"
		bwrapMemory     = "RLIMIT_AS per-process virtual address space plus separate private tmpfs quotas; not aggregate process-tree memory"
	)
	tests := []struct {
		name   string
		policy agenttools.SandboxConfig
		want   CommandPermissions
	}{
		{"host", agenttools.SandboxConfig{}, CommandPermissions{
			Runtime: "host", Network: "unrestricted", Warning: "unsandboxed; runs with the host user's filesystem and network authority",
		}},
		{"seatbelt denied", seatbeltPolicy, CommandPermissions{Runtime: "seatbelt", Network: "denied", Limits: []string{
			seatbeltProfile, "network operations denied", noQuota,
		}}},
		{"seatbelt allowed", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt, AllowNetwork: true}, CommandPermissions{
			Runtime: "seatbelt", Network: "allowed", Limits: []string{seatbeltProfile, netAllowed, noQuota},
		}},
		{"bwrap denied", bwrapPolicy, CommandPermissions{Runtime: "bwrap", Network: "ip-denied", Limits: []string{
			"IP network namespace isolated",
			"pathname Unix sockets reachable inside the shared workspace remain a channel",
			bwrapMounts, bwrapCaps,
		}}},
		{"bwrap allowed", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap, AllowNetwork: true}, CommandPermissions{
			Runtime: "bwrap", Network: "allowed", Limits: []string{netAllowed, bwrapMounts, bwrapCaps},
		}},
		{"bwrap denied with memory cap", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap, MemoryCapMB: 512}, CommandPermissions{
			Runtime: "bwrap", Network: "ip-denied", Limits: []string{
				"IP network namespace isolated",
				"pathname Unix sockets reachable inside the shared workspace remain a channel",
				bwrapMounts, bwrapCaps, bwrapMemory,
			},
		}},
		{"bwrap allowed with memory cap", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap, AllowNetwork: true, MemoryCapMB: 512}, CommandPermissions{
			Runtime: "bwrap", Network: "allowed", Limits: []string{netAllowed, bwrapMounts, bwrapCaps, bwrapMemory},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := newNative(t, t.TempDir(), tc.policy, &countingCaller{}, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := rt.EffectivePermissions().Command; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Command =\n%#v\nwant\n%#v", got, tc.want)
			}
		})
	}
}

func TestVerificationState(t *testing.T) {
	dir, root := canonicalDir(t)
	disabled := mustSlot(t, dir, agenttools.SandboxConfig{})
	disabled.Disable("verification disabled: test")
	armed := mustSlot(t, dir, agenttools.SandboxConfig{})
	ws, err := agenttools.NewWorkspace(dir)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	cmd, err := agenttools.NewVerifyCommand(ws, []string{"go", "version"}, "", time.Minute)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	if err := armed.Set(cmd); err != nil {
		t.Fatalf("Set: %v", err)
	}

	tests := []struct {
		name     string
		profile  Profile
		policy   agenttools.SandboxConfig
		verifier agent.Verifier // nil: none installed
		want     Verification
	}{
		{"none", ProfileTrusted, agenttools.SandboxConfig{}, nil, Verification{State: "none"}},
		{"absent", ProfileTrusted, agenttools.SandboxConfig{}, mustSlot(t, dir, agenttools.SandboxConfig{}), Verification{State: "absent", Target: root}},
		{"disabled", ProfileTrusted, agenttools.SandboxConfig{}, disabled, Verification{State: "disabled", Target: root}},
		{"armed", ProfileTrusted, agenttools.SandboxConfig{}, armed, Verification{State: "armed", Target: root}},
		{"host-verifier", ProfileTrusted, agenttools.SandboxConfig{}, opaqueVerifier{}, Verification{State: "host-verifier"}},
		{"native absent", ProfileTrusted, seatbeltPolicy, mustSlot(t, dir, seatbeltPolicy), Verification{State: "absent", Target: root}},
		// Inspect has no post-write verification, whatever is installed.
		{"inspect with an armed slot", ProfileInspect, agenttools.SandboxConfig{}, armed, Verification{State: "none"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubValidateSandbox(t)
			var opts []agent.Option
			if tc.verifier != nil {
				opts = append(opts, agent.WithVerifier(tc.verifier))
			}
			rt, err := New(context.Background(), Options{
				Root:         dir,
				Profile:      tc.profile,
				Sandbox:      tc.policy,
				Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}, opts...),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { closeTestRuntime(t, rt) })
			if got := rt.EffectivePermissions().Verification; got != tc.want {
				t.Fatalf("Verification = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEffectivePermissionsScratch(t *testing.T) {
	dir := t.TempDir()
	scratchOn := agenttools.ExecToolsOptions{Scratch: agenttools.ScratchConfig{Enabled: true}}
	withScratch := func() []agent.Tool { return hostExecSet(t, dir, scratchOn) }
	without := func(set []agent.Tool, name string) []agent.Tool {
		return slices.DeleteFunc(set, func(tool agent.Tool) bool { return tool.Spec().Name == name })
	}
	named := func(set []agent.Tool, name string) agent.Tool {
		return set[slices.IndexFunc(set, func(tool agent.Tool) bool { return tool.Spec().Name == name })]
	}
	tests := []struct {
		name  string
		tools []agent.Tool
		want  bool
	}{
		{"no command tools", nil, false},
		{"command tools without scratch", hostExecSet(t, dir, agenttools.ExecToolsOptions{}), false},
		{"command tools with scratch", withScratch(), true},
		// The command tools decide, not the query tool: scratch_changes may be
		// left out, and one from another set says nothing about these commands.
		{"scratch command tools without scratch_changes", without(withScratch(), "scratch_changes"), true},
		{"plain command tools beside another set's scratch_changes", append(hostExecSet(t, dir, agenttools.ExecToolsOptions{}), named(withScratch(), "scratch_changes")), false},
		// One command tool on the live workspace makes it false.
		{"scratch run_command beside a plain start_command", []agent.Tool{named(withScratch(), "run_command"), named(hostExecSet(t, dir, agenttools.ExecToolsOptions{}), "start_command")}, false},
		{"opaque tool named run_command", []agent.Tool{&floorStub{name: "run_command", effect: agent.Effect{Class: agent.Exec}}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, err := New(context.Background(), Options{
				Root:         dir,
				Profile:      ProfileTrusted,
				Tools:        tc.tools,
				Orchestrator: agent.New(&countingCaller{}, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { closeTestRuntime(t, rt) })
			if got := rt.EffectivePermissions().Scratch; got != tc.want {
				t.Fatalf("Scratch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNativeConstructionFailsClosed(t *testing.T) {
	saved := validateSandbox
	validateSandbox = func(agenttools.SandboxConfig, string) error {
		return fmt.Errorf("probe refused: %w", agenttools.ErrSandboxPolicy)
	}
	t.Cleanup(func() { validateSandbox = saved })
	// No Orchestrator: bootstrap would load the missing config and fail with a
	// config error, so ErrSandboxPolicy proves validation ran first.
	_, err := New(context.Background(), Options{
		Root:       t.TempDir(),
		Profile:    ProfileTrusted,
		Sandbox:    seatbeltPolicy,
		ConfigPath: filepath.Join(t.TempDir(), "missing-models.json"),
	})
	if !errors.Is(err, agenttools.ErrSandboxPolicy) {
		t.Fatalf("New = %v, want ErrSandboxPolicy (not a config error)", err)
	}
}

// TestSeatbeltSessionBindsCanonicalRootSpelling needs a case-insensitive
// filesystem: a differently cased root resolves to the same directory, but
// native tools and the verifier slot carry the workspace's own spelling, so the
// session must bind to that. The darwin Seatbelt CI job selects it with
// GO_LLM_REQUIRE_SEATBELT=1, where a case-sensitive filesystem fails instead of
// skipping. On a case-sensitive filesystem (Linux) both spellings always agree,
// so the rule cannot be observed there.
func TestSeatbeltSessionBindsCanonicalRootSpelling(t *testing.T) {
	stubValidateSandbox(t)
	dir, canonical := canonicalDir(t)
	alias := strings.ToUpper(canonical)
	if _, err := os.Stat(alias); alias == canonical || err != nil {
		if os.Getenv("GO_LLM_REQUIRE_SEATBELT") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_SEATBELT=1 but %q does not resolve: the filesystem is case-sensitive", alias)
		}
		t.Skip("needs a case-insensitive filesystem")
	}
	rt, err := newNative(t, alias, seatbeltPolicy, &countingCaller{}, nil, agent.WithVerifier(mustSlot(t, dir, seatbeltPolicy)))
	if err != nil {
		t.Fatalf("New over %q: %v", alias, err)
	}
	if got := rt.EffectivePermissions().Root; got != canonical {
		t.Fatalf("Root = %q, want %q", got, canonical)
	}
}
