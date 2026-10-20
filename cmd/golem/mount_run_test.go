package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// startupShape is everything a mount must reproduce from the equivalent
// startup flags: ordered tool names (hence toolSchemaHash), the composed
// prompt, and the capability state. Values only — never the journal,
// manager, or verifier slot themselves, which are independently allocated
// per run (the journal holds an identity-sensitive channel).
type startupShape struct {
	names      []string
	system     string
	hash       string
	write      bool
	exec       bool
	hasJournal bool
	hasManager bool
}

// captureStartup runs golem to the point where the session exists, applies
// commands, snapshots the shape, and stops. -agent-memory keeps a tool AFTER
// the gated slot (agent-memory tools) and memory_search sits BEFORE it, so
// an append-instead-of-insert mutation is visible. The verifier slot is not
// part of the parity shape; TestVerifierSlotIsBoundForEveryREPLSession pins it.
func captureStartup(t *testing.T, extra []string, commands ...string) startupShape {
	t.Helper()
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	args := append([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-project-context", "-no-auto-index", "-agent-memory"}, extra...)
	errStop := errors.New("stop after startup")
	var got startupShape
	err := run(args, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			// The hook stands in for a command typed at a real terminal; the
			// test files themselves are deliberately plain descriptors.
			sess.stdinTerminal = true
			var out strings.Builder
			for _, c := range commands {
				_, _ = dispatchSlash(context.Background(), &out, sess, c)
			}
			if strings.Contains(out.String(), "not enabled") {
				return fmt.Errorf("mount failed: %s", out.String())
			}
			if sess.baseSystem != composeSystem(sess.sysInputs) {
				return errors.New("baseSystem != composeSystem(sysInputs)")
			}
			got = startupShape{
				names: names(sess.tools), system: sess.baseSystem, hash: toolSchemaHash(sess.tools),
				write: sess.allowWrite, exec: sess.allowExec,
				hasJournal: sess.journal != nil, hasManager: sess.bgManager != nil,
			}
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
	return got
}

func TestAllowCommandsMatchStartupFlags(t *testing.T) {
	delegate := []string{"-delegate", "-delegate-role", "agent"}
	for _, tc := range []struct {
		name     string
		startup  []string // flags for the reference run
		mid      []string // flags for the run that then mounts via commands
		commands []string
	}{
		{"write", []string{"-allow-write"}, nil, []string{"/allow-write"}},
		{"exec", []string{"-allow-exec"}, nil, []string{"/allow-exec"}},
		{"both, mounted in reverse order", []string{"-allow-write", "-allow-exec"}, nil, []string{"/allow-exec", "/allow-write"}},
		// Startup writes then a late exec: pins writeToolCount recorded at
		// startup (exec must land after the startup write tools).
		{"startup write, mid exec", []string{"-allow-write", "-allow-exec"}, []string{"-allow-write"}, []string{"/allow-exec"}},
		// A delegate tool sits between the gated slot and the agent-memory
		// tail: pins mountAt against the delegate append.
		{"write with delegate in the tail", append(append([]string(nil), delegate...), "-allow-write"), delegate, []string{"/allow-write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startup := captureStartup(t, tc.startup)
			mid := captureStartup(t, tc.mid, tc.commands...)
			if !reflect.DeepEqual(startup, mid) {
				t.Fatalf("mid-session shape differs from startup flags:\n startup=%+v\n mid=%+v", startup, mid)
			}
			last := startup.names[len(startup.names)-1]
			if last == "stop_command" || last == "edit_file" {
				t.Fatalf("no tool after the gated slot (last=%s); the order pin is vacuous", last)
			}
		})
	}
}

// The default profile is Inspect, where a late mount is refused before the
// terminal requirement is even consulted: run() must carry the resolved profile
// into the session handlers read (#387).
func TestRunRefusesAllowCommandsInDefaultInspect(t *testing.T) {
	for _, command := range []string{"/allow-write", "/allow-exec"} {
		t.Run(command, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			stdin, stdout, stderr := stdinFileWith(t, command+"\n")
			var sess *replSession
			err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
				"-no-session", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index"},
				stdin, stdout, stderr, runHooks{
					afterSessionReady: func(ready *replSession) error {
						sess = ready
						return nil
					},
				})
			if err != nil {
				t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
			}
			if sess == nil || sess.allowWrite || sess.allowExec {
				t.Fatalf("session = %+v, want captured with no capability enabled", sess)
			}
			want := command + " is unavailable in profile inspect: restart with -profile trusted -" + strings.TrimPrefix(command, "/") + "\n"
			if got := readRunTestFile(t, stdout); !strings.Contains(got, want) {
				t.Fatalf("stdout = %q, want it to contain %q", got, want)
			}
		})
	}
}

func TestRunRejectsAllowCommandsFromPipedInput(t *testing.T) {
	for _, command := range []string{"/allow-write", "/allow-exec"} {
		t.Run(command, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			stdin, stdout, stderr := stdinFileWith(t, command+"\n")
			var sess *replSession
			err := run([]string{"-profile", "trusted", "-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
				"-no-session", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index"},
				stdin, stdout, stderr, runHooks{
					afterSessionReady: func(ready *replSession) error {
						sess = ready
						return nil
					},
				})
			if err != nil {
				t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
			}
			if sess == nil {
				t.Fatal("session was not captured")
			}
			if sess.allowWrite || sess.allowExec {
				t.Fatalf("%s from piped input enabled capabilities: write=%t exec=%t",
					command, sess.allowWrite, sess.allowExec)
			}
			if got := readRunTestFile(t, stdout); !strings.Contains(got, command+" requires an interactive terminal") {
				t.Fatalf("stdout = %q, want interactive-terminal denial", got)
			}
		})
	}
}

// The slot exists for every REPL session, and only there (#576): a REPL
// session that starts without -allow-write carries it empty for a later
// /allow-write (Inspect included, which never mounts writes), a startup
// -allow-write session binds its verifier through the same root- and
// policy-bound slot (armed, or disabled with the notice), and one-shot never
// dispatches slash commands or mounts verification. The orchestrator verifies
// through exactly that slot, never a separate runner.
func TestVerifierSlotIsBoundForEveryREPLSession(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"verify":{"argv":["` + self + `","__golem_exec_helper__","echo","ok"]}}`
	for _, tc := range []struct {
		name  string
		body  string
		extra []string
		state string // "" means no slot at all
	}{
		// The config is present but unread: .golem.json is read only once
		// writes are enabled.
		{"REPL without -allow-write", valid, nil, "absent"},
		{"REPL with -allow-write and a valid config", valid, []string{"-allow-write"}, "armed"},
		{"REPL with -allow-write and a malformed config", `{"verify":`, []string{"-allow-write"}, "disabled"},
		{"one-shot", valid, []string{"-p", "hello"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			writeGolemJSON(t, root, tc.body)
			canonRoot, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop after startup")
			var slot *agenttools.VerifierSlot
			var installed agent.Verifier
			args := append([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
				"-no-memory", "-no-project-context", "-no-auto-index"}, tc.extra...)
			err = run(args, stdin, stdout, stderr, runHooks{
				startAutoIndex: func() func() { return func() {} },
				afterSessionReady: func(sess *replSession) error {
					slot, installed = sess.verifier, sess.orch.Verifier()
					return errStop
				},
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
			}
			if tc.state == "" {
				if slot != nil || installed != nil {
					t.Fatalf("one-shot carries slot %v and orchestrator verifier %v; want neither", slot, installed)
				}
				return
			}
			if slot == nil {
				t.Fatal("REPL session has no verifier slot")
			}
			if installed != agent.Verifier(slot) {
				t.Fatalf("orchestrator verifier = %#v, want the session's slot", installed)
			}
			if state, target := slot.State(); state != tc.state || target != canonRoot {
				t.Fatalf("slot State = %q, %q; want %q, %q", state, target, tc.state, canonRoot)
			}
			if !slot.BoundTo(canonRoot, agenttools.SandboxConfig{}) {
				t.Fatal("slot is not bound to the session root and the host policy")
			}
		})
	}
}

// main.go must feed the loaded project-context block into the composed
// prompt; the startup notice alone does not prove the model sees it.
func TestStartupComposesProjectContextIntoSystem(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("repo rule: keep it short\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after startup")
	var system, block string
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
		"-no-memory", "-no-auto-index", "-trust-project-context", trustFixtureDigest(t, root)}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			system, block = sess.baseSystem, sess.sysInputs.projectContext
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
	if block == "" || !strings.Contains(block, "repo rule") {
		t.Fatalf("project context not retained on the session: %q", block)
	}
	if !strings.Contains(system, projectContextOpen) || !strings.Contains(system, "repo rule") {
		t.Fatalf("project context missing from the composed prompt:\n%s", system)
	}
}

// Late mounts are released after the runtime closes (LIFO slot pinned by the
// closed-hook order) and the workspace lease is free once run returns.
func TestRunReleasesLateMountsAfterRuntimeClose(t *testing.T) {
	// No GC during the test: a leaked store would otherwise be finalized
	// (flock released) once run returns, masking a missing closeLateMounts.
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after startup")
	var events []string
	// A late mount needs the Trusted ceiling (#387).
	err := run([]string{"-config", configPath, "-root", root, "-profile", "trusted", "-no-probe", "-no-cap-probe", "-no-session",
		"-no-memory", "-no-project-context", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			sess.stdinTerminal = true // this hook simulates a live REPL command
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
			if !strings.Contains(out.String(), "writes enabled") {
				return fmt.Errorf("mount failed: %s", out.String())
			}
			return errStop
		},
		closed: func(name string) { events = append(events, name) },
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
	runtimeAt, lateAt := -1, -1
	for i, e := range events {
		switch e {
		case "runtime":
			runtimeAt = i
		case "late-mounts":
			lateAt = i
		}
	}
	if runtimeAt < 0 || lateAt < 0 || lateAt < runtimeAt {
		t.Fatalf("late mounts must be released after the runtime closes; events = %v", events)
	}
	// The canonical root is what the session used; the lease must be free.
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openCheckpointStore(context.Background(), os.Getenv, canonical)
	if err != nil {
		t.Fatalf("late checkpoint lease still held after run returned: %v", err)
	}
	_ = store.Close()
}

// A read-only invocation must not even create the signing directory. Startup
// writes and late writes must both use the persistent identity and surface it.
func TestRunMutationSigningLifecycle(t *testing.T) {
	for _, mode := range []string{"read-only", "startup", "late"} {
		t.Run(mode, func(t *testing.T) {
			configPath, root, _ := fenceWireHarness(t, func(req wireRequest) []string {
				if !req.hasToolMessage() {
					return sseToolCall("write", "write_file", `{"path":"a.txt","content":"abc"}`)
				}
				return sseAnswer("done")
			})
			data := os.Getenv("XDG_DATA_HOME")
			// Every mode keeps the pre-#387 host Trusted session: the late
			// mount needs its ceiling, and read-only stays the same session
			// minus writes.
			args := []string{"-config", configPath, "-root", root, "-profile", "trusted", "-no-probe", "-no-cap-probe",
				"-no-rag", "-no-session", "-no-memory", "-no-project-context", "-no-auto-index"}
			if mode == "startup" {
				args = append(args, "-allow-write")
			}
			var mountOut strings.Builder
			stdin, stdout, stderr := runTestFiles(t)
			err := run(args, stdin, stdout, stderr, runHooks{
				afterSessionReady: func(sess *replSession) error {
					if mode == "read-only" {
						if sess.journal != nil || sess.allowWrite {
							t.Error("read-only startup enabled writes")
						}
						return nil
					}
					if mode == "late" {
						sess.stdinTerminal = true
						_, _ = dispatchSlash(context.Background(), &mountOut, sess, "/allow-write")
						if !sess.allowWrite {
							t.Fatalf("late writes did not mount: %s", mountOut.String())
						}
					}
					_, err := runOnce(context.Background(), &mountOut, nil, sess, "write abc", &stubAnswerSource{line: "y", ok: true})
					return err
				},
			})
			if err != nil {
				t.Fatalf("%s startup = %v; stderr: %s", mode, err, readRunTestFile(t, stderr))
			}
			output := mountOut.String() + readRunTestFile(t, stdout) + readRunTestFile(t, stderr)
			if mode == "read-only" {
				if _, err := os.Lstat(filepath.Join(data, "golem", "signing")); !os.IsNotExist(err) || strings.Contains(output, "new signing identity") {
					t.Fatalf("read-only startup touched signing: %v; output: %s", err, output)
				}
				return
			}
			receipts := readHostMutationReceipts(t, data, root)
			if len(receipts) != 1 || receipts[0].Body.Path != "a.txt" || receipts[0].Body.BeforeHash != "absent" || receipts[0].Body.AfterHash != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
				t.Fatalf("%s persisted receipts = %+v, want one abc create", mode, receipts)
			}
			wantNotice := "new signing identity: kid " + receipts[0].Body.AgentID + " (key file " + filepath.Join(data, "golem", "signing", "agent-ed25519.pem") + ")"
			if strings.Count(output, wantNotice) != 1 {
				t.Fatalf("%s output lacks one identity notice %q: %s", mode, wantNotice, output)
			}
			if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "abc" {
				t.Fatalf("%s write = %q, %v; want abc", mode, got, err)
			}
		})
	}
}

func TestRunMutationSigningScratchPromotion(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	data := os.Getenv("XDG_DATA_HOME")
	stdin, stdout, stderr := runTestFiles(t)
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-rag",
		"-no-session", "-no-memory", "-no-project-context", "-no-auto-index", "-allow-exec", "-scratch", "-allow-write"},
		stdin, stdout, stderr, runHooks{afterSessionReady: func(sess *replSession) error {
			result := applyTool(t, sess.tools, "run_command", map[string]any{"argv": []string{"/bin/sh", "-c", "printf abc > artifact.txt; chmod 640 artifact.txt"}})
			fields := strings.Fields(result.Content)
			if result.IsError || len(fields) < 2 || !strings.HasPrefix(fields[1], "id=scr-") {
				t.Fatalf("startup scratch capture = %+v", result)
			}
			if _, err := os.Lstat(filepath.Join(root, "artifact.txt")); !os.IsNotExist(err) {
				t.Fatalf("scratch command changed workspace before promotion: %v", err)
			}
			beginTestTurn(t, sess.journal, "promote")
			result = applyTool(t, sess.tools, "promote_artifact", map[string]any{"id": strings.TrimPrefix(fields[1], "id="), "path": "artifact.txt"})
			if result.IsError {
				t.Fatalf("startup promotion = %+v", result)
			}
			mustSealTurn(t, sess.journal)
			return nil
		}})
	if err != nil {
		t.Fatalf("scratch startup = %v; stderr: %s", err, readRunTestFile(t, stderr))
	}
	receipts := readHostMutationReceipts(t, data, root)
	if len(receipts) != 1 {
		t.Fatalf("scratch promotion persisted %d receipts, want 1", len(receipts))
	}
	body := receipts[0].Body
	if body.Path != "artifact.txt" || body.BeforeHash != "absent" || body.AfterHash != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" || body.AfterMode == nil || *body.AfterMode != 0640 || body.UndoOf != "" {
		t.Fatalf("startup promotion receipt = %+v", body)
	}
}

func TestRunAllowWritePreservesScratchIdentity(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-rag",
		"-no-session", "-no-memory", "-no-project-context", "-no-auto-index", "-allow-exec", "-scratch"},
		stdin, stdout, stderr, runHooks{afterSessionReady: func(sess *replSession) error {
			sess.stdinTerminal = true
			runtime, manager := sess.runtime, sess.bgManager
			exec := toolByName(t, sess.tools, "run_command").(agent.PlanningTool)
			args := json.RawMessage(`{"argv":["/bin/sh","-c","printf abc > retained.txt"]}`)
			before, err := exec.Plan(context.Background(), args)
			if err != nil || !strings.Contains(before.ApprovalKey, "scr:") {
				t.Fatalf("scratch plan lacks approval identity: %v", err)
			}
			result := applyTool(t, sess.tools, "run_command", map[string]any{"argv": []string{"/bin/sh", "-c", "printf abc > retained.txt"}})
			fields := strings.Fields(result.Content)
			if result.IsError || len(fields) < 2 || !strings.HasPrefix(fields[1], "id=scr-") {
				t.Fatalf("scratch capture = %+v", result)
			}
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/allow-write")
			if !sess.allowWrite || !strings.Contains(out.String(), "scratch: promote_artifact stays unavailable") {
				t.Fatalf("late scratch mount = %q", out.String())
			}
			if sess.runtime != runtime || sess.bgManager != manager || strings.Contains(strings.Join(names(sess.tools), ","), "promote_artifact") {
				t.Fatal("late writes rebuilt scratch runtime/manager or enabled promotion")
			}
			after, err := toolByName(t, sess.tools, "run_command").(agent.PlanningTool).Plan(context.Background(), args)
			if err != nil || after.ApprovalKey != before.ApprovalKey {
				t.Fatalf("late writes changed scratch approval identity: %v", err)
			}
			// A command-only rebuild can keep the same approval key while
			// disconnecting new captures from the existing query tool.
			fresh := applyTool(t, sess.tools, "run_command", map[string]any{"argv": []string{"/bin/sh", "-c", "printf abc > retained.txt"}})
			freshFields := strings.Fields(fresh.Content)
			if fresh.IsError || len(freshFields) < 2 || !strings.HasPrefix(freshFields[1], "id=scr-") {
				t.Fatalf("post-mount scratch capture = %+v", fresh)
			}
			query := toolByName(t, sess.tools, "scratch_changes")
			for _, capture := range []string{fields[1], freshFields[1]} {
				raw, _ := json.Marshal(map[string]string{"id": strings.TrimPrefix(capture, "id=")})
				changes, err := query.Invoke(context.Background(), raw)
				if err != nil || changes.IsError || !strings.Contains(changes.Content, "retained.txt") {
					t.Fatalf("late writes lost scratch capture %s: %+v, %v", capture, changes, err)
				}
			}
			return nil
		}})
	if err != nil {
		t.Fatalf("late scratch startup = %v; stderr: %s", err, readRunTestFile(t, stderr))
	}
}

func TestRunLateMountPermissionsNotice(t *testing.T) {
	for _, command := range []string{"/allow-write", "/allow-exec"} {
		for _, mode := range []string{"success", "inspect", "pipe", "closed"} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				configPath, root := writeRunLifecycleConfig(t)
				// Repeating a successful command must not repeat the permissions notice.
				stdin, stdout, stderr := stdinFileWith(t, command+"\n"+command+"\n")
				profile := "trusted"
				if mode == "inspect" {
					profile = "inspect"
				}
				var sess *replSession
				err := run([]string{"-profile", profile, "-config", configPath, "-root", root,
					"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag",
					"-no-project-context", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
					afterSessionReady: func(ready *replSession) error {
						sess = ready
						sess.stdinTerminal = mode != "pipe"
						if mode == "closed" {
							return sess.runtime.Close()
						}
						return nil
					},
				})
				if err != nil {
					t.Fatalf("run: %v; stderr: %s", err, readRunTestFile(t, stderr))
				}
				out, notices := readRunTestFile(t, stdout), readRunTestFile(t, stderr)
				if strings.Contains(out, "permissions:") {
					t.Fatalf("permissions leaked to stdout: %s", out)
				}
				wantCount := 1
				if mode == "success" {
					wantCount = 2
					if !strings.Contains(out, "already enabled") {
						t.Fatalf("mount did not succeed: %s", out)
					}
					want := renderPermissions(sess.runtime.EffectivePermissions()) + "\n"
					if strings.Count(notices, want) != 1 {
						t.Errorf("missing updated permissions %q in stderr: %s", want, notices)
					}
				} else if sess.allowWrite || sess.allowExec {
					t.Fatal("refused/failed mount enabled capabilities")
				}
				if got := strings.Count(notices, "permissions:"); got != wantCount {
					t.Fatalf("permissions notices = %d, want %d: %s", got, wantCount, notices)
				}
			})
		}
	}
}

// A successful late mount prints the whole updated permissions line, scratch
// and verification included (#576). Literal lines: the late write mount reads
// absent without a .golem.json and armed with a valid one.
func TestPermissionsNoticeAfterMount(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const host = " commands=host (unsandboxed; runs with the host user's filesystem and network authority) scratch=off verification="
	const startup = "permissions: profile=trusted tools=read_file,search,glob,list" + host + "absent"
	for _, tc := range []struct {
		name, command, golemJSON, want string
	}{
		{"write", "/allow-write", "",
			"permissions: profile=trusted tools=read_file,search,glob,list,write_file,edit_file" + host + "absent"},
		{"write armed", "/allow-write", `{"verify":{"argv":["` + self + `","__golem_exec_helper__","echo","ok"]}}`,
			"permissions: profile=trusted tools=read_file,search,glob,list,write_file,edit_file" + host + "armed"},
		{"exec", "/allow-exec", "",
			"permissions: profile=trusted tools=read_file,search,glob,list,run_command,start_command,command_status,command_tail,stop_command" + host + "absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			if tc.golemJSON != "" {
				writeGolemJSON(t, root, tc.golemJSON)
			}
			stdin, stdout, stderr := stdinFileWith(t, tc.command+"\n")
			err := run([]string{"-profile", "trusted", "-config", configPath, "-root", root,
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag",
				"-no-project-context", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
				afterSessionReady: func(ready *replSession) error {
					ready.stdinTerminal = true
					return nil
				},
			})
			if err != nil {
				t.Fatalf("run: %v; stderr: %s", err, readRunTestFile(t, stderr))
			}
			var got []string
			for _, line := range stderrLines(t, stderr) {
				if strings.HasPrefix(line, "permissions:") {
					got = append(got, line)
				}
			}
			if want := []string{startup, tc.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("permissions lines =\n %q\nwant\n %q", got, want)
			}
		})
	}
}

// A sandbox-policy error from the startup verifier aborts startup before the
// session is published (spec section 5): never a notice, never a session.
func TestStartupVerifierPolicyErrorAbortsStartup(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	orig := buildVerifierFn
	t.Cleanup(func() { buildVerifierFn = orig })
	calls := 0
	buildVerifierFn = func(string, agenttools.SandboxConfig) (*agenttools.VerifyCommand, string, error) {
		calls++
		return nil, "", fmt.Errorf("stub backend: %w", agenttools.ErrSandboxPolicy)
	}
	stdin, stdout, stderr := runTestFiles(t)
	ready := false
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
		"-no-memory", "-no-rag", "-no-project-context", "-no-auto-index", "-allow-write"}, stdin, stdout, stderr,
		runHooks{afterSessionReady: func(*replSession) error { ready = true; return nil }})
	if calls != 1 {
		t.Fatalf("buildVerifierFn calls = %d, want 1", calls)
	}
	if !errors.Is(err, agenttools.ErrSandboxPolicy) || err.Error() != "golem: verifier: stub backend: tools: sandbox policy unavailable" {
		t.Fatalf("run = %v, want the wrapped policy error", err)
	}
	if ready {
		t.Fatal("startup published a session after a verifier policy error")
	}
	if strings.Contains(readRunTestFile(t, stderr), "permissions:") {
		t.Fatalf("startup printed permissions after a verifier policy error:\n%s", readRunTestFile(t, stderr))
	}
}
