package compat

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSSEWriter_EventAndDone verifies the basic event -> DONE sequence lands
// on the wire with the expected "data: ..." framing and the SSE content-type
// header is set. This exercises the lazy-start path (headers written before
// WriteHeader, status committed on first event).
func TestSSEWriter_EventAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sw.writeEvent(map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("writeEvent: %v", err)
	}
	if err := sw.writeDone(); err != nil {
		t.Fatalf("writeDone: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data: {"hello":"world"}`) {
		t.Errorf("missing data line: %q", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("missing DONE sentinel: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", cc)
	}
}

// TestSSEWriter_Comment exercises the keepalive-comment path. The helper is
// not wired into the streaming chat handler yet (Task 17 territory), but it
// is exported to the test package so the code path stays live and the "data:"
// vs ": " framing difference is asserted here rather than regressing silently.
func TestSSEWriter_Comment(t *testing.T) {
	rec := httptest.NewRecorder()
	sw, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := sw.writeComment("keepalive"); err != nil {
		t.Fatalf("writeComment: %v", err)
	}
	if !strings.Contains(rec.Body.String(), ": keepalive\n\n") {
		t.Errorf("missing comment: %q", rec.Body.String())
	}
}
