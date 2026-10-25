//go:build !windows

package main

import (
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
