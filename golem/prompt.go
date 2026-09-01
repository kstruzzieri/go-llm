package golem

const basePrompt = "You are Golem, a terminal coding assistant for this workspace. " +
	"Use the read-only tools to inspect files before answering repo-specific questions. " +
	"Keep answers concise, cite file paths and line numbers when they matter, and say when the available evidence is insufficient."

const noWriteFragment = " Do not claim to modify files or change project state on disk."
const noExecFragment = " Do not claim to run shell commands, install packages, or otherwise execute processes."
const writeFragment = " You may propose changes with write_file and edit_file; every change is shown to the user as a diff and is applied only after they approve it, so keep edits minimal and targeted and explain what you are changing. Prefer edit_file for small changes and write_file for new files or full rewrites."
const execFragment = " You may run commands with run_command to build, test, or lint and verify your work; every command is shown to the user and runs only after they approve it, so prefer minimal, targeted commands. A non-zero exit code is a normal result to read and react to. Respect AGENTS.md guidance."

// backgroundFragment describes the #346 background command tools. It follows
// execFragment whenever exec is enabled, because the background tool set is
// registered together with run_command. It deliberately claims no
// descendant-tree containment: escaped descendants are unsupported.
const backgroundFragment = " For a long-running command (a dev server, a watcher, a long build) use start_command instead: it returns a job handle immediately and the command keeps running under the session's background manager until it exits, a stop is approved, or the session ends. Poll its state with command_status, and read new output with command_tail, passing the previous next_cursor to fetch only what is new. Job output is a bounded tail of the most recent bytes, so read it promptly; dropped_bytes reports anything already evicted. Stop a job explicitly with stop_command; stopping is shown to the user and runs only after they approve it."

const priorityNote = " Prior session messages are context only; the current user request is authoritative."

// SystemPrompt returns Golem's standard prompt for the enabled capabilities.
func SystemPrompt(allowWrite, allowExec bool) string {
	prompt := basePrompt
	if allowWrite {
		prompt += writeFragment
	} else {
		prompt += noWriteFragment
	}
	if allowExec {
		prompt += execFragment + backgroundFragment
	} else {
		prompt += noExecFragment
	}
	return prompt + priorityNote
}

// HeadlessToolCaps names the exact gated tools mounted for a headless run
// whose authorization came from -allow-tool (#352). StartCommand implies the
// ungated command_status/command_tail readers are mounted with it.
type HeadlessToolCaps struct {
	WriteFile    bool
	EditFile     bool
	RunCommand   bool
	StartCommand bool
	StopCommand  bool
}

// Headless fragments (#352). They differ from the interactive fragments in two
// deliberate ways: they name only the tools that are actually mounted (the
// interactive fragments describe whole groups), and they never describe
// interactive approval — a headless run's calls were pre-authorized by flag,
// and telling the model a user will review each call would be false.
const (
	headlessWritePair = " You may modify files with write_file and edit_file; keep edits minimal and targeted, and prefer edit_file for small changes and write_file for new files or full rewrites."
	headlessWriteOnly = " You may create or fully rewrite files with write_file; it is the only file-mutation tool available, so a change to an existing file means rewriting it completely."
	headlessEditOnly  = " You may edit existing files with edit_file; it is the only file-mutation tool available, so you cannot create new files."
	headlessRun       = " You may run commands with run_command to build, test, or lint and verify your work; prefer minimal, targeted commands. A non-zero exit code is a normal result to read and react to. Respect AGENTS.md guidance."
	headlessStart     = " For a long-running command (a dev server, a watcher, a long build) use start_command: it returns a job handle immediately and the command keeps running until it exits or the run ends. Poll its state with command_status, and read new output with command_tail, passing the previous next_cursor to fetch only what is new; job output is a bounded tail of the most recent bytes, so read it promptly."
	headlessStop      = " Stop a background job explicitly with stop_command."
	headlessNoStop    = " There is no tool to stop a background job early: jobs end when they exit or when the run ends."
	headlessAuthNote  = " This is a non-interactive run: the tools above were pre-authorized by the operator, run without per-call review, and are the ONLY mutating tools that exist — do not claim access to any other."
)

// SystemPromptHeadless returns the prompt for a headless one-shot run with
// selectively mounted gated tools (#352). All-false caps yield the same
// read-only stance as SystemPrompt(false, false), phrased for a run with no
// approver.
func SystemPromptHeadless(c HeadlessToolCaps) string {
	prompt := basePrompt
	anyMutating := c.WriteFile || c.EditFile || c.RunCommand || c.StartCommand || c.StopCommand
	switch {
	case c.WriteFile && c.EditFile:
		prompt += headlessWritePair
	case c.WriteFile:
		prompt += headlessWriteOnly
	case c.EditFile:
		prompt += headlessEditOnly
	default:
		prompt += noWriteFragment
	}
	if c.RunCommand {
		prompt += headlessRun
	}
	if c.StartCommand {
		prompt += headlessStart
		if c.StopCommand {
			prompt += headlessStop
		} else {
			prompt += headlessNoStop
		}
	} else if c.StopCommand {
		prompt += headlessStop
	}
	if !c.RunCommand && !c.StartCommand && !c.StopCommand {
		prompt += noExecFragment
	}
	if anyMutating {
		prompt += headlessAuthNote
	}
	return prompt + priorityNote
}
