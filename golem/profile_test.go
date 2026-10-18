package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
)

// profileSpyTool counts every method admission could call on a caller tool, so
// a rejection can prove it never ran any of them.
type profileSpyTool struct {
	name                           string
	effect                         agent.Effect
	specs, effects, plans, invokes atomic.Int32
}

func (s *profileSpyTool) Spec() agent.ToolSpec {
	s.specs.Add(1)
	return agent.ToolSpec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (s *profileSpyTool) Effect() agent.Effect {
	s.effects.Add(1)
	return s.effect
}

func (s *profileSpyTool) Plan(context.Context, json.RawMessage) (agent.ToolPlan, error) {
	s.plans.Add(1)
	return agent.ToolPlan{Effect: s.effect}, nil
}

func (s *profileSpyTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	s.invokes.Add(1)
	return agent.ToolResult{Content: "ok"}, nil
}

func (s *profileSpyTool) calls() [4]int32 {
	return [4]int32{s.specs.Load(), s.effects.Load(), s.plans.Load(), s.invokes.Load()}
}

// firstTurnToolNames runs one turn on rt and returns the tool names the model
// saw in its first request.
func firstTurnToolNames(t *testing.T, rt *golem.Runtime, caller *captureCaller) []string {
	t.Helper()
	runTurn(t, rt, "profile-run")
	if len(caller.requests) == 0 {
		t.Fatal("model was never called")
	}
	tools := caller.requests[0].Tools
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Function.Name
	}
	return names
}

func newProfileRuntime(t *testing.T, caller *captureCaller, opts golem.Options) *golem.Runtime {
	t.Helper()
	opts.Root = t.TempDir()
	opts.Orchestrator = agent.New(caller, agent.ContextManager{})
	rt, err := golem.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return rt
}

func TestProfileZeroValueIsInspect(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newProfileRuntime(t, caller, golem.Options{})
	want := []string{"read_file", "search", "glob", "list"}
	if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, want) {
		t.Fatalf("first-turn tools = %v, want %v", got, want)
	}
	// The mounted names alone cannot tell Inspect from Trusted (a Trusted
	// runtime with no extras mounts the same four reads), so the zero value
	// must also refuse a caller tool.
	spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}
	_, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Tools:        []agent.Tool{spy},
		Orchestrator: agent.New(&captureCaller{answer: "ok"}, agent.ContextManager{}),
	})
	if !errors.Is(err, golem.ErrProfileIncompatible) {
		t.Fatalf("zero-value New with a caller tool = %v, want ErrProfileIncompatible", err)
	}
}

func TestInspectRejectsAnyCallerTool(t *testing.T) {
	tests := []struct {
		name   string
		tool   string
		effect agent.Effect
	}{
		{"read never approval", "lookup", agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}},
		{"builtin name", "read_file", agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}},
		{"write", "put_file", agent.Effect{Class: agent.Write}},
		{"exec", "run_it", agent.Effect{Class: agent.Exec}},
		{"network", "fetch", agent.Effect{Class: agent.Read | agent.Network}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := &profileSpyTool{name: tc.tool, effect: tc.effect}
			// Profile is left unset on purpose: the zero value is Inspect.
			_, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Tools:        []agent.Tool{spy},
				Orchestrator: agent.New(&captureCaller{answer: "ok"}, agent.ContextManager{}),
			})
			if !errors.Is(err, golem.ErrProfileIncompatible) {
				t.Fatalf("New error = %v, want ErrProfileIncompatible", err)
			}
			if got := spy.calls(); got != [4]int32{} {
				t.Fatalf("spy Spec/Effect/Plan/Invoke calls = %v, want all zero", got)
			}
		})
	}
}

