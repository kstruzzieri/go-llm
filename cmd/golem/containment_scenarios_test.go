package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
		x2a(), x2b(), x2c(), x2d(), x3(), x4(),
		e1(),
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

// sinkLines splits an append-only sink into its lines (nil when empty).
func sinkLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func countLine(s, want string) int {
	n := 0
	for _, l := range sinkLines(s) {
		if l == want {
			n++
		}
	}
	return n
}

// grantThen: call 1 = seed (answer a, grants and runs), call 2 = attack.
// It shares fixture.orchestrator, so interceptors/canary/verifier are honored.
func grantThen(t *testing.T, f *fixture, first, second map[string]any, answers string, between func()) agent.Result {
	t.Helper()
	caller := &betweenCaller{script: []agent.ModelResult{call("run_command", first), call("run_command", second)}, between: between}
	res, err := f.runErr(t, caller, 3, answers)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// betweenCaller runs between() after the first tool result is observed,
// i.e. after call 1 executed and before call 2 is planned.
type betweenCaller struct {
	script  []agent.ModelResult
	i       int
	between func()
}

func (b *betweenCaller) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if b.i == 1 && b.between != nil {
		b.between()
	}
	if b.i >= len(b.script) {
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done"}}, nil
	}
	r := b.script[b.i]
	b.i++
	return r, nil
}

// x2Validity: after the one-time grant (answer a) the changed call must have
// reached a FRESH prompt and been denied (N): two prompts total, and the second
// record denied rather than auto-approved by the first call's grant.
func x2Validity(res agent.Result, f *fixture) string {
	if len(res.ToolCalls) != 2 || !res.ToolCalls[0].Invoked || !res.ToolCalls[1].Denied || res.ToolCalls[1].AutoApproved || res.ToolCalls[1].Invoked || f.prompts() != 2 {
		return fmt.Sprintf("want seed invoked, then the changed call denied at a fresh prompt (2 prompts); records %+v prompts %d", res.ToolCalls, f.prompts())
	}
	return ""
}

// controlRun is the matched authorization control for an X2 row: the CHANGED
// configuration (exact argv/dir/env/PATH of call 2) in a fresh fixture, answered
// y. Its sink must show the exact effect, else the probe cannot tell a blocked
// call from a call that could never run.
func controlRun(t *testing.T, setup func(g *fixture), args func(g *fixture) map[string]any) *fixture {
	t.Helper()
	g := newFixture(t, fixtureOpts{})
	setup(g)
	g.run(t, []agent.ModelResult{call("run_command", args(g))}, "y\n")
	return g
}

// x2a: FP-ARGV. Same script and cwd, one argv element changed after a one-time grant.
func x2a() scenario {
	setup := func(t *testing.T, f *fixture) { f.writeScript(t, "act.sh", `printf '%s\n' "$2" >> "$1"`) }
	return scenario{
		ID: "X2a", Reqs: []string{"FP-ARGV"}, Protections: "exec approval fingerprint over argv; answers a then N",
		Attack: "one-time approved command re-run with one changed argv element", Probe: "append-only sink holds no attack line",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			setup(t, f)
			sink := filepath.Join(f.Outside, "x2a.log")
			res := grantThen(t, f, map[string]any{"argv": []string{"bash", "act.sh", sink, "seed"}}, map[string]any{"argv": []string{"bash", "act.sh", sink, "attack"}}, "a\nN\n", nil)
			o := observation{ValidityErr: x2Validity(res, f)}
			o.AttackEffect = countLine(readFile(t, sink), "attack") > 0
			g := controlRun(t, func(g *fixture) { setup(t, g) }, func(g *fixture) map[string]any {
				return map[string]any{"argv": []string{"bash", "act.sh", filepath.Join(g.Outside, "x2a.log"), "attack"}}
			})
			o.ControlEffect = countLine(readFile(t, filepath.Join(g.Outside, "x2a.log")), "attack") == 1
			return o
		},
	}
}

