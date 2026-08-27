//go:build darwin && race

package tools

// raceInstrumented reports that this test binary carries the race detector.
// A race-instrumented helper process cannot initialize inside the Seatbelt
// profile (ThreadSanitizer's startup needs syscalls the policy denies), so
// helper-based behavioral legs skip under -race; the release gate runs the
// suite both with and without -race so helper coverage is never lost.
const raceInstrumented = true
