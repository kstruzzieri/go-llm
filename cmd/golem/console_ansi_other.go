//go:build !windows

package main

// supportsANSI needs no additional console-mode check on a terminal here.
func (realTermOps) supportsANSI(int) bool { return true }
