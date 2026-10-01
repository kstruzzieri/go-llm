//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsultGateRequiresFullChainThroughStartup (#575 D5): sessions built
// by the real startup path admit /consult only with -interceptors. The gate
// is never seeded by hand, so reverting it to "a chain is installed" fails
// here: the always-on guards are a chain that scans no advisory text.
func TestConsultGateRequiresFullChainThroughStartup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flags    []string
		admitted bool
	}{
		{"omitted", nil, false},
		{"explicit false", []string{"-interceptors=false"}, false},
		{"full chain", []string{"-interceptors"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			cfg := fakeConsultantsFile(t, "claude", consultTranscript("prefer the alt path"), 0)
			fx.withSession(t, append([]string{"-consultants-config", cfg}, tc.flags...), func(t *testing.T, sess *replSession) {
				out := slash(t, sess, "/consult claude how should I fix this?")
				// The fake consultant writes its stdin to this marker on a real
				// launch only; its --version probe does not.
				marker := filepath.Join(filepath.Dir(sess.consultants["claude"].Command), "stdin")
				_, statErr := os.Stat(marker)
				launched := statErr == nil
				if tc.admitted {
					if !launched || sess.advisory == nil {
						t.Fatalf("full chain: launched=%v advisory=%v out=%q", launched, sess.advisory, out)
					}
					return
				}
				if !strings.Contains(out, "consult requires -interceptors") || launched || sess.advisory != nil {
					t.Fatalf("guards only: launched=%v advisory=%v out=%q", launched, sess.advisory, out)
				}
			})
		})
	}
}
