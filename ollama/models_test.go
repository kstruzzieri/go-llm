package ollama

import (
	"context"
	"encoding/json"
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
