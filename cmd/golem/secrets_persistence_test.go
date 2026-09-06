package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

func secretTestValue() string { return "sk-" + strings.Repeat("ab7C9DeF", 3) }

func TestSecretHistoryWalksEveryErrorBranchAndFinding(t *testing.T) {
	secret := &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictBlock}}}
	other := &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "other", Verdict: agent.VerdictAbort}}}
	for _, tc := range []struct {
		name   string
		err    error
		record bool
	}{
		{name: "success", record: true},
		{name: "unrelated failure", err: errors.New("provider unavailable"), record: true},
		{name: "other block", err: other, record: true},
		{name: "empty block", err: &agent.BlockedError{}, record: true},
		{name: "secrets tag", err: &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictTag}}}, record: true},
		{name: "secrets block", err: secret},
		{name: "secrets abort", err: &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictAbort}}}},
		{name: "later finding", err: &agent.BlockedError{Findings: append(append([]agent.Finding(nil), other.Findings...), secret.Findings...)}},
		{name: "later sibling", err: errors.Join(other, secret)},
		{name: "nested wrappers", err: fmt.Errorf("outer: %w", errors.Join(other, fmt.Errorf("inner: %w", errors.Join(errors.New("side error"), secret))))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			goal := secretTestValue()
			sess := newTestSession(t, errCaller{err: tc.err}, t.TempDir())
			src := &recordingSource{scannerSource: newScannerSource(strings.NewReader(goal+"\n"), &out)}
			if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
				t.Fatal("runREPL unexpectedly failed")
			}
			if got := len(src.recorded) == 1 && src.recorded[0] == goal; got != tc.record {
				t.Errorf("runREPL recorded goal = %v, want %v", got, tc.record)
			}
		})
	}
}

// Return collected content without streaming it: streamed deltas are outside
// the diagnostic-suppression contract, even when the final output is blocked.
type secretPartialCaller struct {
	content string
	err     error
	before  func()
}

func (c secretPartialCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.before != nil {
		c.before()
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: c.content}}, c.err
}

func TestSecretTraceAndDiagnosticsSuppressJoinedProviderError(t *testing.T) {
	content := secretTestValue()
	providerValue := "npm_" + strings.Repeat("9Ax2Bcd7", 3)
	providerErr := errors.New("provider diagnostic: " + providerValue)
	for _, cancelErr := range []error{nil, context.Canceled} {
		name := "error"
		if cancelErr != nil {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			caller := secretPartialCaller{content: content, err: errors.Join(providerErr, cancelErr)}
			sess, traceDir := newTracingSession(t, caller)
			sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
			sess.runtime = newTestRuntime(t, t.TempDir(), sess.baseSystem, sess.orch, nil)
			var out strings.Builder
			res, err := runOnce(context.Background(), &out, nil, sess, "review the output", nil)
			var blocked *agent.BlockedError
			if !errors.As(err, &blocked) || !errors.Is(err, providerErr) || (cancelErr != nil && !errors.Is(err, cancelErr)) {
				t.Fatal("runOnce did not preserve the complete typed error tree")
			}
			if res.Risk == nil || len(res.Risk.Findings) == 0 {
				t.Fatal("blocked output lost its risk findings")
			}
			entries, readErr := os.ReadDir(traceDir)
			if readErr != nil {
				t.Fatal("cannot inspect trace directory")
			}
			if len(entries) != 0 {
				t.Errorf("trace file count = %d, want 0", len(entries))
			}
			if strings.Contains(out.String(), content) || strings.Contains(out.String(), providerValue) {
				t.Error("blocked run leaked sensitive content in diagnostics")
			}
			if !strings.Contains(out.String(), "error: run blocked: sensitive content detected\n") ||
				!strings.Contains(out.String(), "warning: trace not written: sensitive content detected\n") {
				t.Error("blocked run did not emit the fixed error and trace warning")
			}
		})
	}
}

type secretDeltaFailureCaller struct{}

func (secretDeltaFailureCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	// Only harmless text is streamed. The collected response still needs
	// inspection after the sink rejects that earlier delta.
	err := onToken(provider.ChatResponse{Content: "ordinary partial output"})
	return agent.ModelResult{Response: provider.ChatResponse{Content: secretTestValue()}}, err
}

type secretEventFailureWriter struct {
	events   []golemruntime.Event
	failType string
	err      error
}

func (w *secretEventFailureWriter) Write(p []byte) (int, error) {
	var event golemruntime.Event
	if json.Unmarshal(p, &event) != nil {
		return 0, errors.New("test received invalid runtime event")
	}
	w.events = append(w.events, event)
	if event.Type == w.failType {
		return 0, w.err
	}
	return len(p), nil
}

