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
