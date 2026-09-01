package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

func ev(seq uint64, typ, payload string) golemruntime.Event {
	return golemruntime.Event{
		Protocol: golemruntime.ProtocolVersion,
		RunID:    "run-1",
		Seq:      seq,
		Type:     typ,
		Payload:  json.RawMessage(payload),
	}
}

func TestStreamJSONWritesEveryEventImmediatelyOnePerLine(t *testing.T) {
	var out bytes.Buffer
	w := newMachineWriter(&out, outputStreamJSON)
	events := []golemruntime.Event{
		ev(1, "run.started", `{}`),
		ev(2, "message.delta", `{"messageId":"m1","text":"hi"}`),
		ev(3, "tool.started", `{"toolCallId":"t1","name":"read_file","preview":"p"}`),
		ev(4, "tool.finished", `{"toolCallId":"t1","name":"read_file","preview":"r","isError":false}`),
	}
	for i, e := range events {
		if err := w.sink()(e); err != nil {
			t.Fatalf("sink(%s): %v", e.Type, err)
		}
		// Liveness: each event is on stdout the moment the sink returns —
		// a consumer watching the stream sees progress during the run, and a
		// broken pipe surfaces immediately, not after grounding.
		if got := strings.Count(out.String(), "\n"); got != i+1 {
			t.Fatalf("after event %d: %d lines on stdout, want %d (no buffering)", i+1, got, i+1)
		}
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	for i, line := range lines {
		var got golemruntime.Event
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d is not parseable JSON: %v (%q)", i, err, line)
		}
		if got.Seq != uint64(i+1) || got.Protocol != golemruntime.ProtocolVersion {
			t.Errorf("line %d = seq %d protocol %d, want seq %d protocol %d",
				i, got.Seq, got.Protocol, i+1, golemruntime.ProtocolVersion)
		}
	}
}

func TestStreamJSONWritesTerminalEventVerbatimAndCapturesIt(t *testing.T) {
	for _, terminal := range []string{"run.finished", "run.failed", "run.canceled"} {
		t.Run(terminal, func(t *testing.T) {
			var out bytes.Buffer
			w := newMachineWriter(&out, outputStreamJSON)
			_ = w.sink()(ev(1, "run.started", `{}`))
			payload := `{"stopReason":"completed","model":"m"}`
			switch terminal {
			case "run.failed":
				payload = `{"code":"provider","message":"boom"}`
			case "run.canceled":
				payload = `{}`
			}
			_ = w.sink()(ev(2, terminal, payload))
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d lines, want 2", len(lines))
			}
			var got golemruntime.Event
			if err := json.Unmarshal([]byte(lines[1]), &got); err != nil || got.Type != terminal {
				t.Fatalf("terminal line = %q (err %v), want type %s VERBATIM on stdout", lines[1], err, terminal)
			}
			if string(got.Payload) != payload {
				t.Errorf("terminal payload = %s, want %s untouched — protocol events are never decorated", got.Payload, payload)
			}
			if w.terminal == nil || w.terminal.Type != terminal {
				t.Errorf("the writer must also CAPTURE the terminal event for the result record")
			}
		})
	}
}

func TestJSONModeWritesNoEventsButCapturesTheTerminal(t *testing.T) {
	var out bytes.Buffer
	w := newMachineWriter(&out, outputJSON)
	_ = w.sink()(ev(1, "run.started", `{}`))
	_ = w.sink()(ev(2, "message.delta", `{"messageId":"m1","text":"hi"}`))
	_ = w.sink()(ev(3, "run.finished", `{"stopReason":"completed","model":"m"}`))
	if out.Len() != 0 {
		t.Fatalf("json mode must write no protocol events, got %q", out.String())
	}
	if w.terminal == nil || w.terminal.Type != "run.finished" {
		t.Fatal("json mode must still capture the terminal event")
	}
}

func TestTextModeWriterIsNil(t *testing.T) {
	if w := newMachineWriter(&bytes.Buffer{}, outputText); w != nil {
		t.Fatal("text mode must produce no machine writer at all")
	}
	var nilWriter *machineWriter
	if err := nilWriter.sink()(ev(1, "run.started", `{}`)); err != nil {
		t.Fatalf("the nil writer's sink must be the pre-#352 discarding sink: %v", err)
	}
}

func TestMachineWriterPropagatesWriteErrors(t *testing.T) {
	// A broken pipe must surface so Runtime.Run cancels the run rather than
	// streaming into a closed reader forever.
	w := newMachineWriter(errWriter{}, outputStreamJSON)
	err := w.sink()(ev(1, "run.started", `{}`))
	if !errors.Is(err, errBrokenTestWriter) {
		t.Fatalf("err = %v, want it to wrap the write error", err)
	}
}