func TestSecretRuntimeSideErrorsPreserveClassification(t *testing.T) {
	sinkErr := errors.New("sink diagnostic: " + secretTestValue())
	for _, tc := range []struct {
		name       string
		goal       string
		caller     agent.ModelCaller
		cancel     bool
		failType   string
		wantErr    error
		wantHook   agent.Hook
		wantEvents []string
	}{
		{
			name: "cancellation after detection", goal: secretTestValue(), caller: &scriptCaller{},
			cancel: true, wantErr: context.Canceled, wantHook: agent.HookInput,
			wantEvents: []string{"run.started", "run.canceled"},
		},
		{
			name: "terminal sink failure", goal: secretTestValue(), caller: &scriptCaller{},
			failType: "run.failed", wantErr: sinkErr, wantHook: agent.HookInput,
			wantEvents: []string{"run.started", "run.failed"},
		},
		{
			name: "delta sink failure before collected output block", goal: "review output", caller: secretDeltaFailureCaller{},
			failType: "message.delta", wantErr: sinkErr, wantHook: agent.HookOutput,
			wantEvents: []string{"run.started", "message.delta"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, repl := range []bool{false, true} {
				name := "runOnce"
				if repl {
					name = "runREPL"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					sess, traceDir := newTracingSession(t, tc.caller)
					sess.orch = newOrchestratorFactory(tc.caller, flags{interceptors: true}, nil)()
					runtime, err := golemruntime.New(ctx, golemruntime.Options{
						Root: t.TempDir(), System: sess.baseSystem, Orchestrator: sess.orch,
						FailureMessage: func(code string, cause error) string {
							if tc.cancel && secretsBlocked(cause) {
								cancel() // Cancel the real runtime context after detection.
							}
							return runFailureMessage(code, cause)
						},
					})
					if err != nil {
						t.Fatal("cannot create runtime")
					}
					t.Cleanup(func() { _ = runtime.Close() })
					sess.runtime = runtime
					writer := &secretEventFailureWriter{failType: tc.failType, err: sinkErr}
					sess.machine = newMachineWriter(writer, outputStreamJSON)
					var out strings.Builder
					if repl {
						src := &recordingSource{scannerSource: newScannerSource(strings.NewReader(tc.goal+"\n"), &out)}
						if err := runREPL(ctx, src, &out, nil, sess); err != nil {
							t.Error("runREPL unexpectedly failed")
						}
						if len(src.recorded) != 0 {
							t.Error("runREPL recorded a classified blocked goal")
						}
					} else {
						res, err := runOnce(ctx, &out, nil, sess, tc.goal, nil)
						var blocked *agent.BlockedError
						if !errors.As(err, &blocked) || blocked.Hook != tc.wantHook || !secretsBlocked(err) {
							t.Error("runOnce lost the typed secrets block")
						}
						if !errors.Is(err, tc.wantErr) {
							t.Error("runOnce lost the cancellation or sink cause")
						}
						if res.Risk == nil || len(res.Risk.Findings) == 0 {
							t.Error("runOnce lost the detected risk findings")
						}
					}
					entries, readErr := os.ReadDir(traceDir)
					if readErr != nil || len(entries) != 0 {
						t.Error("blocked run did not leave the full-trace directory empty")
					}
					if strings.Contains(out.String(), secretTestValue()) ||
						!strings.Contains(out.String(), "error: "+secretsBlockedMessage+"\n") ||
						!strings.Contains(out.String(), "warning: trace not written: sensitive content detected\n") {
						t.Error("blocked run did not use only safe fixed diagnostics")
					}
					var eventTypes []string
					for _, event := range writer.events {
						eventTypes = append(eventTypes, event.Type)
						if strings.Contains(string(event.Payload), secretTestValue()) {
							t.Error("runtime event leaked sensitive content")
						}
						if event.Type == "run.canceled" && string(event.Payload) != "{}" {
							t.Error("run.canceled changed its empty payload")
						}
					}
					if !slices.Equal(eventTypes, tc.wantEvents) {
						t.Error("runtime events did not match the expected cancellation or sink boundary")
					}
				})
			}
		})
	}
}

type secretSaveFailureStore struct{ err error }

func (secretSaveFailureStore) Load(context.Context, string) (*conversation.Conversation, error) {
	return nil, conversation.ErrNotFound
}

func (s secretSaveFailureStore) Save(context.Context, conversation.Conversation) error { return s.err }

