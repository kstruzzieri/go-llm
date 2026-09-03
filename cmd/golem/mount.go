package main

import (
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
