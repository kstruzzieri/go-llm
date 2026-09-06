package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
)

// terminalEventTypes is the protocol-v1 terminal set (golem/runtime.go).
// Exactly one of these ends a run's event stream.
var terminalEventTypes = map[string]struct{}{
	"run.finished": {},
	"run.failed":   {},
	"run.canceled": {},
}

// machineWriter adapts the protocol-v1 event stream to -output-format (#352).
//
// stream-json writes every event to stdout the moment it arrives — no
// buffering, so a consumer sees live progress and a broken pipe cancels the
// run immediately instead of after grounding. json writes no events at all.
// Both modes CAPTURE the terminal event: the golem.result.v1 record (spec 7.2)
// reads its status, stopReason/model, and failure code/message from that
// capture, so the record and the stream can never disagree.
//
// Protocol events are never modified: the 128 KiB envelope cap
// (golem/event.go) makes a decorated terminal event invalid protocol-v1, so
// the answer and grounding travel in the separate result record instead.
//
// Not safe for concurrent use; Runtime.Run calls its sink serially.
type machineWriter struct {
	out       io.Writer
	format    outputFormat
	terminal  *golemruntime.Event
	grounding json.RawMessage
}

// newMachineWriter returns the writer for a machine format, or nil for text.
// A nil writer means "no machine output"; every method and sink() is
// nil-receiver safe so call sites need no guards.
func newMachineWriter(out io.Writer, format outputFormat) *machineWriter {
	if !format.machine() {
		return nil
	}
	return &machineWriter{out: out, format: format}
}

// sink adapts the writer to the runtime's EventSink. A nil writer yields the
// discarding sink runOnce used before #352.
func (w *machineWriter) sink() golemruntime.EventSink {
	if w == nil {
		return func(golemruntime.Event) error { return nil }
	}
	return w.emit
}

func (w *machineWriter) emit(e golemruntime.Event) error {
	if _, terminal := terminalEventTypes[e.Type]; terminal {
		held := e
		w.terminal = &held
	}
	if w.format == outputJSON {
		return nil
	}
	return writeJSONLine(w.out, e)
}

// setGrounding records the #348 verdict for the result record. The argument is
// the same json.RawMessage runOnce already marshals for the trace, so the
// machine surface and the trace can never disagree.
func (w *machineWriter) setGrounding(raw json.RawMessage) {
	if w == nil {
		return
	}
	w.grounding = raw
}

// writeJSONLine writes one compact JSON value followed by a newline, encoding
// into memory first so a marshal failure cannot leave a half-written line.
func writeJSONLine(out io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("golem: encode machine output: %w", err)
	}
	if _, err := out.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("golem: write machine output: %w", err)
	}
	return nil
}

// resultSchema is the frozen identity of the CLI result record (#352, spec
// 7.2). It is a SEPARATE contract from protocol-v1: the discrimination rule is
// that a protocol event carries "protocol" and never "schema", and a result
// record carries "schema" and never "protocol".
const resultSchema = "golem.result.v1"

// CLI-owned failure codes. Runtime failures copy the bounded public run.failed
// code instead; these cover what the runtime never sees.
const (
	resultCodeEmptyAnswer    = "empty_answer"
	resultCodeProviderPreRun = "provider_unavailable"
	resultCodeDestDenied     = "destination_denied"
	// resultCodeInvalidRequest is the defensive fallback for a run that ended
	// with no terminal event. Reserved; practically unreachable because the
	// CLI pre-caps the prompt at the runtime's own message bound.
	resultCodeInvalidRequest = "invalid_request"
)

// headlessResultError is the error half of the record; code is bounded, the
// message is diagnostic text.
type headlessResultError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// headlessResult is the golem.result.v1 record. Every key is ALWAYS present —
// pointers marshal as null, never as absent — so consumers can key on shape.
// It has NO byte cap: a 10 MB answer is one 10 MB line (documented; consumers
// must not use a default 64 KiB line scanner).
type headlessResult struct {
	Schema     string               `json:"schema"`
	Status     string               `json:"status"` // completed | error | canceled
	Answer     *string              `json:"answer"`
	StopReason *string              `json:"stopReason"`
	Model      *string              `json:"model"`
	Error      *headlessResultError `json:"error"`
	Grounding  json.RawMessage      `json:"grounding"` // nil marshals as null
}

// buildResult assembles the record from the run outcome plus the captured
// terminal event. The terminal event is the single source of truth for
// status, stopReason, model, and the failure code/message, so the record can
// never disagree with the protocol stream a stream-json consumer just read.
func (w *machineWriter) buildResult(res agent.Result, runErr error) headlessResult {
	rec := headlessResult{Schema: resultSchema, Status: "error"}
	if w == nil {
		return rec
	}
	rec.Grounding = w.grounding
	terminal := w.terminal
	if terminal == nil {
		msg := "run produced no terminal event"
		if runErr != nil {
			msg = runFailureMessage("", runErr)
		}
		rec.Error = &headlessResultError{Code: resultCodeInvalidRequest, Message: msg}
		rec.Grounding = nil
		return rec
	}
	switch terminal.Type {
	case "run.finished":
		var payload struct {
			StopReason string `json:"stopReason"`
			Model      string `json:"model"`
		}
		_ = json.Unmarshal(terminal.Payload, &payload)
		rec.Status = "completed"
		answer := res.Answer
		rec.Answer = &answer
		rec.StopReason = &payload.StopReason
		rec.Model = &payload.Model
		if strings.TrimSpace(res.Answer) == "" {
			// #352: an empty answer is a scripting failure (exit 1) but the
			// run DID finish — status flips to error with the answer and the
			// terminal fields preserved, mirroring the pre-#352 text-mode
			// error for the same condition.
			rec.Status = "error"
			rec.Error = &headlessResultError{Code: resultCodeEmptyAnswer, Message: "one-shot: model produced no final answer"}
		}
	case "run.canceled":
		rec.Status = "canceled"
		rec.Grounding = nil
	case "run.failed":
		var payload struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(terminal.Payload, &payload)
		rec.Status = "error"
		rec.Error = &headlessResultError{Code: payload.Code, Message: payload.Message}
		rec.Grounding = nil
	}
	return rec
}

// preRunFailureResult is the record for a failure BEFORE Runtime.Run in a
// machine mode: it is the only stdout line of that invocation (spec 7.2).
// Exactly two codes reach it: provider_unavailable and destination_denied.
func preRunFailureResult(code, message string) headlessResult {
	return headlessResult{
		Schema: resultSchema,
		Status: "error",
		Error:  &headlessResultError{Code: code, Message: message},
	}
}

// reportPreRunFailure writes the machine-mode record for a failure before
// Runtime.Run. In text mode it does nothing — stderr already carries the
// diagnostic through the normal error path. The write error is deliberately
// dropped: the invocation is already failing with a more specific cause.
func reportPreRunFailure(stdout io.Writer, format outputFormat, code string, err error) {
	if !format.machine() {
		return
	}
	_ = writeJSONLine(stdout, preRunFailureResult(code, err.Error()))
}
