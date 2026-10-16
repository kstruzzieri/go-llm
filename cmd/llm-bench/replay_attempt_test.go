//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
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
	code, rec, _ := runAttemptStderr(t, ctx, opts)
	return code, rec
}

// runAttemptStderr is runAttempt that also returns what the run wrote to
// stderr, the supervisor's evidence channel (spec amendment A4).
func runAttemptStderr(t *testing.T, ctx context.Context, opts replayAttemptOptions) (int, attemptRecord, string) {
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
	return code, rec, stderr.String()
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

// assertUsage: the row kept the prompt and completion counts the server
// returned, and reasoning stays unknown.
func assertUsage(t *testing.T, row attemptRequestRow, prompt, completion int) {
	t.Helper()
	u := row.Usage
	if u == nil || u.PromptTokens == nil || u.CompletionTokens == nil || *u.PromptTokens != prompt || *u.CompletionTokens != completion || u.ReasoningTokens != nil {
		t.Fatalf("row %d usage %+v; want prompt %d completion %d, reasoning null", row.Ordinal, u, prompt, completion)
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
	cases := []struct {
		name, body string
		wantUsage  bool // the body is parseable JSON carrying usage 11/3
	}{
		{"trailing data", body, false},
		{"error envelope in a 200", `{"error":{"message":"overloaded"},"choices":[{"index":0,"message":{"role":"assistant","content":"x"}}],"usage":{"prompt_tokens":11,"completion_tokens":3}}`, true},
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
			// Spec 6.1 step 7: usage is parsed for any status, before the envelope
			// check, so a rejected body still reports what the server returned.
			if tc.wantUsage {
				assertUsage(t, rec.Requests[0], 11, 3)
			}
		})
	}
}

// The recorder reads member names as the provider's struct decode does. A
// body the provider reads differently must not pass the envelope check and
// then fail as internal.
func TestReplayAttempt_CaseVariantMembers(t *testing.T) {
	msg := `[{"index":0,"message":{"role":"assistant","content":"smoke-ok"},"finish_reason":"stop"}]`
	cases := []struct {
		name, body, code, stage string // code "": the attempt completes
	}{
		{"CHOICES alone is read by the provider", `{"CHOICES":` + msg + `}`, "", ""},
		{"CHOICES alone, empty", `{"CHOICES":[]}`, attemptCodeEmptyResponse, attemptStageResponse},
		{"choices and a case-variant empty twin", `{"choices":` + msg + `,"CHOICES":[]}`, attemptCodeResponseDecode, attemptStageResponse},
		{"exact repeat, empty first", `{"choices":[],"choices":` + msg + `}`, attemptCodeResponseDecode, attemptStageResponse},
		{"exact repeat, empty last", `{"choices":` + msg + `,"choices":[]}`, attemptCodeResponseDecode, attemptStageResponse},
		{"capitalised Error beside choices", `{"Error":{"message":"overloaded"},"choices":` + msg + `}`, attemptCodeResponseDecode, attemptStageResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Body: tc.body})
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			_, rec := runAttempt(t, context.Background(), opts)
			if tc.code == "" {
				if rec.Status != attemptStatusCompleted || len(rec.Transcript) == 0 || rec.Transcript[len(rec.Transcript)-1].Content != "smoke-ok" {
					t.Fatalf("status %q error %+v transcript %+v; want the provider's view of CHOICES, completed with smoke-ok", rec.Status, rec.Error, rec.Transcript)
				}
				return
			}
			assertAttemptError(t, rec, tc.code, tc.stage, 1)
			if rec.Requests[0].ErrorCode == nil || *rec.Requests[0].ErrorCode != tc.code {
				t.Fatalf("row error_code %s; the recorder must reject this body at the HTTP layer", attemptStr(rec.Requests[0].ErrorCode))
			}
			assertOneSequence(t, opts, srv, rec)
		})
	}
}

