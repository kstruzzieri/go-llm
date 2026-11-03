package openaicompat

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// TestChatUsageAbacusNonStream replays the usage blocks Abacus RouteLLM sent on
// non-streaming gpt-5-mini chat completions (measured 2026-11-01; the usage
// objects are verbatim, the envelope around them is rebuilt). Abacus sends no
// prompt_tokens: raw_input_tokens is the uncached input, cache_read_input_tokens
// the cached part, and input_tokens is billing-weighted (597 = 98 uncached +
// 4,992 cached at a tenth), so it must not be read as the prompt count. Abacus
// omits cache_read_input_tokens when nothing is cached, so on an Abacus client
// its shape without one reads as 0 cached, reported; any other host, or the
// OpenAI shape, leaves an absent counter unreported.
func TestChatUsageAbacusNonStream(t *testing.T) {
	envelope := func(usageObj string) string {
		return `{"model": "gpt-5-mini", "choices": [{"index": 0, "message": {"role": "assistant", "content": "OK"}, "finish_reason": "stop", "native_finish_reason": "stop"}], "usage": ` + usageObj + `, "error": null}`
	}

	tests := []struct {
		name      string
		usage     string
		elsewhere bool // a non-Abacus host
		want      provider.Usage
	}{
		{
			name:  "warm request",
			usage: `{"input_tokens": 597, "output_tokens": 45, "cache_read_input_tokens": 4992, "raw_input_tokens": 98}`,
			want: provider.Usage{PromptTokens: 5090, CompletionTokens: 45, TotalTokens: 5135,
				CachedPromptTokens: 4992, CachedPromptTokensReported: true},
		},
		{
			name:  "cold request with reasoning",
			usage: `{"input_tokens": 5090, "output_tokens": 104, "reasoning_tokens": 64, "raw_input_tokens": 5090}`,
			want: provider.Usage{PromptTokens: 5090, CompletionTokens: 104, TotalTokens: 5194,
				ReasoningTokens: 64, ReasoningTokensReported: true, CachedPromptTokensReported: true},
		},
		{
			name:  "tiny request",
			usage: `{"input_tokens": 8, "output_tokens": 77, "raw_input_tokens": 8}`,
			want:  provider.Usage{PromptTokens: 8, CompletionTokens: 77, TotalTokens: 85, CachedPromptTokensReported: true},
		},
		{
			// gpt-6-luna, cold (2026-11-03): 4,726 = 3 + floor(3,779 * 1.25),
			// so input_tokens weights the written tokens at the write premium.
			name:  "cold request writing the cache",
			usage: `{"input_tokens": 4726, "output_tokens": 5, "cache_creation_input_tokens": 3779, "raw_input_tokens": 3}`,
			want: provider.Usage{PromptTokens: 3782, CompletionTokens: 5, TotalTokens: 3787,
				CachedPromptTokensReported: true, CacheWriteTokens: 3779, CacheWriteTokensReported: true},
		},
		{
			// Claude Haiku 4.5, a grown turn (2026-11-03): reads the earlier
			// turn, writes the new one; the same turn repeated read 5,799 with
			// 3 uncached, so the exact input is 3 + 4,550 + 1,249 = 5,802.
			name:  "warm request writing new tokens",
			usage: `{"input_tokens": 2019, "output_tokens": 4, "cache_read_input_tokens": 4550, "cache_creation_input_tokens": 1249, "raw_input_tokens": 3}`,
			want: provider.Usage{PromptTokens: 5802, CompletionTokens: 4, TotalTokens: 5806,
				CachedPromptTokens: 4550, CachedPromptTokensReported: true, CacheWriteTokens: 1249, CacheWriteTokensReported: true},
		},
		{
			name:  "abacus shape elsewhere: counter unreported",
			usage: `{"input_tokens": 8, "output_tokens": 77, "raw_input_tokens": 8}`, elsewhere: true,
			want: provider.Usage{PromptTokens: 8, CompletionTokens: 77, TotalTokens: 85},
		},
		{
			name:  "openai shape on abacus: counter unreported",
			usage: `{"prompt_tokens": 8, "completion_tokens": 77, "total_tokens": 85}`,
			want:  provider.Usage{PromptTokens: 8, CompletionTokens: 77, TotalTokens: 85},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, envelope(tc.usage))
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			c.abacusRouteLLM = !tc.elsewhere // the stream-only recovery must not touch Chat
			resp, err := NewProvider(c).Chat(context.Background(), provider.ChatRequest{
				Model:    "gpt-5-mini",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if resp.Usage != tc.want {
				t.Errorf("Usage = %+v\nwant    %+v", resp.Usage, tc.want)
			}
		})
	}
}

