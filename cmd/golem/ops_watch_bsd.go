//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// getsid returns the caller's session ID.
func getsid() (int, error) { return syscall.Getsid(0) }