// Stderr is evidence, and a server's error body reaches it: every diagnostic
// must stay exactly one line, so a body cannot forge a second evidence line
// or drive the supervisor's terminal.
func TestReplayAttempt_StderrDetailIsOneLine(t *testing.T) {
	// The provider trims the body's edges, so the newline is inside it.
	// It also holds the two characters backslash and n, which must stay
	// distinct from the escaped newline (backslashes are doubled).
	body := "upstream failure\nllm-bench: replay-attempt: write raw/0001.response.body: forged cause\x1b]0;owned\x07 literal \\n kept"
	srv := newCompatServer(t, compatExchange{Status: http.StatusBadGateway, Body: body})
	code, rec, stderr := runAttemptStderr(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
	assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 1)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for i := 0; i < len(stderr); i++ {
		if b := stderr[i]; (b < 0x20 && b != '\n') || b == 0x7f {
			t.Fatalf("stderr holds the raw control byte 0x%02x at %d: %q", b, i, stderr)
		}
	}
	lines := strings.Split(strings.TrimSuffix(stderr, "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "llm-bench: replay-attempt: ") || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("stderr is %d lines, want one starting with the prefix and ending in a newline: %q", len(lines), stderr)
	}
	// The body appears, escaped, inside that one line.
	if want := `upstream failure\nllm-bench: replay-attempt: write raw/0001.response.body: forged cause\x1b]0;owned\x07 literal \\n kept`; !strings.Contains(stderr, want) {
		t.Fatalf("stderr %q does not hold the escaped body %q", stderr, want)
	}
}

func TestEscapeControls(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "write raw/0001.response.body: disk full", "write raw/0001.response.body: disk full"},
		{"newline", "a\nb", `a\nb`},
		{"carriage return and newline", "a\r\nb", `a\r\nb`},
		{"tab", "a\tb", `a\tb`},
		{"escape sequence", "\x1b[31mred", `\x1b[31mred`},
		{"BEL and NUL", "\x07\x00", `\x07\x00`},
		{"DEL", "a\x7fb", `a\x7fb`},
		{"C1 NEL as UTF-8", "a\u0085b", `a\u0085b`},
		{"C1 CSI as UTF-8", "a\u009bb", `a\u009bb`},
		{"lone C1 byte is not valid UTF-8", "a\x9bb", `a\x9bb`},
		{"truncated multibyte sequence", "a\xe2\x82", `a\xe2\x82`},
		{"line separator", "a\u2028b\u2029c", `a\u2028b\u2029c`},
		{"printable non-ASCII is kept", "h\u00e9llo \u2713 \U0001F600", "h\u00e9llo \u2713 \U0001F600"},
		{"backslash is doubled", `C:\dir\n`, `C:\\dir\\n`},
		{"a literal backslash-n differs from a newline", `a\nb`, `a\\nb`},
		{"a lone backslash", `\`, `\\`},
		{"a backslash before a real newline", "\\\n", `\\\n`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := escapeControls(tc.in); got != tc.want {
				t.Fatalf("escapeControls(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The key is redacted before escaping: escaping first would turn a key that
// holds a control character into text the key no longer matches.
func TestReportAttemptDetailRedactsBeforeEscaping(t *testing.T) {
	var buf bytes.Buffer
	reportAttemptDetail(&buf, errors.New("bad ab\ncd key\nnext"), "ab\ncd")
	if want := "llm-bench: replay-attempt: bad [REDACTED] key\\nnext\n"; buf.String() != want {
		t.Fatalf("got %q, want %q", buf.String(), want)
	}
}

func TestReplayAttempt_EmptyResponse(t *testing.T) {
	t.Run("zero choices", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: `{"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":0}}`})
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeEmptyResponse, attemptStageResponse, 1)
		assertUsage(t, rec.Requests[0], 9, 0)
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
		compatExchange{Status: http.StatusBadGateway, Body: `{"error":{"message":"upstream down"},"usage":{"prompt_tokens":31,"completion_tokens":2}}`},
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
	// Acceptance 2: a non-2xx response still reports the usage it carried.
	assertUsage(t, rec.Requests[1], 31, 2)
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
		// The key is echoed in the arguments and as the tool-call id, which the
		// replay copies onto the tool turn and the next request.
		compatExchange{Body: compatToolCallID(attemptTestKey, "rag_search", `{"query":"`+attemptTestKey+`"}`, "")},
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
	if got := rec.Transcript[1].ToolCallID; got != attemptRedacted {
		t.Fatalf("transcript tool_call_id %q; the candidate's call id is candidate text", got)
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

// The candidate names a tool after the key. On a plain-chat trace the call
// stays on the transcript as a divergence; on a scripted route it stays as the
// mismatch that fails the attempt.
func TestReplayAttempt_RedactsKeyInToolName(t *testing.T) {
	cases := []struct {
		name  string
		trace string
		code  string // want error code, "" for a completed attempt
	}{
		{"plain-chat divergence", fixtureSmokeTrace, ""},
		{"scripted route mismatch", fixtureToolRouteTrace, attemptCodeReplayDivergence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Body: compatToolCall(attemptTestKey, `{}`, "")})
			opts := attemptTestOptions(t, srv.URL, tc.trace)
			code, rec := runAttempt(t, context.Background(), opts) // fails on a key in attempt.json or stderr
			if code != 0 {
				t.Fatalf("exit %d", code)
			}
			if (tc.code == "") != (rec.Error == nil) || (rec.Error != nil && rec.Error.Code != tc.code) {
				t.Fatalf("error %+v; want code %q", rec.Error, tc.code)
			}
			if got := rec.Transcript[len(rec.Transcript)-1].ToolCalls[0].Name; got != attemptRedacted {
				t.Fatalf("transcript tool call name %q; the candidate's tool name is candidate text", got)
			}
		})
	}
}

// The 16-byte threshold (spec amendment A8) is a published contract: a key of
// 16 bytes is redacted, one of 15 is not and says so. The lengths are literals
// on purpose; derived from minRedactableKeyLen they would follow it wherever
// it was moved.
func TestReplayAttempt_RedactionThreshold(t *testing.T) {
	cases := []struct {
		name     string
		keyLen   int
		redacted bool
	}{
		{"15 bytes", 15, false},
		{"16 bytes", 16, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "key-" + strings.Repeat("x", tc.keyLen-4)
			if len(key) != tc.keyLen {
				t.Fatalf("fixture key is %d bytes; want %d", len(key), tc.keyLen)
			}
			srv := newCompatServer(t, compatExchange{Body: compatText("echo "+key, "")})
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			opts.APIKey = key
			code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
			if code != 0 || rec.Status != attemptStatusCompleted {
				t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
			}
			if got := strings.Contains(stderr, "not redacted"); got == tc.redacted {
				t.Fatalf("stderr %q; the not-redacted warning must appear exactly when the key is below the threshold", stderr)
			}
			wantContent := "echo " + key
			if tc.redacted {
				wantContent = "echo " + attemptRedacted
			}
			raw, err := os.ReadFile(filepath.Join(opts.AttemptDir, filepath.FromSlash(rec.Requests[0].Response.Path)))
			if err != nil {
				t.Fatal(err)
			}
			if got := rec.Transcript[0].Content; got != wantContent {
				t.Fatalf("transcript content %q; want %q", got, wantContent)
			}
			if rec.Requests[0].Response.Redacted != tc.redacted || bytes.Contains(raw, []byte(key)) == tc.redacted {
				t.Fatalf("response redacted flag %v, raw holds key %v; want redacted %v", rec.Requests[0].Response.Redacted, bytes.Contains(raw, []byte(key)), tc.redacted)
			}
		})
	}
}

// tool-canary-01's tool-result turn (index 2) carries raw: the trace's own
// JSON, copied verbatim into the transcript. The trace file can hold the key
// only if the supervisor put it there, but then it must not reach attempt.json.
func TestReplayAttempt_RedactsKeyInTraceRaw(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // compacted transcript raw
	}{
		{"holding the key", `{"auth":"` + attemptTestKey + `"}`, `{"auth":"[REDACTED]"}`},
		{"holding the key in an array", `["x",{"a":["` + attemptTestKey + `"]}]`, `["x",{"a":["[REDACTED]"]}]`},
		{"holding the key as a member name", `{"` + attemptTestKey + `":1}`, `{"[REDACTED]":1}`},
		// The key as the trace file stores it is not byte-for-byte the key.
		{"holding the key JSON-escaped", `{"auth":"lab-attempt-key-\u0030123456789"}`, `{"auth":"[REDACTED]"}`},
		// A raw that never held the key is not re-encoded: that would sort
		// its members and reformat its numbers.
		{"without the key", `{"z":1,"a":[2,1.50,"x"]}`, `{"z":1,"a":[2,1.50,"x"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(fixtureToolRouteTrace)
			if err != nil {
				t.Fatal(err)
			}
			var tr map[string]any
			if err := json.Unmarshal(data, &tr); err != nil {
				t.Fatal(err)
			}
			tr["turns"].([]any)[2].(map[string]any)["raw"] = json.RawMessage(tc.raw)
			data, _ = json.Marshal(tr)
			tracePath := filepath.Join(t.TempDir(), "trace.json")
			if err := os.WriteFile(tracePath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			srv := newCompatServer(t,
				compatExchange{Body: compatToolCall("rag_search", `{"query":"q"}`, "")},
				compatExchange{Body: compatText("done", "")},
			)
			opts := attemptTestOptions(t, srv.URL, tracePath)
			code, rec := runAttempt(t, context.Background(), opts) // fails on a key in attempt.json
			if code != 0 || rec.Status != attemptStatusCompleted {
				t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
			}
			var compacted bytes.Buffer
			if err := json.Compact(&compacted, rec.Transcript[1].Raw); err != nil {
				t.Fatalf("transcript raw %q is not JSON: %v", rec.Transcript[1].Raw, err)
			}
			if compacted.String() != tc.want {
				t.Fatalf("transcript raw %s; want %s", compacted.String(), tc.want)
			}
		})
	}
}

