package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
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
	hasSlot    bool
}

// captureStartup runs golem to the point where the session exists, applies
// commands, snapshots the shape, and stops. -agent-memory keeps a tool AFTER
// the gated slot (agent-memory tools) and memory_search sits BEFORE it, so
// an append-instead-of-insert mutation is visible.
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
				hasJournal: sess.journal != nil, hasManager: sess.bgManager != nil, hasSlot: sess.verifier != nil,
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
	for _, tc := range []struct {
		name     string
		flags    []string
		commands []string
	}{
		{"write", []string{"-allow-write"}, []string{"/allow-write"}},
		{"exec", []string{"-allow-exec"}, []string{"/allow-exec"}},
		{"both, mounted in reverse order", []string{"-allow-write", "-allow-exec"}, []string{"/allow-exec", "/allow-write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startup := captureStartup(t, tc.flags)
			mid := captureStartup(t, nil, tc.commands...)
			if !reflect.DeepEqual(startup, mid) {
				t.Fatalf("mid-session shape differs from startup flags:\n startup=%+v\n mid=%+v", startup, mid)
			}
			last := startup.names[len(startup.names)-1]
			if last == "stop_command" || last == "edit_file" {
				t.Fatalf("no tool after the gated slot (last=%s); the order pin is vacuous", last)
			}
			if !startup.hasSlot {
				t.Fatal("REPL mode must carry the late verifier slot")
			}
		})
	}
}

func TestLateVerifierSlotIsREPLOnly(t *testing.T) {
	if got := captureStartup(t, nil); !got.hasSlot {
		t.Fatal("REPL session has no verifier slot")
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
