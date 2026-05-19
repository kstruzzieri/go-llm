package openaicompat

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// sseReader iterates one OpenAI-shape SSE event at a time. The wire format is
// "data: <json>\n\n" frames terminated by the literal "data: [DONE]\n\n"
// sentinel. Comments (": keepalive") and non-data fields (event:, id:, retry:)
// are tolerated and skipped so we don't choke on proxy keepalives.
//
// The reader is single-use: callers MUST Close the underlying body. Errors
// other than io.EOF are returned from Next(); io.EOF and the [DONE] sentinel
// both produce errStreamDone, which callers should treat as normal termination.
type sseReader struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
}

// errStreamDone is returned by sseReader.Next when the stream ends, either
// via OpenAI's [DONE] sentinel or via io.EOF on the underlying connection.
// Both are NORMAL termination signals — callers should not surface this
// as an error to upstream code. Sentinel value (not a wrapped error) so
// errors.Is checks are cheap.
var errStreamDone = errors.New("openaicompat: sse stream done")

// newSSEReader wraps body in an sseReader. The scanner uses a generous
// buffer (1 MiB) because individual chat-completion chunks can include
// long tool-call argument payloads; the default 64 KiB would error on
// those.
func newSSEReader(body io.ReadCloser) *sseReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &sseReader{
		body:    body,
		scanner: scanner,
	}
}

// Next returns the next data: payload as raw JSON bytes. It returns
// errStreamDone when the [DONE] sentinel is reached or the underlying
// connection EOFs. Other read errors are wrapped and returned verbatim.
//
// Per the SSE spec, an event is terminated by a blank line — but for the
// OpenAI dialect, each event is exactly one "data: <json>" line followed
// by a blank line, so we treat each data: line as a complete event and
// skip the blank.
func (r *sseReader) Next() ([]byte, error) {
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		// Skip blank lines (event separators) and SSE comments (":...").
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		// Only data: lines carry the JSON payload. Skip event:/id:/retry:.
		const dataPrefix = "data:"
		if len(line) < len(dataPrefix) || string(line[:len(dataPrefix)]) != dataPrefix {
			continue
		}
		payload := line[len(dataPrefix):]
		// Trim a single leading space per the SSE spec ("data: <text>" vs "data:<text>").
		if len(payload) > 0 && payload[0] == ' ' {
			payload = payload[1:]
		}
		if string(payload) == "[DONE]" {
			return nil, errStreamDone
		}
		// Copy: scanner.Bytes() may be reused on the next call.
		out := make([]byte, len(payload))
		copy(out, payload)
		return out, nil
	}
	if err := r.scanner.Err(); err != nil {
		return nil, fmt.Errorf("openaicompat: sse read: %w", err)
	}
	return nil, errStreamDone
}

// Close closes the underlying response body. Safe to call multiple times.
func (r *sseReader) Close() error {
	return r.body.Close()
}