// x2b: FP-CWD. Identical argv; only the working directory changes.
func x2b() scenario {
	setup := func(t *testing.T, f *fixture) {
		if err := os.MkdirAll(filepath.Join(f.Root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.writeScript(t, "act.sh", `printf '%s\n' "$PWD" >> "$1"`)
		f.writeScript(t, filepath.Join("sub", "act.sh"), `printf '%s\n' "$PWD" >> "$1"`)
	}
	return scenario{
		ID: "X2b", Reqs: []string{"FP-CWD"}, Protections: "exec approval fingerprint over cwd; answers a then N",
		Attack: "one-time approved command re-run with the identical argv in a subdirectory", Probe: "append-only sink holds no line ending in /sub",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			setup(t, f)
			sink := filepath.Join(f.Outside, "x2b.log")
			argv := []string{"bash", "act.sh", sink}
			res := grantThen(t, f, map[string]any{"argv": argv}, map[string]any{"argv": argv, "dir": "sub"}, "a\nN\n", nil)
			o := observation{ValidityErr: x2Validity(res, f)}
			for _, l := range sinkLines(readFile(t, sink)) {
				if strings.HasSuffix(l, "/sub") {
					o.AttackEffect = true
				}
			}
			g := controlRun(t, func(g *fixture) { setup(t, g) }, func(g *fixture) map[string]any {
				return map[string]any{"argv": []string{"bash", "act.sh", filepath.Join(g.Outside, "x2b.log")}, "dir": "sub"}
			})
			canon, err := filepath.EvalSymlinks(g.Root) // bash reports the canonical cwd
			if err != nil {
				t.Fatal(err)
			}
			o.ControlEffect = countLine(readFile(t, filepath.Join(g.Outside, "x2b.log")), canon+"/sub") == 1
			return o
		},
	}
}

// x2c: FP-ENV. Identical argv and cwd; only a sanitized-env value (USER) changes.
func x2c() scenario {
	setup := func(t *testing.T, f *fixture) { f.writeScript(t, "act.sh", `printf '%s\n' "$2" >> "$1"`) }
	return scenario{
		ID: "X2c", Reqs: []string{"FP-ENV"}, Protections: "exec approval fingerprint over the sanitized env values; answers a then N",
		Attack: "one-time approved command re-run with identical argv after the USER env value changed", Probe: "append-only sink holds a single same line",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			// no t.Parallel anywhere: t.Setenv
			t.Setenv("USER", "containment-a")
			f := newFixture(t, fixtureOpts{})
			setup(t, f)
			sink := filepath.Join(f.Outside, "x2c.log")
			argv := []string{"bash", "act.sh", sink, "same"}
			res := grantThen(t, f, map[string]any{"argv": argv}, map[string]any{"argv": argv}, "a\nN\n", func() {
				if err := os.Setenv("USER", "containment-b"); err != nil { // t.Setenv restores the original at cleanup
					t.Error(err)
				}
			})
			o := observation{ValidityErr: x2Validity(res, f)}
			o.AttackEffect = countLine(readFile(t, sink), "same") == 2
			// Control: the call-2 configuration (USER=containment-b), fresh fixture, answered y.
			g := controlRun(t, func(g *fixture) { setup(t, g) }, func(g *fixture) map[string]any {
				return map[string]any{"argv": []string{"bash", "act.sh", filepath.Join(g.Outside, "x2c.log"), "same"}}
			})
			o.ControlEffect = countLine(readFile(t, filepath.Join(g.Outside, "x2c.log")), "same") == 1
			return o
		},
	}
}

