package golem_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/golem"
)

// The three admission paths share one rule set, so each test below drives all
// of them through admitVia.
var admissionPaths = []string{"New", "Replace", "ReplaceConfiguration"}

// ownedReads is the Inspect universe, spelled out as a literal so no expected
// value is read back from the code under test.
var ownedReads = []string{"read_file", "search", "glob", "list"}

// admitVia offers tools to a runtime built from base through one admission
// path. For Replace and ReplaceConfiguration the runtime is first built with no
// extras, so a rejection leaves the original snapshot to inspect. The returned
// runtime is nil when New itself refused.
func admitVia(t *testing.T, path string, base golem.Options, tools []agent.Tool) (*golem.Runtime, *captureCaller, error) {
	t.Helper()
	caller := &captureCaller{answer: "ok"}
	if path == "New" {
		base.Root = t.TempDir()
		base.Tools = tools
		base.Orchestrator = agent.New(caller, agent.ContextManager{})
		rt, err := golem.New(context.Background(), base)
		if err != nil {
			return nil, caller, err
		}
		t.Cleanup(func() {
			if err := rt.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return rt, caller, nil
	}
	rt := newProfileRuntime(t, caller, base)
	switch path {
	case "Replace":
		return rt, caller, rt.Replace("", tools)
	case "ReplaceConfiguration":
		return rt, caller, rt.ReplaceConfiguration(context.Background(), golem.Configuration{
			Tools:        tools,
			Orchestrator: agent.New(caller, agent.ContextManager{}),
		})
	}
	t.Fatalf("unknown admission path %q", path)
	return nil, nil, nil
}

// realDispatch builds the library's own dispatch tool over its own read tools.
func realDispatch(t *testing.T) agent.Tool {
	t.Helper()
	reads, err := agenttools.NewFileTools(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileTools: %v", err)
	}
	d, err := agenttools.NewDispatch(&captureCaller{answer: "ok"}, agent.ContextManager{}, reads, agenttools.DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	return d
}

func TestReplaceEnforcesProfile(t *testing.T) {
	for _, path := range []string{"Replace", "ReplaceConfiguration"} {
		t.Run(path, func(t *testing.T) {
			spy := &profileSpyTool{name: "put_file", effect: agent.Effect{Class: agent.Write}}
			// Profile is left unset on purpose: the zero value is Inspect.
			rt, caller, err := admitVia(t, path, golem.Options{System: "ORIGINAL"}, []agent.Tool{spy})
			if !errors.Is(err, golem.ErrProfileIncompatible) {
				t.Fatalf("%s error = %v, want ErrProfileIncompatible", path, err)
			}
			if got := spy.calls(); got != [4]int32{} {
				t.Fatalf("spy Spec/Effect/Plan/Invoke calls = %v, want all zero", got)
			}
			// The rejected replacement installed nothing: the next turn still
			// sees the original system and only the four owned reads.
			if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, ownedReads) {
				t.Fatalf("next-turn tools = %v, want %v", got, ownedReads)
			}
			if got := shapeOf(caller.requests[0]).system; got != "ORIGINAL" {
				t.Fatalf("next-turn system = %q, want %q", got, "ORIGINAL")
			}
		})
	}
}

func TestReplaceCannotWidenAllowlist(t *testing.T) {
	allowlist := []string{"read_file", "search", "glob", "list", "run_command"}
	tests := []struct {
		extra string
		want  []string // nil: rejected
	}{
		{"run_command", []string{"read_file", "search", "glob", "list", "run_command"}},
		{"write_file", nil},
	}
	for _, path := range admissionPaths {
		for _, tc := range tests {
			t.Run(path+"/"+tc.extra, func(t *testing.T) {
				spy := &profileSpyTool{name: tc.extra, effect: agent.Effect{Class: agent.Exec}}
				rt, caller, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted, ToolAllowlist: allowlist}, []agent.Tool{spy})
				if tc.want != nil {
					if err != nil {
						t.Fatalf("%s %s error = %v, want nil", path, tc.extra, err)
					}
					if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, tc.want) {
						t.Fatalf("first-turn tools = %v, want %v", got, tc.want)
					}
					return
				}
				if !errors.Is(err, golem.ErrProfileIncompatible) {
					t.Fatalf("%s %s error = %v, want ErrProfileIncompatible", path, tc.extra, err)
				}
				if rt == nil {
					return // New refused: there is no runtime to widen.
				}
				if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, ownedReads) {
					t.Fatalf("next-turn tools = %v, want %v", got, ownedReads)
				}
			})
		}
	}
}

