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
// an append-instead-of-insert mutation is visible. hasSlot reports the
// late verifier slot separately: it is a wiring detail, not part of the
// parity shape (a startup -allow-write session binds its verifier directly).
func captureStartup(t *testing.T, extra []string, commands ...string) (startupShape, bool) {
	t.Helper()
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	args := append([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-project-context", "-no-auto-index", "-agent-memory"}, extra...)
	errStop := errors.New("stop after startup")
	var got startupShape
	var hasSlot bool
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
			hasSlot = sess.verifier != nil
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
	return got, hasSlot
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
			startup, _ := captureStartup(t, tc.startup)
			mid, _ := captureStartup(t, tc.mid, tc.commands...)
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

func TestRunRejectsAllowCommandsFromPipedInput(t *testing.T) {
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

// The slot exists exactly where /allow-write can still need it: a REPL
// session that started without -allow-write. A startup -allow-write session
// binds its verifier directly (byte-identical to pre-#372), and one-shot
// never dispatches slash commands.
func TestLateVerifierSlotIsREPLOnly(t *testing.T) {
	if _, hasSlot := captureStartup(t, nil); !hasSlot {
		t.Fatal("REPL session without -allow-write has no verifier slot")
	}
	if _, hasSlot := captureStartup(t, []string{"-allow-write"}); hasSlot {
		t.Fatal("a startup -allow-write session must bind its verifier directly, not through the slot")
	}
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after startup")
	var slot bool
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
		"-no-memory", "-no-project-context", "-no-auto-index", "-p", "hello"}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			slot = sess.verifier != nil
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
	if slot {
		t.Fatal("one-shot mode must not carry the verifier slot")
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
		"-no-memory", "-no-auto-index"}, stdin, stdout, stderr, runHooks{
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
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
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
			args := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
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