// A trace that declares a tool named with the key: the candidate must call
// that name, so it reaches the assistant turn's tool call and the tool turn.
func TestReplayAttempt_RedactsKeyInTraceToolName(t *testing.T) {
	data, err := os.ReadFile(fixtureToolRouteTrace)
	if err != nil {
		t.Fatal(err)
	}
	tracePath := filepath.Join(t.TempDir(), "trace.json")
	if err := os.WriteFile(tracePath, []byte(strings.ReplaceAll(string(data), "rag_search", attemptTestKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := newCompatServer(t,
		compatExchange{Body: compatToolCall(attemptTestKey, `{"query":"q"}`, "")},
		compatExchange{Body: compatText("done", "")},
	)
	opts := attemptTestOptions(t, srv.URL, tracePath)
	code, rec := runAttempt(t, context.Background(), opts) // fails on a key in attempt.json
	if code != 0 || rec.Status != attemptStatusCompleted {
		t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
	}
	if got := rec.Transcript[0].ToolCalls[0].Name; got != attemptRedacted {
		t.Fatalf("assistant tool call name %q", got)
	}
	if got := rec.Transcript[1].Name; got != attemptRedacted {
		t.Fatalf("tool turn name %q; a trace-declared tool name is trace text", got)
	}
}

// redactRecordKey rewrites every text field of the transcript and notes. One
// is unreachable end to end: a reply's assistant turn is built without a
// tool-call id, so r(c.ID) is covered only here. A tool turn's name is
// reachable only through a trace that declares a tool named with the key
// (TestReplayAttempt_RedactsKeyInTraceToolName); this test hits it directly
// with the rest, so a dropped replacement in any of them is caught.
func TestRedactRecordKeyCoversEveryTextField(t *testing.T) {
	const key = attemptTestKey
	rec := attemptRecord{
		Transcript: []Turn{{
			Role: "tool", Content: "content " + key, Thinking: "thinking " + key, Name: "name " + key, ToolCallID: "id " + key,
			Raw: json.RawMessage(`{"` + key + `":["` + key + `",1.50]}`),
			ToolCalls: []ToolCall{{
				ID: "call id " + key, Name: "call name " + key, Arguments: json.RawMessage(`{"` + key + `":["` + key + `",1.50]}`),
			}},
		}},
		Notes: []string{"note " + key},
	}
	redactRecordKey(&rec, key)
	turn, call := rec.Transcript[0], rec.Transcript[0].ToolCalls[0]
	for _, f := range []struct{ field, got, want string }{
		{"content", turn.Content, "content [REDACTED]"},
		{"thinking", turn.Thinking, "thinking [REDACTED]"},
		{"name", turn.Name, "name [REDACTED]"},
		{"tool_call_id", turn.ToolCallID, "id [REDACTED]"},
		{"raw", string(turn.Raw), `{"[REDACTED]":["[REDACTED]",1.50]}`},
		{"call id", call.ID, "call id [REDACTED]"},
		{"call name", call.Name, "call name [REDACTED]"},
		{"call arguments", string(call.Arguments), `{"[REDACTED]":["[REDACTED]",1.50]}`},
		{"note", rec.Notes[0], "note [REDACTED]"},
	} {
		if f.got != f.want {
			t.Errorf("%s = %q; want %q", f.field, f.got, f.want)
		}
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

// Test binaries are not VCS-stamped, so provenance is pinned through the
// function that reads it from a *debug.BuildInfo.
func TestAttemptBuildFrom(t *testing.T) {
	strp := func(s string) *string { return &s }
	boolp := func(b bool) *bool { return &b }
	vcs := func(rev, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: rev}, {Key: "vcs.time", Value: "2026-10-12T00:00:00Z"}, {Key: "vcs.modified", Value: modified}}
	}
	cases := []struct {
		name string
		info *debug.BuildInfo
		want attemptBuild
	}{
		{"build info unavailable", nil, attemptBuild{GoVersion: runtime.Version()}},
		{"stamped and modified", &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "v0.1.1-0.20261012000000-abcdef123456+dirty"}, Settings: vcs("abcdef1234567890", "true")},
			attemptBuild{GoVersion: "go1.27.1", ModuleVersion: strp("v0.1.1-0.20261012000000-abcdef123456+dirty"), VCSRevision: strp("abcdef1234567890"), VCSModified: boolp(true)}},
		{"stamped and clean", &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "v0.2.0"}, Settings: vcs("0123456789abcdef", "false")},
			attemptBuild{GoVersion: "go1.27.1", ModuleVersion: strp("v0.2.0"), VCSRevision: strp("0123456789abcdef"), VCSModified: boolp(false)}},
		{"no vcs settings and no module version", &debug.BuildInfo{GoVersion: "go1.27.1", Settings: []debug.BuildSetting{{Key: "-compiler", Value: "gc"}}},
			attemptBuild{GoVersion: "go1.27.1"}},
		{"devel module version is kept verbatim", &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "(devel)"}},
			attemptBuild{GoVersion: "go1.27.1", ModuleVersion: strp("(devel)")}},
		{"tagged module version", &debug.BuildInfo{GoVersion: "go1.27.1", Main: debug.Module{Version: "v1.2.3"}},
			attemptBuild{GoVersion: "go1.27.1", ModuleVersion: strp("v1.2.3")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attemptBuildFrom(tc.info); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("attemptBuildFrom = %s\nwant %s", describeAttemptBuild(got), describeAttemptBuild(tc.want))
			}
		})
	}
	// Unknown is null on the wire, never an empty string.
	data, err := json.Marshal(attemptBuildFrom(nil))
	if err != nil || !strings.Contains(string(data), `"module_version":null,"vcs_revision":null,"vcs_modified":null`) {
		t.Fatalf("encoded %s (%v); unknown provenance must be null", data, err)
	}
}

