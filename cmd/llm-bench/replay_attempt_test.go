//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const attemptTestKey = "lab-attempt-key-0123456789"

func attemptTestOptions(t *testing.T, baseURL, tracePath string) replayAttemptOptions {
	t.Helper()
	return replayAttemptOptions{
		AttemptDir: filepath.Join(t.TempDir(), "attempt"),
		AttemptID:  "att-1",
		Arm:        "arm-a",
		TracePath:  tracePath,
		Models:     "openai-compat/cand",
		BaseURL:    baseURL,
		APIKey:     attemptTestKey,
		Timeout:    10 * time.Second,
	}
}

// runAttempt runs one attempt and returns its exit code and parsed record.
// It fails the test if the key leaks into stderr or attempt.json.
func runAttempt(t *testing.T, ctx context.Context, opts replayAttemptOptions) (int, attemptRecord) {
	t.Helper()
	var stderr bytes.Buffer
	code := runReplayAttempt(ctx, opts, &stderr)
	if strings.Contains(stderr.String(), attemptTestKey) {
		t.Fatalf("stderr leaked the candidate key: %s", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(opts.AttemptDir, attemptTerminalName))
	if err != nil {
		t.Fatalf("read terminal (exit %d, stderr %q): %v", code, stderr.String(), err)
	}
	if bytes.Contains(data, []byte(attemptTestKey)) {
		t.Fatalf("attempt.json leaked the candidate key")
	}
	var rec attemptRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse terminal: %v\n%s", err, data)
	}
	if rec.Schema != replayAttemptSchema || rec.AttemptID != opts.AttemptID {
		t.Fatalf("schema %q attempt %q", rec.Schema, rec.AttemptID)
	}
	return code, rec
}

// assertOneSequence: the server received exactly the requests the record
// lists, in order, with matching correlation values; each stored request is
// the wire bytes with the key redacted.
func assertOneSequence(t *testing.T, opts replayAttemptOptions, srv *compatServer, rec attemptRecord) {
	t.Helper()
	got := srv.Received()
	if len(got) != len(rec.Requests) {
		t.Fatalf("server received %d requests; record has %d rows", len(got), len(rec.Requests))
	}
	for i, r := range got {
		want := fmt.Sprintf("%s/%d", rec.AttemptID, i+1)
		if r.Correlation != want || rec.Requests[i].Correlation != want || rec.Requests[i].Ordinal != i+1 {
			t.Fatalf("request %d: server correlation %q, row %q ordinal %d; want %q", i+1, r.Correlation, rec.Requests[i].Correlation, rec.Requests[i].Ordinal, want)
		}
		ref := rec.Requests[i].Request
		if ref == nil {
			t.Fatalf("request %d has no raw ref", i+1)
		}
		wantRaw, redacted := redactAttemptKey(r.Body, opts.APIKey)
		stored, err := os.ReadFile(filepath.Join(opts.AttemptDir, filepath.FromSlash(ref.Path)))
		if err != nil || !bytes.Equal(stored, wantRaw) || ref.SHA256 != sha256Hex(wantRaw) || ref.Bytes != len(wantRaw) || ref.Redacted != redacted {
			t.Fatalf("request %d raw evidence does not match the (redacted) wire bytes (%v)", i+1, err)
		}
	}
}

