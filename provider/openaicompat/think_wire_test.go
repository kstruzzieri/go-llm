package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type thinkWire struct {
	effort        string
	effortPresent bool // key on the wire, even as "" or null
	kwargs        map[string]any
	kwargsPresent bool // key on the wire, even as null
}

// captureThinkWire records actual key presence, not just decoded values, so
// dropping omitempty (which emits "" / null) still fails the absent cases.
func captureThinkWire(t *testing.T, body []byte) thinkWire {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Errorf("unmarshal request: %v", err)
		return thinkWire{}
	}
	var w thinkWire
	if raw, ok := keys["reasoning_effort"]; ok {
		w.effortPresent = true
		if err := json.Unmarshal(raw, &w.effort); err != nil {
			t.Errorf("unmarshal reasoning_effort: %v", err)
		}
	}
	if raw, ok := keys["chat_template_kwargs"]; ok {
		w.kwargsPresent = true
		if err := json.Unmarshal(raw, &w.kwargs); err != nil {
			t.Errorf("unmarshal chat_template_kwargs: %v", err)
		}
	}
	return w
}

func TestChatWiresThinkControls(t *testing.T) {
	fp := func(b bool) *bool { return &b }
	tests := []struct {
		name       string
		opts       provider.ModelOptions
		wantEffort string
		wantKwargs map[string]any // nil => field absent
	}{
		{"unset omits both", provider.ModelOptions{}, "", nil},
		{"effort sends both", provider.ModelOptions{ThinkEffort: "low"},
			"low", map[string]any{"enable_thinking": true}},
		{"think true sends kwargs only", provider.ModelOptions{Think: fp(true)},
			"", map[string]any{"enable_thinking": true}},
		{"think false disables", provider.ModelOptions{Think: fp(false)},
			"", map[string]any{"enable_thinking": false}},
		{"think false suppresses effort", provider.ModelOptions{Think: fp(false), ThinkEffort: "high"},
			"", map[string]any{"enable_thinking": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got thinkWire
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = captureThinkWire(t, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer srv.Close()

			p := NewProvider(NewClient(srv.URL))
			_, err := p.Chat(context.Background(), provider.ChatRequest{
				Model:    "m",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
				Options:  tt.opts,
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if tt.wantEffort == "" {
				if got.effortPresent {
					t.Errorf("reasoning_effort key present (%q), want absent", got.effort)
				}
			} else if got.effort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", got.effort, tt.wantEffort)
			}
			assertKwargs(t, got, tt.wantKwargs)
		})
	}
}

func TestChatStreamWiresThinkControls(t *testing.T) {
	var got thinkWire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = captureThinkWire(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewProvider(NewClient(srv.URL))
	err := p.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
		Options:  provider.ModelOptions{ThinkEffort: "medium"},
	}, func(provider.ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.effort != "medium" {
		t.Errorf("reasoning_effort = %q, want medium", got.effort)
	}
	assertKwargs(t, got, map[string]any{"enable_thinking": true})
}

// assertKwargs checks the chat_template_kwargs field against want; want == nil
// asserts the KEY was absent from the wire, not merely a nil decoded map.
func assertKwargs(t *testing.T, got thinkWire, want map[string]any) {
	t.Helper()
	if want == nil {
		if got.kwargsPresent {
			t.Fatalf("chat_template_kwargs key present (%v), want absent", got.kwargs)
		}
		return
	}
	if !got.kwargsPresent || got.kwargs == nil {
		t.Fatalf("chat_template_kwargs absent, want %v", want)
	}
	wantOn, gotOn := want["enable_thinking"].(bool), got.kwargs["enable_thinking"]
	if gotOn != wantOn {
		t.Fatalf("enable_thinking = %v, want %v", gotOn, wantOn)
	}
	if len(got.kwargs) != 1 {
		t.Fatalf("unexpected extra kwargs: %v", got.kwargs)
	}
}

// TestThinkEffortActivatesToggleParser proves per-request parse controls for
// this provider: the instance default is ThinkNone, and the request routes to
// ThinkToggle via ParseThinkMode. A bare effort hint must activate the toggle
// (effort implies on); explicit Think=false must win over effort.
func TestThinkEffortActivatesToggleParser(t *testing.T) {
	fp := func(b bool) *bool { return &b }
	toggle := provider.ThinkToggle

	newChatServer := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"<think>why</think>answer"}}]}`))
		}))
	}

	t.Run("effort only extracts (Chat)", func(t *testing.T) {
		srv := newChatServer()
		defer srv.Close()

		p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
		resp, err := p.Chat(context.Background(), provider.ChatRequest{
			Model:          "m",
			Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
			Options:        provider.ModelOptions{ThinkEffort: "high"},
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

	t.Run("explicit false leaves tags (Chat)", func(t *testing.T) {
		srv := newChatServer()
		defer srv.Close()

		p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
		resp, err := p.Chat(context.Background(), provider.ChatRequest{
			Model:          "m",
			Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
			Options:        provider.ModelOptions{Think: fp(false), ThinkEffort: "high"},
			ParseThinkMode: &toggle,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if resp.Thinking != "" {
			t.Errorf("Thinking = %q, want empty", resp.Thinking)
		}
		if resp.Content != "<think>why</think>answer" {
			t.Errorf("Content = %q, want tags left in place", resp.Content)
		}
	})

	t.Run("effort only extracts (ChatStream, split deltas)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"<think>why</th\"},\"finish_reason\":null}]}\n\n" +
				"data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"ink>answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
				"data: [DONE]\n\n"))
		}))
		defer srv.Close()

		p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
		var thinking, content string
		err := p.ChatStream(context.Background(), provider.ChatRequest{
			Model:          "m",
			Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
			Options:        provider.ModelOptions{ThinkEffort: "high"},
			ParseThinkMode: &toggle,
		}, func(r provider.ChatResponse) error {
			thinking += r.Thinking
			content += r.Content
			return nil
		})
		if err != nil {
			t.Fatalf("ChatStream: %v", err)
		}
		if thinking != "why" {
			t.Errorf("accumulated Thinking = %q, want %q", thinking, "why")
		}
		if content != "answer" {
			t.Errorf("accumulated Content = %q, want %q", content, "answer")
		}
	})
}