// describeAttemptBuild renders a build with its pointers dereferenced, so a
// failing comparison shows values, not addresses.
func describeAttemptBuild(b attemptBuild) string {
	out := "{go " + b.GoVersion
	if b.ModuleVersion != nil {
		out += " module " + *b.ModuleVersion
	}
	if b.VCSRevision != nil {
		out += " rev " + *b.VCSRevision
	}
	if b.VCSModified != nil {
		out += fmt.Sprintf(" modified %v", *b.VCSModified)
	}
	return out + "}"
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
	// Parses, but its id is empty: the id is unknown, so trace.id stays null.
	noID := filepath.Join(t.TempDir(), "no-id.json")
	if err := os.WriteFile(noID, []byte(`{"id":"","system":"s","turns":[{"role":"user","content":"hi"}]}`), 0o600); err != nil {
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
		{"arm of 257 bytes", func(o *replayAttemptOptions) { o.Arm = strings.Repeat("a", 257) }, knownNothing},
		{"arm that is not UTF-8", func(o *replayAttemptOptions) { o.Arm = "a\xffb" }, knownNothing},
		{"arm of 258 bytes in 129 runes", func(o *replayAttemptOptions) { o.Arm = strings.Repeat("\u00e9", 129) }, knownNothing},
		{"ollama target", func(o *replayAttemptOptions) { o.Models = "ollama/m" }, knownNothing},
		{"two targets", func(o *replayAttemptOptions) { o.Models = "openai-compat/a,openai-compat/b" }, knownNothing},
		{"empty base url", func(o *replayAttemptOptions) { o.BaseURL = "" }, knownNothing},
		{"non-http base url", func(o *replayAttemptOptions) { o.BaseURL = "ftp://localhost" }, knownNothing},
		{"base url with userinfo", func(o *replayAttemptOptions) {
			o.BaseURL = "http://user:hunter2@" + strings.TrimPrefix(srv.URL, "http://")
		}, knownNothing},
		{"base url with userinfo and no password", func(o *replayAttemptOptions) { o.BaseURL = "http://token@" + strings.TrimPrefix(srv.URL, "http://") }, knownNothing},
		{"base url with a query", func(o *replayAttemptOptions) { o.BaseURL = srv.URL + "?x=1" }, knownNothing},
		{"base url with a bare question mark", func(o *replayAttemptOptions) { o.BaseURL = srv.URL + "?" }, knownNothing},
		{"base url with a fragment", func(o *replayAttemptOptions) { o.BaseURL = srv.URL + "/#frag" }, knownNothing},
		{"base url with a bare hash", func(o *replayAttemptOptions) { o.BaseURL = srv.URL + "#" }, knownNothing},
		{"non-positive timeout", func(o *replayAttemptOptions) { o.Timeout = 0 }, knownNothing},
		{"timeout under a millisecond", func(o *replayAttemptOptions) { o.Timeout = 999 * time.Microsecond }, knownNothing},
		{"trace is a directory", func(o *replayAttemptOptions) { o.TracePath = t.TempDir() }, knownEndpoint},
		{"trace missing", func(o *replayAttemptOptions) { o.TracePath = filepath.Join(t.TempDir(), "absent.json") }, knownEndpoint},
		{"trace not json", func(o *replayAttemptOptions) { o.TracePath = invalidJSON }, knownDigest},
		{"trace with an empty id", func(o *replayAttemptOptions) { o.TracePath = noID }, knownDigest},
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

// A refused input never echoes the value that was refused: the base URL may
// carry credentials, and the key is never named.
func TestReplayAttempt_RefusedBaseURLIsNeverEchoed(t *testing.T) {
	srv := newCompatServer(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, base := range []string{"http://user:hunter2@" + host, "http://hunter2@" + host, srv.URL + "?token=hunter2", srv.URL + "/#hunter2"} {
		t.Run(base, func(t *testing.T) {
			opts := attemptTestOptions(t, base, fixtureSmokeTrace)
			code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
			assertAttemptError(t, rec, attemptCodeInput, attemptStageInput, 0)
			if code != 0 || rec.Settings.Endpoint != nil {
				t.Fatalf("exit %d endpoint %s; a refused URL is input with a null endpoint", code, attemptStr(rec.Settings.Endpoint))
			}
			terminal, err := os.ReadFile(filepath.Join(opts.AttemptDir, attemptTerminalName))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stderr, "hunter2") || bytes.Contains(terminal, []byte("hunter2")) {
				t.Fatalf("a credential from the refused URL leaked.\nstderr: %s\nattempt.json: %s", stderr, terminal)
			}
			if want := "llm-bench: replay-attempt: -candidate-base-url must not carry userinfo, a query or a fragment\n"; stderr != want {
				t.Fatalf("stderr %q, want %q", stderr, want)
			}
		})
	}
	if n := len(srv.Received()); n != 0 {
		t.Fatalf("server received %d requests", n)
	}
}

// llm-bench refuses a key with any control character, tab included, as a
// paste error (net/http would also refuse most of them, which would read as a
// candidate connection failure). Neither the key nor a piece of it may appear
// in stderr or attempt.json.
func TestReplayAttempt_ControlCharacterKeyIsInput(t *testing.T) {
	srv := newCompatServer(t)
	isControl := func(r rune) bool { return r < 0x20 || r == 0x7f }
	for name, key := range map[string]string{
		"newline": "kQ7vZpR2\nwXm9Tb4LsN6",
		"ESC":     "kQ7vZpR2\x1bwXm9Tb4LsN6",
		"DEL":     "kQ7vZpR2\x7fwXm9Tb4LsN6",
		"tab":     "kQ7vZpR2\twXm9Tb4LsN6",
		"NUL":     "kQ7vZpR2\x00wXm9Tb4LsN6",
	} {
		t.Run(name, func(t *testing.T) {
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			opts.APIKey = key
			code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
			assertAttemptError(t, rec, attemptCodeInput, attemptStageInput, 0)
			if code != 0 || len(rec.Requests) != 0 {
				t.Fatalf("exit %d, %d requests; a bad key is input and nothing reaches the wire", code, len(rec.Requests))
			}
			if want := "llm-bench: replay-attempt: candidate API key contains a control character\n"; stderr != want {
				t.Fatalf("stderr %q, want %q", stderr, want)
			}
			terminal, err := os.ReadFile(filepath.Join(opts.AttemptDir, attemptTerminalName))
			if err != nil {
				t.Fatal(err)
			}
			for _, segment := range strings.FieldsFunc(key, isControl) {
				for i := 0; i+5 <= len(segment); i++ {
					if piece := segment[i : i+5]; strings.Contains(stderr, piece) || bytes.Contains(terminal, []byte(piece)) {
						t.Fatalf("the key piece %q leaked.\nstderr: %s\nattempt.json: %s", piece, stderr, terminal)
					}
				}
			}
		})
	}
	if n := len(srv.Received()); n != 0 {
		t.Fatalf("server received %d requests", n)
	}
	// Surrounding whitespace is trimmed before the key is sent, so it is not
	// a control character in the credential.
	t.Run("trailing newline is trimmed, not refused", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.APIKey = "kQ7vZpR2wXm9Tb4LsN6\n"
		code, rec := runAttempt(t, context.Background(), opts)
		if got := srv.Received(); code != 0 || rec.Status != attemptStatusCompleted || len(got) != 1 || got[0].Authorization != "Bearer kQ7vZpR2wXm9Tb4LsN6" {
			t.Fatalf("exit %d status %q error %+v requests %+v", code, rec.Status, rec.Error, got)
		}
	})
}

// timeout_ms is the timeout truncated toward zero to whole milliseconds.
func TestReplayAttempt_TimeoutMsTruncatesTowardZero(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Delay: 5 * time.Second})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.Timeout = 1900 * time.Microsecond
	_, rec := runAttempt(t, context.Background(), opts)
	if rec.Settings.TimeoutMs != 1 {
		t.Fatalf("timeout_ms %d for a 1900us timeout; want 1", rec.Settings.TimeoutMs)
	}
}

