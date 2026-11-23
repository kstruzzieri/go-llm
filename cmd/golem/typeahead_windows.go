package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// discardTypedAhead empties the console's input buffer; see the Linux
// implementation. The editor is declined on Windows, but the scanner REPL on a
// console hands typed-ahead input to the shell the same way.
func discardTypedAhead(f *os.File) error {
	return windows.FlushConsoleInputBuffer(windows.Handle(f.Fd()))
}