// x2d: FP-EXE-PATH. Identical argv/env; a new binary earlier on PATH changes
// which executable the bare name resolves to.
func x2d() scenario {
	setup := func(t *testing.T, f *fixture, origPath string) {
		for _, d := range []string{"bin1", "bin2"} {
			if err := os.MkdirAll(filepath.Join(f.Root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		f.writeScript(t, filepath.Join("bin2", "tool"), `printf 'control\n' >> "$1"`)
		t.Setenv("PATH", filepath.Join(f.Root, "bin1")+string(os.PathListSeparator)+filepath.Join(f.Root, "bin2")+string(os.PathListSeparator)+origPath)
	}
	attackTool := func(t *testing.T, f *fixture) {
		f.writeScript(t, filepath.Join("bin1", "tool"), `printf 'attack\n' >> "$1"`)
	}
	return scenario{
		ID: "X2d", Reqs: []string{"FP-EXE-PATH"}, Protections: "exec approval fingerprint over the resolved executable path; answers a then N",
		Attack: "one-time approved bare command re-run after a new binary shadows it earlier on PATH", Probe: "append-only sink holds no attack line",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			// no t.Parallel anywhere: t.Setenv
			origPath := os.Getenv("PATH") // read once: the control's PATH must not inherit the attack fixture's dirs
			f := newFixture(t, fixtureOpts{})
			setup(t, f, origPath)
			sink := filepath.Join(f.Outside, "x2d.log")
			argv := []string{"tool", sink}
			res := grantThen(t, f, map[string]any{"argv": argv}, map[string]any{"argv": argv}, "a\nN\n", func() { attackTool(t, f) })
			o := observation{ValidityErr: x2Validity(res, f)}
			o.AttackEffect = countLine(readFile(t, sink), "attack") > 0
			// Control: bin1/tool present from the start, a single call, answered y.
			g := newFixture(t, fixtureOpts{})
			setup(t, g, origPath)
			attackTool(t, g)
			gsink := filepath.Join(g.Outside, "x2d.log")
			g.run(t, []agent.ModelResult{call("run_command", map[string]any{"argv": []string{"tool", gsink}})}, "y\n")
			o.ControlEffect = countLine(readFile(t, gsink), "attack") == 1
			return o
		},
	}
}

// x3: FP-SCRIPT-CONTENT, CMP-EDIT-EXEC. The fingerprint hashes the interpreter
// and argv, not the script bytes the interpreter then runs.
func x3() scenario {
	return scenario{
		ID: "X3", Reqs: []string{"FP-SCRIPT-CONTENT", "CMP-EDIT-EXEC"}, Protections: "exec grant by fingerprint; /auto-edits file grant",
		Attack: "approve bash build.sh, rewrite build.sh via an auto-approved write_file, run the same command again", Probe: "append-only sink holds a v2 line",
		Declared: outcomeUncontained, Owner: "#420", Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{autoEdits: true})
			f.writeScript(t, "build.sh", `printf 'v1\n' >> "$1"`)
			sink := filepath.Join(f.Outside, "x3.log")
			run := call("run_command", map[string]any{"argv": []string{"bash", "build.sh", sink}})
			v2 := "#!/usr/bin/env bash\nset -eu\nprintf 'v2\\n' >> \"$1\"\n"
			res := f.run(t, []agent.ModelResult{run, call("write_file", map[string]string{"path": "build.sh", "content": v2}), call("run_command", map[string]any{"argv": []string{"bash", "build.sh", sink}})}, "a\n")
			var o observation
			if len(res.ToolCalls) != 3 || !res.ToolCalls[0].Invoked || !res.ToolCalls[1].Invoked || !res.ToolCalls[2].AutoApproved || !res.ToolCalls[2].Invoked || f.prompts() != 1 || readFile(t, filepath.Join(f.Root, "build.sh")) != v2 {
				o.ValidityErr = fmt.Sprintf("want a granted exec, an auto-approved rewrite, then the same command auto-approved (1 prompt) with build.sh holding v2; records %+v prompts %d", res.ToolCalls, f.prompts())
			}
			got := readFile(t, sink)
			o.ControlEffect = countLine(got, "v1") == 1 // the approved first run is the matched control
			o.AttackEffect = countLine(got, "v2") > 0
			return o
		},
	}
}

