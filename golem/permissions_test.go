package golem_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/golem"
)

// hostWarning is spec section 7's required host-backend description, spelled out
// so no expected value is read back from the code under test.
const hostWarning = "unsandboxed; runs with the host user's filesystem and network authority"

// ownedReadPerms is what the four runtime-owned reads report: Read class, the
// ApprovalNever agent/tools gives them, and runtime provenance.
func ownedReadPerms() []golem.ToolPermission {
	perms := make([]golem.ToolPermission, 0, len(ownedReads))
	for _, name := range ownedReads {
		perms = append(perms, golem.ToolPermission{
			Name: name, Class: agent.Read, Approval: agent.ApprovalNever, Provenance: golem.ProvenanceRuntime,
		})
	}
	return perms
}

// newPermsRuntime builds a runtime over a fresh root and returns it with the
// root's canonical spelling, derived from the standard library rather than from
// the code under test.
func newPermsRuntime(t *testing.T, opts golem.Options) (*golem.Runtime, string) {
	t.Helper()
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	caller := &captureCaller{answer: "ok"}
	opts.Root = dir
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
	return rt, canonical
}

// libraryWriteTools builds the library's own write_file and edit_file.
func libraryWriteTools(t *testing.T) []agent.Tool {
	t.Helper()
	ws, err := agenttools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return agenttools.NewMutatingTools(ws, nil)
}

func TestEffectivePermissionsInspect(t *testing.T) {
	rt, root := newPermsRuntime(t, golem.Options{}) // the zero value is Inspect
	got := rt.EffectivePermissions()
	if got.Profile != golem.ProfileInspect {
		t.Errorf("Profile = %v, want inspect", got.Profile)
	}
	if got.Root != root {
		t.Errorf("Root = %q, want %q", got.Root, root)
	}
	if got.Ceiling != nil {
		t.Errorf("Ceiling = %#v, want nil for a runtime with no allowlist", got.Ceiling)
	}
	if want := ownedReadPerms(); !slices.Equal(got.Tools, want) {
		t.Errorf("Tools = %+v, want %+v", got.Tools, want)
	}
	// Inspect runs no commands, so it must not imply any sandbox or host runtime.
	if want := (golem.CommandPermissions{Runtime: "n/a", Network: "n/a", Warning: ""}); got.Command != want {
		t.Errorf("Command = %+v, want %+v", got.Command, want)
	}
}

func TestEffectivePermissionsTrustedHost(t *testing.T) {
	// A Read|Network extra at ApprovalDefault is floored to Always; a plain
	// Read extra at ApprovalNever is not touched by the floor, so reporting
	// "Always" for every extra would be wrong.
	tools := func() []agent.Tool {
		return []agent.Tool{
			&profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read | agent.Network}},
			&profileSpyTool{name: "peek", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}},
		}
	}
	want := append(ownedReadPerms(),
		golem.ToolPermission{Name: "lookup", Class: agent.Read | agent.Network, Approval: agent.ApprovalAlways, Provenance: golem.ProvenanceHostExtension},
		golem.ToolPermission{Name: "peek", Class: agent.Read, Approval: agent.ApprovalNever, Provenance: golem.ProvenanceHostExtension},
	)
	for _, path := range admissionPaths {
		t.Run(path, func(t *testing.T) {
			rt, _, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted}, tools())
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			got := rt.EffectivePermissions()
			if got.Profile != golem.ProfileTrusted {
				t.Errorf("Profile = %v, want trusted", got.Profile)
			}
			if !slices.Equal(got.Tools, want) {
				t.Errorf("Tools = %+v, want %+v", got.Tools, want)
			}
			wantCommand := golem.CommandPermissions{Runtime: "host", Network: "unrestricted", Warning: hostWarning}
			if got.Command != wantCommand {
				t.Errorf("Command = %+v, want %+v", got.Command, wantCommand)
			}
		})
	}
}

