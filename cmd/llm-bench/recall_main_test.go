//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The -build-recall-corpus flags: -recall-split and -recall-out are required
// with the mode and refused without it, the mode excludes every other, and a
// refused or failing invocation creates nothing.
func TestRecallCLIFlagsRefusedByMain(t *testing.T) {
	cases := filepath.FromSlash(recallFixturePath)
	out := filepath.Join(t.TempDir(), "out")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-build-recall-corpus", cases}, "-build-recall-corpus requires -recall-split"},
		{[]string{"-build-recall-corpus", cases, "-recall-split", "pilot"}, "-build-recall-corpus requires -recall-out"},
		{[]string{"-recall-out", out, "-fc-render"}, "-recall-out require -build-recall-corpus"},
		{[]string{"-recall-split", "pilot", "-fc-render"}, "-recall-split require -build-recall-corpus"},
		{[]string{"-build-recall-corpus", cases, "-recall-split", "pilot", "-recall-out", out, "-fc-render"}, "are mutually exclusive"},
		// The flags reach the builder: the two-family fixture cannot satisfy the
		// confirmatory template counts, so the build refuses before writing.
		{[]string{"-build-recall-corpus", cases, "-recall-split", "confirmatory", "-recall-out", out}, "build-recall-corpus: recall build: template"},
	} {
		code, log := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(log, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, log, tc.want)
		}
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused invocation created the output dir")
	}
}
