package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		resp := listModelsResponse{
			Models: []listModelEntry{
				{Name: "qwen2.5:72b", Size: 42000000000},
				{Name: "nomic-embed-text", Size: 300000000},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].Name != "qwen2.5:72b" {
		t.Errorf("expected model name %q, got %q", "qwen2.5:72b", models[0].Name)
	}
	if models[1].Size != 300000000 {
		t.Errorf("expected size 300000000, got %d", models[1].Size)
	}
}

func TestShowModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		resp := showModelResponse{
			Details: modelDetails{
				ParamSize:  "72B",
				QuantLevel: "Q4_K_M",
			},
			Digest: "sha256:abc123def456",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	info, err := c.ShowModel(context.Background(), "qwen2.5:72b")
	if err != nil {
		t.Fatalf("ShowModel() error: %v", err)
	}
	if info.ParamSize != "72B" {
		t.Errorf("expected param size %q, got %q", "72B", info.ParamSize)
	}
	if info.QuantLevel != "Q4_K_M" {
		t.Errorf("expected quant level %q, got %q", "Q4_K_M", info.QuantLevel)
	}
	if info.Digest != "sha256:abc123def456" {
		t.Errorf("expected digest %q, got %q", "sha256:abc123def456", info.Digest)
	}
	// Verify new fields are zero-valued when not present (older Ollama versions)
	if info.Family != "" {
		t.Errorf("expected empty Family, got %q", info.Family)
	}
	if info.Template != "" {
		t.Errorf("expected empty Template, got %q", info.Template)
	}
	if info.Capabilities != nil {
		t.Errorf("expected nil Capabilities, got %v", info.Capabilities)
	}
}

func TestPullModel(t *testing.T) {
	updates := []pullModelResponse{
		{Status: "pulling manifest"},
		{Status: "downloading", Completed: 50, Total: 100},
		{Status: "success"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pull" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		for _, u := range updates {
			data, _ := json.Marshal(u)
			fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	var statuses []string
	err := c.PullModel(context.Background(), "test-model", func(status string, completed, total int64) {
		statuses = append(statuses, status)
	})
	if err != nil {
		t.Fatalf("PullModel() error: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("expected 3 status updates, got %d", len(statuses))
	}
	if statuses[2] != "success" {
		t.Errorf("expected final status %q, got %q", "success", statuses[2])
	}
}

func TestAvailableModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		resp := listModelsResponse{
			Models: []listModelEntry{
				{Name: "qwen3.5:27b", Size: 15000000000},
				{Name: "qwen3:8b", Size: 5000000000},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	names, err := c.AvailableModels(context.Background())
	if err != nil {
		t.Fatalf("AvailableModels() error: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}
	if names[0] != "qwen3.5:27b" {
		t.Errorf("expected name %q, got %q", "qwen3.5:27b", names[0])
	}
	if names[1] != "qwen3:8b" {
		t.Errorf("expected name %q, got %q", "qwen3:8b", names[1])
	}
}

func TestAvailableModelsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.AvailableModels(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
}

func TestListModelsServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error for 503 response")
	}
}

func TestShowModel_NewFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"details": map[string]any{
				"parameter_size":     "27B",
				"quantization_level": "Q4_K_M",
				"family":             "qwen2",
			},
			"digest":       "sha256:abc123",
			"template":     "{{ .System }}\n{{ .Prompt }}",
			"capabilities": []string{"completion", "tools"},
		})
	}))
	defer srv.Close()

	client := NewClient(WithBaseURL(srv.URL))
	info, err := client.ShowModel(context.Background(), "test-model")
	if err != nil {
		t.Fatalf("ShowModel() error: %v", err)
	}
	if info.Family != "qwen2" {
		t.Errorf("Family = %q, want %q", info.Family, "qwen2")
	}
	wantTemplate := "{{ .System }}\n{{ .Prompt }}"
	if info.Template != wantTemplate {
		t.Errorf("Template = %q, want %q", info.Template, wantTemplate)
	}
	if len(info.Capabilities) != 2 || info.Capabilities[0] != "completion" {
		t.Errorf("Capabilities = %v, want [completion tools]", info.Capabilities)
	}
}

func TestAPIError_ErrorsAs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.ShowModel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadRequest)
	}
	if apiErr.Message != "model not found" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "model not found")
	}
}

func TestChatStream_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer srv.Close()

	c := NewClient(WithBaseURL(srv.URL))
	err := c.ChatStream(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(resp ChatResponse) error { return nil })
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError from streaming path, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadRequest)
	}
	if apiErr.Message != "bad request" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "bad request")
	}
}
