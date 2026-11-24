package main

import "golang.org/x/sys/windows"

// supportsANSI checks the output handle being styled. Windows configures
// virtual terminal processing independently for each console screen buffer.
func (realTermOps) supportsANSI(fd int) bool {
	var mode uint32
	if windows.GetConsoleMode(windows.Handle(fd), &mode) != nil {
		return false
	}
	const required = windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING
	return mode&required == required
}
