package main

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
