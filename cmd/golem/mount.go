package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

// systemInputs are the CLI-owned prompt composition inputs (#372): every
// value main.go uses at startup, held so a mid-session mount recomposes with
// exactly one input changed. replSession keeps the invariant
// baseSystem == composeSystem(sysInputs) at all times; a mount MUST derive
// its inputs from the live session (next := sess.sysInputs; next.allowWrite
// = true), never from a fresh struct carrying only the changed flag. #354
// adds its Git fragment here. There is no fragment registry in
// golem.Runtime by design — composition is a CLI concern and this is its
// one path.
type systemInputs struct {
	// headless is non-nil only for a -allow-tool one-shot (#352): the prompt
	// is built from the exact mounted set. Never recomposed (no REPL there).
	headless       *golemruntime.HeadlessToolCaps
	allowWrite     bool
	allowExec      bool
	delegate       bool
	dispatch       bool
	memory         bool
	projectContext string // raw fenced project-context block; "" when none
	agentMemory    bool
	sessionUp      bool
}

// composeSystem is the one composition path. Order and separators are the
// startup sequence byte-for-byte; the delegate fragment's write-awareness is
// derived the way startup's buildWrite was (flag OR a headless write cap).
func composeSystem(in systemInputs) string {
	var system string
	headlessWrite := in.headless != nil && (in.headless.WriteFile || in.headless.EditFile)
	if in.headless != nil {
		system = golemruntime.SystemPromptHeadless(*in.headless)
	} else {
		system = buildSystemPrompt(in.allowWrite, in.allowExec)
	}
	system += delegateSystemFragment(in.delegate, in.allowWrite || headlessWrite)
	system += dispatchSystemFragment(in.dispatch)
	system += memorySystemFragment(in.memory)
	if in.projectContext != "" {
		system += "\n\n" + in.projectContext
	}
	return system + agentMemorySystemFragment(in.agentMemory, in.sessionUp)
}

// mount installs add at index at of the tool list together with the system
// recomposed from in, in ONE runtime replacement. On error the session is
// untouched: the candidate list is built on a clone, and fields are assigned
// only after Replace succeeds.
func (s *replSession) mount(at int, add []agent.Tool, in systemInputs) error {
	cand := slices.Insert(slices.Clone(s.tools), at, add...)
	system := composeSystem(in)
	if err := s.runtime.Replace(system, cand[s.readToolCount:]); err != nil {
		return err
	}
	s.tools, s.sysInputs, s.baseSystem = cand, in, system
	return nil
}

// handleAllowWrite implements /allow-write (#372): the startup -allow-write
// sequence (store, journal, recovery, then the verifier) applied to the
// live session, atomically with the recomposed system prompt. Recovery runs
// before the mount exactly as at startup and is idempotent, so a retry after
// a failed replacement is safe. Every failure path closes the store, so the
// workspace lease is never leaked. Mounting never grants approval: the first
// write prompts exactly as it would after the flag.
func handleAllowWrite(ctx context.Context, out io.Writer, sess *replSession, fields []string) {
	if len(fields) != 1 {
		_, _ = fmt.Fprintln(out, "usage: /allow-write")
		return
	}
	if sess.allowWrite {
		_, _ = fmt.Fprintln(out, "writes already enabled")
		return
	}
	fail := func(err error) { _, _ = fmt.Fprintf(out, "writes not enabled: %v\n", err) }
	// D6 parity: -allow-write fails closed on ANY checkpoint lifecycle
	// failure; here that means nothing mounts and the session is unchanged.
	store, err := openCheckpointStore(ctx, os.Getenv, sess.root)
	if err != nil {
		fail(fmt.Errorf("checkpoint store: %w", err))
		return
	}
	writeTools, journal, err := buildWriteTools(sess.root, store)
	if err != nil {
		_ = store.Close()
		fail(err)
		return
	}
	notice, err := journal.recoverStartup(ctx)
	if err != nil {
		_ = store.Close()
		fail(fmt.Errorf("checkpoint recovery: %w", err))
		return
	}
	undoing, err := store.countState(ctx, checkpointUndoing)
	if err != nil {
		_ = store.Close()
		fail(fmt.Errorf("checkpoint state: %w", err))
		return
	}
	// Derived from the LIVE inputs: a fresh struct would erase the other
	// capability and every other fragment.
	next := sess.sysInputs
	next.allowWrite = true
	if err := sess.mount(sess.mountAt, writeTools, next); err != nil {
		_ = store.Close()
		fail(fmt.Errorf("runtime: %w", err))
		return
	}
	sess.writeToolCount = len(writeTools)
	sess.journal, sess.lateStore, sess.allowWrite = journal, store, true
	// #347: .golem.json is read only once writes are enabled, as at startup;
	// the runner lands in the slot the REPL orchestrator was built with.
	verifier, vwarn := buildVerifier(sess.root)
	sess.verifier.set(verifier)
	_, _ = fmt.Fprintln(out, "writes enabled (approval per change; /auto-edits, /undo, /checkpoints available)")
	if notice != "" {
		_, _ = fmt.Fprintln(out, notice)
	}
	if undoing > 0 {
		_, _ = fmt.Fprintf(out, "an interrupted undo exists (%d checkpoint(s)); /undo resumes it\n", undoing)
	}
	if vwarn != "" {
		_, _ = fmt.Fprintln(out, vwarn)
	}
	if sess.scratch {
		// D9: the exec set was frozen at startup without a promotion journal;
		// rebuilding it would re-key scratch approvals and orphan captured
		// outcomes, so promotion stays as it was.
		_, _ = fmt.Fprintln(out, "scratch: promote_artifact stays unavailable (bound at startup; restart with -allow-exec -scratch -allow-write)")
	}
}

// closeLateMounts releases what /allow-write and /allow-exec opened after
// startup. Shutdown is idempotent, so a startup-owned manager (already shut
// down by its own defer) is harmless; a startup checkpoint store is never in
// lateStore and keeps main.go's own defer. Safe to call more than once.
func (s *replSession) closeLateMounts() error {
	if s.bgManager != nil {
		s.bgManager.Shutdown()
	}
	if s.lateStore == nil {
		return nil
	}
	store := s.lateStore
	s.lateStore = nil
	return store.Close()
}
