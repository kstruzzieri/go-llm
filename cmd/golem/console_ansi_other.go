//go:build !windows

package main

// supportsANSI needs no additional console-mode check on a terminal here.
func (consoleTermOps) supportsANSI(int) bool { return true }
