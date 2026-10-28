package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// The contract's fixed presentation, spelled out so these tests never read the
// production constant they pin.
const (
	sensitiveFixedMessage   = "run blocked: sensitive content detected"
	sensitiveTraceWarning   = "warning: trace not written: sensitive content detected"
	sessionNotSavedWarnLead = "warning: session not saved:"
)

// configuredSecretFixture is a synthetic stand-in for a configured credential.
// No guard is installed in these tests, so it is only ever detected through
// the block each test injects.
func configuredSecretFixture() string { return "cs_" + strings.Repeat("Tq7vXm2L", 3) }

// leaksFixture reports whether text carries the fixture literally or with
// every character written as a \uXXXX escape, the form an echoed JSON
// document would use.
func leaksFixture(text string) bool {
	var lower, upper strings.Builder
	for _, r := range configuredSecretFixture() {
		_, _ = fmt.Fprintf(&lower, `\u%04x`, r)
		_, _ = fmt.Fprintf(&upper, `\u%04X`, r)
	}
	return strings.Contains(text, configuredSecretFixture()) ||
		strings.Contains(text, lower.String()) || strings.Contains(text, upper.String())
}

// sensitiveIdentity is one way a run can end in a sensitive block. A later
// identity that is not an interceptor finding adds a row with its own block.
type sensitiveIdentity struct {
	name  string
	block func() error
}

func blockFinding(name, rule string) func() error {
	return func() error {
		return &agent.BlockedError{Hook: agent.HookOutput, Findings: []agent.Finding{{
			Interceptor: name, Rule: rule, Verdict: agent.VerdictBlock, Risk: 100,
		}}}
	}
}

func sensitiveIdentities() []sensitiveIdentity {
	return []sensitiveIdentity{
		{name: "secrets", block: blockFinding("secrets", "sensitive_openai_token")},
		{name: "configured_secrets", block: blockFinding("configured_secrets", "configured_secret")},
	}
}

// newSensitiveBlockSession builds a session with a real saving runtime, trace
// and telemetry, so a test can observe every persistence surface a block must
// keep clean. A nil store selects the session's own SQLite store.
func newSensitiveBlockSession(t *testing.T, caller agent.ModelCaller, store golemruntime.SessionStore) *replSession {
	t.Helper()
	root := t.TempDir()
	sess := newSessionedTestSession(t, caller, root, "workspace:sensitive-block")
	if store == nil {
		store = sess.session.store
	}
	runtime, err := golemruntime.New(context.Background(), golemruntime.Options{
		Root: root, System: sess.baseSystem, Orchestrator: sess.orch, Profile: golemruntime.ProfileTrusted,
		MaxSteps: 16, SessionStore: store, FailureMessage: runFailureMessage,
	})
	if err != nil {
		t.Fatal("cannot create runtime")
	}
	t.Cleanup(func() { _ = runtime.Close() })
	sess.runtime = runtime
	data := t.TempDir()
	obs, err := newObserv(func(key string) string {
		if key == "XDG_DATA_HOME" {
			return data
		}
		return ""
	}, root, true, true, func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatal("cannot initialize observability")
	}
	sess.obs = obs
	return sess
}

// runGoalREPL feeds one goal through the real REPL and returns what the REPL
// printed and which goals it recorded for history.
func runGoalREPL(t *testing.T, sess *replSession, goal string) (output string, recorded []string) {
	t.Helper()
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader(goal+"\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal("runREPL unexpectedly failed")
	}
	return out.String(), src.recorded
}

func traceFileCount(t *testing.T, sess *replSession) int {
	t.Helper()
	entries, err := os.ReadDir(sess.obs.traceDir)
	if err != nil {
		t.Fatal("cannot inspect trace directory")
	}
	return len(entries)
}

func sessionRowSaved(t *testing.T, sess *replSession) bool {
	t.Helper()
	_, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil && !errors.Is(err, conversation.ErrNotFound) {
		t.Fatal("cannot inspect the session store")
	}
	return err == nil
}

