// provider/ollama_think_wire_test.go

package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// captureThink decodes the raw /api/chat body and returns the "think" field:
// present true, present false, or absent (nil). Called from the handler
// goroutine, so failures use Errorf rather than Fatalf.
func captureThink(t *testing.T, body io.Reader) *bool {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		t.Errorf("unmarshal request: %v", err)
		return nil
	}
	tv, ok := raw["think"]
	if !ok {
		return nil
	}
	var b bool
	if err := json.Unmarshal(tv, &b); err != nil {
		t.Errorf("think field not a bool: %s", tv)
		return nil
	}
	return &b
}

func assertThinkField(t *testing.T, got, want *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("think field present (%v), want absent", *got)
	case want != nil && got == nil:
		t.Fatalf("think field absent, want %v", *want)
	case want != nil && got != nil && *got != *want:
		t.Fatalf("think = %v, want %v", *got, *want)
	}
}

func TestOllamaChatWiresThink(t *testing.T) {
	tests := []struct {
		name string
		opts ModelOptions
		want *bool // nil => field must be absent
	}{
		{"unset options omit think", ModelOptions{}, nil},
		{"think true sent", ModelOptions{Think: Ptr(true)}, Ptr(true)},
		{"think false sent", ModelOptions{Think: Ptr(false)}, Ptr(false)},
		{"effort alone implies true", ModelOptions{ThinkEffort: "high"}, Ptr(true)},
		{"explicit false wins over effort", ModelOptions{Think: Ptr(false), ThinkEffort: "high"}, Ptr(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = captureThink(t, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
			}))
			defer srv.Close()

			p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
			_, err := p.Chat(context.Background(), ChatRequest{
				Model:    "m",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
				Options:  tt.opts,
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			assertThinkField(t, got, tt.want)
		})
	}
}

func TestOllamaChatStreamWiresThink(t *testing.T) {
	var got *bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = captureThink(t, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}` + "\n"))
	}))
	defer srv.Close()

	p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Options:  ModelOptions{Think: Ptr(true)},
	}, func(ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	assertThinkField(t, got, Ptr(true))
}

// TestOllamaThinkEffortActivatesToggleParser proves the per-request
// ParseThinkMode override reaches the chat parser and that an effort-only
// request activates a ThinkToggle parser, while an explicit Think=false
// deactivates it even when an effort hint is present.
func TestOllamaThinkEffortActivatesToggleParser(t *testing.T) {
	newToggleServer := func(stream bool) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"<think>why</think>"},"done":false}` + "\n"))
				_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"answer"},"done":true}` + "\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"<think>why</think>answer"},"done":true}`))
		}))
	}

	toggle := ThinkToggle

	t.Run("chat effort-only extracts thinking", func(t *testing.T) {
		srv := newToggleServer(false)
		defer srv.Close()

		p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
		resp, err := p.Chat(context.Background(), ChatRequest{
			Model:          "m",
			Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
			Options:        ModelOptions{ThinkEffort: "high"},
			ParseThinkMode: &toggle,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Thinking != "why" {
			t.Errorf("Thinking = %q, want %q", resp.Thinking, "why")
		}
		if resp.Content != "answer" {
			t.Errorf("Content = %q, want %q", resp.Content, "answer")
		}
	})

	t.Run("chat explicit false passes tags through", func(t *testing.T) {
		srv := newToggleServer(false)
		defer srv.Close()

		p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
		resp, err := p.Chat(context.Background(), ChatRequest{
			Model:          "m",
			Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
			Options:        ModelOptions{Think: Ptr(false), ThinkEffort: "high"},
			ParseThinkMode: &toggle,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Thinking != "" {
			t.Errorf("Thinking = %q, want empty", resp.Thinking)
		}
		if resp.Content != "<think>why</think>answer" {
			t.Errorf("Content = %q, want tags passed through", resp.Content)
		}
	})

	t.Run("stream effort-only extracts thinking", func(t *testing.T) {
		srv := newToggleServer(true)
		defer srv.Close()

		p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
		var thinking, content string
		err := p.ChatStream(context.Background(), ChatRequest{
			Model:          "m",
			Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
			Options:        ModelOptions{ThinkEffort: "high"},
			ParseThinkMode: &toggle,
		}, func(resp ChatResponse) error {
			thinking += resp.Thinking
			content += resp.Content
			return nil
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		if thinking != "why" {
			t.Errorf("Thinking = %q, want %q", thinking, "why")
		}
		if content != "answer" {
			t.Errorf("Content = %q, want %q", content, "answer")
		}
	})
}
