package compat

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestFeedback_AcceptsKnownID(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()
	srv.completionStore.put(CompletionRecord{ID: "cmpl_abc", Provider: "ollama", Model: "qwen3:8b"})

	body := FeedbackRequest{CompletionID: "cmpl_abc", Action: "accepted"}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", bytes.NewReader(raw)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp FeedbackResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if !resp.Recorded {
		t.Error("recorded=false for valid id")
	}
}

func TestFeedback_UnknownID404(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()
	body := FeedbackRequest{CompletionID: "nope", Action: "accepted"}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", bytes.NewReader(raw)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestFeedback_InvalidAction(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()
	srv.completionStore.put(CompletionRecord{ID: "cmpl_abc"})
	body := FeedbackRequest{CompletionID: "cmpl_abc", Action: "weird"}
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", bytes.NewReader(raw)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

func TestFeedback_SecondCallReturns404(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()
	srv.completionStore.put(CompletionRecord{ID: "cmpl_once", Provider: "ollama", Model: "qwen3:8b"})

	body := FeedbackRequest{CompletionID: "cmpl_once", Action: "accepted"}
	raw, _ := json.Marshal(body)

	rec1 := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", bytes.NewReader(raw)))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first call: status=%d want 200 body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", bytes.NewReader(raw)))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("second call: status=%d want 404 body=%s", rec2.Code, rec2.Body.String())
	}
	if !bytes.Contains(rec2.Body.Bytes(), []byte("already consumed")) {
		t.Errorf("expected 'already consumed' in error message, got %s", rec2.Body.String())
	}
}

func TestFeedback_EmptyBodyReturns400(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/completions/feedback", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("empty_body")) {
		t.Errorf("expected empty_body error code, got %s", rec.Body.String())
	}
}

func TestFeedback_BodyTooLargeReturns413(t *testing.T) {
	srv, teardown := newTestServer(t, &mockProvider{name: "ollama", caps: provider.CapGenerate})
	defer teardown()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/completions/feedback",
		oversizedJSONStringReader(
			`{"completion_id":"`,
			maxFeedbackRequestBodyBytes+1,
			`","action":"accepted"}`,
		))
	req.Header.Set("Content-Type", "application/json")
	srv.buildHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("body_too_large")) {
		t.Errorf("expected body_too_large error code, got %s", rec.Body.String())
	}
}