// telemetryRunStatuses returns the status of every run span and fails when the
// file is missing or leaks the fixture, so a block cannot pass by leaving no
// record at all.
func telemetryRunStatuses(t *testing.T, sess *replSession) []string {
	t.Helper()
	raw, err := os.ReadFile(sess.obs.telemetryPath)
	if err != nil {
		t.Fatal("telemetry file unreadable")
	}
	// Schema tripwire: the run span carries only enumerated and numeric fields
	// today, so this cannot fail now; the caller's status assertion discriminates.
	if leaksFixture(string(raw)) {
		t.Error("telemetry leaked the fixture")
	}
	var statuses []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var span map[string]any
		if json.Unmarshal([]byte(line), &span) != nil {
			t.Fatal("telemetry is not valid JSONL")
		}
		if span["kind"] == "run" {
			status, _ := span["status"].(string)
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func TestConfiguredSecretsBlockSkipsGoalHistory(t *testing.T) {
	// The controls show the same session records, saves, traces and demotes
	// when nothing sensitive happened, so an empty record below is suppression
	// and not a session that never records anything.
	if !t.Run("control", func(t *testing.T) {
		t.Run("clean run", func(t *testing.T) {
			caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "ordinary answer"}}}}
			sess := newSensitiveBlockSession(t, caller, nil)
			out, recorded := runGoalREPL(t, sess, "ordinary goal")
			if !slices.Equal(recorded, []string{"ordinary goal"}) {
				t.Errorf("recorded = %q, want the ordinary goal", recorded)
			}
			if strings.Contains(out, sensitiveFixedMessage) || strings.Contains(out, sensitiveTraceWarning) {
				t.Error("clean run printed the sensitive presentation")
			}
			if traceFileCount(t, sess) != 1 || !sessionRowSaved(t, sess) {
				t.Error("clean run did not write its trace and session row")
			}
			if statuses := telemetryRunStatuses(t, sess); !slices.Equal(statuses, []string{"completed"}) {
				t.Errorf("telemetry run statuses = %q, want one completed run", statuses)
			}
		})
		t.Run("unrelated session save error", func(t *testing.T) {
			caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "completed answer"}}}}
			sess := newSensitiveBlockSession(t, caller, secretSaveFailureStore{err: errors.New("store unavailable")})
			out, recorded := runGoalREPL(t, sess, "ordinary goal")
			if !strings.Contains(out, sessionNotSavedWarnLead) || strings.Contains(out, "error: ") {
				t.Error("an unrelated session save error was not demoted to a warning")
			}
			if !slices.Equal(recorded, []string{"ordinary goal"}) {
				t.Errorf("recorded = %q, want the ordinary goal", recorded)
			}
		})
	}) {
		return
	}
	for _, id := range sensitiveIdentities() {
		t.Run(id.name, func(t *testing.T) {
			fixture := configuredSecretFixture()
			goal := "deploy with " + fixture
			t.Run("model block", func(t *testing.T) {
				side := errors.New("provider diagnostic: " + fixture)
				sess := newSensitiveBlockSession(t, errCaller{err: errors.Join(id.block(), side)}, nil)
				out, recorded := runGoalREPL(t, sess, goal)
				if len(recorded) != 0 {
					t.Errorf("recorded = %q, want no goal in history", recorded)
				}
				if !strings.Contains(out, "error: "+sensitiveFixedMessage+"\n") || leaksFixture(out) {
					t.Error("output did not carry only the fixed message")
				}
				if !strings.Contains(out, sensitiveTraceWarning+"\n") || traceFileCount(t, sess) != 0 {
					t.Error("blocked run did not warn and skip its trace")
				}
				// Runtime tripwire: a failed model call never saves a session, so this
				// passes whatever the classification; the "session save error stays an
				// error" rows discriminate.
				if sessionRowSaved(t, sess) {
					t.Error("blocked run saved a session row")
				}
				if statuses := telemetryRunStatuses(t, sess); !slices.Equal(statuses, []string{"error"}) {
					t.Errorf("telemetry run statuses = %q, want one error run", statuses)
				}
			})
			t.Run("session save error stays an error", func(t *testing.T) {
				side := errors.New("store diagnostic: " + fixture)
				caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "completed answer"}}}}
				sess := newSensitiveBlockSession(t, caller, secretSaveFailureStore{err: errors.Join(id.block(), side)})
				out, recorded := runGoalREPL(t, sess, goal)
				if len(recorded) != 0 {
					t.Errorf("recorded = %q, want no goal in history", recorded)
				}
				if strings.Contains(out, sessionNotSavedWarnLead) {
					t.Error("a sensitive session error was demoted to a warning")
				}
				if !strings.Contains(out, "error: "+sensitiveFixedMessage+"\n") || leaksFixture(out) {
					t.Error("output did not carry only the fixed message")
				}
				if !strings.Contains(out, sensitiveTraceWarning+"\n") || traceFileCount(t, sess) != 0 {
					t.Error("blocked run did not warn and skip its trace")
				}
				if statuses := telemetryRunStatuses(t, sess); !slices.Equal(statuses, []string{"error"}) {
					t.Errorf("telemetry run statuses = %q, want one error run", statuses)
				}
			})
		})
	}
}

