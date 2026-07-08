package main

import (
	"fmt"
	"io"
	"sync"
)

// promptText is the interactive REPL prompt. Centralized so the loop and the
// async-notice reprint stay in sync.
const promptText = "golem> "

// ctrlCHint is shown on the first Ctrl-C at an idle prompt. The second Ctrl-C
// (with no intervening input) quits — matching the Node/Claude-Code convention
// where a single Ctrl-C at an idle prompt never exits outright.
const ctrlCHint = "(press Ctrl-C again or type /exit to quit)"

// replControl coordinates three concurrent writers to the terminal — the REPL
// loop, asynchronous notices (e.g. startup auto-index warnings on their own
// goroutine), and the Ctrl-C signal handler — behind one mutex.
//
// It fixes two UX problems:
//   - Async notices printed after the "golem> " prompt used to leave no visible
//     prompt, so the REPL looked hung. notice() reprints the prompt when idle.
//   - Ctrl-C used to only ever cancel the in-flight turn, with no way to quit
//     short of Ctrl-D or /exit. interrupt() now quits on a second idle Ctrl-C.
type replControl struct {
	mu         sync.Mutex
	out        io.Writer       // stdout: the prompt and the idle-time notice reprint
	errOut     io.Writer       // stderr: mid-turn notices, kept off the renderer's stream
	interrupts chan<- struct{} // buffered(1): request cancellation of the in-flight turn
	quit       func()          // cancels the REPL context so runREPL returns (clean exit)
	atPrompt   bool            // true while idle at the prompt, waiting for input
	armed      bool            // a prior idle Ctrl-C armed the next one to quit
}

func newReplControl(out, errOut io.Writer, interrupts chan<- struct{}, quit func()) *replControl {
	return &replControl{out: out, errOut: errOut, interrupts: interrupts, quit: quit}
}

// prompt prints the prompt and marks the REPL idle. Called at the top of each
// loop iteration, before the blocking read. Resets the quit-arm: reaching a
// fresh prompt (e.g. after a completed turn) is intervening activity.
func (c *replControl) prompt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.atPrompt = true
	c.armed = false
	fmt.Fprint(c.out, promptText)
}

// enterTurn marks the REPL busy: a line was read and is being dispatched or
// run. A Ctrl-C from here on cancels the turn rather than quitting.
func (c *replControl) enterTurn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.atPrompt = false
	c.armed = false
}

// notice prints an asynchronous message. When idle at the prompt it reprints
// the prompt underneath so the prompt is never buried by the message.
func (c *replControl) notice(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.atPrompt {
		fmt.Fprintf(c.out, "\n%s\n%s", line, promptText)
		return
	}
	// Mid-turn: write to stderr so the notice never splices into the renderer's
	// stdout stream (the renderer writes stdout unsynchronized during a turn).
	fmt.Fprintln(c.errOut, line)
}

// interrupt handles one Ctrl-C. Mid-turn: request turn cancellation. Idle at
// the prompt: the first Ctrl-C arms and hints; the second quits.
func (c *replControl) interrupt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.atPrompt {
		// Non-blocking: the turn watcher drains at most one; extras are no-ops.
		select {
		case c.interrupts <- struct{}{}:
		default:
		}
		return
	}
	if c.armed {
		c.quit()
		return
	}
	c.armed = true
	fmt.Fprintf(c.out, "\n%s\n%s", ctrlCHint, promptText)
}
