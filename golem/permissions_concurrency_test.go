package golem

// This test lives in package golem because it reads the published snapshot
// (r.snap) under r.mu: the projection is only meaningful beside the tools it
// describes, and the public API shows the projection alone.

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// concurrentReads is the four runtime-owned reads every snapshot starts with,
// spelled out as literals.
func concurrentReads() []ToolPermission {
	perms := make([]ToolPermission, 0, 6)
	for _, name := range []string{"read_file", "search", "glob", "list"} {
		perms = append(perms, ToolPermission{Name: name, Class: agent.Read, Approval: agent.ApprovalNever, Provenance: ProvenanceRuntime})
	}
	return perms
}

// A reader must never see a projection that does not belong to one published
// tool set. Replacements alternate between two literal sets while readers hammer
// EffectivePermissions: each result must be exactly one of the two lists, and the
// snapshot a reserved turn would hold must carry the projection of its own
// tools, not of the previous publication. Run under -race, which is also what
// exposes an unlocked read.
func TestEffectivePermissionsConcurrentWithReplace(t *testing.T) {
	const (
		replacements = 1001 // odd: the last publication is set B
		readers      = 4
	)
	namesA := []string{"read_file", "search", "glob", "list", "alpha", "beta"}
	namesB := []string{"read_file", "search", "glob", "list", "write_file", "gamma"}
	wantA := append(concurrentReads(),
		ToolPermission{Name: "alpha", Class: agent.Read | agent.Network, Approval: agent.ApprovalAlways, Provenance: ProvenanceHostExtension},
		ToolPermission{Name: "beta", Class: agent.Read, Approval: agent.ApprovalNever, Provenance: ProvenanceHostExtension},
	)
	wantB := append(concurrentReads(),
		ToolPermission{Name: "write_file", Class: agent.Write, Approval: agent.ApprovalOnWrite, Provenance: ProvenanceLibrary},
		ToolPermission{Name: "gamma", Class: agent.Exec, Approval: agent.ApprovalAlways, Provenance: ProvenanceHostExtension},
	)

	for _, path := range []string{"Replace", "ReplaceConfiguration"} {
		t.Run(path, func(t *testing.T) {
			ws, err := agenttools.NewWorkspace(t.TempDir())
			if err != nil {
				t.Fatalf("NewWorkspace: %v", err)
			}
			setA := []agent.Tool{
				&floorStub{name: "alpha", effect: agent.Effect{Class: agent.Read | agent.Network}},
				&floorStub{name: "beta", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}},
			}
			setB := []agent.Tool{
				agenttools.NewWriteFile(ws, nil),
				&floorStub{name: "gamma", effect: agent.Effect{Class: agent.Exec, Approval: agent.ApprovalNever}},
			}
			orch := agent.New(&oneToolCaller{}, agent.ContextManager{})
			rt, err := New(context.Background(), Options{Root: t.TempDir(), Profile: ProfileTrusted, Tools: setA, Orchestrator: orch})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			t.Cleanup(func() { closeTestRuntime(t, rt) })

			var failOnce sync.Once
			var failed atomic.Bool
			fail := func(format string, args ...any) {
				failed.Store(true)
				failOnce.Do(func() { t.Errorf(format, args...) })
			}
			// check validates one observation of both the public snapshot and the
			// published tools beside their projection.
			check := func() {
				got := rt.EffectivePermissions().Tools
				if !slices.Equal(got, wantA) && !slices.Equal(got, wantB) {
					fail("EffectivePermissions Tools = %+v, want exactly one of the two published sets", got)
					return
				}
				// The same critical section must hand out tools and projection that
				// belong together.
				rt.mu.Lock()
				tools, perms := rt.snap.tools, rt.snap.permissions
				rt.mu.Unlock()
				names := make([]string, len(tools))
				for i, tool := range tools {
					names[i] = tool.Spec().Name
				}
				want := wantA
				switch {
				case slices.Equal(names, namesA):
				case slices.Equal(names, namesB):
					want = wantB
				default:
					fail("published tools = %v, want one of the two sets", names)
					return
				}
				if !slices.Equal(perms, want) {
					fail("published tools %v carry projection %+v, want %+v", names, perms, want)
				}
			}

			var ready, done sync.WaitGroup
			var writerDone atomic.Bool
			start := make(chan struct{})
			for range readers {
				ready.Add(1)
				done.Add(1)
				go func() {
					defer done.Done()
					// Before any replacement the runtime holds set A.
					if got := rt.EffectivePermissions().Tools; !slices.Equal(got, wantA) {
						fail("initial Tools = %+v, want set A %+v", got, wantA)
					}
					ready.Done()
					<-start
					for !writerDone.Load() && !failed.Load() {
						check()
					}
					// Once the writer has finished, the final publication is visible.
					if got := rt.EffectivePermissions().Tools; !slices.Equal(got, wantB) {
						fail("final Tools = %+v, want set B %+v", got, wantB)
					}
				}()
			}
			ready.Wait()
			close(start)
			for i := 0; i < replacements && !failed.Load(); i++ {
				tools := setB
				if i%2 == 1 {
					tools = setA
				}
				var err error
				if path == "Replace" {
					err = rt.Replace("", tools)
				} else {
					err = rt.ReplaceConfiguration(context.Background(), Configuration{Tools: tools, Orchestrator: orch})
				}
				if err != nil {
					fail("%s #%d: %v", path, i, err)
				}
			}
			writerDone.Store(true)
			done.Wait()
		})
	}
}
