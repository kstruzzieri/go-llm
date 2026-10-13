package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newRecorderClient(rec *attemptRecorder) *http.Client {
	return &http.Client{Transport: rec, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func recorderPost(t *testing.T, ctx context.Context, c *http.Client, baseURL, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

func drain(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAttemptRecorder_SnapshotsAndCorrelates(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Body: compatText("one", "")}, compatExchange{Body: compatText("two", "")})
	rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
	c := newRecorderClient(rec)
	for _, body := range []string{`{"n":1}`, `{"n":2}`} {
		resp, err := recorderPost(t, context.Background(), c, srv.URL, body)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		drain(t, resp)
	}
	rows, got := rec.Rows(), srv.Received()
	if len(rows) != 2 || len(got) != 2 {
		t.Fatalf("rows = %d, server saw %d; want 2 and 2", len(rows), len(got))
	}
	for i, row := range rows {
		wantCorr := []string{"att-1/1", "att-1/2"}[i]
		if row.Ordinal != i+1 || row.Correlation != wantCorr || got[i].Correlation != wantCorr {
			t.Fatalf("row %d: ordinal %d, correlation %q, server saw %q; want %q", i, row.Ordinal, row.Correlation, got[i].Correlation, wantCorr)
		}
		if !bytes.Equal(row.RequestBody, got[i].Body) {
			t.Fatalf("row %d request snapshot %q != wire %q", i, row.RequestBody, got[i].Body)
		}
		if got[i].AcceptEncoding != "" {
			t.Fatalf("row %d sent Accept-Encoding %q; compression must be off so recorded bytes are wire bytes (A2)", i, got[i].AcceptEncoding)
		}
		if row.HTTPStatus != 200 || !row.ResponseComplete || row.Rejected != "" {
			t.Fatalf("row %d: status %d complete %v rejected %q", i, row.HTTPStatus, row.ResponseComplete, row.Rejected)
		}
		if row.StartedAt.IsZero() || row.HeadersAt.Before(row.StartedAt) || row.EndedAt.Before(row.HeadersAt) {
			t.Fatalf("row %d timing out of order: %v %v %v", i, row.StartedAt, row.HeadersAt, row.EndedAt)
		}
	}
	if !bytes.Equal(rows[0].ResponseBody, []byte(compatText("one", ""))) {
		t.Fatalf("response bytes %q", rows[0].ResponseBody)
	}
}

func TestAttemptRecorder_ReadsWholeBodyAndRejectsTrailingData(t *testing.T) {
	body := compatText("ok", "") + "  trailing"
	srv := newCompatServer(t, compatExchange{Body: body})
	rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
	if _, err := recorderPost(t, context.Background(), newRecorderClient(rec), srv.URL, `{}`); err == nil {
		t.Fatal("a 2xx body with trailing data reached the caller")
	}
	row := rec.Rows()[0]
	if string(row.ResponseBody) != body || !row.ResponseComplete || row.Rejected != attemptCodeResponseDecode {
		t.Fatalf("row body %q complete %v rejected %q; want the full body recorded", row.ResponseBody, row.ResponseComplete, row.Rejected)
	}
}

func TestAttemptRecorder_ResendsTheSnapshotViaGetBody(t *testing.T) {
	var sawBody, sawGetBody string
	base := attemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		sawBody = string(b)
		rc, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		g, _ := io.ReadAll(rc)
		sawGetBody = string(g)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(compatText("ok", ""))), Request: r}, nil
	})
	rec := newAttemptRecorder(base, "att-1", maxAttemptResponseBytes)
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/chat/completions", strings.NewReader("A"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("B")), nil }
	resp, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	drain(t, resp)
	if sawBody != "A" || sawGetBody != "A" {
		t.Fatalf("transport saw body %q and GetBody %q; a resend must send the recorded snapshot %q", sawBody, sawGetBody, "A")
	}
}

// An upgraded body is a raw connection. Even a context deadline does not
// bound its Read, so rejecting the status must close it without reading.
func TestAttemptRecorder_RejectsUpgradeWithoutReading(t *testing.T) {
	body := &upgradeRecorderBody{}
	base := attemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: http.Header{}, Body: body, Request: r}, nil
	})
	rec := newAttemptRecorder(base, "att-1", maxAttemptResponseBytes)
	req, _ := http.NewRequest(http.MethodPost, "http://example.invalid/v1/chat/completions", strings.NewReader("{}"))
	resp, err := rec.RoundTrip(req)
	if body.read || !body.closed {
		t.Fatalf("upgrade body read=%v closed=%v; want closed without reading", body.read, body.closed)
	}
	if err == nil || resp != nil {
		t.Fatalf("RoundTrip = (%v, %v); want upgrade rejected", resp, err)
	}
	row := rec.Rows()[0]
	if row.HTTPStatus != 101 || len(row.ResponseBody) != 0 || row.ResponseComplete || row.ReadErr != nil || row.CancelCause != nil {
		t.Fatalf("upgrade row %+v; want status 101 and zero incomplete response bytes", row)
	}
	if row.HeadersAt.IsZero() || row.EndedAt.Before(row.HeadersAt) {
		t.Fatalf("upgrade timing headers=%v ended=%v", row.HeadersAt, row.EndedAt)
	}
	if failure := classifyAttempt(err, rec.Rows(), nil); failure == nil || failure.Code != attemptCodeHTTPStatus {
		t.Fatalf("upgrade failure %+v; want http_status", failure)
	}
}

