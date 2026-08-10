//go:build windows

package main

// watchResize is a stub on Windows. Selection declines the editor there before
// any terminal work happens, so nothing consumes resizes; this exists only so
// the package compiles and the seam has one shape on every platform.
func watchResize() (<-chan struct{}, func()) { return nil, func() {} }
