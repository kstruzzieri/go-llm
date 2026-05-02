package analysis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func newMockChatServer(t *testing.T, wantContent string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		resp := ollama.ChatResponse{
			Model:   req.Model,
			Message: ollama.ChatMessage{Role: "assistant", Content: wantContent},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestNewCodeReviewer(t *testing.T) {
	client := ollama.NewClient()

	tests := []struct {
		name      string
		client    *ollama.Client
		model     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid",
			client:  client,
			model:   "test-model",
			wantErr: false,
		},
		{
			name:      "nil client",
			client:    nil,
			model:     "test-model",
			wantErr:   true,
			errSubstr: "client is required",
		},
		{
			name:      "empty model",
			client:    client,
			model:     "",
			wantErr:   true,
			errSubstr: "model is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr, err := NewCodeReviewer(tt.client, nil, tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cr == nil {
				t.Fatal("expected non-nil CodeReviewer")
			}
		})
	}
}

func TestReview(t *testing.T) {
	const reviewResponse = "Code looks good. Consider adding error handling."
	srv := newMockChatServer(t, reviewResponse)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	cr, err := NewCodeReviewer(client, nil, "test-model")
	if err != nil {
		t.Fatalf("NewCodeReviewer() error: %v", err)
	}

	result, err := cr.Review(context.Background(), "func main() { fmt.Println(\"hello\") }")
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}
	if result != reviewResponse {
		t.Errorf("expected %q, got %q", reviewResponse, result)
	}
}

func TestReviewWithOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Verify the prompt includes language and focus
		userMsg := req.Messages[len(req.Messages)-1].Content
		if !strings.Contains(userMsg, "language: Go") {
			t.Error("expected prompt to contain language: Go")
		}
		if !strings.Contains(userMsg, "focus on security") {
			t.Error("expected prompt to contain focus on security")
		}

		resp := ollama.ChatResponse{
			Model:   req.Model,
			Message: ollama.ChatMessage{Role: "assistant", Content: "Security review complete."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	cr, err := NewCodeReviewer(client, nil, "test-model")
	if err != nil {
		t.Fatalf("NewCodeReviewer() error: %v", err)
	}

	result, err := cr.Review(context.Background(), "func main() {}",
		WithLanguage("Go"),
		WithFocus("security"),
	)
	if err != nil {
		t.Fatalf("Review() error: %v", err)
	}
	if result != "Security review complete." {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestReviewEmptyCode(t *testing.T) {
	client := ollama.NewClient()
	cr, err := NewCodeReviewer(client, nil, "test-model")
	if err != nil {
		t.Fatalf("NewCodeReviewer() error: %v", err)
	}

	_, err = cr.Review(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty code")
	}
	if !strings.Contains(err.Error(), "code is required") {
		t.Errorf("error %q should mention code is required", err.Error())
	}
}

func TestReviewServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	cr, err := NewCodeReviewer(client, nil, "test-model")
	if err != nil {
		t.Fatalf("NewCodeReviewer() error: %v", err)
	}

	_, err = cr.Review(context.Background(), "some code")
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "analysis: review code:") {
		t.Errorf("error %q should have analysis: prefix", err.Error())
	}
}

func TestReviewContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant", Content: "ok"},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	cr, err := NewCodeReviewer(client, nil, "test-model")
	if err != nil {
		t.Fatalf("NewCodeReviewer() error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = cr.Review(ctx, "some code")
	if err == nil {
		t.Fatal("expected error for canceled context")
	}
}

func TestNewCodeReviewerWithChat_AcceptsEmptyModel(t *testing.T) {
	called := false
	chat := func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		called = true
		if useCase != "code-review" {
			t.Errorf("useCase = %q, want \"code-review\"", useCase)
		}
		if req.Model != "" {
			t.Errorf("Model = %q, want empty (deferred to chat impl)", req.Model)
		}
		return &provider.ChatResponse{Content: "looks good", Done: true}, nil
	}
	cr, err := NewCodeReviewerWithChat(chat, nil, "") // empty model OK
	if err != nil {
		t.Fatalf("NewCodeReviewerWithChat: %v", err)
	}
	got, err := cr.Review(context.Background(), "package main")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if got != "looks good" {
		t.Errorf("Review content = %q, want \"looks good\"", got)
	}
	if !called {
		t.Error("ChatFunc was not invoked")
	}
}

func TestNewCodeReviewer_StillRequiresModel(t *testing.T) {
	_, err := NewCodeReviewer(&ollama.Client{}, nil, "")
	if err == nil {
		t.Fatal("expected error for empty model in compat shim")
	}
}

func TestNewCodeReviewerWithChat_RequiresChat(t *testing.T) {
	_, err := NewCodeReviewerWithChat(nil, nil, "some-model")
	if err == nil {
		t.Fatal("expected error for nil chat func")
	}
}
