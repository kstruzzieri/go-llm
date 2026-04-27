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

func TestExplain(t *testing.T) {
	const explanation = "This function prints hello world to stdout."
	srv := newMockChatServer(t, explanation)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	result, err := Explain(context.Background(), client, "test-model", "fmt.Println(\"hello\")")
	if err != nil {
		t.Fatalf("Explain() error: %v", err)
	}
	if result != explanation {
		t.Errorf("expected %q, got %q", explanation, result)
	}
}

func TestExplainValidation(t *testing.T) {
	client := ollama.NewClient()

	tests := []struct {
		name      string
		client    *ollama.Client
		model     string
		code      string
		errSubstr string
	}{
		{
			name:      "nil client",
			client:    nil,
			model:     "test-model",
			code:      "some code",
			errSubstr: "client is required",
		},
		{
			name:      "empty model",
			client:    client,
			model:     "",
			code:      "some code",
			errSubstr: "model is required",
		},
		{
			name:      "empty code",
			client:    client,
			model:     "test-model",
			code:      "",
			errSubstr: "code is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Explain(context.Background(), tt.client, tt.model, tt.code)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
			if !strings.Contains(err.Error(), "analysis: explain:") {
				t.Errorf("error %q should have analysis: explain: prefix", err.Error())
			}
		})
	}
}

func TestExplainPromptContent(t *testing.T) {
	const code = "func add(a, b int) int { return a + b }"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Verify system message
		if req.Messages[0].Role != "system" {
			t.Errorf("first message should be system, got %q", req.Messages[0].Role)
		}

		// Verify code is in user prompt
		userMsg := req.Messages[1].Content
		if !strings.Contains(userMsg, code) {
			t.Error("expected user prompt to contain the code")
		}

		resp := ollama.ChatResponse{
			Model:   req.Model,
			Message: ollama.ChatMessage{Role: "assistant", Content: "Adds two integers."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	result, err := Explain(context.Background(), client, "test-model", code)
	if err != nil {
		t.Fatalf("Explain() error: %v", err)
	}
	if result != "Adds two integers." {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestExplainServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	_, err := Explain(context.Background(), client, "test-model", "some code")
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "analysis: explain:") {
		t.Errorf("error %q should have analysis: explain: prefix", err.Error())
	}
}

func TestExplainWithChat_AcceptsEmptyModel(t *testing.T) {
	chat := func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if useCase != "analysis" {
			t.Errorf("useCase = %q, want \"analysis\"", useCase)
		}
		return &provider.ChatResponse{Content: "this is a hello world program", Done: true}, nil
	}
	got, err := ExplainWithChat(context.Background(), chat, "", "package main")
	if err != nil {
		t.Fatalf("ExplainWithChat: %v", err)
	}
	if got == "" {
		t.Error("expected non-empty explanation")
	}
}

func TestExplain_StillRequiresModel(t *testing.T) {
	_, err := Explain(context.Background(), &ollama.Client{}, "", "package main")
	if err == nil {
		t.Fatal("expected error for empty model")
	}
}
