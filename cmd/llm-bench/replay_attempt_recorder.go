package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// attemptCorrelationHeader carries <attempt-id>/<ordinal> on every
// -replay-attempt request. The golem-lab gateway records it verbatim (at
// most 256 bytes) and never trusts it for attribution (#687).
const attemptCorrelationHeader = "X-Client-Correlation"

// maxAttemptResponseBytes bounds ONE response body, so a runaway server cannot
// feed a single read without limit for the length of -timeout. A 2xx body past
// it is response_decode; a non-2xx one keeps http_status, with the truncated
// body kept as evidence. It is not an aggregate cap: every request and
// response body stays in memory until publication (D2), and request k+1
// carries every earlier reply. -timeout bounds the run, and the caller is a
// trusted local harness, so there is no total-bytes limit.
const maxAttemptResponseBytes = 64 << 20

// recordedRequest is one logical RoundTrip of a -replay-attempt. Failure
// facts are frozen when they happen and never re-derived from live context
// state. A transport-internal resend (HTTP/1 nothing-written, HTTP/2
// REFUSED_STREAM or GOAWAY) stays inside one row, so one row can match more
// than one gateway receipt.
type recordedRequest struct {
	Ordinal          int
	Correlation      string
	StartedAt        time.Time
	HeadersAt        time.Time // zero when no response arrived
	EndedAt          time.Time
	HTTPStatus       int // 0 when no response arrived
	RequestBody      []byte
	ResponseBody     []byte
	ResponseComplete bool
	Usage            *attemptUsage // nil when the body carries no parseable usage object
	TransportErr     error         // no response arrived
	ReadErr          error         // the body read failed
	Rejected         string        // recorder rejection code: response_decode or empty_response
	CancelCause      error         // the request context's cause, when it was done at the failure
}

// attemptUsage is provider-reported usage with presence kept: an absent
// field is nil, never 0.
type attemptUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	ReasoningTokens  *int `json:"reasoning_tokens"`
}

// attemptRecorder is the -replay-attempt evidence seam: an http.RoundTripper
// that owns what the wire did. Requests in one attempt are sequential; the
// mutex only guards the row slice.
type attemptRecorder struct {
	base      http.RoundTripper
	attemptID string
	maxBody   int64

	mu   sync.Mutex
	rows []*recordedRequest
}

func newAttemptRecorder(base http.RoundTripper, attemptID string, maxBody int64) *attemptRecorder {
	return &attemptRecorder{base: base, attemptID: attemptID, maxBody: maxBody}
}

// newAttemptBaseTransport clones http.DefaultTransport with compression off.
// Go only sends Accept-Encoding: gzip, and only then decodes transparently,
// when compression is on; with it off the recorded bytes are the wire bytes
// the gateway hashes (spec amendment A2).
func newAttemptBaseTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	return t
}

// Rows returns the recorded requests in order.
func (r *attemptRecorder) Rows() []*recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*recordedRequest(nil), r.rows...)
}

// RoundTrip snapshots the request body, tags the request with its
// correlation value, sends it, and reads the whole response before anyone
// parses it. A 2xx body whose outer envelope is invalid never reaches the
// provider: the replay stops at that request.
func (r *attemptRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("replay-attempt: snapshot request body: %w", err)
		}
	}
	r.mu.Lock()
	row := &recordedRequest{Ordinal: len(r.rows) + 1, RequestBody: body}
	row.Correlation = r.attemptID + "/" + strconv.Itoa(row.Ordinal)
	r.rows = append(r.rows, row)
	r.mu.Unlock()

	out := req.Clone(req.Context())
	if req.Body != nil {
		// GetBody is how the transport re-sends after a connection loss;
		// it must replay the recorded snapshot, not the caller's source.
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		out.ContentLength = int64(len(body))
	}
	out.Header.Set(attemptCorrelationHeader, row.Correlation)

	row.StartedAt = time.Now()
	resp, err := r.base.RoundTrip(out)
	if err != nil {
		row.EndedAt = time.Now()
		row.TransportErr = err
		row.CancelCause = doneCause(req.Context())
		return nil, err
	}
	row.HeadersAt = time.Now()
	row.HTTPStatus = resp.StatusCode
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, r.maxBody+1))
	var readCause error
	if readErr != nil {
		// Freeze the cause at the failed read, before Close or parsing can
		// run past a deadline and relabel an I/O failure as cancellation.
		readCause = doneCause(req.Context())
	}
	_ = resp.Body.Close()
	row.EndedAt = time.Now()
	oversize := int64(len(raw)) > r.maxBody
	if oversize {
		raw = raw[:r.maxBody]
	}
	row.ResponseBody = raw
	row.ResponseComplete = readErr == nil && !oversize
	row.Usage = parseAttemptUsage(raw)
	switch {
	case readErr != nil:
		row.ReadErr, row.CancelCause = readErr, readCause
		return nil, fmt.Errorf("replay-attempt: read response body: %w", readErr)
	case oversize:
		return nil, reject(row, attemptCodeResponseDecode, fmt.Sprintf("response body exceeds %d bytes", r.maxBody))
	}
	if resp.StatusCode/100 == 2 {
		if code, reason := validateChatEnvelope(raw); code != "" {
			return nil, reject(row, code, reason)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	resp.ContentLength = int64(len(raw))
	return resp, nil
}

// reject records why the recorder refused a response; the provider never
// sees that response.
func reject(row *recordedRequest, code, reason string) error {
	row.Rejected = code
	return errors.New("replay-attempt: " + reason)
}

// doneCause freezes a request context's cancellation cause at the moment of
// a failure: nil while the context is still live.
func doneCause(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return context.Cause(ctx)
}

// validateChatEnvelope checks a 2xx chat body's outer shape only: exactly
// one JSON object, no non-null error member, and a choices array with at
// least one element. Tool-call arguments are left to the provider, so a
// genuine argument failure still classifies as argument_decode.
func validateChatEnvelope(raw []byte) (code, reason string) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var top map[string]json.RawMessage
	if err := dec.Decode(&top); err != nil || top == nil {
		return attemptCodeResponseDecode, "2xx body is not a JSON object"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return attemptCodeResponseDecode, "2xx body has data after its JSON value"
	}
	if e, ok := top["error"]; ok && !bytes.Equal(bytes.TrimSpace(e), []byte("null")) {
		return attemptCodeResponseDecode, "2xx body carries an error member"
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(top["choices"], &choices); err != nil || choices == nil {
		return attemptCodeResponseDecode, "2xx body has no choices array"
	}
	if len(choices) == 0 {
		return attemptCodeEmptyResponse, "2xx body has zero choices"
	}
	return "", ""
}

// parseAttemptUsage reads usage from raw response bytes with presence kept,
// independent of the provider's zero-filling decode. A body that does not
// parse, or carries no usage object, yields nil.
func parseAttemptUsage(raw []byte) *attemptUsage {
	var env struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			Details          *struct {
				ReasoningTokens *int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Usage == nil {
		return nil
	}
	u := &attemptUsage{PromptTokens: env.Usage.PromptTokens, CompletionTokens: env.Usage.CompletionTokens}
	if env.Usage.Details != nil {
		u.ReasoningTokens = env.Usage.Details.ReasoningTokens
	}
	return u
}
