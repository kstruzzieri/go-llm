package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// bindingSeatbelt is the native policy the binding rows use. Normalization
// never probes, so the rows run on every OS over stub backends.
var bindingSeatbelt = SandboxConfig{Runtime: SandboxRuntimeSeatbelt}

// stubManager returns a manager whose resolved backend is a stub bound to cfg,
// exactly as NewSandboxedBackgroundManager would bind a real one.
func stubManager(t *testing.T, cfg SandboxConfig) *BackgroundManager {
	t.Helper()
	backend := bindExecBackend(hostBackend{commandRunner: &markerRunner{}, backgroundStarter: starterOf()}, mustNormalizeSandbox(t, cfg))
	m := newBackgroundManagerWithBackend(backend, &countingRandom{})
	t.Cleanup(m.Shutdown)
	return m
}

func canonicalTempRoot(t *testing.T) string {
	t.Helper()
	root, err := CanonicalWorkspaceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// execSet builds the full background exec set over m; with scratch it also
// carries a shared scratch runtime, scratch_changes and (where the platform
// supports promotion) promote_artifact.
func execSet(t *testing.T, root string, m *BackgroundManager, scratch bool) []agent.Tool {
	t.Helper()
	var opts ExecToolsOptions
	if scratch {
		opts.Scratch = ScratchConfig{Enabled: true}
		if scratchPromotionSupported {
			opts.PromotionJournal = nopPreparingJournal{}
		}
	}
	set, err := NewExecToolsWithBackgroundOptions(root, m, opts)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func toolNamed(t *testing.T, set []agent.Tool, name string) agent.Tool {
	t.Helper()
	for _, tool := range set {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("no %s in the set", name)
	return nil
}

// boundRunCommand is a foreground run_command stamped the way the sandboxed
// constructors stamp one, over a stub backend for cfg.
func boundRunCommand(t *testing.T, root string, cfg SandboxConfig) *RunCommand {
	t.Helper()
	backend := bindExecBackend(hostBackend{commandRunner: &markerRunner{}, backgroundStarter: starterOf()}, mustNormalizeSandbox(t, cfg))
	rc := NewRunCommand(mustWorkspace(t, root), backend)
	rc.sandbox = backend.approval
	return rc
}

// foreignFileTool returns the named library file tool built over a fresh
// directory that is not the session root.
func foreignFileTool(t *testing.T, name string) agent.Tool {
	t.Helper()
	dir := t.TempDir()
	reads, err := NewFileTools(dir)
	if err != nil {
		t.Fatal(err)
	}
	return toolNamed(t, append(reads, NewMutatingTools(mustWorkspace(t, dir), nil)...), name)
}

// opaqueWrapper is a floor-style wrapper around a real admitted tool: a
// concrete type this package did not build.
type opaqueWrapper struct{ inner agent.Tool }

func (w opaqueWrapper) Spec() agent.ToolSpec { return w.inner.Spec() }
func (w opaqueWrapper) Effect() agent.Effect { return w.inner.Effect() }
func (w opaqueWrapper) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	return w.inner.Invoke(ctx, args)
}

func TestCheckSandboxBindings(t *testing.T) {
	type fixture struct {
		root  string
		tools []agent.Tool
		key   string // scratchKey argument
		// wantKey is the identity the call must return; set only by ok rows.
		wantKey string
	}
	tests := []struct {
		name string
		cfg  SandboxConfig
		// build returns the row's fixture over a fresh canonical root.
		build func(t *testing.T, root string) fixture
		// reject names the tool the error must name; "" means admitted.
		reject string
	}{
		{name: "bound file tools and background set", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			reads, err := NewFileTools(root)
			if err != nil {
				t.Fatal(err)
			}
			set := append(reads, NewMutatingTools(mustWorkspace(t, root), nil)...)
			return fixture{root: root, tools: append(set, execSet(t, root, stubManager(t, bindingSeatbelt), false)...)}
		}},
		{name: "bound RunCommand", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{boundRunCommand(t, root, bindingSeatbelt)}}
		}},
		{name: "empty list", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			return fixture{root: root}
		}},
		{name: "scratch set freezes its identity", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			return fixture{root: root, tools: set, wantKey: set[0].(*RunCommand).scratchRT.approval.keyComponent}
		}},
		{name: "scratch_changes pointer form", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			for i, tool := range set {
				if sc, ok := tool.(ScratchChanges); ok {
					set[i] = &sc
				}
			}
			return fixture{root: root, tools: set, wantKey: set[0].(*RunCommand).scratchRT.approval.keyComponent}
		}},
		{name: "matching frozen scratchKey", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			key := set[0].(*RunCommand).scratchRT.approval.keyComponent
			return fixture{root: root, tools: set, key: key, wantKey: key}
		}},
		{name: "frozen scratchKey with no command tools", cfg: bindingSeatbelt, build: func(t *testing.T, root string) fixture {
			return fixture{root: root, key: "scr:frozen:", wantKey: "scr:frozen:"}
		}},

		{name: "NewRunCommand custom runner with stamped approval", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			rc := NewRunCommand(mustWorkspace(t, root), &markerRunner{})
			rc.sandbox = approvalForSandbox(mustNormalizeSandbox(t, bindingSeatbelt))
			return fixture{root: root, tools: []agent.Tool{rc}}
		}},
		{name: "host runner with stamped seatbelt approval", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			rc := boundRunCommand(t, root, SandboxConfig{})
			rc.sandbox = approvalForSandbox(mustNormalizeSandbox(t, bindingSeatbelt))
			return fixture{root: root, tools: []agent.Tool{rc}}
		}},
		{name: "seatbelt runner with unstamped approval", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			rc := boundRunCommand(t, root, bindingSeatbelt)
			rc.sandbox = sandboxApproval{}
			return fixture{root: root, tools: []agent.Tool{rc}}
		}},
		{name: "host-bound tools under a seatbelt policy", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: execSet(t, root, stubManager(t, SandboxConfig{}), false)}
		}},
		{name: "zero resolved backend under host", cfg: SandboxConfig{}, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{NewRunCommand(mustWorkspace(t, root), resolvedExecBackend{})}}
		}},
		{name: "foreign root read_file", cfg: bindingSeatbelt, reject: "read_file", build: func(t *testing.T, root string) fixture {
			reads, err := NewFileTools(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return fixture{root: root, tools: reads[:1]}
		}},
		{name: "foreign root search", cfg: bindingSeatbelt, reject: "search", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{foreignFileTool(t, "search")}}
		}},
		{name: "foreign root glob", cfg: bindingSeatbelt, reject: "glob", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{foreignFileTool(t, "glob")}}
		}},
		{name: "foreign root list", cfg: bindingSeatbelt, reject: "list", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{foreignFileTool(t, "list")}}
		}},
		{name: "foreign root write_file", cfg: bindingSeatbelt, reject: "write_file", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{foreignFileTool(t, "write_file")}}
		}},
		{name: "foreign root edit_file", cfg: bindingSeatbelt, reject: "edit_file", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{foreignFileTool(t, "edit_file")}}
		}},
		{name: "foreign root promote_artifact over an admitted rt", cfg: bindingSeatbelt, reject: "promote_artifact", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			rt := set[0].(*RunCommand).scratchRT
			return fixture{root: root, tools: append(set[:6], NewPromoteArtifact(mustWorkspace(t, t.TempDir()), rt))}
		}},
		{name: "foreign root run_command", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{boundRunCommand(t, canonicalTempRoot(t), bindingSeatbelt)}}
		}},
		{name: "foreign root start_command", cfg: bindingSeatbelt, reject: "start_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{NewStartCommand(mustWorkspace(t, t.TempDir()), stubManager(t, bindingSeatbelt))}}
		}},
		{name: "typed-nil RunCommand", cfg: bindingSeatbelt, reject: "*tools.RunCommand", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{(*RunCommand)(nil)}}
		}},
		{name: "nil interface", cfg: bindingSeatbelt, reject: "tool at index 0", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{nil}}
		}},
		{name: "zero RunCommand", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{&RunCommand{}}}
		}},
		{name: "start_command without a manager", cfg: bindingSeatbelt, reject: "start_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{NewStartCommand(mustWorkspace(t, root), nil)}}
		}},
		{name: "start_command over a zero manager under host", cfg: SandboxConfig{}, reject: "start_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{NewStartCommand(mustWorkspace(t, root), &BackgroundManager{})}}
		}},
		{name: "start_command over a host manager", cfg: bindingSeatbelt, reject: "start_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{NewStartCommand(mustWorkspace(t, root), stubManager(t, SandboxConfig{}))}}
		}},
		{name: "CommandStatus over an unadmitted manager", cfg: bindingSeatbelt, reject: "command_status", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), false)
			return fixture{root: root, tools: append(set[:2], NewCommandStatus(stubManager(t, bindingSeatbelt)))}
		}},
		{name: "CommandTail over an unadmitted manager", cfg: bindingSeatbelt, reject: "command_tail", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), false)
			return fixture{root: root, tools: append(set[:2], NewCommandTail(stubManager(t, bindingSeatbelt)))}
		}},
		{name: "StopCommand over an unadmitted manager", cfg: bindingSeatbelt, reject: "stop_command", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), false)
			return fixture{root: root, tools: append(set[:2], NewStopCommand(stubManager(t, bindingSeatbelt)))}
		}},
		{name: "ScratchChanges with a foreign rt", cfg: bindingSeatbelt, reject: "scratch_changes", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			foreign := toolNamed(t, execSet(t, root, stubManager(t, bindingSeatbelt), true), "scratch_changes")
			return fixture{root: root, tools: append(set[:5], foreign)}
		}},
		{name: "ScratchChanges pointer with a foreign rt", cfg: bindingSeatbelt, reject: "scratch_changes", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			foreign := toolNamed(t, execSet(t, root, stubManager(t, bindingSeatbelt), true), "scratch_changes").(ScratchChanges)
			return fixture{root: root, tools: append(set[:5], &foreign)}
		}},
		{name: "zero ScratchChanges", cfg: bindingSeatbelt, reject: "scratch_changes", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{ScratchChanges{}}}
		}},
		{name: "run_command scratch runtime for another root", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			foreign := execSet(t, canonicalTempRoot(t), stubManager(t, bindingSeatbelt), true)
			rc := boundRunCommand(t, root, bindingSeatbelt)
			rc.scratchRT = foreign[0].(*RunCommand).scratchRT
			return fixture{root: root, tools: []agent.Tool{rc}}
		}},
		{name: "start_command scratch runtime for another root", cfg: bindingSeatbelt, reject: "start_command", build: func(t *testing.T, root string) fixture {
			foreign := execSet(t, canonicalTempRoot(t), stubManager(t, bindingSeatbelt), true)
			sc := NewStartCommand(mustWorkspace(t, root), stubManager(t, bindingSeatbelt))
			sc.scratchRT = foreign[0].(*RunCommand).scratchRT
			return fixture{root: root, tools: []agent.Tool{sc}}
		}},
		{name: "changed scratchKey", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: execSet(t, root, stubManager(t, bindingSeatbelt), true), key: "scr:other:"}
		}},
		{name: "command tools disagree on scratch", cfg: bindingSeatbelt, reject: "start_command", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			set[1].(*StartCommand).scratchRT = nil
			return fixture{root: root, tools: set}
		}},
		{name: "Dispatch", cfg: bindingSeatbelt, reject: DispatchToolName, build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{newBudgetDispatch(t, &fakeCaller{}, agent.ContextManager{}, DispatchLimits{})}}
		}},
		{name: "Retrieve", cfg: bindingSeatbelt, reject: "retrieve", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{Retrieve{}}}
		}},
		{name: "MemorySearch", cfg: bindingSeatbelt, reject: "memory_search", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{MemorySearch{}}}
		}},
		{name: "floor wrapper around an admitted tool", cfg: bindingSeatbelt, reject: "run_command", build: func(t *testing.T, root string) fixture {
			return fixture{root: root, tools: []agent.Tool{opaqueWrapper{inner: boundRunCommand(t, root, bindingSeatbelt)}}}
		}},
	}
	if scratchPromotionSupported {
		tests = append(tests, struct {
			name   string
			cfg    SandboxConfig
			build  func(t *testing.T, root string) fixture
			reject string
		}{name: "PromoteArtifact with a foreign rt", cfg: bindingSeatbelt, reject: "promote_artifact", build: func(t *testing.T, root string) fixture {
			set := execSet(t, root, stubManager(t, bindingSeatbelt), true)
			foreign := toolNamed(t, execSet(t, root, stubManager(t, bindingSeatbelt), true), "promote_artifact")
			return fixture{root: root, tools: append(set[:6], foreign)}
		}})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.build(t, canonicalTempRoot(t))
			key, err := CheckSandboxBindings(f.tools, tc.cfg, f.root, f.key)
			if tc.reject == "" {
				if err != nil {
					t.Fatalf("CheckSandboxBindings: %v", err)
				}
				if key != f.wantKey {
					t.Fatalf("scratch key = %q, want %q", key, f.wantKey)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckSandboxBindings admitted the set (key %q), want %s rejected", key, tc.reject)
			}
			if !strings.Contains(err.Error(), tc.reject) || !strings.Contains(err.Error(), "is not admitted in a native sandbox session") {
				t.Fatalf("error = %q, want it to name %s as not admitted", err, tc.reject)
			}
			if key != "" {
				t.Fatalf("rejected call returned scratch key %q, want empty", key)
			}
		})
	}
}

