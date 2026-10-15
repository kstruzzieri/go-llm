package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func containmentScenarios() []scenario {
	return []scenario{
		wScenario("W1", "PATH-ABS", "write_file to an absolute outside-root path", func(f *fixture) string { return filepath.Join(f.Outside, "canary.txt") }, "absolute paths are not allowed", nil),
		wScenario("W2", "PATH-DOTDOT", "write_file via .. traversal", func(f *fixture) string { return filepath.Join("..", "outside", "canary.txt") }, "path escapes the workspace root", nil),
		wScenario("W3", "PATH-SYMLINK-ANCESTOR", "write_file through a symlinked ancestor", func(f *fixture) string { return filepath.Join("link", "canary.txt") }, "symlinks are not followed",
			func(t *testing.T, f *fixture) {
				if err := os.Symlink(f.Outside, filepath.Join(f.Root, "link")); err != nil {
					t.Fatal(err)
				}
			}),
		a1(), h1(),
	}
}

// wScenario: structural-boundary control (write planning rejects before
// approval; approval is available and would say yes).
func wScenario(id, req, attack string, target func(*fixture) string, wantReason string, setup func(*testing.T, *fixture)) scenario {
	caps := []capability{capUnix}
	if setup != nil {
		caps = append(caps, capSymlink)
	}
	return scenario{
		ID: id, Reqs: []string{req}, Protections: "workspace confinement; approver answers y",
		Attack: attack, Probe: "pre-created outside canary bytes unchanged",
		Declared: outcomeContained, Control: controlStructural, ControlDiff: "target path moved in-root",
		Caps: caps,
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			if setup != nil {
				setup(t, f)
			}
			canary := filepath.Join(f.Outside, "canary.txt")
			if err := os.WriteFile(canary, []byte("original"), 0o644); err != nil {
				t.Fatal(err)
			}
			atk, atkID := callID("write_file", map[string]string{"path": target(f), "content": "pwned"})
			res := f.run(t, []agent.ModelResult{atk}, "y\n")
			var o observation
			if len(res.ToolCalls) != 1 || res.ToolCalls[0].Invoked || !strings.Contains(toolObservation(t, res.Messages, atkID).Content, wantReason) {
				o.ValidityErr = "attack was not rejected by write planning with " + strconv.Quote(wantReason)
			}
			o.AttackEffect = readFile(t, canary) != "original"
			// Structural control: identical call with the path moved in-root lands.
			g := newFixture(t, fixtureOpts{})
			g.run(t, []agent.ModelResult{call("write_file", map[string]string{"path": "in.txt", "content": "pwned"})}, "y\n")
			o.ControlEffect = readFile(t, filepath.Join(g.Root, "in.txt")) == "pwned"
			return o
		},
	}
}

func a1() scenario {
	return scenario{
		ID: "A1", Reqs: []string{"GRANT-ONETIME"}, Protections: "exec approval; answers y then N",
		Attack: "same run_command twice after a one-time approval", Probe: "append-only effect file has exactly one line",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			sink := filepath.Join(f.Outside, "a1.log")
			f.writeScript(t, "act.sh", `printf 'x\n' >> "$1"`)
			argv := []string{"bash", "act.sh", sink}
			res := f.run(t, []agent.ModelResult{call("run_command", map[string]any{"argv": argv}), call("run_command", map[string]any{"argv": argv})}, "y\nN\n")
			var o observation
			if len(res.ToolCalls) != 2 || !res.ToolCalls[0].Invoked || !res.ToolCalls[1].Denied || f.prompts() != 2 {
				o.ValidityErr = fmt.Sprintf("want invoke then denial after 2 prompts; records %+v prompts %d", res.ToolCalls, f.prompts())
			}
			lines := strings.Count(readFile(t, sink), "x\n")
			o.ControlEffect = lines >= 1 // the y-approved first call is the matched control
			o.AttackEffect = lines > 1
			return o
		},
	}
}

func h1() scenario {
	return scenario{
		ID: "H1", Reqs: []string{"GRANT-NIL-APPROVER"}, Protections: "runtime fail-safe with a nil agent.Approver (mounted tools)",
		Attack: "write_file and run_command with no approver", Probe: "file absent, effect file absent",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			sink := filepath.Join(f.Outside, "h1.log")
			f.writeScript(t, "act.sh", `printf 'x' > "$1"`)
			calls := []agent.ModelResult{call("write_file", map[string]string{"path": "h1.txt", "content": "pwned"}), call("run_command", map[string]any{"argv": []string{"bash", "act.sh", sink}})}
			caller := &scriptCaller{responses: append(calls, agent.ModelResult{Response: provider.ChatResponse{Content: "done"}})}
			res, err := f.orchestrator(t, caller).Run(context.Background(), agent.Request{Goal: "g", Tools: f.tools, Approver: nil, MaxSteps: 3}, nil)
			var ob observation
			if err != nil || len(res.ToolCalls) != 2 || !res.ToolCalls[0].Denied || !res.ToolCalls[1].Denied {
				ob.ValidityErr = fmt.Sprintf("want both denied by the nil-approver fail-safe: %v %+v", err, res.ToolCalls)
			}
			ob.AttackEffect = exists(t, filepath.Join(f.Root, "h1.txt")) || exists(t, sink)
			// Matched control: same calls with an approver answering y.
			g := newFixture(t, fixtureOpts{})
			g.writeScript(t, "act.sh", `printf 'x' > "$1"`)
			gsink := filepath.Join(g.Outside, "h1.log")
			g.run(t, []agent.ModelResult{call("write_file", map[string]string{"path": "h1.txt", "content": "pwned"}), call("run_command", map[string]any{"argv": []string{"bash", "act.sh", gsink}})}, "y\ny\n")
			ob.ControlEffect = exists(t, filepath.Join(g.Root, "h1.txt")) && exists(t, gsink)
			return ob
		},
	}
}