func TestEffectivePermissionsCeiling(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		wantCeil  []string // nil: no allowlist
		wantTools []string // mounted names
	}{
		{"nil allowlist", nil, nil, ownedReads},
		{"empty allowlist mounts nothing", []string{}, []string{}, []string{}},
		{"sorted copy", []string{"search", "lookup", "read_file"}, []string{"lookup", "read_file", "search"}, []string{"read_file", "search"}},
		// The ceiling is a set: a repeated name is reported once.
		{"duplicate entries collapse", []string{"search", "list", "search"}, []string{"list", "search"}, []string{"search", "list"}},
		// A name the ceiling admits but nothing mounts is a ceiling, not a tool.
		{"unmounted ceiling name", []string{"run_command", "read_file"}, []string{"read_file", "run_command"}, []string{"read_file"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, _ := newPermsRuntime(t, golem.Options{Profile: golem.ProfileTrusted, ToolAllowlist: tc.allowlist})
			got := rt.EffectivePermissions()
			if (got.Ceiling == nil) != (tc.wantCeil == nil) || !slices.Equal(got.Ceiling, tc.wantCeil) {
				t.Errorf("Ceiling = %#v, want %#v", got.Ceiling, tc.wantCeil)
			}
			names := make([]string, len(got.Tools))
			for i, p := range got.Tools {
				names[i] = p.Name
			}
			if !slices.Equal(names, tc.wantTools) {
				t.Errorf("mounted = %v, want %v", names, tc.wantTools)
			}
		})
	}
}

func TestEffectivePermissionsIsACopy(t *testing.T) {
	spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read | agent.Network}}
	rt, _ := newPermsRuntime(t, golem.Options{
		Profile:       golem.ProfileTrusted,
		Tools:         []agent.Tool{spy},
		ToolAllowlist: []string{"read_file", "lookup"},
	})
	first := rt.EffectivePermissions()
	for i := range first.Tools {
		first.Tools[i] = golem.ToolPermission{Name: "hijacked"}
	}
	for i := range first.Ceiling {
		first.Ceiling[i] = "hijacked"
	}

	second := rt.EffectivePermissions()
	wantTools := []golem.ToolPermission{
		{Name: "read_file", Class: agent.Read, Approval: agent.ApprovalNever, Provenance: golem.ProvenanceRuntime},
		{Name: "lookup", Class: agent.Read | agent.Network, Approval: agent.ApprovalAlways, Provenance: golem.ProvenanceHostExtension},
	}
	if !slices.Equal(second.Tools, wantTools) {
		t.Errorf("Tools after caller mutation = %+v, want %+v", second.Tools, wantTools)
	}
	if want := []string{"lookup", "read_file"}; !slices.Equal(second.Ceiling, want) {
		t.Errorf("Ceiling after caller mutation = %v, want %v", second.Ceiling, want)
	}
}

func TestEffectivePermissionsMakesNoToolCalls(t *testing.T) {
	spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read | agent.Network}}
	rt, _ := newPermsRuntime(t, golem.Options{Profile: golem.ProfileTrusted, Tools: []agent.Tool{spy}})
	before := spy.calls()
	for range 10 {
		got := rt.EffectivePermissions()
		// Guard against a vacuous pass: the spy is in the snapshot, so a
		// read-time Effect() call would have had something to count.
		if !slices.ContainsFunc(got.Tools, func(p golem.ToolPermission) bool { return p.Name == "lookup" }) {
			t.Fatalf("snapshot lacks the spy tool: %+v", got.Tools)
		}
	}
	if after := spy.calls(); after != before {
		t.Fatalf("spy Spec/Effect/Plan/Invoke calls went from %v to %v across 10 reads, want unchanged", before, after)
	}
}

// lastTurnToolNames runs one turn and returns the tool names the model saw in
// that turn's request.
func lastTurnToolNames(t *testing.T, rt *golem.Runtime, caller *captureCaller, runID string) []string {
	t.Helper()
	runTurn(t, rt, runID)
	tools := caller.requests[len(caller.requests)-1].Tools
	names := make([]string, len(tools))
	for i := range tools {
		names[i] = tools[i].Function.Name
	}
	return names
}

func permissionNames(p golem.EffectivePermissions) []string {
	names := make([]string, len(p.Tools))
	for i, tool := range p.Tools {
		names[i] = tool.Name
	}
	return names
}