func TestCheckSandboxBindingsRejectsInvalidPolicy(t *testing.T) {
	_, err := CheckSandboxBindings(nil, SandboxConfig{MemoryCapMB: -1}, canonicalTempRoot(t), "")
	if !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("CheckSandboxBindings(invalid policy) = %v, want ErrSandboxPolicy", err)
	}
}

func TestValidateSandboxUnavailable(t *testing.T) {
	// Every backend for another OS: seatbelt off darwin, bwrap off Linux, so
	// Windows (the CI windows job runs this) refuses both (spec section 10).
	cfgs := []SandboxConfig{
		{Runtime: "container"},
		{AllowNetwork: true}, // host accepts no constraints
	}
	if runtime.GOOS != "darwin" {
		cfgs = append(cfgs, SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	}
	if runtime.GOOS != "linux" {
		cfgs = append(cfgs, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	}
	for _, cfg := range cfgs {
		if err := ValidateSandbox(cfg, t.TempDir()); !errors.Is(err, ErrSandboxPolicy) {
			t.Errorf("ValidateSandbox(%+v) = %v, want ErrSandboxPolicy", cfg, err)
		}
	}
	if err := ValidateSandbox(SandboxConfig{}, t.TempDir()); err != nil {
		t.Errorf("ValidateSandbox(host) = %v, want nil", err)
	}
}

// TestNativeBackendValidateSandboxRootBound needs this OS's native backend: a
// probe that passes still refuses a root the backend cannot bound. Both native
// CI jobs select the TestNativeBackend prefix with GO_LLM_REQUIRE_SEATBELT or
// GO_LLM_REQUIRE_BWRAP=1, which turns an unavailable backend into a failure.
func TestNativeBackendValidateSandboxRootBound(t *testing.T) {
	cfg, require := SandboxConfig{Runtime: SandboxRuntimeSeatbelt}, "GO_LLM_REQUIRE_SEATBELT"
	switch runtime.GOOS {
	case "darwin":
	case "linux":
		cfg, require = SandboxConfig{Runtime: SandboxRuntimeBwrap}, "GO_LLM_REQUIRE_BWRAP"
	default:
		t.Skip("no native backend on " + runtime.GOOS)
	}
	if _, err := newExecBackend(cfg); err != nil {
		if os.Getenv(require) == "1" {
			t.Fatalf("%s=1 but %s is unavailable: %v", require, cfg.Runtime, err)
		}
		t.Skipf("%s unavailable on this host: %v", cfg.Runtime, err)
	}
	if err := ValidateSandbox(cfg, t.TempDir()); err != nil {
		t.Fatalf("ValidateSandbox(%s, temp root) = %v, want nil", cfg.Runtime, err)
	}
	if err := ValidateSandbox(cfg, "/"); !errors.Is(err, ErrSandboxPolicy) {
		t.Fatalf("ValidateSandbox(%s, /) = %v, want ErrSandboxPolicy", cfg.Runtime, err)
	}
}
