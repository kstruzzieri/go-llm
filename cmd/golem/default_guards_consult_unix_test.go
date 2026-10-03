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

// TestAgentflowStatusPrintsNoChainNotice (#575): -agentflow-status returns
// before chain construction for every flag value, so no chain notice appears:
// stdout is exactly the relayed status payload and stderr is empty.
func TestAgentflowStatusPrintsNoChainNotice(t *testing.T) {
	const payload = "{\"state\":\"uninitialized\"}\n"
	t.Setenv("PATH", writeFakeAgentflow(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOLEM_AGENTFLOW_STATUS_PAYLOAD", payload)
	for _, tc := range []struct {
		name string
		flag []string
	}{
		{"omitted", nil},
		{"explicit false", []string{"-interceptors=false"}},
		{"full chain", []string{"-interceptors"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin, stdout, stderr := runTestFiles(t)
			// The fake agentflow reads its payload from the environment, which
			// Agentflow children receive only for approved names (#577).
			args := []string{"-agentflow-status", "-json", "-root", t.TempDir(), "-agentflow-env", "GOLEM_AGENTFLOW_STATUS_PAYLOAD"}
			err := run(append(args, tc.flag...), stdin, stdout, stderr)
			// The status path must really have run, or an early argument error
			// would also print no notice: it relays the payload and exits 3.
			var statusErr *agentflowStatusExit
			if !errors.As(err, &statusErr) || statusErr.ExitCode() != 3 {
				t.Fatalf("run error = %v, want agentflow status exit 3", err)
			}
			if got := readRunTestFile(t, stdout); got != payload {
				t.Fatalf("stdout = %q, want the relayed payload %q", got, payload)
			}
			if got := readRunTestFile(t, stderr); got != "" {
				t.Fatalf("stderr = %q, want empty", got)
			}
		})
	}
}
