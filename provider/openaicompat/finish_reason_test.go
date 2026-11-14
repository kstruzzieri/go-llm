package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

var finishReasons = []string{"stop", "length", "tool_calls"}

func TestChatStream_FinishReasonReachesCaller(t *testing.T) {
	for _, reason := range finishReasons {
		t.Run(reason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(t, w, []chatChunk{
					{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "hi"}}}},
					{Model: "m", Choices: []chatChunkChoice{{FinishReason: stringPtr(reason)}},
						Usage: &usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}},
				})
			}))
			defer srv.Close()

			p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
			cb, getFinal := provider.Collect(nil)
			var last provider.ChatResponse
			err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
				last = r
				return cb(r)
			})
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			if !last.Done || last.FinishReason != reason {
				t.Errorf("final callback = %+v, want Done with FinishReason %q", last, reason)
			}
			if got := getFinal().FinishReason; got != reason {
				t.Errorf("Collect final FinishReason = %q, want %q", got, reason)
			}
		})
	}
}

func TestChatStream_CancelledPartialHasNoFinishReason(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", marshalJSON(t, chatChunk{
			Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "partial"}}},
		}))
		w.(http.Flusher).Flush()
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hold)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewProvider(NewClient(srv.URL), WithThinkMode(provider.ThinkNone))
	var last provider.ChatResponse
	_ = p.ChatStream(ctx, provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		last = r
		if r.Content == "partial" {
			cancel()
		}
		return nil
	})
	if !last.Done || !last.Partial {
		t.Fatalf("last = %+v, want Done+Partial", last)
	}
	if last.FinishReason != "" {
		t.Errorf("partial FinishReason = %q, want empty", last.FinishReason)
	}
}

func TestChat_FinishReasonReachesCaller(t *testing.T) {
	for _, reason := range finishReasons {
		t.Run(reason, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(chatResponse{
					Model: "m",
					Choices: []chatChoice{{
						Message:      chatMessage{Role: "assistant", Content: "hi"},
						FinishReason: reason,
					}},
				})
			}))
			defer srv.Close()

			p := NewProvider(NewClient(srv.URL))
			resp, err := p.Chat(context.Background(), provider.ChatRequest{Model: "m"})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.FinishReason != reason {
				t.Errorf("FinishReason = %q, want %q", resp.FinishReason, reason)
			}
		})
	}
}
