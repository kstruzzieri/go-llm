package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// cacheUsageWant is the cache-related slice of provider.Usage a case expects.
type cacheUsageWant struct {
	cached, write             int
	cachedRep, writeRep       bool
	prompt, completion, total int
}

// checkCacheUsage asserts the cache counters and the accounting invariant the
// agent budget relies on (TotalTokens is 0 or PromptTokens+CompletionTokens).
func checkCacheUsage(t *testing.T, got provider.Usage, w cacheUsageWant) {
	t.Helper()
	if got.CachedPromptTokens != w.cached || got.CachedPromptTokensReported != w.cachedRep {
		t.Errorf("cached = %d (reported %v), want %d (reported %v)",
			got.CachedPromptTokens, got.CachedPromptTokensReported, w.cached, w.cachedRep)
	}
	if got.CacheWriteTokens != w.write || got.CacheWriteTokensReported != w.writeRep {
		t.Errorf("cache write = %d (reported %v), want %d (reported %v)",
			got.CacheWriteTokens, got.CacheWriteTokensReported, w.write, w.writeRep)
	}
	if got.PromptTokens != w.prompt {
		t.Errorf("PromptTokens = %d, want %d (OpenAI semantics: total input, including cached)", got.PromptTokens, w.prompt)
	}
	if got.TotalTokens != 0 && got.TotalTokens != got.PromptTokens+got.CompletionTokens {
		t.Errorf("TotalTokens = %d, want 0 or PromptTokens+CompletionTokens = %d",
			got.TotalTokens, got.PromptTokens+got.CompletionTokens)
	}
}

func readUsageFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(b)
}

// TestChatUsageCacheCounters pins how the non-streaming usage block maps to
// provider.Usage cache counters.
//
// Observed llama-server shape (llama.cpp b11146 behind llama-swap, fixture
// llama_usage_nonstream.json, second identical request): usage carries
// prompt_tokens_details.cached_tokens (1267) and ALSO a top-level timings block
// (cache_n 1267, prompt_n 4). prompt_tokens (1271) INCLUDES the cached part
// (cached 1267 + freshly evaluated prompt_n 4), i.e. OpenAI semantics, so no
// normalization is needed and timings is deliberately not parsed.
func TestChatUsageCacheCounters(t *testing.T) {
	chatBody := func(usageFrag string) string {
		return `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{` + usageFrag + `}}`
	}

	tests := []struct {
		name string
		body string
		want cacheUsageWant
	}{
		{
			name: "openai details",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":1536}`),
			want: cacheUsageWant{cached: 1536, cachedRep: true, prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "explicit zero",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":0}`),
			want: cacheUsageWant{cachedRep: true, prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "details null",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":null`),
			want: cacheUsageWant{prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "absent",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010`),
			want: cacheUsageWant{prompt: 2000, completion: 10, total: 2010},
		},
		{
			// The provider reports the wire value raw, even when it exceeds
			// prompt_tokens; any clamp belongs to the consumer (none exists
			// today; cost clamping is planned in a later task).
			name: "cached exceeds prompt passes through raw",
			body: chatBody(`"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":150}`),
			want: cacheUsageWant{cached: 150, cachedRep: true, prompt: 100, completion: 10, total: 110},
		},
		{
			name: "cached_tokens null is unreported",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":null}`),
			want: cacheUsageWant{prompt: 2000, completion: 10, total: 2010},
		},
		{
			// Step 0 decision: the llama-server timings block is not a counter
			// source; only prompt_tokens_details.cached_tokens is.
			name: "timings alone is ignored",
			body: `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
				`"usage":{"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010},"timings":{"cache_n":1800}}`,
			want: cacheUsageWant{prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "cache write",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":900}`),
			want: cacheUsageWant{write: 900, cachedRep: true, writeRep: true, prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "cache write only, cached absent",
			body: chatBody(`"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cache_write_tokens":900}`),
			want: cacheUsageWant{write: 900, writeRep: true, prompt: 2000, completion: 10, total: 2010},
		},
		{
			name: "llama fixture",
			body: readUsageFixture(t, "llama_usage_nonstream.json"),
			want: cacheUsageWant{cached: 1267, cachedRep: true, prompt: 1271, completion: 16, total: 1287},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			p := NewProvider(NewClient(srv.URL))
			resp, err := p.Chat(context.Background(), provider.ChatRequest{
				Model:    "m",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.Usage.CompletionTokens != tc.want.completion || resp.Usage.TotalTokens != tc.want.total {
				t.Errorf("completion/total = %d/%d, want %d/%d",
					resp.Usage.CompletionTokens, resp.Usage.TotalTokens, tc.want.completion, tc.want.total)
			}
			checkCacheUsage(t, resp.Usage, tc.want)
		})
	}
}

// TestChatStreamUsageCacheCounters pins that the Done chunk of a streaming
// chat carries the cache counters from the final usage frame. The llama case
// replays the raw SSE body saved from llama-server (second identical request),
// whose usage frame has the same shape as the non-streaming fixture.
func TestChatStreamUsageCacheCounters(t *testing.T) {
	const sseHead = `data: {"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

	tests := []struct {
		name string
		sse  string
		want cacheUsageWant
	}{
		{
			name: "details cached tokens",
			sse: sseHead +
				`data: {"model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":64}}}` + "\n\n" +
				"data: [DONE]\n\n",
			want: cacheUsageWant{cached: 64, cachedRep: true, prompt: 100, completion: 5, total: 105},
		},
		{
			name: "details absent",
			sse: sseHead +
				`data: {"model":"m","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105}}` + "\n\n" +
				"data: [DONE]\n\n",
			want: cacheUsageWant{prompt: 100, completion: 5, total: 105},
		},
		{
			name: "llama fixture",
			sse:  readUsageFixture(t, "llama_usage_stream.json"),
			want: cacheUsageWant{cached: 1267, cachedRep: true, prompt: 1271, completion: 16, total: 1287},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.sse)
			}))
			defer srv.Close()

			p := NewProvider(NewClient(srv.URL))
			var got []provider.ChatResponse
			err := p.ChatStream(context.Background(), provider.ChatRequest{
				Model:    "m",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			}, func(r provider.ChatResponse) error {
				got = append(got, r)
				return nil
			})
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			if len(got) == 0 || !got[len(got)-1].Done {
				t.Fatalf("last chunk should be Done, got %+v", got)
			}
			last := got[len(got)-1]
			if last.Usage.CompletionTokens != tc.want.completion || last.Usage.TotalTokens != tc.want.total {
				t.Errorf("completion/total = %d/%d, want %d/%d",
					last.Usage.CompletionTokens, last.Usage.TotalTokens, tc.want.completion, tc.want.total)
			}
			checkCacheUsage(t, last.Usage, tc.want)
		})
	}
}