var errBrokenTestWriter = errors.New("broken writer")

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errBrokenTestWriter }

// decodeResult unmarshals one golem.result.v1 line and asserts every key is
// PRESENT (null over absent — spec 7.2).
func decodeResult(t *testing.T, line string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("result line not parseable: %v (%q)", err, line)
	}
	for _, key := range []string{"schema", "status", "answer", "stopReason", "model", "error", "grounding"} {
		if _, ok := m[key]; !ok {
			t.Errorf("result must carry %q even when null: %s", key, line)
		}
	}
	if _, ok := m["protocol"]; ok {
		t.Errorf("a result record must NOT carry the protocol key (discrimination rule): %s", line)
	}
	if got := string(m["schema"]); got != `"golem.result.v1"` {
		t.Errorf("schema = %s, want golem.result.v1", got)
	}
	return m
}

func TestBuildResultCompleted(t *testing.T) {
	w := newMachineWriter(&bytes.Buffer{}, outputJSON)
	_ = w.emit(ev(9, "run.finished", `{"stopReason":"completed","model":"local/m"}`))
	w.setGrounding(json.RawMessage(`{"status":"supported","report":{}}`))
	rec := w.buildResult(agent.Result{Answer: "the answer"}, nil)
	var buf bytes.Buffer
	if err := writeJSONLine(&buf, rec); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	for key, want := range map[string]string{
		"status":     `"completed"`,
		"answer":     `"the answer"`,
		"stopReason": `"completed"`,
		"model":      `"local/m"`,
		"error":      `null`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var ground map[string]any
	if err := json.Unmarshal(m["grounding"], &ground); err != nil || ground["status"] != "supported" {
		t.Errorf("grounding = %s, want the verbatim #348 object", m["grounding"])
	}
}

func TestBuildResultRuntimeFailureCopiesTheRunFailedCode(t *testing.T) {
	w := newMachineWriter(&bytes.Buffer{}, outputJSON)
	_ = w.emit(ev(2, "run.failed", `{"code":"provider","message":"backend exploded"}`))
	rec := w.buildResult(agent.Result{}, errors.New("backend exploded"))
	var buf bytes.Buffer
	_ = writeJSONLine(&buf, rec)
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	for key, want := range map[string]string{
		"status":     `"error"`,
		"answer":     `null`,
		"stopReason": `null`,
		"model":      `null`,
		"grounding":  `null`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	var e struct{ Code, Message string }
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "provider" || e.Message != "backend exploded" {
		t.Errorf("error = %s, want the run.failed code/message copied verbatim", m["error"])
	}
}

func TestBuildResultCanceled(t *testing.T) {
	w := newMachineWriter(&bytes.Buffer{}, outputJSON)
	_ = w.emit(ev(2, "run.canceled", `{}`))
	rec := w.buildResult(agent.Result{Answer: "partial"}, context.Canceled)
	var buf bytes.Buffer
	_ = writeJSONLine(&buf, rec)
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	for key, want := range map[string]string{
		"status": `"canceled"`, "answer": `null`, "stopReason": `null`,
		"model": `null`, "error": `null`, "grounding": `null`,
	} {
		if got := string(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}

func TestBuildResultEmptyAnswerIsAnErrorWithTheAnswerPreserved(t *testing.T) {
	w := newMachineWriter(&bytes.Buffer{}, outputJSON)
	_ = w.emit(ev(9, "run.finished", `{"stopReason":"completed","model":"local/m"}`))
	rec := w.buildResult(agent.Result{Answer: "   \n"}, nil)
	var buf bytes.Buffer
	_ = writeJSONLine(&buf, rec)
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	if got := string(m["status"]); got != `"error"` {
		t.Errorf("status = %s, want error", got)
	}
	if got := string(m["answer"]); got != `"   \n"` {
		t.Errorf("answer = %s, want the whitespace preserved verbatim", got)
	}
	if got := string(m["stopReason"]); got != `"completed"` {
		t.Errorf("stopReason = %s, want kept from the terminal event", got)
	}
	var e struct{ Code string }
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "empty_answer" {
		t.Errorf("error = %s, want code empty_answer", m["error"])
	}
}

func TestBuildResultNoTerminalFallsBackToInvalidRequest(t *testing.T) {
	// Defensive: a run that died before any terminal event (practically
	// unreachable — the CLI pre-caps the prompt at the runtime's own bound —
	// but the record must never fabricate a terminal it did not see).
	w := newMachineWriter(&bytes.Buffer{}, outputJSON)
	rec := w.buildResult(agent.Result{}, errors.New("golem: invalid request"))
	var buf bytes.Buffer
	_ = writeJSONLine(&buf, rec)
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	var e struct{ Code string }
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "invalid_request" {
		t.Errorf("error = %s, want the reserved invalid_request code", m["error"])
	}
}

// runMachineModeHarness executes one deterministic scripted one-shot run — a
// read_file tool call, a streamed answer, then runOneShot's result path — in
// the given format, returning stdout and stderr. The same buffer backs the
// writer and runOneShot's stdout, exactly as main.go wires production.
func runMachineModeHarness(t *testing.T, format outputFormat) (string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(root+"/hello.txt", []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"hello.txt"}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "machine answer"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: toolCall}, {Response: finalAns}}}
	sess := newTestSession(t, caller, root)
	var stdout, stderr strings.Builder
	sess.machine = newMachineWriter(&stdout, format)
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "read the file"); err != nil {
		t.Fatalf("runOneShot: %v; stderr=%s", err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// splitMachineLines classifies stdout lines by the frozen discrimination rule:
// exactly one of "protocol"/"schema" per line.
func splitMachineLines(t *testing.T, out string) (events []golemruntime.Event, results []map[string]json.RawMessage) {
	t.Helper()
	if out == "" {
		return nil, nil
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("machine output must end with a newline: %q", out)
	}
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("line %d not JSON: %v (%q)", i, err, line)
		}
		_, hasProtocol := probe["protocol"]
		_, hasSchema := probe["schema"]
		if hasProtocol == hasSchema {
			t.Fatalf("line %d violates the discrimination rule (exactly one of protocol/schema): %q", i, line)
		}
		if hasProtocol {
			if len(results) > 0 {
				t.Fatalf("protocol event after the result record; the record must complete the stream: %q", line)
			}
			var e golemruntime.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("line %d: %v", i, err)
			}
			events = append(events, e)
		} else {
			results = append(results, decodeResult(t, line))
		}
	}
	return events, results
}

