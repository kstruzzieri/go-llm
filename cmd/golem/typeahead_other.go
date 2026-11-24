//go:build !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !windows

package main

import "os"

// discardTypedAhead discards nothing here.
//
// ponytail: no flush on these platforms, so input typed ahead of a consent
// exit still reaches the shell; add one when golem supports the platform.
func discardTypedAhead(*os.File) error { return nil }
