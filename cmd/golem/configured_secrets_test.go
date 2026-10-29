package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
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

// configuredBlockedText is the agent error for a configured_secrets block of a
// model response. The dispatch envelope and the agentflow worker carry it
// verbatim.
const configuredBlockedText = "agent: output blocked by interceptor configured_secrets (configured_secret)"

// configuredProbeBody is the text of the file a configured-key turn reads, so a
// run that is not blocked shows that its call ran.
const configuredProbeBody = "configured probe: the call ran\n"

// withConfiguredKey gives fx's alt provider the api_key "${GOLEM_T_KEY}" and
// sets that variable to value through t.Setenv. It is the only variable the
// config references, and value is always a synthetic fixture.
func withConfiguredKey(t *testing.T, fx modelSwitchFixture, value string) {
	t.Helper()
	t.Setenv("GOLEM_T_KEY", value)
	raw, err := os.ReadFile(fx.configPath)
	if err != nil {
		t.Fatal(err)
	}
	const alt = `"alt": {`
	if strings.Count(string(raw), alt) != 1 {
		t.Fatal("fixture config has no single alt provider")
	}
	keyed := strings.Replace(string(raw), alt, alt+`"api_key": "${GOLEM_T_KEY}", `, 1)
	if err := os.WriteFile(fx.configPath, []byte(keyed), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeConfiguredProbe writes the file configuredScript(value) reads.
func writeConfiguredProbe(t *testing.T, root, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, value+".txt"), []byte(configuredProbeBody), 0o600); err != nil {
		t.Fatal(err)
	}
}

// configuredScript proposes c1, a read_file of value+".txt", as a turn's first
// response; the next response is the backend's labeled answer.
func configuredScript(value string) func(http.ResponseWriter, *http.Request, string) bool {
	args, _ := json.Marshal(map[string]string{"path": value + ".txt"})
	return scriptedToolCalls(sseToolCall("c1", "read_file", string(args)))
}

// testConfiguredBinding is testCanaryBinding with the exact guard set the way
// run() sets it from the bundle; match is the guard's credential lookup.
func testConfiguredBinding(t *testing.T, match func(string) bool) *canaryBinding {
	t.Helper()
	b := testCanaryBinding(t)
	guard, err := interceptor.NewConfiguredSecrets(match)
	if err != nil {
		t.Fatal(err)
	}
	b.configured = guard
	return b
}

// checkConfiguredRun judges one observation of a configured-key turn. An empty
// blockedAs means the call must run: the run completes with no finding, and
// the model is shown the file's text as c1's observation. Otherwise the path
// must report blockedAs without the fixture and send no request after the one
// that proposed the call, and a Result must carry exactly the
// configured_secrets finding. Observations are never printed: an unblocked
// row's carry the fixture.
func checkConfiguredRun(t *testing.T, o pathObservation, exposes exposure, blockedAs string) {
	t.Helper()
	if blockedAs == "" {
		if o.failure != "" {
			t.Errorf("%s: run failed, want the call to run", o.surface)
			return
		}
		if exposes != exposesWire && o.answer != o.label+" answer" {
			t.Errorf("%s: answer is not the %s backend's", o.surface, o.label)
		}
		if exposes == exposesResult && len(o.findings) != 0 {
			t.Errorf("%s: findings = %v, want none", o.surface, o.findings)
		}
		if got := c1Observation(t, o.bodies); got != configuredProbeBody {
			t.Errorf("%s: c1 observation is not the file's text", o.surface)
		}
		return
	}
	if leaksFixture(o.failure) || leaksFixture(o.answer) || leaksFixture(o.stderr) {
		t.Errorf("%s: the rejection carried the fixture", o.surface)
	} else if !strings.Contains(o.failure, blockedAs) {
		t.Errorf("%s: failure = %q, want it to contain %q", o.surface, o.failure, blockedAs)
	}
	if len(o.bodies) != 1 {
		t.Errorf("%s: sent %d chat requests, want only the one that proposed the call", o.surface, len(o.bodies))
	}
	if exposes == exposesResult {
		want := []findingPair{{"configured_secrets", "configured_secret"}}
		if o.score != 100 || !slices.Equal(o.findings, want) {
			t.Errorf("%s: risk = %d %v, want 100 %v", o.surface, o.score, o.findings, want)
		}
	}
}