// attemptStr reads a nullable string field, rendering null as "<null>" so a
// nil never panics a comparison.
func attemptStr(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

// assertUTCInstant: a published instant is set and already UTC, so
// attempt.json carries the same offset everywhere.
func assertUTCInstant(t *testing.T, what string, at time.Time) {
	t.Helper()
	if at.IsZero() {
		t.Fatalf("%s is the zero time", what)
	}
	if at.Location() != time.UTC {
		t.Fatalf("%s location %v; want UTC", what, at.Location())
	}
}

func assertAttemptError(t *testing.T, rec attemptRecord, code, stage string, ordinal int) {
	t.Helper()
	if rec.Status != attemptStatusFailed || rec.Error == nil || rec.Error.Code != code || rec.Error.Stage != stage || rec.Error.RequestOrdinal != ordinal {
		t.Fatalf("status %q error %+v; want failed %s/%s ordinal %d", rec.Status, rec.Error, code, stage, ordinal)
	}
}

func TestReplayAttempt_CompletedScriptedToolRoute(t *testing.T) {
	srv := newCompatServer(t,
		compatExchange{Body: compatToolCall("rag_search", `{"query":"retry budget"}`, `{"prompt_tokens":20,"completion_tokens":5}`)},
		compatExchange{Body: compatText("RetryBudget defaults to 3.", `{"prompt_tokens":30,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":2}}`)},
	)
	opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusCompleted || rec.Error != nil || rec.ArtifactError != nil {
		t.Fatalf("exit %d status %q error %+v artifact %+v", code, rec.Status, rec.Error, rec.ArtifactError)
	}
	assertOneSequence(t, opts, srv, rec)
	for _, r := range srv.Received() {
		if r.Authorization != "Bearer "+attemptTestKey || r.Path != "/v1/chat/completions" {
			t.Fatalf("request auth %q path %q", r.Authorization, r.Path)
		}
	}
	traceBytes, _ := os.ReadFile(fixtureToolRouteTrace)
	if attemptStr(rec.Trace.ID) != "tool-canary-01" || attemptStr(rec.Trace.SHA256) != sha256Hex(traceBytes) || attemptStr(rec.ReplayKind) != "scripted" {
		t.Fatalf("trace id %s sha %s kind %s", attemptStr(rec.Trace.ID), attemptStr(rec.Trace.SHA256), attemptStr(rec.ReplayKind))
	}
	if rec.Settings.Transport != openAICompatTransport || attemptStr(rec.Settings.Endpoint) != srv.URL || rec.Settings.Model != "openai-compat/cand" || rec.Settings.Temperature != nil || rec.Settings.TimeoutMs != 10000 {
		t.Fatalf("settings %+v", rec.Settings)
	}
	if rec.Build.GoVersion == "" || rec.EndedAt.Before(rec.StartedAt) || rec.ElapsedMs < 0 {
		t.Fatalf("build %+v started %v ended %v elapsed %d", rec.Build, rec.StartedAt, rec.EndedAt, rec.ElapsedMs)
	}
	if len(rec.Transcript) != 3 || len(rec.Divergences) != 0 || rec.SentinelSubstituted {
		t.Fatalf("transcript %d turns, divergences %v, sentinel %v", len(rec.Transcript), rec.Divergences, rec.SentinelSubstituted)
	}
	r1, r2 := rec.Requests[0], rec.Requests[1]
	if r1.HTTPStatus == nil || *r1.HTTPStatus != 200 || r1.HeadersAt == nil || r1.ErrorCode != nil || r1.Response == nil || !r1.Response.Complete {
		t.Fatalf("row 1 %+v", r1)
	}
	if *r1.Usage.PromptTokens != 20 || *r1.Usage.CompletionTokens != 5 || r1.Usage.ReasoningTokens != nil {
		t.Fatalf("row 1 usage %+v", r1.Usage)
	}
	if *r2.Usage.ReasoningTokens != 2 {
		t.Fatalf("row 2 usage %+v", r2.Usage)
	}
}

func TestReplayAttempt_PrefilledRecordsTemperature(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("The drift detector averages over a 35 minute window.", "")})
	opts := attemptTestOptions(t, srv.URL, fixturePrefilledTrace)
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusCompleted || attemptStr(rec.ReplayKind) != "prefilled" || rec.Settings.Temperature == nil || *rec.Settings.Temperature != 0 {
		t.Fatalf("exit %d status %q kind %s temperature %v", code, rec.Status, attemptStr(rec.ReplayKind), rec.Settings.Temperature)
	}
	// Decode rather than substring-match: "temperature":0 is also a prefix of 0.5.
	var wire struct {
		Temperature *float64 `json:"temperature"`
	}
	err := json.Unmarshal(srv.Received()[0].Body, &wire)
	if err != nil || wire.Temperature == nil || *wire.Temperature != 0 {
		got := "absent"
		if wire.Temperature != nil {
			got = fmt.Sprint(*wire.Temperature)
		}
		t.Fatalf("wire request temperature %s (decode error %v); want exactly 0", got, err)
	}
	if rec.Requests[0].Usage != nil {
		t.Fatalf("usage %+v; an omitted usage object must stay null", rec.Requests[0].Usage)
	}
}

