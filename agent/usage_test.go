package agent

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestAddUsageSumsCacheCounters(t *testing.T) {
	tests := []struct {
		name string
		a, b provider.Usage
		want provider.Usage
	}{
		{
			name: "reported plus unreported sums and keeps the flag",
			a:    provider.Usage{PromptTokens: 100, CachedPromptTokens: 60, CachedPromptTokensReported: true},
			b:    provider.Usage{PromptTokens: 50},
			want: provider.Usage{PromptTokens: 150, CachedPromptTokens: 60, CachedPromptTokensReported: true},
		},
		{
			name: "unreported plus reported ORs the flag",
			a:    provider.Usage{PromptTokens: 50},
			b:    provider.Usage{PromptTokens: 100, CachedPromptTokens: 60, CachedPromptTokensReported: true},
			want: provider.Usage{PromptTokens: 150, CachedPromptTokens: 60, CachedPromptTokensReported: true},
		},
		{
			name: "two unreported stay unreported",
			a:    provider.Usage{PromptTokens: 100},
			b:    provider.Usage{PromptTokens: 50},
			want: provider.Usage{PromptTokens: 150},
		},
		{
			name: "measured zero stays reported",
			a:    provider.Usage{PromptTokens: 10, CachedPromptTokensReported: true},
			b:    provider.Usage{PromptTokens: 20, CachedPromptTokensReported: true},
			want: provider.Usage{PromptTokens: 30, CachedPromptTokensReported: true},
		},
		{
			name: "cache writes sum and OR independently of cached reads",
			a:    provider.Usage{CacheWriteTokens: 7, CacheWriteTokensReported: true},
			b:    provider.Usage{CachedPromptTokens: 3, CachedPromptTokensReported: true, CacheWriteTokens: 5, CacheWriteTokensReported: true},
			want: provider.Usage{CachedPromptTokens: 3, CachedPromptTokensReported: true, CacheWriteTokens: 12, CacheWriteTokensReported: true},
		},
		{
			name: "cache write reported on the left only ORs the flag",
			a:    provider.Usage{CacheWriteTokens: 5, CacheWriteTokensReported: true},
			b:    provider.Usage{},
			want: provider.Usage{CacheWriteTokens: 5, CacheWriteTokensReported: true},
		},
		{
			name: "cache write reported on the right only ORs the flag",
			a:    provider.Usage{},
			b:    provider.Usage{CacheWriteTokens: 5, CacheWriteTokensReported: true},
			want: provider.Usage{CacheWriteTokens: 5, CacheWriteTokensReported: true},
		},
		{
			name: "reasoning is still not accumulated",
			a:    provider.Usage{TotalTokens: 1},
			b:    provider.Usage{TotalTokens: 2, ReasoningTokens: 4, ReasoningTokensReported: true},
			want: provider.Usage{TotalTokens: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := addUsage(tt.a, tt.b); got != tt.want {
				t.Fatalf("addUsage = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A nested Run's validated usage reaches the ancestor's DescendantUsage with
// the cache counters, and the Reported flags OR across children.
func TestDescendantUsageIncludesCacheCounters(t *testing.T) {
	cached := provider.Usage{
		PromptTokens: 100, CompletionTokens: 5, TotalTokens: 105,
		CachedPromptTokens: 40, CachedPromptTokensReported: true,
		CacheWriteTokens: 6, CacheWriteTokensReported: true,
	}
	plain := provider.Usage{PromptTokens: 20, CompletionTokens: 2, TotalTokens: 22}
	tests := []struct {
		name     string
		children []provider.Usage
		want     provider.Usage
	}{
		{
			name:     "one reporting child",
			children: []provider.Usage{cached},
			want:     cached,
		},
		{
			name:     "reporting and unreporting children",
			children: []provider.Usage{cached, plain},
			want: provider.Usage{
				PromptTokens: 120, CompletionTokens: 7, TotalTokens: 127,
				CachedPromptTokens: 40, CachedPromptTokensReported: true,
				CacheWriteTokens: 6, CacheWriteTokensReported: true,
			},
		},
		{
			name:     "unreporting child leaves the counters unreported",
			children: []provider.Usage{plain},
			want:     plain,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := New(budgetModelFunc(func(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
				for _, u := range tt.children {
					child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{Content: "child", Usage: u}}}}, ContextManager{})
					if _, err := child.Run(ctx, Request{Goal: "q"}, nil); err != nil {
						t.Fatal(err)
					}
				}
				return ModelResult{Response: provider.ChatResponse{Content: "parent"}}, nil
			}), ContextManager{})
			res, err := parent.Run(t.Context(), Request{Goal: "q"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.DescendantUsage == nil || *res.DescendantUsage != tt.want {
				t.Fatalf("DescendantUsage = %+v, want %+v", res.DescendantUsage, tt.want)
			}
		})
	}
}