func TestRunOneShotStreamJSONEmitsEventsThenOneResultTrailer(t *testing.T) {
	stdout, stderr := runMachineModeHarness(t, outputStreamJSON)
	events, results := splitMachineLines(t, stdout)
	if len(results) != 1 {
		t.Fatalf("got %d result records, want exactly 1:\n%s", len(results), stdout)
	}
	types := make([]string, 0, len(events))
	terminals := 0
	for _, e := range events {
		types = append(types, e.Type)
		if _, ok := terminalEventTypes[e.Type]; ok {
			terminals++
		}
	}
	if terminals != 1 || events[len(events)-1].Type != "run.finished" {
		t.Fatalf("want exactly one terminal run.finished as the LAST protocol event, got %v", types)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"run.started", "tool.started", "tool.finished", "message.delta"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream must carry %s, got %v", want, types)
		}
	}
	if got := string(results[0]["answer"]); got != `"machine answer"` {
		t.Errorf("result answer = %s, want the final answer", got)
	}
	// The plain answer must not ALSO appear as a bare stdout line.
	for _, line := range strings.Split(stdout, "\n") {
		if line == "machine answer" {
			t.Error("the bare answer line must be suppressed in machine modes")
		}
	}
	if stderr == "" {
		t.Error("the human progress stream must still reach stderr")
	}
}

func TestRunOneShotJSONWritesExactlyOneResultLine(t *testing.T) {
	stdout, _ := runMachineModeHarness(t, outputJSON)
	events, results := splitMachineLines(t, stdout)
	if len(events) != 0 {
		t.Fatalf("json mode must write no protocol events, got %d:\n%s", len(events), stdout)
	}
	if len(results) != 1 {
		t.Fatalf("json mode must write exactly one result line, got %d:\n%s", len(results), stdout)
	}
	if got := string(results[0]["status"]); got != `"completed"` {
		t.Errorf("status = %s, want completed", got)
	}
	if got := string(results[0]["answer"]); got != `"machine answer"` {
		t.Errorf("answer = %s, want the final answer", got)
	}
}

func TestRunOneShotTextModeIsByteIdenticalToPre352(t *testing.T) {
	// Regression pin for acceptance criterion 4: with no machine writer the
	// stdout bytes are exactly the answer plus one trailing newline.
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "plain answer"}}}}
	sess := newTestSession(t, caller, root)
	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "say it"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if stdout.String() != "plain answer\n" {
		t.Fatalf("stdout = %q, want %q byte-identical", stdout.String(), "plain answer\n")
	}
}