// Acceptance 2 and 3: a failed attempt keeps timing and the usage the server reported.
func TestReplayAttempt_ArgumentDecodeKeepsUsageAndTiming(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatToolCall("rag_search", "not json", `{"prompt_tokens":17,"completion_tokens":4}`)})
	opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 {
		t.Fatalf("exit %d; a published failed attempt exits 0 (D3)", code)
	}
	assertAttemptError(t, rec, attemptCodeArgumentDecode, attemptStageReplay, 1)
	assertOneSequence(t, opts, srv, rec)
	row := rec.Requests[0]
	if row.Usage == nil || *row.Usage.PromptTokens != 17 || *row.Usage.CompletionTokens != 4 || row.Usage.ReasoningTokens != nil {
		t.Fatalf("usage %+v; the failed request's reported usage must survive", row.Usage)
	}
	if row.HeadersAt == nil || row.EndedAt.Before(row.StartedAt) || row.ErrorCode != nil {
		t.Fatalf("row %+v", row)
	}
	// Acceptance 2: a failed attempt still reports its timing. The ordering
	// checks above pass for zero times, so pin that every instant is set and UTC.
	assertUTCInstant(t, "record started_at", rec.StartedAt)
	assertUTCInstant(t, "record ended_at", rec.EndedAt)
	assertUTCInstant(t, "row started_at", row.StartedAt)
	assertUTCInstant(t, "row ended_at", row.EndedAt)
	assertUTCInstant(t, "row headers_at", *row.HeadersAt)
	if rec.EndedAt.Before(rec.StartedAt) {
		t.Fatalf("record ended %v before it started %v", rec.EndedAt, rec.StartedAt)
	}
}

