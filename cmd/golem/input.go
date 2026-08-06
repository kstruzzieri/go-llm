package main

import (
	"context"
	"errors"
)

// lineSource is the one seam every interactive read goes through. Before it,
// the REPL loop and the approval prompt each held their own reader and each
// printed their own prompt; the editor arriving in task 5 can do neither,
// because x/term.Terminal prints and repaints its own prompt and must own the
// descriptor for the whole read.
//
// Exactly one implementation exists in this commit (scannerSource, today's
// behavior). The interface is introduced separately from the editor so the
// refactor is provable on its own.
type lineSource interface {
	// ReadGoal reads one top-level goal. The source prints prompt; callers
	// must not.
	ReadGoal(ctx context.Context, prompt string) (string, bool, error)

	// ReadAnswer reads one approval answer. Never recorded to history.
	ReadAnswer(ctx context.Context, prompt string) (string, bool, error)

	// RecordGoal appends an accepted goal to history. runREPL calls it after
	// trimming and classifying the line as neither empty nor a slash command,
	// immediately before running the turn, so a rejected or non-goal line can
	// never be recorded.
	RecordGoal(goal string)

	// IdleDisplay renders an asynchronous message while the user is at the
	// prompt, preserving whatever is already on screen.
	//
	// replControl calls this while holding its own mutex, so the atPrompt
	// decision and the display action stay one policy operation. An
	// implementation must therefore not call back into replControl, and must
	// not hold a lock that any path taken by replControl could already own --
	// the editor arriving in task 5 has to release its display and
	// Terminal-binding mutexes before it invokes the interrupt entry point, or
	// the two lock orders form a cycle.
	IdleDisplay(msg string)

	// Close releases whatever the source owns. Safe to call more than once.
	Close() error
}

// lineSourceMode is the final-dispatch decision about whether an interactive
// read can happen at all, and if so what kind. Keeping it a pure function of
// flags makes the table testable without constructing a session.
type lineSourceMode int

const (
	// sourceNone: nothing reads stdin interactively. One-shot (-p), Agentflow
	// task mode (-plan), and auto-approved planning (-goal -approve-plan-lock)
	// all fall here, so none of them opens a reader or a history file.
	sourceNone lineSourceMode = iota

	// sourceREPL: the interactive loop, which reads goals and approval answers
	// and records history.
	sourceREPL

	// sourceAnswerOnly: interactive planning (-goal). It reads only the
	// plan-lock approval, so it gets a source but no history.
	sourceAnswerOnly
)

func lineSourceModeFor(f flags) lineSourceMode {
	switch {
	case f.goalSet && !f.approvePlanLock:
		return sourceAnswerOnly
	case f.goalSet, f.planPath != "", f.promptSet:
		return sourceNone
	default:
		return sourceREPL
	}
}

// withLineSource owns Close for the interactive modes, so no caller has to
// remember it on every return path. A Close failure is joined onto fn's error
// rather than replacing it. During a panic the deferred Close still runs and
// the panic continues unrecovered, keeping the original stack.
func withLineSource(src lineSource, fn func(lineSource) error) (err error) {
	defer func() {
		if cerr := src.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	return fn(src)
}
