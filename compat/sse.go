// Package compat — sse.go wires a minimal Server-Sent Events writer used by
// the streaming branch of POST /v1/chat/completions. The writer serializes
// one event per call as a "data: <json>\n\n" block and terminates the stream
// with "data: [DONE]\n\n", matching OpenAI's chat.completion.chunk wire
// format so stock OpenAI SDKs consume it without customization.
package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter serializes one SSE event per call. Each event is a JSON payload
// on a single "data:" line, followed by a blank line. A trailing "data: [DONE]"
// event signals the end of the stream, matching OpenAI's wire format.
//
// Important: this writer is lazy-start. It does not commit the HTTP status
// code until the first event or keepalive comment is written. This is what
// allows the streaming chat handler to fall back to a normal JSON error
// envelope when routing, admission, or the provider fails before any chunk
// has been delivered to the client.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

// newSSEWriter prepares SSE response headers and returns a writer bound to w.
// The HTTP status is NOT yet committed — subsequent writeEvent / writeComment
// / writeDone calls are the first bytes of the response body.
//
// Returns an error when w does not implement http.Flusher, which in net/http
// only happens for exotic response writers (e.g. gzip wrappers that do not
// forward Flush). In that case the caller should fall back to a JSON error.
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("compat: response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	return &sseWriter{w: w, flusher: flusher}, nil
}

// writeEvent marshals payload as JSON and emits it as a single "data: <json>"
// event, flushing afterward so intermediary proxies do not buffer. On the
// first call it also commits the HTTP 200 status.
func (s *sseWriter) writeEvent(payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !s.started {
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// writeComment emits an SSE comment line (": <text>\n\n") and flushes. SSE
// comments are opaque to OpenAI SDKs but keep the connection alive across
// long provider stalls. This helper is intended for future keepalive wiring
// (Task 17 or later); the current streaming handler does not call it yet.
func (s *sseWriter) writeComment(text string) error {
	if !s.started {
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// writeDone emits the "data: [DONE]\n\n" terminator that OpenAI SDKs
// recognize as the end of a chat.completion.chunk stream. It does not
// commit the status code — callers must have already written at least one
// event (otherwise the stream is empty, which OpenAI SDKs tolerate as long
// as the DONE sentinel is present).
func (s *sseWriter) writeDone() error {
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
