package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchSlotCapacity(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    int
		wantErr bool
	}{
		{
			name: "total_slots captured",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots": 4}`))
			},
			want: 4,
		},
		{
			name: "non-200 is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: true,
		},
		{
			name: "malformed JSON is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots"`))
			},
			wantErr: true,
		},
		{
			name: "missing total_slots is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model_path": "/x.gguf"}`))
			},
			wantErr: true,
		},
		{
			name: "zero total_slots is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots": 0}`))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			got, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, "m")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got capacity %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("capacity = %d, want %d", got, tt.want)
			}
		})
	}
}

// The model-qualified query is load-bearing for llama-swap. The model name
// contains characters that break an unescaped query ("&", "="): if
// QueryEscape is dropped, the decoded param truncates at "&" and this test
// fails — a ":"-or-"/"-only name would decode identically and let the
// mutation survive.
func TestFetchSlotCapacityModelQualifiedQuery(t *testing.T) {
	const model = "team/qwen3:8b&rev=q4"
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotModel = r.URL.Query().Get("model")
		_, _ = w.Write([]byte(`{"total_slots": 2}`))
	}))
	defer srv.Close()

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, model); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/props" {
		t.Fatalf("path = %q, want /props", gotPath)
	}
	if gotModel != model {
		t.Fatalf("model param = %q, want %q", gotModel, model)
	}
}

// Probes must authenticate exactly like the provider's own client: llama-swap
// v235 places /props behind its authenticated model-dispatch chain, so a
// keyless probe against a keyed backend 401s and permanently fail-safes a
// working backend.
func TestFetchSlotCapacityAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"total_slots": 2}`))
	}))
	defer srv.Close()

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL, APIKey: "sk-test"}, "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("keyless probe sent Authorization %q, want no header", gotAuth)
	}
}
