package main

import (
	"errors"
	"flag"
	"fmt"
	"testing"
)

func TestExitCodeForClassifiesUsageAsTwo(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil is success", nil, 0},
		{"plain error is runtime failure", errors.New("provider exploded"), 1},
		{"usage error is two", newUsageError("bad flag"), 2},
		{"wrapped usage error is two", fmt.Errorf("golem: %w", newUsageError("bad flag")), 2},
		{"usage wrapping a cause is two", wrapUsageError(errors.New("inner")), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Errorf("exitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestUsageErrorPreservesMessageAndCause(t *testing.T) {
	inner := errors.New("inner cause")
	err := wrapUsageError(inner)
	if !errors.Is(err, inner) {
		t.Errorf("wrapUsageError must unwrap to its cause")
	}
	if err.Error() != "inner cause" {
		t.Errorf("Error() = %q, want %q", err.Error(), "inner cause")
	}
	if newUsageError("plain %s", "text").Error() != "plain text" {
		t.Errorf("newUsageError must format its message")
	}
}

func TestHelpIsNotAUsageFailure(t *testing.T) {
	// -h must still be recognized as flag.ErrHelp after usage wrapping, so
	// main() can exit 0 for it.
	err := wrapUsageError(flag.ErrHelp)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatal("wrapping must preserve flag.ErrHelp for the exit-0 check")
	}
}

func TestRunReturnsUsageErrorForBadHeadlessFlag(t *testing.T) {
	// The taxonomy is scoped to -p (spec R3): the SAME validation failure is 2
	// with -p and stays 1 without it.
	stdin, stdout, stderr := runTestFiles(t)
	err := run([]string{"-p", "hi", "-pressure-warn", "999"}, stdin, stdout, stderr)
	if got := exitCodeFor(err); got != 2 {
		t.Fatalf("with -p: exitCodeFor(%v) = %d, want 2", err, got)
	}
	err = run([]string{"-pressure-warn", "999"}, stdin, stdout, stderr)
	if got := exitCodeFor(err); got != 1 {
		t.Fatalf("without -p: exitCodeFor(%v) = %d, want 1 (non-headless exits are unchanged)", err, got)
	}
}

func TestArgsRequestOneShot(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"-p", "hi", "-nope"}, true},
		{[]string{"--p", "hi"}, true},
		{[]string{"-p=hi"}, true},
		{[]string{"-nope"}, false},
		{[]string{"-plan", "x.json"}, false}, // -plan is not -p
		{[]string{"-config", "-p"}, true},    // conservative: a literal -p token counts
		// An Agentflow-mode token vetoes headless intent outright: exit 2
		// belongs to -agentflow-status's own semantics (resume serially).
		{[]string{"-agentflow-status", "-p", "hi", "-nope"}, false},
		{[]string{"-p", "hi", "--agentflow-resume", "-nope"}, false},
		{nil, false},
	}
	for _, tc := range cases {
		if got := argsRequestOneShot(tc.args); got != tc.want {
			t.Errorf("argsRequestOneShot(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}