func TestSecretSessionSaveErrorIsNotDemoted(t *testing.T) {
	root := t.TempDir()
	blocked := &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictBlock}}}
	sideErr := errors.New("store diagnostic: " + secretTestValue())
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "completed answer"}}}}
	sess := newSessionedTestSession(t, caller, root, "workspace:secret-save")
	runtime, err := golemruntime.New(context.Background(), golemruntime.Options{
		Root: root, Orchestrator: sess.orch,
		SessionStore: secretSaveFailureStore{err: errors.Join(blocked, sideErr)},
	})
	if err != nil {
		t.Fatal("cannot create runtime")
	}
	t.Cleanup(func() { _ = runtime.Close() })
	sess.runtime = runtime
	var out strings.Builder
	res, err := runOnce(context.Background(), &out, nil, sess, "finish the answer", nil)
	if !errors.Is(err, golemruntime.ErrSessionPersistence) || !errors.Is(err, blocked) || !errors.Is(err, sideErr) {
		t.Error("runOnce demoted or lost a joined secrets session error")
	}
	if res.Answer != "completed answer" {
		t.Error("runOnce changed the caller-owned result")
	}
	if strings.Contains(out.String(), secretTestValue()) || strings.Contains(out.String(), "warning: session not saved:") {
		t.Error("session-save demotion leaked a classified error")
	}
}

func TestSecretCheckpointSealDiagnosticUsesCompleteErrorTree(t *testing.T) {
	sealErr := errors.New("checkpoint diagnostic: " + secretTestValue())
	var journal *checkpointJournal
	caller := secretPartialCaller{
		content: secretTestValue(), err: errors.New("provider stopped"),
		before: func() {
			// Inject the retained seal error without canceling the preceding
			// output inspection, which must still classify the model response.
			journal.mu.Lock()
			journal.fatal = sealErr
			journal.mu.Unlock()
		},
	}
	root := t.TempDir()
	sess, j := newCheckpointWriteSession(t, caller, root)
	journal = j
	sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, sess.tools)
	var out strings.Builder
	_, err := runOnce(context.Background(), &out, nil, sess, "review output", nil)
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) || !errors.Is(err, sealErr) {
		t.Fatal("runOnce lost the joined block or checkpoint seal error")
	}
	if strings.Contains(out.String(), secretTestValue()) {
		t.Error("checkpoint diagnostic leaked a side-error before classification")
	}
}

func TestSecretCheckpointLatchedErrorUsesFixedDiagnostic(t *testing.T) {
	caller := &scriptCaller{}
	sess, journal := newCheckpointWriteSession(t, caller, t.TempDir())
	blocked := &agent.BlockedError{Findings: []agent.Finding{{Interceptor: "secrets", Verdict: agent.VerdictBlock}}}
	sideErr := errors.New("retained diagnostic: " + secretTestValue())
	journal.fatal = errors.Join(blocked, sideErr)
	var out strings.Builder
	_, err := runOnce(context.Background(), &out, nil, sess, "another turn", nil)
	if caller.i != 0 || !errors.Is(err, blocked) || !errors.Is(err, sideErr) {
		t.Fatal("latched checkpoint failure did not preserve the original errors before running")
	}
	if strings.Contains(out.String(), secretTestValue()) || !strings.Contains(out.String(), secretsBlockedMessage) {
		t.Error("latched checkpoint failure did not use the fixed diagnostic")
	}
}

func TestSecretInitialBlockLeavesSessionAndCheckpointEmpty(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{}
	sess := newSessionedTestSession(t, caller, root, "workspace:initial-block")
	writeSess, journal := newCheckpointWriteSession(t, caller, root)
	sess.journal, sess.tools = journal, writeSess.tools
	sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, sess.tools)
	goal := secretTestValue()
	var out strings.Builder
	res, err := runOnce(context.Background(), &out, nil, sess, goal, nil)
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) || blocked.Hook != agent.HookInput || caller.i != 0 {
		t.Fatal("initial input was not blocked before the provider call")
	}
	retained := false
	for _, message := range res.Messages {
		retained = retained || (message.Role == "user" && message.Content == goal)
	}
	if !retained {
		t.Error("blocked Result lost the caller-owned original goal")
	}
	if _, err := sess.session.store.Load(context.Background(), sess.session.id); !errors.Is(err, conversation.ErrNotFound) {
		t.Error("initial block saved a conversation session")
	}
	infos, err := journal.store.list(context.Background())
	if err != nil || len(infos) != 0 {
		t.Error("initial block created a checkpoint row")
	}
	if strings.Contains(out.String(), goal) {
		t.Error("initial block leaked its caller-owned transcript to diagnostics")
	}
}