// TestUsageDecodeUnchangedForOpenAIShapes proves the Abacus rule leaves every
// usage block that is not Abacus's non-streaming shape decoding exactly as the
// plain struct decode did before the rule existed: llama.cpp, OpenAI, vLLM and
// embedding shapes, a block that carries prompt_tokens next to Abacus field
// names, and Abacus fields without raw_input_tokens.
func TestUsageDecodeUnchangedForOpenAIShapes(t *testing.T) {
	var llama struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(readUsageFixture(t, "llama_usage_nonstream.json")), &llama); err != nil {
		t.Fatalf("llama fixture: %v", err)
	}

	bodies := map[string]string{
		"llama.cpp fixture":               string(llama.Usage),
		"openai":                          `{"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"prompt_tokens_details":{"cached_tokens":1536,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0}}`,
		"vllm":                            `{"prompt_tokens":12,"total_tokens":20,"completion_tokens":8,"prompt_tokens_details":null}`,
		"embedding":                       `{"prompt_tokens":8,"total_tokens":8}`,
		"abacus stream":                   `{"prompt_tokens": 596, "completion_tokens": 75, "total_tokens": 671, "completion_tokens_details": {"reasoning_tokens": 64}, "prompt_tokens_details": {"cached_tokens": 4992}}`,
		"prompt_tokens wins":              `{"prompt_tokens":2000,"completion_tokens":10,"total_tokens":2010,"raw_input_tokens":5,"cache_read_input_tokens":7,"output_tokens":99,"reasoning_tokens":3}`,
		"prompt wins over raw type":       `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":"unavailable"}`,
		"prompt wins over output type":    `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":5,"output_tokens":"8"}`,
		"prompt wins over cache type":     `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":5,"cache_read_input_tokens":"unknown"}`,
		"prompt wins over write type":     `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":5,"cache_creation_input_tokens":"unknown"}`,
		"prompt wins over cache write":    `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":2,"cache_creation_input_tokens":7}`,
		"prompt wins over reasoning type": `{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":5,"reasoning_tokens":{"count":2}}`,
		"extensions without raw":          `{"completion_tokens":8,"total_tokens":8,"output_tokens":"8","cache_read_input_tokens":"unknown","reasoning_tokens":{"count":2}}`,
		"null prompt wins":                `{"prompt_tokens":null,"completion_tokens":8,"total_tokens":8,"raw_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":99}`,
		"duplicate prompt ending in null": `{"prompt_tokens":12,"prompt_tokens":null,"completion_tokens":8,"total_tokens":20,"raw_input_tokens":2,"cache_read_input_tokens":3,"output_tokens":99}`,
		"no raw_input_tokens":             `{"input_tokens":597,"output_tokens":45,"cache_read_input_tokens":4992,"reasoning_tokens":3}`,
		"null":                            `null`,
	}
	type plain usage // no UnmarshalJSON: the decode before the Abacus rule
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			var got usage
			var want plain
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := json.Unmarshal([]byte(body), &want); err != nil {
				t.Fatalf("plain decode: %v", err)
			}
			if !reflect.DeepEqual(got, usage(want)) {
				t.Errorf("decode = %+v, want the plain decode %+v", got, usage(want))
			}
		})
	}
}

