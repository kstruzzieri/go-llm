//go:build !windows

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The subprocess half of TestStrayCensusFlags: runLLMBenchMain is !windows, so
// the end-to-end refusal lives beside it in a !windows file.
func TestStrayCensusFlagsRefusedByMain(t *testing.T) {
	for _, flag := range []string{"-census-manifest", "-census-root", "-census-out"} {
		// -fc-render alone fails fast on its own missing -fc-sidemap if the
		// census flag were accepted.
		code, out := runLLMBenchMain(t, nil, flag, t.TempDir(), "-fc-render")
		if code != 1 || !strings.Contains(out, flag+" require -interceptor-census") {
			t.Errorf("%s without -interceptor-census: exit %d output %q", flag, code, out)
		}
	}
}

// The #517 benign modes: their flags are refused without their mode, the
// census inputs serve the draft, and the two modes exclude each other.
func TestBenignFlagsRefusedByMain(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-benign-out", t.TempDir(), "-fc-render"}, "-benign-out require -build-interceptor-benign"},
		{[]string{"-census-results", "x.json", "-fc-render"}, "-census-results require -interceptor-benign-draft"},
		{[]string{"-benign-reject", "a/b", "-fc-render"}, "-benign-reject require -interceptor-benign-draft"},
		{[]string{"-interceptor-benign-draft", "d.json", "-census-out", t.TempDir()}, "-census-out require -interceptor-census"},
		{[]string{"-interceptor-benign-draft", "d.json", "-build-interceptor-benign", "c.json"}, "are mutually exclusive"},
		// -census-root is the draft's input: the run gets past the flag checks
		// and fails on the missing manifest instead.
		{[]string{"-interceptor-benign-draft", filepath.Join(t.TempDir(), "d.json"), "-census-root", t.TempDir(), "-census-manifest", filepath.Join(t.TempDir(), "none.json")}, "interceptor-benign-draft: census manifest"},
	} {
		code, out := runLLMBenchMain(t, nil, tc.args...)
		if code != 1 || !strings.Contains(out, tc.want) {
			t.Errorf("%v: exit %d output %q, want %q", tc.args, code, out, tc.want)
		}
	}
}