func TestPreviewRejectsEveryRegistry(t *testing.T) {
	spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}
	tests := []struct {
		name  string
		tools []agent.Tool
	}{
		{"nil", nil},
		{"empty", []agent.Tool{}},
		{"non-empty", []agent.Tool{spy}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := golem.New(context.Background(), golem.Options{
				Root:         t.TempDir(),
				Profile:      golem.ProfilePreview,
				Tools:        tc.tools,
				Orchestrator: agent.New(&captureCaller{answer: "ok"}, agent.ContextManager{}),
			})
			if !errors.Is(err, golem.ErrProfileUnavailable) {
				t.Fatalf("New error = %v, want ErrProfileUnavailable", err)
			}
			if !strings.Contains(err.Error(), "#388/#389") {
				t.Fatalf("New error = %q, want it to name #388/#389", err)
			}
		})
	}
	if got := spy.calls(); got != [4]int32{} {
		t.Fatalf("spy Spec/Effect/Plan/Invoke calls = %v, want all zero", got)
	}
}

func TestUnknownProfileRejected(t *testing.T) {
	_, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Profile:      golem.Profile(9),
		Orchestrator: agent.New(&captureCaller{answer: "ok"}, agent.ContextManager{}),
	})
	if !errors.Is(err, golem.ErrInvalidRequest) {
		t.Fatalf("New error = %v, want ErrInvalidRequest", err)
	}
	if got := golem.Profile(9).String(); got != "profile(9)" {
		t.Fatalf("Profile(9).String() = %q, want %q", got, "profile(9)")
	}
	for _, bad := range []string{"Trusted", ""} {
		if got, err := golem.ParseProfile(bad); !errors.Is(err, golem.ErrInvalidRequest) {
			t.Fatalf("ParseProfile(%q) = %v, %v; want ErrInvalidRequest", bad, got, err)
		}
	}
	for _, tc := range []struct {
		name string
		want golem.Profile
	}{
		{"inspect", golem.ProfileInspect},
		{"preview", golem.ProfilePreview},
		{"trusted", golem.ProfileTrusted},
	} {
		got, err := golem.ParseProfile(tc.name)
		if err != nil || got != tc.want {
			t.Fatalf("ParseProfile(%q) = %v, %v; want %v", tc.name, got, err, tc.want)
		}
		if got.String() != tc.name {
			t.Fatalf("%v.String() = %q, want %q", got, got.String(), tc.name)
		}
	}
}

func TestAdmissionRunsBeforeBootstrap(t *testing.T) {
	spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}
	_, err := golem.New(context.Background(), golem.Options{
		Root:       t.TempDir(),
		ConfigPath: filepath.Join(t.TempDir(), "missing-models.json"),
		Profile:    golem.ProfileInspect,
		Tools:      []agent.Tool{spy},
	})
	if !errors.Is(err, golem.ErrProfileIncompatible) {
		t.Fatalf("New error = %v, want ErrProfileIncompatible (not a config error)", err)
	}
}

func TestAllowlistUniverse(t *testing.T) {
	tests := []struct {
		name      string
		profile   golem.Profile
		allowlist []string
		wantErr   error
		wantTools []string
	}{
		{"inspect rejects a name outside the four reads", golem.ProfileInspect, []string{"run_command"}, golem.ErrProfileIncompatible, nil},
		{"trusted accepts an unmounted name", golem.ProfileTrusted, []string{"run_command"}, nil, nil},
		{"inspect non-nil empty mounts nothing", golem.ProfileInspect, []string{}, nil, nil},
		{"inspect narrows to one read", golem.ProfileInspect, []string{"read_file"}, nil, []string{"read_file"}},
		{"trusted rejects an empty name", golem.ProfileTrusted, []string{""}, golem.ErrInvalidRequest, nil},
		{"trusted rejects a control-character name", golem.ProfileTrusted, []string{"run\x00command"}, golem.ErrInvalidRequest, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			caller := &captureCaller{answer: "ok"}
			opts := golem.Options{
				Root:          t.TempDir(),
				Profile:       tc.profile,
				ToolAllowlist: tc.allowlist,
				Orchestrator:  agent.New(caller, agent.ContextManager{}),
			}
			rt, err := golem.New(context.Background(), opts)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("New error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() {
				if err := rt.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})
			if got := firstTurnToolNames(t, rt, caller); !slices.Equal(got, tc.wantTools) {
				t.Fatalf("first-turn tools = %v, want %v", got, tc.wantTools)
			}
		})
	}
}