func TestUsageDecodeAbacusInvalidCounts(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"raw input":   `{"raw_input_tokens":"5","output_tokens":2}`,
		"cache read":  `{"raw_input_tokens":5,"cache_read_input_tokens":"3","output_tokens":2}`,
		"cache write": `{"raw_input_tokens":5,"cache_creation_input_tokens":"3","output_tokens":2}`,
		"output":      `{"raw_input_tokens":5,"output_tokens":"2"}`,
		"reasoning":   `{"raw_input_tokens":5,"output_tokens":2,"reasoning_tokens":{"count":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got usage
			if err := json.Unmarshal([]byte(body), &got); err == nil {
				t.Errorf("UnmarshalJSON(%s) = %+v, nil; want an invalid token count error", body, got)
			}
		})
	}
}

// TestAbacusRouteLLMHost pins which base URLs get the Abacus usage rules:
// routellm.abacus.ai and its subdomains over any scheme, never a look-alike.
func TestAbacusRouteLLMHost(t *testing.T) {
	for baseURL, want := range map[string]bool{
		"https://routellm.abacus.ai":             true,
		"http://routellm.abacus.ai":              true,
		"https://RouteLLM.Abacus.AI.:443/":       true,
		"https://eu.routellm.abacus.ai":          true,
		"https://notroutellm.abacus.ai":          false,
		"https://routellm.abacus.ai.example.com": false,
		"https://abacus.ai":                      false,
		"https://api.openai.com":                 false,
		"http://127.0.0.1:8090":                  false,
	} {
		if got := NewClient(baseURL).abacusRouteLLM; got != want {
			t.Errorf("NewClient(%q).abacusRouteLLM = %v, want %v", baseURL, got, want)
		}
	}
}

// TestChatStreamUsageAbacusWeighted replays Abacus RouteLLM's streamed usage
// (usage objects verbatim, in the finish-chunk envelope Abacus sent; gpt-5-mini
// measured 2026-11-01, the rest 2026-11-03). Its prompt_tokens is
// uncached + floor(cached * w), w the served model's cached-to-input price
// ratio, and no raw count is sent. Models that bill cache writes add the
// written tokens at 1.25 and report no written count, so a frame from one is
// not invertible, cold or warm. On an Abacus client a frame from a model in
// abacusCacheWeights reports the recovered exact input (each want was checked
// against the same prompt's exact non-streaming count); any other served
// model, or a recovery below zero, reports PromptTokens and TotalTokens as
// unknown (0) and keeps the rest. A cold Abacus frame carries no
// prompt_tokens_details (measured 2026-11-03 on every cold stream of five
// models), so it reads as 0 cached, reported. The same frame from any other
// server passes as sent.
func TestChatStreamUsageAbacusWeighted(t *testing.T) {
	sse := func(model, usageObj string) string {
		modelField := ""
		if model != "" {
			modelField = `"model": "` + model + `", `
		}
		return `data: {"id": "chatcmpl-45a6e76220334f87bf8369e9d200d94c", "object": "chat.completion.chunk", ` + modelField + `"created": 1793587534, ` +
			`"choices": [{"index": 0, "delta": {"role": "assistant", "content": "OK"}, "finish_reason": "stop"}], "usage": ` + usageObj + "}\n\n" +
			"data: [DONE]\n\n"
	}
	const (
		// 596 = 97 uncached + floor(4,992 / 10); the same prompt cold reported 5,089.
		warm = `{"prompt_tokens": 596, "completion_tokens": 75, "total_tokens": 671, "completion_tokens_details": {"reasoning_tokens": 64}, "prompt_tokens_details": {"cached_tokens": 4992}}`
		cold = `{"prompt_tokens": 5089, "completion_tokens": 56, "total_tokens": 5145}`
		// Not recorded: a frame reporting zero cached tokens, whose weighted
		// count equals the raw one.
		zeroCached = `{"prompt_tokens": 5089, "completion_tokens": 56, "total_tokens": 5145, "prompt_tokens_details": {"cached_tokens": 0}}`
		// floor(2,688 / 10) = 268 where rounding gives 269; exact 2,764.
		warmFloor = `{"prompt_tokens": 344, "completion_tokens": 59, "total_tokens": 403, "prompt_tokens_details": {"cached_tokens": 2688}}`
		// 3,200 * 4/25 = 512 exactly; exact 3,279.
		grok = `{"prompt_tokens": 591, "completion_tokens": 1, "total_tokens": 592, "prompt_tokens_details": {"cached_tokens": 3200}}`
		// floor(4,067 / 10) = 406; exact 5,408.
		gemini = `{"prompt_tokens": 1747, "completion_tokens": 1, "total_tokens": 1748, "prompt_tokens_details": {"cached_tokens": 4067}}`
		// floor(4,224 * 7/110) = 268 where rounding gives 269; exact 4,413.
		deepseek = `{"prompt_tokens": 457, "completion_tokens": 1, "total_tokens": 458, "prompt_tokens_details": {"cached_tokens": 4224}}`
		// gpt-6-luna bills cache writes the stream does not report.
		luna = `{"prompt_tokens": 279, "completion_tokens": 5, "total_tokens": 284, "prompt_tokens_details": {"cached_tokens": 2762}}`
		// Models that bill cache writes, recorded 2026-11-03. A cold frame
		// counts written tokens at 1.25: 5,691 = 3 + floor(4,551 * 1.25) for
		// an exact 4,554 (Claude Haiku 4.5), 4,729 = 3 + floor(3,781 * 1.25)
		// for an exact 3,784 (route-llm serving gpt-6-luna).
		claudeCold = `{"prompt_tokens": 5691, "completion_tokens": 4, "total_tokens": 5695}`
		routedCold = `{"prompt_tokens": 4729, "completion_tokens": 5, "total_tokens": 4734}`
		// Below Claude Haiku 4.5's cache minimum nothing is written, so 3,116
		// is exact, but the frame has the same shape as claudeCold.
		claudeUncacheable = `{"prompt_tokens": 3116, "completion_tokens": 4, "total_tokens": 3120}`
		// A grown turn reads the earlier turn and writes the new one:
		// 2,019 = 3 + floor(4,551 / 10 + 1,249 * 1.25) for an exact 5,803.
		claudeGrown = `{"prompt_tokens": 2019, "completion_tokens": 4, "total_tokens": 2023, "prompt_tokens_details": {"cached_tokens": 4551}}`
		// Not recorded: 499 = floor(4,992 / 10), so nothing is uncached.
		allCached = `{"prompt_tokens": 499, "completion_tokens": 75, "total_tokens": 574, "prompt_tokens_details": {"cached_tokens": 4992}}`
		// Not recorded: a details object without cached_tokens.
		detailsNoCached = `{"prompt_tokens": 5089, "completion_tokens": 56, "total_tokens": 5145, "prompt_tokens_details": {"audio_tokens": 0}}`
		// Not recorded: 400 < floor(4,992 / 10), so the uncached count would be negative.
		belowCached = `{"prompt_tokens": 400, "completion_tokens": 75, "total_tokens": 475, "prompt_tokens_details": {"cached_tokens": 4992}}`
	)
	cachedOnly := func(cached, completion int) provider.Usage {
		return provider.Usage{CompletionTokens: completion, CachedPromptTokens: cached, CachedPromptTokensReported: true}
	}
	exact := func(prompt, cached, completion int) provider.Usage {
		u := cachedOnly(cached, completion)
		u.PromptTokens, u.TotalTokens = prompt, prompt+completion
		return u
	}
	withReasoning := func(u provider.Usage) provider.Usage {
		u.ReasoningTokens, u.ReasoningTokensReported = 64, true
		return u
	}

	// The model arrives on an earlier chunk; the usage frame carries none.
	carried := `data: {"id": "c", "object": "chat.completion.chunk", "model": "gpt-5-mini", "choices": [{"index": 0, "delta": {"content": "OK"}, "finish_reason": "stop"}]}` + "\n\n" +
		`data: {"id": "c", "object": "chat.completion.chunk", "choices": [], "usage": ` + warmFloor + "}\n\n" + "data: [DONE]\n\n"

	tests := []struct {
		name               string
		usage, body        string // body, when set, is the whole stream
		reqModel, chunkMod string
		abacus             bool
		want               provider.Usage
	}{
		{
			name: "warm gpt-5-mini on abacus: recovered", usage: warm, abacus: true,
			want: withReasoning(exact(5089, 4992, 75)),
		},
		{name: "gpt-5-mini floors", usage: warmFloor, abacus: true, want: exact(2764, 2688, 59)},
		{name: "grok-4.3 exact product", usage: grok, chunkMod: "grok-4.3", abacus: true, want: exact(3279, 3200, 1)},
		{name: "gemini-3.5-flash", usage: gemini, chunkMod: "gemini-3.5-flash", abacus: true, want: exact(5408, 4067, 1)},
		{
			name: "deepseek-v4-flash floors", usage: deepseek, chunkMod: "deepseek-ai/DeepSeek-V4-Flash-0731", abacus: true,
			want: exact(4413, 4224, 1),
		},
		{
			name: "router: the served model's weight", usage: warmFloor, reqModel: "route-llm", abacus: true,
			want: exact(2764, 2688, 59),
		},
		{
			name: "no chunk model: the requested model's weight", usage: warmFloor, chunkMod: "-", abacus: true,
			want: exact(2764, 2688, 59),
		},
		{
			name: "unmeasured served model: input unknown", usage: luna, reqModel: "route-llm", chunkMod: "gpt-6-luna", abacus: true,
			want: cachedOnly(2762, 5),
		},
		{
			name: "requested model cannot stand in for the served one", usage: luna, chunkMod: "gpt-6-luna", abacus: true,
			want: cachedOnly(2762, 5),
		},
		{
			name: "model from an earlier chunk", body: carried, reqModel: "route-llm", abacus: true,
			want: exact(2764, 2688, 59),
		},
		{name: "nothing uncached: recovered", usage: allCached, abacus: true, want: exact(4992, 4992, 75)},
		{name: "negative uncached: input unknown", usage: belowCached, abacus: true, want: cachedOnly(4992, 75)},
		{
			name: "cold on abacus: exact, 0 cached", usage: cold, abacus: true,
			want: provider.Usage{PromptTokens: 5089, CompletionTokens: 56, TotalTokens: 5145, CachedPromptTokensReported: true},
		},
		{
			name: "cold on abacus, unmeasured model: input unknown", usage: cold, chunkMod: "gpt-5.5", abacus: true,
			want: cachedOnly(0, 56),
		},
		{
			name: "cold claude writes the cache: input unknown", usage: claudeCold, chunkMod: "claude-haiku-4-5-20251001", abacus: true,
			want: cachedOnly(0, 4),
		},
		{
			name: "cold router serving gpt-6-luna: input unknown", usage: routedCold, reqModel: "route-llm", chunkMod: "gpt-6-luna", abacus: true,
			want: cachedOnly(0, 5),
		},
		{
			name: "router without a served model: input unknown", usage: cold, reqModel: "route-llm", chunkMod: "-", abacus: true,
			want: cachedOnly(0, 56),
		},
		{
			name: "claude below its cache minimum: input unknown", usage: claudeUncacheable, chunkMod: "claude-haiku-4-5-20251001", abacus: true,
			want: cachedOnly(0, 4),
		},
		{
			name: "grown claude turn: input unknown", usage: claudeGrown, chunkMod: "claude-haiku-4-5-20251001", abacus: true,
			want: cachedOnly(4551, 4),
		},
		{
			name: "cold claude elsewhere: as sent", usage: claudeCold, chunkMod: "claude-haiku-4-5-20251001", abacus: false,
			want: provider.Usage{PromptTokens: 5691, CompletionTokens: 4, TotalTokens: 5695},
		},
		{
			name: "cold elsewhere: counter unreported", usage: cold, abacus: false,
			want: provider.Usage{PromptTokens: 5089, CompletionTokens: 56, TotalTokens: 5145},
		},
		{
			name: "details without cached_tokens on abacus: unreported", usage: detailsNoCached, abacus: true,
			want: provider.Usage{PromptTokens: 5089, CompletionTokens: 56, TotalTokens: 5145},
		},
		{
			name: "zero cached on abacus: exact", usage: zeroCached, abacus: true,
			want: provider.Usage{PromptTokens: 5089, CompletionTokens: 56, TotalTokens: 5145, CachedPromptTokensReported: true},
		},
		{
			name: "same frame elsewhere: as sent", usage: warm, abacus: false,
			want: provider.Usage{PromptTokens: 596, CompletionTokens: 75, TotalTokens: 671, ReasoningTokens: 64, ReasoningTokensReported: true,
				CachedPromptTokens: 4992, CachedPromptTokensReported: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reqModel, chunkModel := cmp.Or(tc.reqModel, "gpt-5-mini"), cmp.Or(tc.chunkMod, "gpt-5-mini")
			if chunkModel == "-" {
				chunkModel = ""
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				body := tc.body
				if body == "" {
					body = sse(chunkModel, tc.usage)
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			c.abacusRouteLLM = tc.abacus
			var last provider.ChatResponse
			err := NewProvider(c).ChatStream(context.Background(), provider.ChatRequest{
				Model:    reqModel,
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
			}, func(r provider.ChatResponse) error {
				last = r
				return nil
			})
			if err != nil {
				t.Fatalf("ChatStream: %v", err)
			}
			if !last.Done {
				t.Fatalf("last chunk should be Done, got %+v", last)
			}
			if last.Usage != tc.want {
				t.Errorf("Usage = %+v\nwant    %+v", last.Usage, tc.want)
			}
		})
	}
}