func TestReplayAttempt_ResponseDecode(t *testing.T) {
	body := compatText("ok", "") + "garbage"
	cases := []struct{ name, body string }{
		{"trailing data", body},
		{"error envelope in a 200", `{"error":{"message":"overloaded"},"choices":[{"index":0,"message":{"role":"assistant","content":"x"}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Body: tc.body})
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			_, rec := runAttempt(t, context.Background(), opts)
			assertAttemptError(t, rec, attemptCodeResponseDecode, attemptStageResponse, 1)
			assertOneSequence(t, opts, srv, rec)
			if len(rec.Transcript) != 0 {
				t.Fatalf("transcript %v; a rejected response must not advance the replay", rec.Transcript)
			}
			resp := rec.Requests[0].Response
			if resp == nil || resp.SHA256 != sha256Hex([]byte(tc.body)) || !resp.Complete {
				t.Fatalf("response ref %+v; want the full body", resp)
			}
		})
	}
}

func TestReplayAttempt_EmptyResponse(t *testing.T) {
	t.Run("zero choices", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: `{"choices":[]}`})
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeEmptyResponse, attemptStageResponse, 1)
	})
	t.Run("empty assistant reply", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: compatText("", "")})
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeEmptyResponse, attemptStageReplay, 1)
	})
}

// Acceptance 4: exactly one request sequence; a recording proxy sees no retry.
func TestReplayAttempt_HTTPStatusIsOneRequest(t *testing.T) {
	cases := []struct {
		name   string
		ex     compatExchange
		status int
	}{
		{"500", compatExchange{Status: http.StatusInternalServerError, Body: `{"error":{"message":"boom"}}`}, 500},
		{"503", compatExchange{Status: http.StatusServiceUnavailable, Body: "busy", Header: map[string]string{"Retry-After": "1"}}, 503},
		{"307 is not followed", compatExchange{Status: http.StatusTemporaryRedirect, Header: map[string]string{"Location": "/v1/chat/completions"}}, 307},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, tc.ex)
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			_, rec := runAttempt(t, context.Background(), opts)
			assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 1)
			if rec.Error.HTTPStatus != tc.status || len(rec.Requests) != 1 || len(srv.Received()) != 1 {
				t.Fatalf("status %d rows %d server %d; want %d and exactly one request", rec.Error.HTTPStatus, len(rec.Requests), len(srv.Received()), tc.status)
			}
			assertOneSequence(t, opts, srv, rec)
		})
	}
}

func TestReplayAttempt_Connection(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		srv := newCompatServer(t)
		url := srv.URL
		srv.Close()
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, url, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeConnection, attemptStageRequest, 1)
		row := rec.Requests[0]
		if row.HeadersAt != nil || row.HTTPStatus != nil || row.Response != nil || row.Request == nil {
			t.Fatalf("row %+v; want request evidence only", row)
		}
	})
	t.Run("dropped", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{DropConn: true})
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeConnection, attemptStageRequest, 1)
		if len(srv.Received()) != 1 {
			t.Fatalf("server received %d requests; want 1", len(srv.Received()))
		}
	})
}

func TestReplayAttempt_Canceled(t *testing.T) {
	t.Run("attempt timeout", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Delay: 5 * time.Second})
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.Timeout = 200 * time.Millisecond
		_, rec := runAttempt(t, context.Background(), opts)
		assertAttemptError(t, rec, attemptCodeCanceled, attemptStageRequest, 1)
		if rec.Error.CancelSource != attemptCancelTimeout || rec.ElapsedMs >= 4000 {
			t.Fatalf("source %q elapsed %d", rec.Error.CancelSource, rec.ElapsedMs)
		}
		// The deadline is 200ms, so a measured elapsed far below it means the
		// timing was dropped or never measured (acceptance 2).
		if rec.ElapsedMs < 150 || rec.Requests[0].ElapsedMs < 150 {
			t.Fatalf("elapsed record %dms row %dms; want at least 150 for a 200ms timeout", rec.ElapsedMs, rec.Requests[0].ElapsedMs)
		}
	})
	t.Run("signal", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Delay: 5 * time.Second})
		parent, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		time.AfterFunc(100*time.Millisecond, func() { cancel(errors.New("interrupt signal received")) })
		_, rec := runAttempt(t, parent, attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeCanceled, attemptStageRequest, 1)
		if rec.Error.CancelSource != attemptCancelSignal {
			t.Fatalf("source %q", rec.Error.CancelSource)
		}
	})
}

func TestReplayAttempt_ReplayDivergence(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatToolCall("other_tool", `{}`, "")})
	_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureToolRouteTrace))
	assertAttemptError(t, rec, attemptCodeReplayDivergence, attemptStageReplay, 1)
}

// Review Focus 2.
func TestReplayAttempt_SecondRequestFailureKeepsPartial(t *testing.T) {
	srv := newCompatServer(t,
		compatExchange{Body: compatToolCall("rag_search", `{"query":"retry budget"}`, `{"prompt_tokens":20,"completion_tokens":5}`)},
		compatExchange{Status: http.StatusBadGateway, Body: "upstream down"},
	)
	opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
	_, rec := runAttempt(t, context.Background(), opts)
	assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 2)
	assertOneSequence(t, opts, srv, rec)
	if len(rec.Transcript) != 2 || rec.Transcript[0].Role != "assistant" || rec.Transcript[1].Role != "tool" {
		t.Fatalf("partial transcript %+v; want the first assistant turn and its tool result", rec.Transcript)
	}
	if rec.Requests[0].ErrorCode != nil || rec.Requests[1].ErrorCode == nil || *rec.Requests[1].ErrorCode != attemptCodeHTTPStatus {
		t.Fatalf("row codes %v %v", rec.Requests[0].ErrorCode, rec.Requests[1].ErrorCode)
	}
	if rec.Requests[1].Response == nil {
		t.Fatalf("the 502 body must be kept as evidence")
	}
}

func TestReplayAttempt_DivergenceCompletes(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatToolCall("lookup", `{"q":"x"}`, "")})
	code, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
	if code != 0 || rec.Status != attemptStatusCompleted || !rec.SentinelSubstituted || len(rec.Divergences) != 1 || rec.Divergences[0] != divergencePlainChatToolCall || len(rec.Notes) == 0 {
		t.Fatalf("exit %d status %q sentinel %v divergences %v notes %v", code, rec.Status, rec.SentinelSubstituted, rec.Divergences, rec.Notes)
	}
}

// Review Focus 5: the server echoes the key in a tool call, which re-enters
// the conversation, reaches request 2, and is echoed again in the answer.
func TestReplayAttempt_RedactsEchoedKey(t *testing.T) {
	srv := newCompatServer(t,
		compatExchange{Body: compatToolCall("rag_search", `{"query":"`+attemptTestKey+`"}`, "")},
		// The key also comes back in the reasoning channel, which the
		// openai-compat path lifts into the transcript's Thinking.
		compatExchange{Body: strings.Replace(compatText("your key is "+attemptTestKey, ""), `"role":"assistant",`, `"role":"assistant","reasoning_content":"the key is `+attemptTestKey+`",`, 1)},
	)
	opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
	code, rec := runAttempt(t, context.Background(), opts) // runAttempt fails on a key in attempt.json
	if code != 0 || rec.Status != attemptStatusCompleted {
		t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
	}
	assertOneSequence(t, opts, srv, rec)
	if !bytes.Contains(srv.Received()[1].Body, []byte(attemptTestKey)) {
		t.Fatalf("fixture premise broken: request 2 does not carry the echoed key")
	}
	r1, r2 := rec.Requests[0], rec.Requests[1]
	if !r1.Response.Redacted || !r2.Request.Redacted || !r2.Response.Redacted || r1.Request.Redacted {
		t.Fatalf("redaction flags: req1 %v resp1 %v req2 %v resp2 %v", r1.Request.Redacted, r1.Response.Redacted, r2.Request.Redacted, r2.Response.Redacted)
	}
	entries, _ := os.ReadDir(filepath.Join(opts.AttemptDir, "raw"))
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(opts.AttemptDir, "raw", e.Name())); bytes.Contains(b, []byte(attemptTestKey)) {
			t.Fatalf("raw/%s holds the key", e.Name())
		}
	}
	// attempt.json is indented, which re-indents RawMessage arguments:
	// whitespace is not evidence, the content is.
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, rec.Transcript[0].ToolCalls[0].Arguments); err != nil {
		t.Fatalf("transcript arguments are not valid JSON: %v", err)
	}
	if args := compacted.String(); args != `{"query":"[REDACTED]"}` {
		t.Fatalf("transcript arguments %s", args)
	}
	if got := rec.Transcript[2].Content; got != "your key is [REDACTED]" {
		t.Fatalf("transcript answer %q", got)
	}
	if got := rec.Transcript[2].Thinking; got != "the key is [REDACTED]" {
		t.Fatalf("transcript thinking %q; the reasoning channel must be redacted too", got)
	}
}

// A key below minRedactableKeyLen is not redacted, and attempt.json stays
// valid with every measurement intact (plan review P0: byte-level redaction
// of key "0" would rewrite numbers).
func TestReplayAttempt_ShortKeyLeavesRecordIntact(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok 0 0", `{"prompt_tokens":10,"completion_tokens":20}`)})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.APIKey = "0"
	var stderr bytes.Buffer
	if code := runReplayAttempt(context.Background(), opts, &stderr); code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not redacted") {
		t.Fatalf("stderr %q; a short key must be called out", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(opts.AttemptDir, attemptTerminalName))
	if err != nil {
		t.Fatal(err)
	}
	var rec attemptRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("attempt.json does not parse: %v", err)
	}
	row := rec.Requests[0]
	if rec.Status != attemptStatusCompleted || *row.Usage.PromptTokens != 10 || *row.Usage.CompletionTokens != 20 || row.Response.Redacted || rec.Transcript[0].Content != "smoke-ok 0 0" || rec.Settings.TimeoutMs != 10000 {
		t.Fatalf("record altered: status %q usage %+v redacted %v content %q timeout %d", rec.Status, row.Usage, row.Response.Redacted, rec.Transcript[0].Content, rec.Settings.TimeoutMs)
	}
}

// A 2xx body that is one JSON object but has a wrong-typed field passes the
// recorder and fails the provider's decode: response_decode, not internal.
func TestReplayAttempt_WrongTypedFieldIsResponseDecode(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: `{"choices":[{"index":0,"message":{"role":"assistant","content":123}}],"usage":{"prompt_tokens":7}}`})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	_, rec := runAttempt(t, context.Background(), opts)
	assertAttemptError(t, rec, attemptCodeResponseDecode, attemptStageResponse, 1)
	assertOneSequence(t, opts, srv, rec)
	if rec.Error.HTTPStatus != 200 || rec.Requests[0].Usage == nil || *rec.Requests[0].Usage.PromptTokens != 7 {
		t.Fatalf("error %+v usage %+v", rec.Error, rec.Requests[0].Usage)
	}
}

func TestLoadAttemptInputsRefusesOllama(t *testing.T) {
	opts := attemptTestOptions(t, "http://127.0.0.1:1", fixtureSmokeTrace)
	opts.Models = "ollama/m"
	if _, _, err := loadAttemptInputs(opts, &attemptRecord{}); err == nil || !strings.Contains(err.Error(), "supports only openai-compat targets") {
		t.Fatalf("err = %v; want the explicit unsupported-provider refusal", err)
	}
}

func TestReplayAttempt_InputErrors(t *testing.T) {
	srv := newCompatServer(t)
	badArgs := filepath.Join(t.TempDir(), "bad-args.json")
	{
		raw, err := os.ReadFile(fixturePrefilledTrace)
		if err != nil {
			t.Fatal(err)
		}
		var tr Trace
		if err := json.Unmarshal(raw, &tr); err != nil {
			t.Fatal(err)
		}
		for i := range tr.Turns {
			if tr.Turns[i].Role == "assistant" && len(tr.Turns[i].ToolCalls) > 0 {
				tr.Turns[i].ToolCalls[0].Arguments = json.RawMessage(`[1]`)
				break
			}
		}
		out, _ := json.Marshal(tr)
		if err := os.WriteFile(badArgs, out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	invalidJSON := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalidJSON, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A parseable trace that validateTrace rejects: no system prompt.
	noSystem := filepath.Join(t.TempDir(), "no-system.json")
	if err := os.WriteFile(noSystem, []byte(`{"id":"no-system-001","system":"","turns":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// known is how far the run learned the record's nullable identity before
	// it failed. Each stage keeps the earlier ones; everything later is null,
	// never "" (an empty string breaks the scripted|prefilled enum and the
	// hex digest format).
	type known int
	const (
		knownNothing  known = iota // failed before the base URL validated
		knownEndpoint              // base URL validated; the trace is unreadable
		knownDigest                // trace read: sha256 set; not JSON
		knownID                    // trace parsed: id set; validateTrace rejected it
		knownKind                  // trace validated: replay_kind set; the replay then refused it
	)
	hexDigest := regexp.MustCompile(`^[0-9a-f]{64}$`)
	cases := []struct {
		name  string
		mut   func(*replayAttemptOptions)
		known known
	}{
		{"attempt id with a space", func(o *replayAttemptOptions) { o.AttemptID = "has space" }, knownNothing},
		{"attempt id of 129 bytes", func(o *replayAttemptOptions) { o.AttemptID = strings.Repeat("a", 129) }, knownNothing},
		{"empty arm", func(o *replayAttemptOptions) { o.Arm = "" }, knownNothing},
		{"arm with a control character", func(o *replayAttemptOptions) { o.Arm = "a\nb" }, knownNothing},
		{"ollama target", func(o *replayAttemptOptions) { o.Models = "ollama/m" }, knownNothing},
		{"two targets", func(o *replayAttemptOptions) { o.Models = "openai-compat/a,openai-compat/b" }, knownNothing},
		{"empty base url", func(o *replayAttemptOptions) { o.BaseURL = "" }, knownNothing},
		{"non-http base url", func(o *replayAttemptOptions) { o.BaseURL = "ftp://localhost" }, knownNothing},
		{"non-positive timeout", func(o *replayAttemptOptions) { o.Timeout = 0 }, knownNothing},
		{"trace is a directory", func(o *replayAttemptOptions) { o.TracePath = t.TempDir() }, knownEndpoint},
		{"trace missing", func(o *replayAttemptOptions) { o.TracePath = filepath.Join(t.TempDir(), "absent.json") }, knownEndpoint},
		{"trace not json", func(o *replayAttemptOptions) { o.TracePath = invalidJSON }, knownDigest},
		{"trace rejected by validateTrace", func(o *replayAttemptOptions) { o.TracePath = noSystem }, knownID},
		{"prefilled fixture with bad tool arguments", func(o *replayAttemptOptions) { o.TracePath = badArgs }, knownKind},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			tc.mut(&opts)
			code, rec := runAttempt(t, context.Background(), opts)
			if code != 0 {
				t.Fatalf("exit %d; an input failure still publishes its terminal", code)
			}
			assertAttemptError(t, rec, attemptCodeInput, attemptStageInput, 0)
			if len(rec.Requests) != 0 {
				t.Fatalf("requests %v; nothing may reach the wire", rec.Requests)
			}
			if rec.Settings.Transport != openAICompatTransport {
				t.Fatalf("transport %q; it is fixed, never null", rec.Settings.Transport)
			}
			if (rec.Settings.Endpoint != nil) != (tc.known >= knownEndpoint) || (rec.Trace.SHA256 != nil) != (tc.known >= knownDigest) ||
				(rec.Trace.ID != nil) != (tc.known >= knownID) || (rec.ReplayKind != nil) != (tc.known >= knownKind) {
				t.Fatalf("known %d but endpoint %s sha256 %s id %s replay_kind %s; each is null until learned",
					tc.known, attemptStr(rec.Settings.Endpoint), attemptStr(rec.Trace.SHA256), attemptStr(rec.Trace.ID), attemptStr(rec.ReplayKind))
			}
			if tc.known >= knownEndpoint && *rec.Settings.Endpoint != srv.URL {
				t.Fatalf("endpoint %q; want %q", *rec.Settings.Endpoint, srv.URL)
			}
			if tc.known >= knownDigest {
				file, err := os.ReadFile(opts.TracePath)
				if err != nil {
					t.Fatal(err)
				}
				if got := *rec.Trace.SHA256; !hexDigest.MatchString(got) || got != sha256Hex(file) {
					t.Fatalf("sha256 %q; want the lowercase hex digest of the file bytes %s", got, sha256Hex(file))
				}
				if tc.known >= knownID {
					var tr Trace
					if err := json.Unmarshal(file, &tr); err != nil || tr.ID == "" || *rec.Trace.ID != tr.ID {
						t.Fatalf("trace id %q (%v); want the parsed id %q", *rec.Trace.ID, err, tr.ID)
					}
				}
			}
			if tc.known >= knownKind && *rec.ReplayKind != "prefilled" {
				t.Fatalf("replay_kind %q; want prefilled", *rec.ReplayKind)
			}
		})
	}
	if n := len(srv.Received()); n != 0 {
		t.Fatalf("server received %d requests during input failures", n)
	}
}

func TestReplayAttempt_MaxLengthAttemptID(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.AttemptID = strings.Repeat("a", 128)
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusCompleted || len(srv.Received()[0].Correlation) != 130 {
		t.Fatalf("exit %d status %q correlation %q", code, rec.Status, srv.Received()[0].Correlation)
	}
}

func TestReplayAttempt_ArtifactWriteFailureStillPublishes(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	p := newAttemptPublisher(opts.AttemptDir)
	write := p.writeFile
	p.writeFile = func(path string, data []byte) error {
		if strings.HasSuffix(path, "0001.response.body") {
			return errors.New("disk full")
		}
		return write(path, data)
	}
	opts.publisher = p
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusFailed || rec.Error != nil || rec.ArtifactError == nil || rec.ArtifactError.Code != attemptCodeArtifactWrite {
		t.Fatalf("exit %d status %q error %+v artifact %+v", code, rec.Status, rec.Error, rec.ArtifactError)
	}
	if rec.Requests[0].Response != nil || rec.Requests[0].Request == nil {
		t.Fatalf("row %+v", rec.Requests[0])
	}
}

func TestReplayAttempt_TerminalPublishedLast(t *testing.T) {
	srv := newCompatServer(t,
		compatExchange{Body: compatToolCall("rag_search", `{"query":"retry budget"}`, "")},
		compatExchange{Body: compatText("done", "")},
	)
	opts := attemptTestOptions(t, srv.URL, fixtureToolRouteTrace)
	p := newAttemptPublisher(opts.AttemptDir)
	var events []string
	rel := func(path string) string {
		r, _ := filepath.Rel(opts.AttemptDir, path)
		return filepath.ToSlash(r)
	}
	write, syncFile, syncDir, rename := p.writeFile, p.syncFile, p.syncDir, p.rename
	p.writeFile = func(path string, data []byte) error {
		events = append(events, "write "+rel(path))
		return write(path, data)
	}
	p.syncFile = func(f *os.File) error { events = append(events, "fsync "+rel(f.Name())); return syncFile(f) }
	p.syncDir = func(dir string) error { events = append(events, "sync "+rel(dir)); return syncDir(dir) }
	p.rename = func(o, n string) error { events = append(events, "rename "+rel(o)+" "+rel(n)); return rename(o, n) }
	opts.publisher = p
	if code, _ := runAttempt(t, context.Background(), opts); code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := []string{"sync .."}
	for _, f := range []string{"raw/0001.request.json", "raw/0001.response.body", "raw/0002.request.json", "raw/0002.response.body"} {
		want = append(want, "write "+f, "fsync "+f)
	}
	want = append(want, "sync raw", "write .attempt.json.tmp", "fsync .attempt.json.tmp", "rename .attempt.json.tmp attempt.json", "sync .")
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("publication events\n got %v\nwant %v", events, want)
	}
}

