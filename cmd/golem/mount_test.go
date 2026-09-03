package main

import (
	"strings"
	"testing"

	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

// Pins fragment ORDER and separators against the real fragment functions.
// Every fragment is asserted non-empty first so the order pin cannot be
// vacuous.
func TestComposeSystemFragmentOrder(t *testing.T) {
	in := systemInputs{allowWrite: true, allowExec: true, delegate: true, dispatch: true, memory: true,
		projectContext: "<<<PROJECT>>>", agentMemory: true, sessionUp: true}
	want := []string{
		buildSystemPrompt(true, true),
		delegateSystemFragment(true, true),
		dispatchSystemFragment(true),
		memorySystemFragment(true),
		"\n\n<<<PROJECT>>>",
		agentMemorySystemFragment(true, true),
	}
	for i, w := range want {
		if w == "" {
			t.Fatalf("fragment %d is empty; the order pin would be vacuous", i)
		}
	}
	if got := composeSystem(in); got != strings.Join(want, "") {
		t.Fatalf("composeSystem order/separators changed:\n got=%q\nwant=%q", got, strings.Join(want, ""))
	}
}

func TestComposeSystemDisabledInputsAreByteIdenticalToBasePrompt(t *testing.T) {
	if got := composeSystem(systemInputs{}); got != buildSystemPrompt(false, false) {
		t.Fatalf("zero inputs = %q, want the bare read-only prompt", got)
	}
}

// Headless (-allow-tool) uses the exact-set prompt and derives the delegate
// fragment's write-awareness from the mounted caps, as startup's buildWrite
// did; a nil headless pointer (every REPL session) must not panic.
func TestComposeSystemHeadlessDerivesWriteAwareness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		caps      golemruntime.HeadlessToolCaps
		wantWrite bool
	}{
		{"write_file", golemruntime.HeadlessToolCaps{WriteFile: true}, true},
		{"edit_file", golemruntime.HeadlessToolCaps{EditFile: true}, true},
		{"exec only", golemruntime.HeadlessToolCaps{RunCommand: true}, false},
	} {
		caps := tc.caps
		got := composeSystem(systemInputs{headless: &caps, delegate: true})
		if !strings.HasPrefix(got, golemruntime.SystemPromptHeadless(caps)) {
			t.Fatalf("%s: headless prompt not used: %q", tc.name, got)
		}
		if !strings.HasSuffix(got, delegateSystemFragment(true, tc.wantWrite)) {
			t.Fatalf("%s: delegate write-awareness = %v not honored: %q", tc.name, tc.wantWrite, got)
		}
	}
	if got := composeSystem(systemInputs{delegate: true}); !strings.HasSuffix(got, delegateSystemFragment(true, false)) {
		t.Fatalf("nil headless with delegate: %q", got)
	}
}

// Flipping one input changes exactly that fragment: everything after the
// base+delegate pair is byte-identical before and after.
func TestComposeSystemFlipChangesOnlyThatFragment(t *testing.T) {
	base := systemInputs{delegate: true, dispatch: true, memory: true, projectContext: "P", agentMemory: true, sessionUp: true}
	before := composeSystem(base)
	flipped := base
	flipped.allowWrite = true
	after := composeSystem(flipped)
	tailBefore := strings.TrimPrefix(before, buildSystemPrompt(false, false)+delegateSystemFragment(true, false))
	tailAfter := strings.TrimPrefix(after, buildSystemPrompt(true, false)+delegateSystemFragment(true, true))
	if tailBefore == before || tailAfter == after {
		t.Fatal("prefix did not match; the composition head changed")
	}
	if tailBefore != tailAfter {
		t.Fatalf("flipping allowWrite changed more than the write fragment:\n before=%q\n after=%q", tailBefore, tailAfter)
	}
}
