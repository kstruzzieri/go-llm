package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}

		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming chat")
		}
		if req.Model != "test-model" {
			t.Errorf("expected model %q, got %q", "test-model", req.Model)
		}

		resp := ChatResponse{
			Model:   "test-model",
			Message: ChatMessage{Role: "assistant", Content: "Hello!"},
			Done:    true,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Message.Content != "Hello!" {
		t.Errorf("expected content %q, got %q", "Hello!", resp.Message.Content)
	}
	if !resp.Done {
		t.Error("expected done=true")
	}
}

func TestChatStream(t *testing.T) {
	chunks := []ChatResponse{
		{Model: "test-model", Message: ChatMessage{Role: "assistant", Content: "Hel"}, Done: false},
		{Model: "test-model", Message: ChatMessage{Role: "assistant", Content: "lo!"}, Done: false},
		{Model: "test-model", Message: ChatMessage{Role: "assistant", Content: ""}, Done: true, EvalCount: 2},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true for streaming chat")
		}

		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	var received []ChatResponse
	err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(resp ChatResponse) error {
		received = append(received, resp)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}
	if len(received) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(received))
	}
	if received[0].Message.Content != "Hel" {
		t.Errorf("chunk 0 content = %q, want %q", received[0].Message.Content, "Hel")
	}
	if !received[2].Done {
		t.Error("last chunk should have done=true")
	}
}

func TestChatStreamCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := ChatResponse{
			Model:   "test-model",
			Message: ChatMessage{Role: "assistant", Content: "test"},
			Done:    false,
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	callbackErr := fmt.Errorf("stop please")
	err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(resp ChatResponse) error {
		return callbackErr
	})
	if err != callbackErr {
		t.Errorf("expected callback error, got: %v", err)
	}
}

func TestChatServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("model not found"))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.Chat(context.Background(), ChatRequest{
		Model:    "missing-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}