// The refusal bound is 1ms: timeout_ms is whole milliseconds.
func TestReplayAttempt_OneMillisecondTimeoutIsAccepted(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Delay: 5 * time.Second})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.Timeout = time.Millisecond
	_, rec := runAttempt(t, context.Background(), opts)
	assertAttemptError(t, rec, attemptCodeCanceled, attemptStageRequest, 1)
	if rec.Settings.TimeoutMs != 1 || rec.Error.CancelSource != attemptCancelTimeout {
		t.Fatalf("timeout_ms %d source %q; want 1 and attempt_timeout", rec.Settings.TimeoutMs, rec.Error.CancelSource)
	}
}

// The transport sends the trimmed key, so redaction, its 16-byte threshold
// and the stderr redaction must all use that form. A key with trailing
// whitespace that is echoed back must not survive in any evidence.
func TestReplayAttempt_TrailingWhitespaceKeyIsRedactedAsSent(t *testing.T) {
	t.Run("echoed key in raw files and attempt.json", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: compatText("your key is "+attemptTestKey, "")})
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.APIKey = attemptTestKey + "\n"
		// runAttemptStderr fails if stderr or attempt.json holds the key.
		code, rec, _ := runAttemptStderr(t, context.Background(), opts)
		if got := srv.Received(); code != 0 || rec.Status != attemptStatusCompleted || len(got) != 1 || got[0].Authorization != "Bearer "+attemptTestKey {
			t.Fatalf("exit %d status %q error %+v requests %+v; want the trimmed key sent and the attempt completed", code, rec.Status, rec.Error, got)
		}
		entries, err := os.ReadDir(filepath.Join(opts.AttemptDir, "raw"))
		if err != nil || len(entries) == 0 {
			t.Fatalf("raw entries %d (%v)", len(entries), err)
		}
		for _, e := range entries {
			if b, _ := os.ReadFile(filepath.Join(opts.AttemptDir, "raw", e.Name())); bytes.Contains(b, []byte(attemptTestKey)) {
				t.Fatalf("raw/%s holds the key", e.Name())
			}
		}
		if got := rec.Transcript[0].Content; got != "your key is [REDACTED]" || !rec.Requests[0].Response.Redacted {
			t.Fatalf("transcript %q, response redacted %v; want the echoed key redacted", got, rec.Requests[0].Response.Redacted)
		}
	})
	t.Run("stderr", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Status: http.StatusInternalServerError, Body: "rejected key " + attemptTestKey})
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.APIKey = attemptTestKey + "\n"
		_, rec, stderr := runAttemptStderr(t, context.Background(), opts) // fails on a key in stderr
		assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 1)
		if !strings.Contains(stderr, "rejected key [REDACTED]") {
			t.Fatalf("stderr %q; want the sent key redacted in the server's body", stderr)
		}
	})
	// 15 bytes once trimmed, 16 with the newline: the threshold counts the key
	// that is sent.
	t.Run("threshold counts the trimmed key", func(t *testing.T) {
		key := "key-" + strings.Repeat("x", 11)
		srv := newCompatServer(t, compatExchange{Body: compatText("echo "+key, "")})
		opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
		opts.APIKey = key + "\n"
		code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
		if code != 0 || rec.Status != attemptStatusCompleted {
			t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
		}
		if !strings.Contains(stderr, "not redacted") || rec.Transcript[0].Content != "echo "+key {
			t.Fatalf("stderr %q transcript %q; a 15-byte key is below the threshold and stays as is", stderr, rec.Transcript[0].Content)
		}
	})
}