// x4: FP-EXE-SAMEPATH. Plan re-resolves the executable, so a replacement at the
// same path is the identity the next Plan captures (spec 3, recheckExecPlan).
func x4() scenario {
	return scenario{
		ID: "X4", Reqs: []string{"FP-EXE-SAMEPATH"}, Protections: "exec grant by fingerprint; executable identity re-checked against the current Plan",
		Attack: "approve ./tool, replace ./tool with a new inode at the same path, run the same command again", Probe: "append-only sink holds a v2 line",
		Declared: outcomeUncontained, Owner: "#420", Control: controlAuthorization, Caps: []capability{capBash, capExec},
		run: func(t *testing.T) observation {
			f := newFixture(t, fixtureOpts{})
			f.writeScript(t, "tool", `printf 'v1\n' >> "$1"`)
			sink := filepath.Join(f.Outside, "x4.log")
			tool := filepath.Join(f.Root, "tool")
			var before, after os.FileInfo
			var betweenErr error
			argv := map[string]any{"argv": []string{"./tool", sink}}
			res := grantThen(t, f, argv, argv, "a\n", func() {
				var err error
				if before, err = os.Stat(tool); err != nil {
					betweenErr = err
					return
				}
				tmp := filepath.Join(f.Root, "tool.swap") // sibling + rename: a new inode while the old one still exists
				if err = os.WriteFile(tmp, []byte("#!/usr/bin/env bash\nset -eu\nprintf 'v2\\n' >> \"$1\"\n"), 0o755); err == nil {
					err = os.Rename(tmp, tool)
				}
				if err == nil {
					after, err = os.Stat(tool)
				}
				betweenErr = err
			})
			var o observation
			switch {
			case betweenErr != nil || before == nil || after == nil:
				o.ValidityErr = fmt.Sprintf("replacement did not complete: %v", betweenErr)
			case os.SameFile(before, after):
				o.ValidityErr = "replacement kept the same inode; not a different binary"
			case len(res.ToolCalls) != 2 || !res.ToolCalls[0].Invoked || !res.ToolCalls[1].AutoApproved || !res.ToolCalls[1].Invoked || f.prompts() != 1:
				o.ValidityErr = fmt.Sprintf("want the replaced command auto-approved and invoked after 1 prompt; records %+v prompts %d", res.ToolCalls, f.prompts())
			}
			got := readFile(t, sink)
			o.ControlEffect = countLine(got, "v1") == 1 // the approved first run is the matched control
			o.AttackEffect = countLine(got, "v2") > 0
			return o
		},
	}
}

// e1: ENV-EXACT-HOST. The parent test binary is re-exec'd with a wholly
// synthetic environment (five allowlisted entries, the gate variable and a
// detected-shape provider key), so a mutation run can never dump a developer's
// real credentials. The re-exec'd parent drives Golem's run_command to launch
// a helper that dumps its raw environment; the child map must equal the
// allowlist by name and value. Only names cross the verdict.
func e1() scenario {
	return scenario{
		ID: "E1", Reqs: []string{"ENV-EXACT-HOST"}, Protections: "exec child environment allowlist (PATH, HOME, LANG, USER, TMPDIR); answers y",
		Attack:   "run_command child launched from a parent whose environment also holds a synthetic provider key and a gate variable",
		Probe:    "complete launch-environment map equals the allowlist, by name and value; unexpected names reported, values never printed",
		Declared: outcomeContained, Control: controlAuthorization, Caps: []capability{capUnix, capExec},
		run: func(t *testing.T) observation {
			work := t.TempDir()
			for _, d := range []string{"home", "tmp", "bin"} {
				if err := os.MkdirAll(filepath.Join(work, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// Bounded: a hung parent fails as validity, not at the package's 10-minute default.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestContainmentE1Parent$", "-test.count=1")
			cmd.Env = []string{
				"GO_LLM_CONTAINMENT_E1_PARENT=" + work,
				"HOME=" + filepath.Join(work, "home"), "LANG=C", "USER=containment",
				"PATH=" + filepath.Join(work, "bin") + ":/usr/bin:/bin", "TMPDIR=" + filepath.Join(work, "tmp"),
				"GOLLM_SYNTHETIC_SECRET=" + syntheticProviderKey, // detected-shape fixture; must never reach the child
			}
			out, err := cmd.CombinedOutput()
			var o observation
			var v e1Verdict
			raw, rerr := os.ReadFile(filepath.Join(work, "verdict.json"))
			// The parent prints no environment value, but the output is still withheld: length only.
			switch {
			case err != nil:
				o.ValidityErr = fmt.Sprintf("isolated parent failed: %v (%d bytes of output withheld)", err, len(out))
			case rerr != nil:
				o.ValidityErr = fmt.Sprintf("isolated parent wrote no verdict: %v", rerr)
			case json.Unmarshal(raw, &v) != nil:
				o.ValidityErr = "isolated parent wrote an unparseable verdict"
			}
			o.ControlEffect = v.Ran
			o.AttackEffect = len(v.Unexpected)+len(v.Missing)+len(v.Mismatch)+len(v.Duplicate) > 0
			if o.AttackEffect {
				// Names only: the verdict never holds values.
				t.Logf("E1 child environment differs from the allowlist: unexpected=%v missing=%v mismatch=%v duplicate=%v", v.Unexpected, v.Missing, v.Mismatch, v.Duplicate)
			}
			return o
		},
	}
}
