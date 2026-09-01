package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
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