// error.message follows the code, not the text of the error: a server body
// that says "timeout" must not change it.
func TestReplayAttempt_ErrorMessageFollowsTheCode(t *testing.T) {
	t.Run("server body steering", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Status: http.StatusInternalServerError, Body: "upstream timeout: connection refused: json: cannot unmarshal parse"})
		_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, fixtureSmokeTrace))
		assertAttemptError(t, rec, attemptCodeHTTPStatus, attemptStageResponse, 1)
		if rec.Error.Message != "the server returned a non-2xx status" {
			t.Fatalf("message %q", rec.Error.Message)
		}
	})
	t.Run("input", func(t *testing.T) {
		opts := attemptTestOptions(t, newCompatServer(t).URL, filepath.Join(t.TempDir(), "absent-trace.json"))
		_, rec := runAttempt(t, context.Background(), opts)
		assertAttemptError(t, rec, attemptCodeInput, attemptStageInput, 0)
		if rec.Error.Message != "an input to the attempt was invalid" {
			t.Fatalf("message %q", rec.Error.Message)
		}
	})
}

// newCandidateTransport can only fail here on an empty base URL, which
// loadAttemptInputs has refused already, so this site is reachable only by
// calling executeAttempt directly.
func TestExecuteAttemptTransportErrorIsInput(t *testing.T) {
	targets, err := parseModelTargets("openai-compat/cand")
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	opts := replayAttemptOptions{AttemptID: "att-1", Timeout: time.Second} // no BaseURL
	_, rows, aerr := executeAttempt(context.Background(), opts, loadFixtureTrace(t, fixtureSmokeTrace), targets[0], &stderr)
	if len(rows) != 0 || aerr == nil || aerr.Code != attemptCodeInput || aerr.Stage != attemptStageInput || aerr.Message != "an input to the attempt was invalid" {
		t.Fatalf("rows %d error %+v; want an input failure with the fixed message", len(rows), aerr)
	}
	if want := "llm-bench: replay-attempt: candidate openai-compat transport requires -candidate-base-url\n"; stderr.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.String(), want)
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