func TestReplayAttempt_DirSyncFailureAfterRenameExitsOne(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	p := newAttemptPublisher(opts.AttemptDir)
	p.syncDir = func(d string) error {
		if d == opts.AttemptDir {
			return errors.New("fsync failed")
		}
		return syncDirectory(d)
	}
	opts.publisher = p
	var stderr bytes.Buffer
	if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 {
		t.Fatalf("exit %d; an unconfirmed terminal is not a durable publication", code)
	}
	data, err := os.ReadFile(filepath.Join(opts.AttemptDir, attemptTerminalName))
	var rec attemptRecord
	if err != nil || json.Unmarshal(data, &rec) != nil || rec.AttemptID != opts.AttemptID {
		t.Fatalf("attempt.json should be visible and parse with the attempt id (D3): %v", err)
	}
}

func TestReplayAttempt_ClaimFailuresPublishNothing(t *testing.T) {
	srv := newCompatServer(t)
	t.Run("existing dir", func(t *testing.T) {
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		if err := os.Mkdir(opts.AttemptDir, 0o700); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 {
			t.Fatalf("exit %d; want 1", code)
		}
		if entries, _ := os.ReadDir(opts.AttemptDir); len(entries) != 0 {
			t.Fatalf("pre-existing dir modified: %v", entries)
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.AttemptDir = filepath.Join(t.TempDir(), "absent", "attempt")
		var stderr bytes.Buffer
		if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 {
			t.Fatalf("exit %d; want 1", code)
		}
	})
	if n := len(srv.Received()); n != 0 {
		t.Fatalf("server received %d requests", n)
	}
}

func TestReplayAttempt_TerminalRenameFailureExitsOne(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	p := newAttemptPublisher(opts.AttemptDir)
	p.rename = func(string, string) error { return errors.New("rename failed") }
	opts.publisher = p
	var stderr bytes.Buffer
	if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 {
		t.Fatalf("exit %d; want 1", code)
	}
	if _, err := os.Stat(filepath.Join(opts.AttemptDir, attemptTerminalName)); !os.IsNotExist(err) {
		t.Fatalf("attempt.json exists: %v", err)
	}
}

func TestReplayAttempt_RefusesOnWindows(t *testing.T) {
	saved := replayAttemptGOOS
	replayAttemptGOOS = "windows"
	t.Cleanup(func() { replayAttemptGOOS = saved })
	opts := attemptTestOptions(t, "http://127.0.0.1:1", fixtureSmokeTrace)
	var stderr bytes.Buffer
	if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 || !strings.Contains(stderr.String(), "windows") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(opts.AttemptDir); !os.IsNotExist(err) {
		t.Fatalf("attempt dir created on windows: %v", err)
	}
}