func TestDispatchRequiresFourReadsInAllowlist(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		wantErr   bool
	}{
		{"missing read_file", []string{"dispatch", "search", "glob", "list"}, true},
		{"missing search", []string{"dispatch", "read_file", "glob", "list"}, true},
		{"missing glob", []string{"dispatch", "read_file", "search", "list"}, true},
		{"missing list", []string{"dispatch", "read_file", "search", "glob"}, true},
		{"all four reads", []string{"dispatch", "read_file", "search", "glob", "list"}, false},
	}
	for _, path := range admissionPaths {
		for _, tc := range tests {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				rt, caller, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted, ToolAllowlist: tc.allowlist}, []agent.Tool{realDispatch(t)})
				if tc.wantErr {
					if !errors.Is(err, golem.ErrProfileIncompatible) {
						t.Fatalf("%s error = %v, want ErrProfileIncompatible", path, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s error = %v, want nil", path, err)
				}
				want := []string{"read_file", "search", "glob", "list", "dispatch"}
				if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, want) {
					t.Fatalf("first-turn tools = %v, want %v", got, want)
				}
			})
		}
	}
}

func TestDispatchRejectedWithScopeGuard(t *testing.T) {
	allowAll := func(string, bool) error { return nil }
	for _, path := range admissionPaths {
		t.Run(path+"/guard", func(t *testing.T) {
			_, _, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted, ScopeGuard: allowAll}, []agent.Tool{realDispatch(t)})
			if !errors.Is(err, golem.ErrProfileIncompatible) {
				t.Fatalf("%s error = %v, want ErrProfileIncompatible", path, err)
			}
		})
		t.Run(path+"/no guard", func(t *testing.T) {
			rt, caller, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted}, []agent.Tool{realDispatch(t)})
			if err != nil {
				t.Fatalf("%s error = %v, want nil", path, err)
			}
			want := []string{"read_file", "search", "glob", "list", "dispatch"}
			if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, want) {
				t.Fatalf("first-turn tools = %v, want %v", got, want)
			}
		})
	}
}

func TestReplaceConfigurationAcceptsStartupEquivalentSet(t *testing.T) {
	t.Run("trusted startup set including dispatch", func(t *testing.T) {
		lookup := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}
		startup := []agent.Tool{lookup, realDispatch(t)}
		caller := &captureCaller{answer: "ok"}
		rt := newProfileRuntime(t, caller, golem.Options{Profile: golem.ProfileTrusted, Tools: startup})
		want := []string{"read_file", "search", "glob", "list", "lookup", "dispatch"}

		// The /model rebuild shape: a new orchestrator and the same extras.
		if err := rt.ReplaceConfiguration(context.Background(), golem.Configuration{
			System:       "REBUILT",
			Tools:        startup,
			Orchestrator: agent.New(caller, agent.ContextManager{}),
		}); err != nil {
			t.Fatalf("ReplaceConfiguration with the startup set: %v", err)
		}
		// The /think and renewCanary shape: Replace with the same tools.
		if err := rt.Replace("RENEWED", startup); err != nil {
			t.Fatalf("Replace with the startup set: %v", err)
		}
		if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, want) {
			t.Fatalf("first-turn tools = %v, want %v", got, want)
		}
		if got := shapeOf(caller.requests[0]).system; got != "RENEWED" {
			t.Fatalf("first-turn system = %q, want %q", got, "RENEWED")
		}
	})
	t.Run("inspect accepts empty tools", func(t *testing.T) {
		for _, empty := range [][]agent.Tool{nil, {}} {
			caller := &captureCaller{answer: "ok"}
			rt := newProfileRuntime(t, caller, golem.Options{})
			if err := rt.ReplaceConfiguration(context.Background(), golem.Configuration{
				System:       "REBUILT",
				Tools:        empty,
				Orchestrator: agent.New(caller, agent.ContextManager{}),
			}); err != nil {
				t.Fatalf("ReplaceConfiguration(Tools=%#v): %v", empty, err)
			}
			if err := rt.Replace("RENEWED", empty); err != nil {
				t.Fatalf("Replace(tools=%#v): %v", empty, err)
			}
			if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, ownedReads) {
				t.Fatalf("first-turn tools = %v, want %v", got, ownedReads)
			}
			if got := shapeOf(caller.requests[0]).system; got != "RENEWED" {
				t.Fatalf("first-turn system = %q, want %q", got, "RENEWED")
			}
		}
	})
}

func TestRejectedReplaceNeverInvokesTool(t *testing.T) {
	for _, path := range admissionPaths {
		t.Run(path, func(t *testing.T) {
			spy := &profileSpyTool{name: "run_command", effect: agent.Effect{Class: agent.Exec}}
			_, _, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted, ToolAllowlist: []string{"read_file"}}, []agent.Tool{spy})
			if !errors.Is(err, golem.ErrProfileIncompatible) {
				t.Fatalf("%s error = %v, want ErrProfileIncompatible", path, err)
			}
			// Admission needs the tool's name, so Spec may run; planning and
			// invoking a rejected tool never may.
			if plans, invokes := spy.plans.Load(), spy.invokes.Load(); plans != 0 || invokes != 0 {
				t.Fatalf("rejected tool Plan/Invoke calls = %d/%d, want 0/0", plans, invokes)
			}
		})
	}
}
