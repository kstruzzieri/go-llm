//go:build race

package main

// raceEnabled reports a -race build. TestRecallEndToEnd skips its variant
// runs there, and TestRecallRegistrationsLoad its rebuild of every recall
// trace; only a non-race go test runs them (see each test's comment).
const raceEnabled = true