type upgradeRecorderBody struct{ read, closed bool }

func (b *upgradeRecorderBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (b *upgradeRecorderBody) Close() error             { b.closed = true; return nil }

func TestValidateChatEnvelope(t *testing.T) {
	choice := `[{"index":0,"message":{"role":"assistant","content":"x"}}]`
	cases := []struct{ name, body, want string }{
		{"valid", `{"choices":` + choice + `}`, ""},
		{"trailing whitespace", `{"choices":` + choice + "}\n\t ", ""},
		{"null error member accepted (A7)", `{"error":null,"choices":` + choice + `}`, ""},
		{"trailing value", `{"choices":` + choice + `}{}`, attemptCodeResponseDecode},
		{"missing object close", `{"choices":` + choice, attemptCodeResponseDecode},
		{"trailing comma", `{"choices":` + choice + `,}`, attemptCodeResponseDecode},
		{"missing member separator", `{"choices":` + choice + ` "usage":{}}`, attemptCodeResponseDecode},
		{"array", `[]`, attemptCodeResponseDecode},
		{"null", `null`, attemptCodeResponseDecode},
		{"not json", `not json`, attemptCodeResponseDecode},
		{"error envelope beside choices", `{"error":{"message":"x"},"choices":` + choice + `}`, attemptCodeResponseDecode},
		{"missing choices", `{}`, attemptCodeResponseDecode},
		{"null choices", `{"choices":null}`, attemptCodeResponseDecode},
		{"zero choices", `{"choices":[]}`, attemptCodeEmptyResponse},
		// Member names match case-insensitively, as the provider's struct
		// decode does (the end-to-end rows are in TestReplayAttempt_CaseVariantMembers).
		{"upper-case choices alone is the choices array", `{"CHOICES":` + choice + `}`, ""},
		{"escaped choices alone is the choices array", `{"cho\u0069ces":` + choice + `}`, ""},
		{"nested choices is not another top-level member", `{"choices":` + choice + `,"metadata":{"choices":[]}}`, ""},
		{"mixed-case choices alone, empty", `{"Choices":[]}`, attemptCodeEmptyResponse},
		{"case-variant duplicate choices is ambiguous", `{"choices":` + choice + `,"CHOICES":[]}`, attemptCodeResponseDecode},
		{"case-variant duplicate, empty first", `{"CHOICES":[],"choices":` + choice + `}`, attemptCodeResponseDecode},
		{"case-variant duplicate with null", `{"choices":` + choice + `,"Choices":null}`, attemptCodeResponseDecode},
		{"exact duplicate choices", `{"choices":` + choice + `,"choices":[]}`, attemptCodeResponseDecode},
		{"exact duplicate choices, empty first", `{"choices":[],"choices":` + choice + `}`, attemptCodeResponseDecode},
		{"exact duplicate choices can retain stale content", `{"choices":` + choice + `,"choices":[{"message":{}}]}`, attemptCodeResponseDecode},
		{"exact duplicate choices can retain stale content through null", `{"choices":` + choice + `,"choices":[null]}`, attemptCodeResponseDecode},
		{"escaped duplicate choices", `{"choices":` + choice + `,"cho\u0069ces":` + choice + `}`, attemptCodeResponseDecode},
		{"escaped case-variant duplicate choices", `{"choices":` + choice + `,"\u0043HOICES":` + choice + `}`, attemptCodeResponseDecode},
		{"capitalised error member beside choices", `{"Error":{"message":"x"},"choices":` + choice + `}`, attemptCodeResponseDecode},
		{"upper-case error member beside choices", `{"ERROR":"overloaded","choices":` + choice + `}`, attemptCodeResponseDecode},
		{"null capitalised error member accepted", `{"Error":null,"choices":` + choice + `}`, ""},
		{"error member and ambiguous choices", `{"error":{"m":1},"choices":[],"Choices":[]}`, attemptCodeResponseDecode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := validateChatEnvelope([]byte(tc.body)); got != tc.want {
				t.Fatalf("validateChatEnvelope(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestParseAttemptUsage(t *testing.T) {
	intp := func(n int) *int { return &n }
	cases := []struct {
		name string
		body string
		want *attemptUsage
	}{
		{"absent usage stays unknown", `{"choices":[]}`, nil},
		{"invalid json stays unknown", `{"usage":`, nil},
		{"present zero is zero, absent fields stay nil", `{"usage":{"prompt_tokens":0}}`, &attemptUsage{PromptTokens: intp(0)}},
		{"details without reasoning", `{"usage":{"prompt_tokens":3,"completion_tokens":4,"completion_tokens_details":{}}}`, &attemptUsage{PromptTokens: intp(3), CompletionTokens: intp(4)}},
		{"nested reasoning tokens", `{"usage":{"prompt_tokens":3,"completion_tokens":4,"completion_tokens_details":{"reasoning_tokens":2}}}`, &attemptUsage{PromptTokens: intp(3), CompletionTokens: intp(4), ReasoningTokens: intp(2)}},
	}
	render := func(u *attemptUsage) string {
		if u == nil {
			return "nil"
		}
		f := func(p *int) string {
			if p == nil {
				return "nil"
			}
			return strconv.Itoa(*p)
		}
		return f(u.PromptTokens) + "," + f(u.CompletionTokens) + "," + f(u.ReasoningTokens)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseAttemptUsage([]byte(tc.body)); render(got) != render(tc.want) {
				t.Fatalf("parseAttemptUsage(%s) = %s, want %s", tc.body, render(got), render(tc.want))
			}
		})
	}
}

func TestAttemptRecorder_NonSuccessBodyPassesThrough(t *testing.T) {
	srv := newCompatServer(t, compatExchange{Status: http.StatusInternalServerError, Body: `{"error":{"message":"boom"}}`})
	rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
	resp, err := recorderPost(t, context.Background(), newRecorderClient(rec), srv.URL, `{}`)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if got := string(drain(t, resp)); resp.StatusCode != 500 || got != `{"error":{"message":"boom"}}` {
		t.Fatalf("status %d body %q; want the buffered 500 body handed on", resp.StatusCode, got)
	}
	if row := rec.Rows()[0]; row.HTTPStatus != 500 || row.Rejected != "" {
		t.Fatalf("row status %d rejected %q", row.HTTPStatus, row.Rejected)
	}
}

// The cap guard is the only protection for a non-2xx body: without it a
// truncated 500 body would reach the provider's error reader with a nil
// error. A 2xx body is also rejected by envelope validation, so only the 500
// case can tell the guard from validateChatEnvelope.
func TestAttemptRecorder_CapsResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"2xx", http.StatusOK, compatText(strings.Repeat("x", 100), "")},
		{"500", http.StatusInternalServerError, `{"error":{"message":"` + strings.Repeat("x", 100) + `"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCompatServer(t, compatExchange{Status: tc.status, Body: tc.body})
			rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", 16)
			if _, err := recorderPost(t, context.Background(), newRecorderClient(rec), srv.URL, `{}`); err == nil {
				t.Fatal("an oversized body reached the caller")
			}
			row := rec.Rows()[0]
			if len(row.ResponseBody) != 16 || row.ResponseComplete || row.Rejected != attemptCodeResponseDecode || row.HTTPStatus != tc.status {
				t.Fatalf("row kept %d bytes complete=%v rejected=%q status=%d; want 16 bytes, incomplete, response_decode, status %d",
					len(row.ResponseBody), row.ResponseComplete, row.Rejected, row.HTTPStatus, tc.status)
			}
		})
	}
	// The contract says 64 MiB. The literal is deliberate: every other test
	// passes the constant through or uses a 16-byte cap, so none would notice
	// the constant moving.
	t.Run("cap is 64 MiB", func(t *testing.T) {
		if maxAttemptResponseBytes != 67108864 {
			t.Fatalf("maxAttemptResponseBytes = %d; the documented cap is 64 MiB (67108864)", maxAttemptResponseBytes)
		}
	})
	// "A body of exactly the cap is complete" (the contract's wording for
	// 64 MiB). A 500 body passes through the recorder untouched, so the
	// caller receiving all 16 bytes shows the body was not rejected.
	t.Run("exactly the cap", func(t *testing.T) {
		const body = `{"error":"1234"}`
		if len(body) != 16 {
			t.Fatalf("fixture body is %d bytes; want the cap, 16", len(body))
		}
		srv := newCompatServer(t, compatExchange{Status: http.StatusInternalServerError, Body: body})
		rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", 16)
		resp, err := recorderPost(t, context.Background(), newRecorderClient(rec), srv.URL, `{}`)
		if err != nil {
			t.Fatalf("a body of exactly the cap was refused: %v", err)
		}
		if got := string(drain(t, resp)); got != body {
			t.Fatalf("caller received %q; want the whole %d-byte body", got, len(body))
		}
		row := rec.Rows()[0]
		if string(row.ResponseBody) != body || !row.ResponseComplete || row.Rejected != "" || row.HTTPStatus != http.StatusInternalServerError {
			t.Fatalf("row kept %q complete=%v rejected=%q status=%d; want the whole body, complete, not rejected, status 500",
				row.ResponseBody, row.ResponseComplete, row.Rejected, row.HTTPStatus)
		}
	})
}

func TestAttemptRecorder_TransportErrorIsNotCancellation(t *testing.T) {
	srv := newCompatServer(t)
	url := srv.URL
	srv.Close()
	rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
	if _, err := recorderPost(t, context.Background(), newRecorderClient(rec), url, `{}`); err == nil {
		t.Fatal("want a transport error")
	}
	row := rec.Rows()[0]
	if row.TransportErr == nil || row.CancelCause != nil || !row.HeadersAt.IsZero() {
		t.Fatalf("row transportErr %v cancelCause %v headersAt %v", row.TransportErr, row.CancelCause, row.HeadersAt)
	}
}

func TestAttemptRecorder_FreezesCancelCause(t *testing.T) {
	t.Run("before headers", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Delay: 5 * time.Second})
		ctx, cancel := context.WithTimeoutCause(context.Background(), 100*time.Millisecond, errAttemptTimeout)
		defer cancel()
		rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
		_, _ = recorderPost(t, ctx, newRecorderClient(rec), srv.URL, `{}`)
		row := rec.Rows()[0]
		if row.TransportErr == nil || !errors.Is(row.CancelCause, errAttemptTimeout) || !row.HeadersAt.IsZero() {
			t.Fatalf("row transportErr %v cause %v headersAt %v", row.TransportErr, row.CancelCause, row.HeadersAt)
		}
	})
	t.Run("during body read", func(t *testing.T) {
		srv := newCompatServer(t, compatExchange{Body: `{"choices":`, StallAfterBody: true})
		ctx, cancel := context.WithTimeoutCause(context.Background(), 200*time.Millisecond, errAttemptTimeout)
		defer cancel()
		rec := newAttemptRecorder(newAttemptBaseTransport(), "att-1", maxAttemptResponseBytes)
		_, _ = recorderPost(t, ctx, newRecorderClient(rec), srv.URL, `{}`)
		row := rec.Rows()[0]
		if row.ReadErr == nil || !errors.Is(row.CancelCause, errAttemptTimeout) || row.HeadersAt.IsZero() {
			t.Fatalf("row readErr %v cause %v headersAt %v", row.ReadErr, row.CancelCause, row.HeadersAt)
		}
		if string(row.ResponseBody) != `{"choices":` || row.ResponseComplete {
			t.Fatalf("partial body %q complete %v", row.ResponseBody, row.ResponseComplete)
		}
	})
}

// cancelOnCloseBody fails its read while the context is live, then cancels
// the context when closed: a cause read after Close would be the wrong one.
type cancelOnCloseBody struct{ cancel context.CancelCauseFunc }

func (b *cancelOnCloseBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *cancelOnCloseBody) Close() error             { b.cancel(errAttemptTimeout); return nil }

func TestAttemptRecorder_FreezesCauseAtTheFailedRead(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	base := attemptRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &cancelOnCloseBody{cancel: cancel}, Request: r}, nil
	})
	rec := newAttemptRecorder(base, "att-1", maxAttemptResponseBytes)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.invalid/v1/chat/completions", strings.NewReader("{}"))
	if _, err := rec.RoundTrip(req); err == nil {
		t.Fatal("want the read error")
	}
	row := rec.Rows()[0]
	if !errors.Is(row.ReadErr, io.ErrUnexpectedEOF) || row.CancelCause != nil {
		t.Fatalf("readErr %v cause %v; the read failed while the context was live, so this is connection, not cancellation", row.ReadErr, row.CancelCause)
	}
}

// The recorder's transport dials the endpoint itself: http.DefaultTransport's
// proxy-from-environment would put an unrecorded hop between llm-bench and the
// gateway. A loopback test server bypasses ProxyFromEnvironment anyway, so
// the field is the only thing a test can pin.
func TestAttemptBaseTransportDialsDirectly(t *testing.T) {
	tr := newAttemptBaseTransport()
	if tr.Proxy != nil {
		t.Fatal("newAttemptBaseTransport().Proxy is set; -replay-attempt must ignore HTTP(S)_PROXY")
	}
	if !tr.DisableCompression {
		t.Fatal("compression is on; recorded bytes would not be the wire bytes")
	}
}