// The arm limit is 256 bytes, not runes: this one is 255 runes, and it is
// recorded exactly as given.
func TestReplayAttempt_MaxLengthArm(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	opts.Arm = strings.Repeat("a", 254) + "\u00e9"
	if len(opts.Arm) != 256 {
		t.Fatalf("fixture arm is %d bytes; want 256", len(opts.Arm))
	}
	code, rec := runAttempt(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusCompleted || rec.Arm != opts.Arm {
		t.Fatalf("exit %d status %q error %+v arm %q; a 256-byte arm is accepted and recorded verbatim", code, rec.Status, rec.Error, rec.Arm)
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
	code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusFailed || rec.Error != nil || rec.ArtifactError == nil || rec.ArtifactError.Code != attemptCodeArtifactWrite {
		t.Fatalf("exit %d status %q error %+v artifact %+v", code, rec.Status, rec.Error, rec.ArtifactError)
	}
	if rec.Requests[0].Response != nil || rec.Requests[0].Request == nil {
		t.Fatalf("row %+v", rec.Requests[0])
	}
	// The record carries llm-bench's own text; the cause goes to stderr (A4).
	if rec.ArtifactError.Message != "unreferenced evidence: raw/0001.response.body" {
		t.Fatalf("artifact_error message %q", rec.ArtifactError.Message)
	}
	if !strings.Contains(stderr, "raw/0001.response.body") || !strings.Contains(stderr, "disk full") {
		t.Fatalf("stderr %q; an artifact_write failure must name the file and its cause", stderr)
	}
}

// The attempt dir's fsync after raw/ is created fails, so no row may
// reference a file in raw/. The terminal still publishes: its own sync of the
// attempt dir, after the rename, succeeds, so this is exit 0 and a failed
// record whose artifact_error says why.
func TestReplayAttempt_AttemptDirSyncFailureBeforeRawPublishesNoRefs(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("smoke-ok", "")})
	opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
	p := newAttemptPublisher(opts.AttemptDir)
	syncDir := p.syncDir
	var attemptDirSyncs int
	p.syncDir = func(d string) error {
		if d == opts.AttemptDir {
			attemptDirSyncs++
			if attemptDirSyncs == 1 {
				return errors.New("fsync failed")
			}
		}
		return syncDir(d)
	}
	opts.publisher = p
	code, rec, stderr := runAttemptStderr(t, context.Background(), opts)
	if code != 0 || rec.Status != attemptStatusFailed || rec.Error != nil || rec.ArtifactError == nil || rec.ArtifactError.Code != attemptCodeArtifactWrite || rec.ArtifactError.Message != "unreferenced evidence: raw/" {
		t.Fatalf("exit %d status %q error %+v artifact %+v", code, rec.Status, rec.Error, rec.ArtifactError)
	}
	if len(rec.Requests) != 1 || rec.Requests[0].Request != nil || rec.Requests[0].Response != nil {
		t.Fatalf("rows %+v; no ref may point into a raw/ whose directory entry is not durable", rec.Requests)
	}
	if !strings.Contains(stderr, "fsync failed") {
		t.Fatalf("stderr %q; the sync failure's cause must reach the supervisor", stderr)
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
	want := []string{"sync ..", "sync ."}
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
	rename := p.rename
	var renamed bool
	p.rename = func(o, n string) error { renamed = true; return rename(o, n) }
	// Only the sync after the rename fails: the attempt dir is also synced
	// before it, when raw/ is created, and that one must stay out of this test.
	p.syncDir = func(d string) error {
		if d == opts.AttemptDir && renamed {
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
	opts := attemptTestOptions(t, "http://127.0.0.1:1", fixtureSmokeTrace)
	opts.goos = "windows"
	var stderr bytes.Buffer
	if code := runReplayAttempt(context.Background(), opts, &stderr); code != 1 || !strings.Contains(stderr.String(), "windows") {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	if _, err := os.Stat(opts.AttemptDir); !os.IsNotExist(err) {
		t.Fatalf("attempt dir created on windows: %v", err)
	}
}

// Injection traces run through -calibrate-capture; attempt.json v1 has no
// injection replay kind, so loadAttemptInputs refuses them (#452).
func TestReplayAttemptRefusesInjectionTrace(t *testing.T) {
	srv := newCompatServer(t)
	path := filepath.Join(t.TempDir(), "inj.json")
	data, err := json.Marshal(validInjectionTrace())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, rec, stderr := runAttemptStderr(t, context.Background(), attemptTestOptions(t, srv.URL, path))
	if code != 0 {
		t.Fatalf("exit %d; an input failure still publishes its terminal", code)
	}
	assertAttemptError(t, rec, attemptCodeInput, attemptStageInput, 0)
	if len(rec.Requests) != 0 || len(srv.Received()) != 0 {
		t.Fatalf("requests %v; nothing may reach the wire", rec.Requests)
	}
	if want := "llm-bench: replay-attempt: injection traces are not supported by -replay-attempt; use -calibrate-capture\n"; stderr != want {
		t.Fatalf("stderr %q, want %q", stderr, want)
	}
}

// -candidate-session-id reaches the wire on -replay-attempt exactly as on
// runOne and -calibrate-capture; empty sends no header at all.
func TestReplayAttempt_SessionIDReachesHeader(t *testing.T) {
	for _, id := range []string{"llmb-452-test", ""} {
		t.Run("id="+id, func(t *testing.T) {
			var present bool
			var value string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, present = r.Header["X-Opencode-Session"]
				value = r.Header.Get("x-opencode-session")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(compatText("ok", "")))
			}))
			t.Cleanup(srv.Close)
			opts := attemptTestOptions(t, srv.URL, fixtureSmokeTrace)
			opts.SessionID = id
			if code, rec := runAttempt(t, context.Background(), opts); code != 0 || rec.Status != attemptStatusCompleted {
				t.Fatalf("exit %d status %q error %+v", code, rec.Status, rec.Error)
			}
			if present != (id != "") || value != id {
				t.Fatalf("x-opencode-session present=%v value=%q; want present=%v value=%q", present, value, id != "", id)
			}
		})
	}
}