func TestSecretLateOutputBlockSealsEarlierCheckpointForUndo(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{
		writeToolCallResponse("write-1", "allowed.txt", "permitted write\n"),
		{Response: provider.ChatResponse{Content: secretTestValue()}},
	}}
	sess, journal := newCheckpointWriteSession(t, caller, root)
	sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, sess.tools)
	var out strings.Builder
	_, err := runOnce(context.Background(), &out, nil, sess, "write allowed.txt", &stubAnswerSource{line: "y", ok: true})
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) || blocked.Hook != agent.HookOutput {
		t.Fatal("late model output was not blocked")
	}
	path := filepath.Join(root, "allowed.txt")
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "permitted write\n" {
		t.Fatal("permitted write did not land before the output block")
	}
	infos, err := journal.store.list(context.Background())
	if err != nil || len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 1 {
		t.Fatal("late block did not retain a sealed checkpoint for the earlier write")
	}
	journal.undo(context.Background(), &out, 1)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("earlier permitted write was not undoable after the block")
	}
}

func TestSecretPreInspectionFailureRetainsHistoryAndTrace(t *testing.T) {
	caller := &scriptCaller{}
	sess, traceDir := newTracingSession(t, caller)
	sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
	sess.runtime = newTestRuntime(t, t.TempDir(), sess.baseSystem, sess.orch, nil)
	// The test runtime's 64 KiB message limit rejects this before inspection.
	// Such unclassified failures deliberately retain their previous behavior.
	goal := strings.Repeat("ordinary ", 8<<10) + secretTestValue()
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader(goal+"\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal("runREPL failed while handling a pre-inspection rejection")
	}
	if caller.i != 0 || len(src.recorded) != 1 || src.recorded[0] != goal {
		t.Error("pre-inspection rejection changed provider/history behavior")
	}
	entries, err := os.ReadDir(traceDir)
	if err != nil || len(entries) != 1 {
		t.Fatal("pre-inspection rejection did not preserve the existing trace")
	}
	raw, err := os.ReadFile(filepath.Join(traceDir, entries[0].Name()))
	if err != nil || !strings.Contains(string(raw), secretTestValue()) {
		t.Error("pre-inspection rejection unexpectedly scrubbed the unclassified trace")
	}
}

func TestSecretTelemetryCountsRuntimeFindings(t *testing.T) {
	value := secretTestValue()
	for _, tc := range []struct {
		name    string
		goal    string
		caller  agent.ModelCaller
		blocked bool
		count   int
	}{
		{"initial block", value, &scriptCaller{}, true, 1},
		{"output block", "review output", secretPartialCaller{content: value}, true, 1},
		{"replaced observation", "read remote", &oneCallCaller{name: "remote"}, false, 1},
		{"clean completion", "answer briefly", &scriptCaller{}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, data := t.TempDir(), t.TempDir()
			sess := newTestSession(t, tc.caller, root)
			sess.orch = newOrchestratorFactory(tc.caller, flags{interceptors: true}, nil)()
			sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, []agent.Tool{foreignTool{content: value}})
			obs, err := newObserv(func(key string) string {
				if key == "XDG_DATA_HOME" {
					return data
				}
				return ""
			}, root, false, true, func() time.Time { return time.Unix(0, 0) })
			if err != nil {
				t.Fatal("cannot initialize telemetry")
			}
			sess.obs = obs
			var out strings.Builder
			res, err := runOnce(context.Background(), &out, nil, sess, tc.goal, nil)
			if (err != nil) != tc.blocked {
				t.Fatal("runtime outcome did not match the expected block/completion")
			}
			if !tc.blocked && res.Answer != "done" {
				t.Error("permitted run did not complete after observation handling")
			}
			raw, err := os.ReadFile(obs.telemetryPath)
			if err != nil || strings.Contains(string(raw), value) {
				t.Fatal("runtime telemetry unreadable or leaked sensitive content")
			}
			runs := 0
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				var span map[string]any
				if json.Unmarshal([]byte(line), &span) != nil {
					t.Fatal("runtime telemetry is not valid JSONL")
				}
				if span["kind"] != "run" {
					continue
				}
				runs++
				count, exists := span["secret_findings"]
				if (tc.count == 0 && exists) || (tc.count != 0 && count != float64(tc.count)) {
					t.Errorf("runtime secret_findings = %v, want %d (zero omitted)", count, tc.count)
				}
				if (span["status"] == "error") != tc.blocked {
					t.Error("telemetry changed the run status")
				}
			}
			if runs != 1 {
				t.Errorf("run span count = %d, want 1", runs)
			}
		})
	}
}
