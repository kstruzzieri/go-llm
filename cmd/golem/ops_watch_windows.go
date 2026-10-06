//go:build windows

package main

// watchJob is a stub on Windows, which refuses -watch before reaching it and
// has no job control; it exists only so the package compiles.
func watchJob() (opsJob, func()) { return opsJob{}, func() {} }