func TestEffectivePermissionsTracksReplace(t *testing.T) {
	apply := map[string]func(rt *golem.Runtime, caller *captureCaller, tools []agent.Tool) error{
		"Replace": func(rt *golem.Runtime, _ *captureCaller, tools []agent.Tool) error {
			return rt.Replace("", tools)
		},
		"ReplaceConfiguration": func(rt *golem.Runtime, caller *captureCaller, tools []agent.Tool) error {
			return rt.ReplaceConfiguration(context.Background(), golem.Configuration{
				Tools:        tools,
				Orchestrator: agent.New(caller, agent.ContextManager{}),
			})
		},
	}
	for _, path := range []string{"Replace", "ReplaceConfiguration"} {
		t.Run(path, func(t *testing.T) {
			caller := &captureCaller{answer: "ok"}
			rt := newProfileRuntime(t, caller, golem.Options{Profile: golem.ProfileTrusted})
			if got, want := rt.EffectivePermissions().Tools, ownedReadPerms(); !slices.Equal(got, want) {
				t.Fatalf("initial Tools = %+v, want %+v", got, want)
			}

			// Mount the library's own write tools and dispatch beside an opaque
			// one. dispatch is floor-exempt, so it keeps its own ApprovalNever.
			spy := &profileSpyTool{name: "lookup", effect: agent.Effect{Class: agent.Read | agent.Network}}
			mounted := append(libraryWriteTools(t), realDispatch(t), spy)
			if err := apply[path](rt, caller, mounted); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			want := append(ownedReadPerms(),
				golem.ToolPermission{Name: "write_file", Class: agent.Write, Approval: agent.ApprovalOnWrite, Provenance: golem.ProvenanceLibrary},
				golem.ToolPermission{Name: "edit_file", Class: agent.Write, Approval: agent.ApprovalOnWrite, Provenance: golem.ProvenanceLibrary},
				golem.ToolPermission{Name: "dispatch", Class: agent.Read | agent.Network, Approval: agent.ApprovalNever, Provenance: golem.ProvenanceLibrary},
				golem.ToolPermission{Name: "lookup", Class: agent.Read | agent.Network, Approval: agent.ApprovalAlways, Provenance: golem.ProvenanceHostExtension},
			)
			got := rt.EffectivePermissions()
			if !slices.Equal(got.Tools, want) {
				t.Fatalf("Tools after %s = %+v, want %+v", path, got.Tools, want)
			}
			// The projection is published with the tools, so it names exactly
			// what the model is offered next.
			if seen := lastTurnToolNames(t, rt, caller, "after-mount"); !slices.Equal(seen, permissionNames(got)) {
				t.Fatalf("model saw %v, permissions list %v", seen, permissionNames(got))
			}

			// A rejected replacement installs nothing, projection included.
			if err := apply[path](rt, caller, []agent.Tool{nil}); err == nil {
				t.Fatalf("%s with a nil tool succeeded", path)
			}
			if after := rt.EffectivePermissions(); !slices.Equal(after.Tools, want) {
				t.Fatalf("Tools after a rejected %s = %+v, want %+v", path, after.Tools, want)
			}

			// Unmounting drops them again.
			if err := apply[path](rt, caller, nil); err != nil {
				t.Fatalf("%s(nil): %v", path, err)
			}
			if got, want := rt.EffectivePermissions().Tools, ownedReadPerms(); !slices.Equal(got, want) {
				t.Fatalf("Tools after unmount = %+v, want %+v", got, want)
			}
		})
	}
}

// The projection needs each tool's Effect, but a rejected batch must leave every
// tool in it untouched: Effect is read only once every check has passed.
func TestEffectivePermissionsReadsEffectOnlyAfterEveryCheck(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		names     [2]string
	}{
		{"outside the ceiling", []string{"read_file", "lookup"}, [2]string{"lookup", "outside"}},
		{"duplicate name", nil, [2]string{"lookup", "lookup"}},
	}
	for _, path := range admissionPaths {
		for _, tc := range tests {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				effect := agent.Effect{Class: agent.Read | agent.Network}
				first := &profileSpyTool{name: tc.names[0], effect: effect}
				second := &profileSpyTool{name: tc.names[1], effect: effect}
				_, _, err := admitVia(t, path, golem.Options{Profile: golem.ProfileTrusted, ToolAllowlist: tc.allowlist}, []agent.Tool{first, second})
				if err == nil {
					t.Fatalf("%s admitted a batch that must be rejected", path)
				}
				// first is valid on its own: only the batch is rejected, so it
				// is the tool a premature projection would have touched.
				for _, spy := range []*profileSpyTool{first, second} {
					if got := spy.effects.Load(); got != 0 {
						t.Errorf("%s Effect called %d time(s) in a rejected batch, want 0", spy.name, got)
					}
				}
			})
		}
	}
}