func TestConfiguredSecretsJoinedErrorsPresentFixedMessage(t *testing.T) {
	fixture := configuredSecretFixture()
	configured := blockFinding("configured_secrets", "configured_secret")
	sinkErr := errors.New("sink diagnostic: " + fixture)
	sealErr := errors.New("checkpoint diagnostic: " + fixture)
	for _, tc := range []struct {
		name string
		// sensitive rows must present only the fixed message; the controls
		// show the same joined text is printed when no sensitive block exists.
		sensitive bool
		// setup returns the session, the joined causes errors.Is must find,
		// and the event writer when the row watches run.failed.
		setup func(t *testing.T) (*replSession, []error, *secretEventFailureWriter)
	}{
		{
			name: "terminal sink failure", sensitive: true,
			setup: func(t *testing.T) (*replSession, []error, *secretEventFailureWriter) {
				return sinkFailureSession(t, errCaller{err: configured()}, sinkErr)
			},
		},
		{
			name: "checkpoint seal failure beside a model block", sensitive: true,
			setup: func(t *testing.T) (*replSession, []error, *secretEventFailureWriter) {
				return sealFailureSession(t, sealErr, func(latch func(error)) agent.ModelCaller {
					return secretPartialCaller{err: configured(), before: func() { latch(sealErr) }}
				})
			},
		},
		{
			name: "checkpoint seal failure carrying the block", sensitive: true,
			setup: func(t *testing.T) (*replSession, []error, *secretEventFailureWriter) {
				return sealFailureSession(t, sealErr, func(latch func(error)) agent.ModelCaller {
					return secretPartialCaller{content: "completed answer", before: func() { latch(errors.Join(configured(), sealErr)) }}
				})
			},
		},
		{
			name: "control: terminal sink failure beside an unrelated failure",
			setup: func(t *testing.T) (*replSession, []error, *secretEventFailureWriter) {
				return sinkFailureSession(t, errCaller{err: errors.New("provider unavailable")}, sinkErr)
			},
		},
		{
			name: "control: checkpoint seal failure beside an unrelated failure",
			setup: func(t *testing.T) (*replSession, []error, *secretEventFailureWriter) {
				return sealFailureSession(t, sealErr, func(latch func(error)) agent.ModelCaller {
					return secretPartialCaller{err: errors.New("provider unavailable"), before: func() { latch(sealErr) }}
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, causes, writer := tc.setup(t)
			var out strings.Builder
			_, err := runOnce(context.Background(), &out, nil, sess, "review output", nil)
			if err == nil {
				t.Fatal("runOnce unexpectedly succeeded")
			}
			for _, cause := range causes {
				if !errors.Is(err, cause) {
					t.Errorf("errors.Is lost joined cause %q", cause)
				}
			}
			var blocked *agent.BlockedError
			if tc.sensitive != errors.As(err, &blocked) {
				t.Error("runOnce changed the typed block")
			}
			lines := strings.Split(out.String(), "\n")
			if tc.sensitive {
				if !slices.Contains(lines, "error: "+sensitiveFixedMessage) {
					t.Error("REPL line is not the fixed message")
				}
				if leaksFixture(out.String()) {
					t.Error("a printed line leaked the fixture")
				}
				for _, line := range lines {
					if strings.HasPrefix(line, "checkpoint:") {
						t.Error("a checkpoint line was printed after a sensitive block")
					}
				}
			} else if !leaksFixture(out.String()) {
				t.Error("control printed no joined cause, so the leak check above could not fail")
			}
			if writer == nil {
				return
			}
			var failed []golemruntime.Event
			for _, event := range writer.events {
				if event.Type == "run.failed" {
					failed = append(failed, event)
				}
				if tc.sensitive && leaksFixture(string(event.Payload)) {
					t.Errorf("%s event leaked the fixture", event.Type)
				}
			}
			if len(failed) != 1 {
				t.Fatalf("run.failed events = %d, want 1", len(failed))
			}
			var payload struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(failed[0].Payload, &payload) != nil {
				t.Fatal("run.failed payload is not JSON")
			}
			if tc.sensitive && payload.Message != sensitiveFixedMessage {
				t.Errorf("run.failed message = %q, want the fixed message", payload.Message)
			}
			// The sink failure is raised by this very event, so its text can
			// never be in the event; the control shows an unrelated failure
			// keeps its own message instead of being rewritten.
			if !tc.sensitive && payload.Message != "provider unavailable" {
				t.Errorf("control run.failed message = %q, want the unrelated failure unchanged", payload.Message)
			}
		})
	}
}

// sinkFailureSession fails the machine event sink at run.failed, so the
// runtime joins sinkErr onto the run error.
func sinkFailureSession(t *testing.T, caller agent.ModelCaller, sinkErr error) (*replSession, []error, *secretEventFailureWriter) {
	t.Helper()
	sess := newTestSession(t, caller, t.TempDir())
	writer := &secretEventFailureWriter{failType: "run.failed", err: sinkErr}
	sess.machine = newMachineWriter(writer, outputStreamJSON)
	return sess, []error{sinkErr}, writer
}

// sealFailureSession builds a checkpointing session whose caller latches a
// seal failure, through latch, while the run is in flight. The seal failure
// is sealErr or carries it, so errors.Is must still find it afterward.
func sealFailureSession(t *testing.T, sealErr error, caller func(latch func(error)) agent.ModelCaller) (*replSession, []error, *secretEventFailureWriter) {
	t.Helper()
	root := t.TempDir()
	sess, journal := newCheckpointWriteSession(t, &scriptCaller{}, root)
	orch := agent.New(caller(func(err error) {
		journal.mu.Lock()
		journal.fatal = err
		journal.mu.Unlock()
	}), agent.ContextManager{})
	sess.orch = orch
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, orch, sess.tools)
	return sess, []error{sealErr}, nil
}