// c1Observation returns what the model was shown for c1, read from the one
// request in bodies that carries it.
func c1Observation(t *testing.T, bodies []string) string {
	t.Helper()
	var seen []string
	for _, b := range bodies {
		var req wireRequest
		if err := json.Unmarshal([]byte(b), &req); err != nil {
			t.Fatal(err)
		}
		for _, m := range req.toolMessages() {
			if m.ToolCallID == "c1" {
				seen = append(seen, unframe(t, m.Content))
			}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("%d requests carried the c1 observation, want exactly 1", len(seen))
	}
	return seen[0]
}

// TestConfiguredSecretsEmptySetSessionRuns (#424 Review Focus 4): a config with
// no credential still installs configured_secrets under -interceptors, with an
// empty set. The startup notice names it exactly as with a credential, with no
// count, and a turn whose call carries the fixture runs on every surface,
// dispatch children included: an empty set blocks nothing and raises no hook
// error. Config: nil is TestBundleSecretMatcherFromEffectiveConfig's.
func TestConfiguredSecretsEmptySetSessionRuns(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	value := configuredSecretFixture()
	writeConfiguredProbe(t, fx.root, value)
	fx.primary.chatResponse = configuredScript(value)
	stderr := fx.withSession(t, []string{"-interceptors", "-dispatch"}, func(t *testing.T, sess *replSession) {
		for _, o := range sessionSurfaces(t, sess, fx.primary) {
			checkConfiguredRun(t, o, exposesResult, "")
		}
		checkConfiguredRun(t, dispatchObservation(t, sess, fx.primary), exposesScore, "")
	})
	if lines := strings.Split(strings.TrimSpace(stderr), "\n"); !slices.Contains(lines, fullNoticeLine) {
		t.Errorf("startup lines = %q, want the line %q", lines, fullNoticeLine)
	}
}

// TestModelSwitchKeepsConfiguredSecrets (#424 Review Focus 5): /model set
// rebuilds the orchestrator factory and the parent-following dispatch tool,
// and both keep the session's guard with the snapshot it started with. The
// variable changes after startup: the startup key stays blocked on every
// rebuilt surface, and the new value, which nothing resolved, runs (spec D2:
// a /model switch keeps the snapshot; rotation takes a new bundle).
func TestModelSwitchKeepsConfiguredSecrets(t *testing.T) {
	startup, rotated := configuredSecretFixture(), "cs_"+strings.Repeat("Hw4pRz8N", 3)
	fx := newModelSwitchFixture(t, "")
	withConfiguredKey(t, fx, startup)
	writeConfiguredProbe(t, fx.root, startup)
	writeConfiguredProbe(t, fx.root, rotated)
	fx.withSession(t, []string{"-interceptors", "-dispatch"}, func(t *testing.T, sess *replSession) {
		t.Setenv("GOLEM_T_KEY", rotated)
		mustSlash(t, sess, "/model set swap", "model: swap\n")
		for _, tc := range []struct{ name, value, blockedAs string }{
			{"startup key", startup, configuredBlockedText},
			{"rotated value", rotated, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				fx.alt.chatResponse = configuredScript(tc.value)
				for _, o := range sessionSurfaces(t, sess, fx.alt) {
					checkConfiguredRun(t, o, exposesResult, tc.blockedAs)
				}
				checkConfiguredRun(t, dispatchObservation(t, sess, fx.alt), exposesScore, tc.blockedAs)
			})
		}
	})
}

// TestConfiguredSecretsBlocksEscapedKeyInMalformedArguments (#424): the model
// writes the key as \u escapes in tool arguments Go's decoder rejects. The
// OpenAI-compatible client wraps them in a JSON string literal, and the
// guard's lenient pass still finds the key, so every surface blocks the turn
// before the call is shown, approved or run.
func TestConfiguredSecretsBlocksEscapedKeyInMalformedArguments(t *testing.T) {
	key := configuredSecretFixture()
	var esc strings.Builder
	for i := 0; i < len(key); i++ {
		_, _ = fmt.Fprintf(&esc, `\u%04x`, key[i])
	}
	malformed := `{"path":"` + esc.String() + `.txt"`
	fx := newModelSwitchFixture(t, "")
	withConfiguredKey(t, fx, key)
	fx.primary.chatResponse = scriptedToolCalls(sseToolCall("c1", "read_file", malformed))
	fx.withSession(t, []string{"-interceptors", "-dispatch"}, func(t *testing.T, sess *replSession) {
		for _, o := range sessionSurfaces(t, sess, fx.primary) {
			checkConfiguredRun(t, o, exposesResult, configuredBlockedText)
		}
		checkConfiguredRun(t, dispatchObservation(t, sess, fx.primary), exposesScore, configuredBlockedText)
	})
}

// TestInterceptorsNoticeNamesConfiguredSecrets (#424): with a configured key,
// the -interceptors startup notice names configured_secrets, first and by name
// only: the exact line carries no member, count or source, and nothing on
// stderr names the credential or the variable it came from. Without the flag
// the guards-only line names it among the additions.
func TestInterceptorsNoticeNamesConfiguredSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"interceptors", []string{"-interceptors"}, fullNoticeLine},
		{"guards only", nil, guardsNoticeLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModelSwitchFixture(t, "")
			withConfiguredKey(t, fx, configuredSecretFixture())
			stderr := fx.withSession(t, tc.args, func(*testing.T, *replSession) {})
			if leaksFixture(stderr) || strings.Contains(stderr, "GOLEM_T_KEY") {
				t.Fatal("startup output names the credential or its variable")
			}
			if lines := strings.Split(strings.TrimSpace(stderr), "\n"); !slices.Contains(lines, tc.want) {
				t.Errorf("startup lines = %q, want the line %q", lines, tc.want)
			}
		})
	}
}
