//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package main

// watchJob is a stub where golem does no job control: Windows, which refuses
// -watch before reaching it, and Unix systems without a getsid here.
func watchJob() (opsJob, func()) { return opsJob{}, func() {} }
