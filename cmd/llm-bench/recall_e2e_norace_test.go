//go:build !race

package main

// raceEnabled reports a -race build. TestRecallEndToEnd skips its variant
// runs there; only a non-race go test runs them (see TestRecallEndToEnd).
const raceEnabled = false
