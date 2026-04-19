package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateAllowsSuffixWithoutPrompt(t *testing.T) {
	var captured GenerateRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(GenerateResponse{
			Model:    "test-model",
			Response: "ok",
			Done:     true,
		})
	}))
	defer srv.Close()

	client := NewClient(WithBaseURL(srv.URL))
	resp, err := client.Generate(context.Background(), GenerateRequest{
		Model:  "test-model",
		Suffix: "after",
	})
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if resp.Response != "ok" {
		t.Fatalf("Response = %q, want %q", resp.Response, "ok")
	}
	if captured.Prompt != "" {
		t.Errorf("Prompt = %q, want empty prompt", captured.Prompt)
	}
	if captured.Suffix != "after" {
		t.Errorf("Suffix = %q, want %q", captured.Suffix, "after")
	}
}

func TestGenerateRejectsEmptyPromptAndSuffix(t *testing.T) {
	client := NewClient()
	_, err := client.Generate(context.Background(), GenerateRequest{Model: "test-model"})
	if err == nil {
		t.Fatal("Generate() error = nil, want validation error")
	}
	if err.Error() != "ollama: generate: prompt or suffix is required" {
		t.Fatalf("Generate() error = %q", err)
	}
}

func TestGenerateStreamAllowsSuffixWithoutPrompt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"model":"test-model","response":"chunk","done":true}` + "\n"))
	}))
	defer srv.Close()

	client := NewClient(WithBaseURL(srv.URL))
	var chunks int
	err := client.GenerateStream(context.Background(), GenerateRequest{
		Model:  "test-model",
		Suffix: "after",
	}, func(resp GenerateResponse) error {
		chunks++
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream() error: %v", err)
	}
	if chunks != 1 {
		t.Fatalf("chunks = %d, want 1", chunks)
	}
}

func TestGenerateStreamRejectsEmptyPromptAndSuffix(t *testing.T) {
	client := NewClient()
	err := client.GenerateStream(context.Background(), GenerateRequest{Model: "test-model"}, func(GenerateResponse) error {
		return nil
	})
	if err == nil {
		t.Fatal("GenerateStream() error = nil, want validation error")
	}
	if err.Error() != "ollama: generate stream: prompt or suffix is required" {
		t.Fatalf("GenerateStream() error = %q", err)
	}
}
