package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// #767 end to end: a reply that is only reasoning reaches Golem through the
// real CLI, the openai-compat client and a stub server, and Golem spends one
// tool-free request asking for the answer.

const (
	recoveryReasoning = "The code word is PELICAN."
	// Written out rather than read from the agent package, so wording drift fails here.
	recoveryNudge = "Your last reply had reasoning but no answer. Reply now with your final answer only."
)

// sseReasoning streams text as a reasoning_content delta and then stops with
// no content.
func sseReasoning(text string) []string {
	return []string{
		fmt.Sprintf(`{"model":"agent-model","choices":[{"delta":{"reasoning_content":%q}}]}`, text),
		`{"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}
}

// endsWithNudge reports whether req is a recovery request: its last two
// messages are the placeholder assistant turn and the nudge.
func (r wireRequest) endsWithNudge() bool {
	n := len(r.Messages)
	return n >= 2 && r.Messages[n-2] == wireMessage{Role: "assistant", Content: "(reasoning only, no reply)"} &&
		r.Messages[n-1] == wireMessage{Role: "user", Content: recoveryNudge}
}

// recoveryScript answers every request with reasoning alone, except a
// recovery request when answer is set.
func recoveryScript(answer string) func(wireRequest) []string {
	return func(req wireRequest) []string {
		if answer != "" && req.endsWithNudge() {
			return sseAnswer(answer)
		}
		return sseReasoning(recoveryReasoning)
	}
}

// runRecoveryOneShot runs one -p turn and returns its stdout, stderr and error.
func runRecoveryOneShot(t *testing.T, configPath, root string, extra ...string) (string, string, error) {
	t.Helper()
	args := append([]string{"-config", configPath, "-root", root, "-p", "what is the code word?",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context"}, extra...)
	stdin, stdout, stderr := runTestFiles(t)
	err := run(args, stdin, stdout, stderr)
	return readRunTestFile(t, stdout), readRunTestFile(t, stderr), err
}

func hasLinePrefix(text, prefix string) bool {
	return slices.ContainsFunc(strings.Split(text, "\n"), func(line string) bool {
		return strings.HasPrefix(line, prefix)
	})
}

// assertRecoveryRequests checks that the run made exactly two chat requests:
// the first offering Golem's tools, the second a tool-free recovery request.
func assertRecoveryRequests(t *testing.T, reqs []wireRequest) {
	t.Helper()
	if len(reqs) != 2 {
		t.Fatalf("chat requests = %d, want 2", len(reqs))
	}
	if len(reqs[0].Tools) == 0 {
		t.Errorf("first request offers no tools, want Golem's mounted tools")
	}
	if len(reqs[1].Tools) != 0 {
		t.Errorf("recovery request offers %d tools, want none", len(reqs[1].Tools))
	}
	if !reqs[1].endsWithNudge() {
		t.Errorf("recovery request does not end with the placeholder assistant turn and the nudge: %+v", reqs[1].Messages)
	}
}

func TestFinalAnswerRecoveryWire_TextOneShot(t *testing.T) {
	t.Run("recovered", func(t *testing.T) {
		configPath, root, requests := fenceWireHarness(t, recoveryScript("PELICAN"))
		stdout, stderr, err := runRecoveryOneShot(t, configPath, root)
		if err != nil {
			t.Fatalf("run: %v\nstderr:\n%s", err, stderr)
		}
		assertRecoveryRequests(t, requests())
		if stdout != "PELICAN\n" {
			t.Errorf("stdout = %q, want %q", stdout, "PELICAN\n")
		}
		if !hasLinePrefix(stderr, "done · ") || hasLinePrefix(stderr, "no answer · ") {
			t.Errorf("stderr lacks a footer starting %q:\n%s", "done · ", stderr)
		}
	})
	t.Run("reasoning twice", func(t *testing.T) {
		configPath, root, requests := fenceWireHarness(t, recoveryScript(""))
		stdout, stderr, err := runRecoveryOneShot(t, configPath, root, "-max-steps", "4")
		if err == nil || !strings.Contains(err.Error(), "no final answer") || exitCodeFor(err) != 1 {
			t.Fatalf("run error = %v, want exit 1 with no final answer", err)
		}
		assertRecoveryRequests(t, requests())
		if stdout != "" {
			t.Errorf("stdout = %q, want empty", stdout)
		}
		if !hasLinePrefix(stderr, "no answer · ") {
			t.Errorf("stderr lacks a footer starting %q:\n%s", "no answer · ", stderr)
		}
		if hasLinePrefix(stderr, "done · ") {
			t.Errorf("stderr has a footer starting %q for a turn with no answer:\n%s", "done · ", stderr)
		}
	})
}

func TestFinalAnswerRecoveryWire_MachineFormats(t *testing.T) {
	for _, tc := range []struct {
		name, answer           string
		wantStatus, wantAnswer string
		wantCode               string
	}{
		{name: "recovered", answer: "PELICAN", wantStatus: "completed", wantAnswer: "PELICAN"},
		{name: "reasoning twice", wantStatus: "error", wantCode: "empty_answer"},
	} {
		for _, format := range []string{"json", "stream-json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				configPath, root, requests := fenceWireHarness(t, recoveryScript(tc.answer))
				stdout, stderr, err := runRecoveryOneShot(t, configPath, root, "-output-format", format, "-max-steps", "4")
				wantExit := 0
				if tc.wantCode != "" {
					wantExit = 1
				}
				if got := exitCodeFor(err); got != wantExit {
					t.Fatalf("exit = %d (%v), want %d\nstderr:\n%s", got, err, wantExit, stderr)
				}
				assertRecoveryRequests(t, requests())
				if strings.Contains(stdout, recoveryReasoning) {
					t.Errorf("stdout carries the reasoning:\n%s", stdout)
				}

				events, results := splitMachineLines(t, stdout)
				if len(results) != 1 {
					t.Fatalf("result records = %d, want 1", len(results))
				}
				rec := results[0]
				var status, answer, stop, model string
				for key, dst := range map[string]*string{"status": &status, "answer": &answer, "stopReason": &stop, "model": &model} {
					if err := json.Unmarshal(rec[key], dst); err != nil {
						t.Fatalf("record %s = %s: %v", key, rec[key], err)
					}
				}
				if status != tc.wantStatus || answer != tc.wantAnswer || stop != "completed" || model == "" {
					t.Errorf("record status=%q answer=%q stopReason=%q model=%q, want %q %q completed and a model",
						status, answer, stop, model, tc.wantStatus, tc.wantAnswer)
				}
				if tc.wantCode == "" {
					if string(rec["error"]) != "null" {
						t.Errorf("record error = %s, want null", rec["error"])
					}
				} else {
					var e struct{ Code string }
					if err := json.Unmarshal(rec["error"], &e); err != nil || e.Code != tc.wantCode {
						t.Errorf("record error = %s, want code %q", rec["error"], tc.wantCode)
					}
				}
				if format == "json" {
					if len(events) != 0 {
						t.Errorf("json mode emitted %d events", len(events))
					}
					return
				}

				// stream-json: one run.finished, last before the record (splitMachineLines
				// rejects any event after it), with the record's stop reason and model.
				finished := 0
				var deltaIDs []string
				var text strings.Builder
				for _, e := range events {
					switch e.Type {
					case "run.finished":
						finished++
					case "message.delta":
						var d struct {
							MessageID string `json:"messageId"`
							Text      string `json:"text"`
						}
						if err := json.Unmarshal(e.Payload, &d); err != nil {
							t.Fatal(err)
						}
						deltaIDs = append(deltaIDs, d.MessageID)
						text.WriteString(d.Text)
					}
				}
				if finished != 1 || len(events) == 0 || events[len(events)-1].Type != "run.finished" {
					t.Fatalf("events = %+v, want exactly one run.finished, last", events)
				}
				last := events[len(events)-1]
				var fin struct {
					StopReason string `json:"stopReason"`
					Model      string `json:"model"`
				}
				if err := json.Unmarshal(last.Payload, &fin); err != nil {
					t.Fatal(err)
				}
				if fin.StopReason != stop || fin.Model != model {
					t.Errorf("run.finished = %+v, want stopReason %q and model %q as in the record", fin, stop, model)
				}
				if tc.wantAnswer == "" {
					return
				}
				// The recovered answer streams as step 1's message; nothing from step 0 does.
				wantID := last.RunID + ":1"
				if len(deltaIDs) == 0 || slices.ContainsFunc(deltaIDs, func(id string) bool { return id != wantID }) {
					t.Errorf("message.delta ids = %v, want only %q", deltaIDs, wantID)
				}
				if text.String() != tc.wantAnswer {
					t.Errorf("message.delta text = %q, want %q", text.String(), tc.wantAnswer)
				}
			})
		}
	}
}

// traceRecord is the part of a -trace record these tests read.
type traceRecord struct {
	Request struct {
		Goal                      string                 `json:"goal"`
		RecoverThinkingOnlyAnswer bool                   `json:"recover_thinking_only_answer"`
		History                   []provider.ChatMessage `json:"history"`
	} `json:"request"`
	// agent.Result itself does not round-trip (a RouteOutcome status
	// marshals as a string), so decode only the fields read here.
	Result struct {
		Messages []provider.ChatMessage
		Events   []agent.EventRecord
	} `json:"result"`
}

func readTraceRecords(t *testing.T) []traceRecord {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "golem", "traces")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []traceRecord
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var rec traceRecord
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("trace %s: %v", e.Name(), err)
		}
		out = append(out, rec)
	}
	return out
}

// assertNoRecoveryTail fails if msgs carry the nudge or the placeholder assistant turn.
func assertNoRecoveryTail(t *testing.T, where string, msgs []provider.ChatMessage) {
	t.Helper()
	for _, m := range msgs {
		if m.Content == recoveryNudge || m.Content == "(reasoning only, no reply)" {
			t.Errorf("%s carries the recovery tail: %+v", where, msgs)
			return
		}
	}
}

func TestFinalAnswerRecoveryWire_TraceAndTelemetry(t *testing.T) {
	configPath, root, requests := fenceWireHarness(t, recoveryScript("PELICAN"))
	if _, stderr, err := runRecoveryOneShot(t, configPath, root, "-trace", "-telemetry"); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, stderr)
	}
	assertRecoveryRequests(t, requests())

	traces := readTraceRecords(t)
	if len(traces) != 1 {
		t.Fatalf("trace records = %d, want 1", len(traces))
	}
	rec := traces[0]
	want := []agent.EventRecord{{Step: 0, Kind: "step"}, {Step: 1, Kind: "final_answer_retry"},
		{Step: 1, Kind: "token"}, {Step: 1, Kind: "step"}, {Step: 1, Kind: "stop"}}
	if !slices.Equal(rec.Result.Events, want) {
		t.Errorf("trace result events = %+v, want %+v", rec.Result.Events, want)
	}
	if !rec.Request.RecoverThinkingOnlyAnswer {
		t.Errorf("trace request does not record the recovery opt-in")
	}
	assertNoRecoveryTail(t, "trace request history", rec.Request.History)
	assertNoRecoveryTail(t, "trace result messages", rec.Result.Messages)

	raw, err := os.ReadFile(filepath.Join(os.Getenv("XDG_DATA_HOME"), "golem", "telemetry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var span struct {
			Kind  string `json:"kind"`
			Steps int    `json:"steps"`
		}
		if err := json.Unmarshal([]byte(line), &span); err != nil {
			t.Fatal(err)
		}
		if span.Kind == "run" {
			runs++
			if span.Steps != 2 {
				t.Errorf("telemetry run span steps = %d, want 2", span.Steps)
			}
		}
	}
	if runs != 1 {
		t.Errorf("telemetry run spans = %d, want 1", runs)
	}
}

// The recovered answer, and nothing of the recovery tail, is what the next
// goal of the same REPL session replays.
func TestFinalAnswerRecoveryWire_NextTurnReplaysOnlyTheAnswer(t *testing.T) {
	base := recoveryScript("PELICAN")
	configPath, root, requests := fenceWireHarness(t, func(req wireRequest) []string {
		if n := len(req.Messages); n > 0 && req.Messages[n-1] == (wireMessage{Role: "user", Content: "second goal"}) {
			return sseAnswer("ok")
		}
		return base(req)
	})
	stdin, stdout, stderr := runTestFiles(t)
	if _, err := io.WriteString(stdin, "first goal\nsecond goal\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	args := []string{"-config", configPath, "-root", root, "-trace",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context", "-no-memory", "-no-git-context"}
	if err := run(args, stdin, stdout, stderr); err != nil {
		t.Fatalf("run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	reqs := requests()
	if len(reqs) != 3 {
		t.Fatalf("chat requests = %d, want 3\nstdout:\n%s", len(reqs), readRunTestFile(t, stdout))
	}
	var got []wireMessage
	for _, m := range reqs[2].Messages {
		if m.Role != "system" {
			got = append(got, m)
		}
	}
	want := []wireMessage{{Role: "user", Content: "first goal"}, {Role: "assistant", Content: "PELICAN"}, {Role: "user", Content: "second goal"}}
	if !slices.Equal(got, want) {
		t.Errorf("second goal's non-system messages = %+v, want %+v", got, want)
	}

	// The second goal's trace records the same replayed history.
	traces := readTraceRecords(t)
	i := slices.IndexFunc(traces, func(r traceRecord) bool { return r.Request.Goal == "second goal" })
	if len(traces) != 2 || i < 0 {
		t.Fatalf("traces = %d, want 2 with one for the second goal", len(traces))
	}
	var history []wireMessage
	for _, m := range traces[i].Request.History {
		history = append(history, wireMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID})
	}
	if !slices.Equal(history, want[:2]) {
		t.Errorf("second goal's trace history = %+v, want %+v", history, want[:2])
	}
}
