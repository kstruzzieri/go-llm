package main

import "errors"

// Shared input ceilings for the REPL line editor and goal history.
//
// These live in one file because three layers must agree on them: the key
// filter bounds a single bracketed paste, the editor bounds a composed goal and
// detects x/term's per-line refusal, and the history projection refuses to
// recall an entry the editor could not safely re-edit.
const (
	// maxEditorRunes mirrors the unexported maxLineLength in
	// golang.org/x/term's terminal.go. x/term silently drops the keystroke that
	// would push a line past this bound, so golem detects the refusal and warns
	// rather than losing input silently. It also caps what history may offer to
	// arrow recall: x/term recalls through setLine with no bound, while its
	// insertion guard tests len(line) == maxLineLength exactly, so a recalled
	// longer entry would slip past the upstream limit and keep growing.
	maxEditorRunes = 4096

	// maxGoalBytes bounds one composed goal, one bracketed paste, and one
	// /edit result. It matches the pre-existing scanner and runtime ceilings so
	// the editor path admits exactly what the scanner path always has.
	maxGoalBytes = 1024 * 1024
)

// User-visible ceiling messages. lineLimitWarning reports x/term's refused
// 4097th insertion, which would otherwise drop the keystroke silently;
// goalLimitWarning reports a composed goal or paste the editor discarded.
const (
	lineLimitWarning = "warning: input line is limited to 4096 runes; use /edit for longer input"
	goalLimitWarning = "warning: goal exceeds 1 MiB and was discarded"
)

// errPasteTooLarge is the key filter's rejection of a single bracketed paste
// whose non-marker content exceeds its budget. It surfaces only after the
// paste has been drained through its end marker (or the stream ended, in which
// case the stream error is joined), so the editor can recreate the Terminal
// and keep reading.
var errPasteTooLarge = errors.New("bracketed paste exceeds 1 MiB")