func TestRunOneShotMachineModeProviderFailureStillWritesAResult(t *testing.T) {
	sess := newTestSession(t, errCaller{err: errors.New("model unreachable")}, t.TempDir())
	var stdout, stderr strings.Builder
	sess.machine = newMachineWriter(&stdout, outputStreamJSON)
	err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "anything")
	if !errors.Is(err, errOneShotFailed) {
		t.Fatalf("want errOneShotFailed, got %v", err)
	}
	events, results := splitMachineLines(t, stdout.String())
	if len(results) != 1 {
		t.Fatalf("a failed run must still end on one result record:\n%s", stdout.String())
	}
	if got := string(results[0]["status"]); got != `"error"` {
		t.Errorf("status = %s, want error", got)
	}
	var failedCode string
	for _, e := range events {
		if e.Type == "run.failed" {
			var p struct{ Code string }
			_ = json.Unmarshal(e.Payload, &p)
			failedCode = p.Code
		}
	}
	if failedCode == "" {
		t.Fatalf("stream must carry the run.failed event:\n%s", stdout.String())
	}
	var recErr struct{ Code string }
	if err := json.Unmarshal(results[0]["error"], &recErr); err != nil || recErr.Code != failedCode {
		t.Errorf("result error.code = %q, want the stream's run.failed code %q", recErr.Code, failedCode)
	}
}

// normalizeMachineGolden replaces the nondeterministic fields — the run id on
// protocol lines, the model name on run.finished and on the result record —
// so the fixture pins the CONTRACT (envelope shape, event set, ordering,
// record shape) rather than one run's identity.
func normalizeMachineGolden(t *testing.T, out string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			t.Fatalf("golden line not JSON: %v (%q)", err, line)
		}
		_, hasProtocol := probe["protocol"]
		_, hasSchema := probe["schema"]
		if hasProtocol == hasSchema {
			t.Fatalf("discrimination rule violated: line must carry exactly one of protocol/schema: %q", line)
		}
		if hasProtocol {
			var e golemruntime.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				t.Fatalf("protocol line: %v", err)
			}
			e.RunID = "RUNID"
			if e.Type == "run.finished" {
				e.Payload = normalizeModelField(t, e.Payload)
			}
			if e.Type == "message.delta" {
				// messageId is "<runID>:<step>" (golem/runtime.go emitDelta);
				// the run id half is nondeterministic, the step half is
				// contract-relevant ordering, so keep it.
				var p struct {
					MessageID string `json:"messageId"`
					Text      string `json:"text"`
				}
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					t.Fatalf("delta payload: %v", err)
				}
				if i := strings.LastIndex(p.MessageID, ":"); i >= 0 {
					p.MessageID = "RUNID" + p.MessageID[i:]
				}
				raw, err := json.Marshal(p)
				if err != nil {
					t.Fatalf("re-encode delta: %v", err)
				}
				e.Payload = raw
			}
			raw, err := json.Marshal(e)
			if err != nil {
				t.Fatalf("re-encode event: %v", err)
			}
			b.Write(raw)
		} else {
			probe["model"] = json.RawMessage(`"MODEL"`)
			raw, err := json.Marshal(probe)
			if err != nil {
				t.Fatalf("re-encode record: %v", err)
			}
			b.Write(raw)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func normalizeModelField(t *testing.T, payload json.RawMessage) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("payload: %v", err)
	}
	m["model"] = json.RawMessage(`"MODEL"`)
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return raw
}

func TestMachineOutputGoldenFixtures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format outputFormat
		golden string
	}{
		{"json", outputJSON, "testdata/headless/json.golden"},
		{"stream-json", outputStreamJSON, "testdata/headless/stream-json.golden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _ := runMachineModeHarness(t, tc.format)
			got := normalizeMachineGolden(t, stdout)
			want, err := os.ReadFile(tc.golden)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if got != string(want) {
				t.Errorf("machine output drifted from the frozen contract.\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestPreRunFailureResult(t *testing.T) {
	rec := preRunFailureResult("provider_unavailable", "no backend answered")
	var buf bytes.Buffer
	_ = writeJSONLine(&buf, rec)
	m := decodeResult(t, strings.TrimSuffix(buf.String(), "\n"))
	if got := string(m["status"]); got != `"error"` {
		t.Errorf("status = %s, want error", got)
	}
	var e struct{ Code, Message string }
	if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != "provider_unavailable" || e.Message != "no backend answered" {
		t.Errorf("error = %s, want the pre-run code/message", m["error"])
	}
}
