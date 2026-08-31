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
	// golang.org/x/term's terminal.go. x/term silently drops the TYPED keystroke
	// that would push a line past this bound, so golem detects the refusal and
	// warns rather than losing input silently. It also caps what history may
	// offer to arrow recall: x/term recalls through setLine with no bound, while
	// its insertion guard tests len(line) == maxLineLength exactly, so a
	// recalled longer entry would slip past the upstream limit and keep growing.
	//
	// It is deliberately NOT a ceiling on pasted lines. handleKey short-circuits
	// while a paste is active, bypassing both AutoCompleteCallback and its own
	// length guard, so a pasted line may exceed this and the spec permits it:
	// pasted input is bounded by maxGoalBytes instead.
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

	// Spec 13b rejections. Malformed input is refused rather than sanitized:
	// the provider transport is JSON, which would substitute U+FFFD for bad
	// bytes anyway, so rejecting loudly beats corrupting quietly.
	invalidUTF8Warning = "warning: input was not valid UTF-8 and was discarded"
	replacementWarning = "warning: input contains U+FFFD, which the line editor cannot distinguish from a decode error; it was discarded"

	// A framing defect is golem's bug, not the user's, and says so.
	framingDefectWarning = "internal error: terminal input framing was lost; the line was discarded"
)

// rejectionWarning names the actual problem, so a literal U+FFFD is not
// reported as malformed bytes.
func rejectionWarning(err error) string {
	if errors.Is(err, errLiteralReplacement) {
		return replacementWarning
	}
	return invalidUTF8Warning
}

// errPasteTooLarge is the key filter's rejection of a single bracketed paste
// whose non-marker content exceeds its budget. It surfaces only after the
// paste has been drained through its end marker (or the stream ended, in which
// case the stream error is joined), so the editor can recreate the Terminal
// and keep reading.
var errPasteTooLarge = errors.New("bracketed paste exceeds 1 MiB")
