//go:build unix

package main

import (
	"errors"
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

// TestAgentflowStatusPrintsNoChainNotice (§4.3): -agentflow-status returns
// before chain construction for every flag value, so neither notice appears
// on either channel.
func TestAgentflowStatusPrintsNoChainNotice(t *testing.T) {
	for _, flag := range [][]string{nil, {"-interceptors=false"}, {"-interceptors"}} {
		root := t.TempDir()
		t.Setenv("PATH", writeFakeAgentflow(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
		const payload = "{\"state\":\"uninitialized\"}\n"
		t.Setenv("GOLEM_AGENTFLOW_STATUS_PAYLOAD", payload)
		stdin, stdout, stderr := runTestFiles(t)
		err := run(append([]string{"-agentflow-status", "-json", "-root", root}, flag...), stdin, stdout, stderr)
		// The status path must really have run, or an early argument error
		// would also print no notice: it relays the payload and exits 3.
		var statusErr *agentflowStatusExit
		if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
			t.Fatalf("flags %v: run error = %v, want agentflow status exit 3", flag, err)
		}
		if got := readRunTestFile(t, stdout); got != payload {
			t.Fatalf("flags %v: stdout = %q, want the relayed payload %q", flag, got, payload)
		}
		for name, s := range map[string]string{"stdout": readRunTestFile(t, stdout), "stderr": readRunTestFile(t, stderr)} {
			if strings.Contains(s, "guards:") || strings.Contains(s, "interceptors:") {
				t.Fatalf("flags %v: %s = %q, want no chain notice", flag, name, s)
			}
		}
	}
}
