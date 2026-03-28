package prefetch

import "time"

// StateProvider exposes IDE state to the prefetch engine.
// Implementations are expected to be safe for concurrent access.
type StateProvider interface {
	// ActiveFile returns the path of the currently focused file.
	ActiveFile() string
	// OpenFiles returns all currently open file paths.
	OpenFiles() []string
	// RecentEdits returns edit events ordered newest-first.
	RecentEdits() []EditEvent
	// CursorContext returns the current cursor position and surrounding context.
	CursorContext() CursorContext
}

// EditEvent represents a user edit action on a file.
type EditEvent struct {
	// Source is the file path that was edited.
	Source string
	// Timestamp is when the edit occurred.
	Timestamp time.Time
	// Lines are the line numbers that were modified (1-indexed).
	Lines []int
}

// CursorContext describes the user's cursor position within a file.
type CursorContext struct {
	// File is the path of the file containing the cursor.
	File string
	// Line is the cursor's line number (1-indexed).
	Line int
	// Language is the detected language of the file (e.g., "go", "python").
	Language string
	// FunctionName is the name of the function enclosing the cursor, if any.
	FunctionName string
}
