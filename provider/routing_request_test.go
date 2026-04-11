package provider

import (
	"testing"
)

func TestPriorityString(t *testing.T) {
	tests := []struct {
		name     string
		priority Priority
		want     string
	}{
		{name: "background", priority: PriorityBackground, want: "background"},
		{name: "normal", priority: PriorityNormal, want: "normal"},
		{name: "high", priority: PriorityHigh, want: "high"},
		{name: "critical", priority: PriorityCritical, want: "critical"},
		{name: "unknown", priority: Priority(99), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.priority.String()
			if got != tt.want {
				t.Errorf("Priority(%d).String() = %q, want %q", tt.priority, got, tt.want)
			}
		})
	}
}

func TestRouteKindString(t *testing.T) {
	tests := []struct {
		name string
		kind RouteKind
		want string
	}{
		{name: "chat", kind: RouteKindChat, want: "chat"},
		{name: "generate", kind: RouteKindGenerate, want: "generate"},
		{name: "embed", kind: RouteKindEmbed, want: "embed"},
		{name: "unknown", kind: RouteKind(99), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.kind.String()
			if got != tt.want {
				t.Errorf("RouteKind(%d).String() = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestDefaultExpectedOutput(t *testing.T) {
	tests := []struct {
		name    string
		useCase string
		want    int
	}{
		{name: "fim", useCase: "fim", want: 200},
		{name: "chat", useCase: "chat", want: 2048},
		{name: "embedding", useCase: "embedding", want: 0},
		{name: "code-review", useCase: "code-review", want: 4096},
		{name: "reasoning", useCase: "reasoning", want: 4096},
		{name: "unknown falls back to chat default", useCase: "unknown", want: 2048},
		{name: "empty falls back to chat default", useCase: "", want: 2048},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultExpectedOutput(tt.useCase)
			if got != tt.want {
				t.Errorf("DefaultExpectedOutput(%q) = %d, want %d", tt.useCase, got, tt.want)
			}
		})
	}
}

func TestStickyKeyDeterminism(t *testing.T) {
	req := RoutingRequest{
		AffinityKey:  "session-123",
		Model:        "qwen3:8b",
		UseCase:      "chat",
		RequiredCaps: CapChat | CapStream,
		Priority:     PriorityNormal,
		Messages: []ChatMessage{
			{Role: "user", Content: "hello world, this is a test message"},
		},
		ExpectedOutput: 2048,
	}

	key1 := StickyKey(req)
	key2 := StickyKey(req)

	if key1 == "" {
		t.Fatal("StickyKey() returned empty string")
	}
	if key1 != key2 {
		t.Errorf("StickyKey() not deterministic: %q != %q", key1, key2)
	}
}

func TestStickyKeyDiffers(t *testing.T) {
	base := RoutingRequest{
		AffinityKey:  "session-123",
		Model:        "qwen3:8b",
		UseCase:      "chat",
		RequiredCaps: CapChat | CapStream,
		Priority:     PriorityNormal,
		Messages: []ChatMessage{
			{Role: "user", Content: "hello world, this is a test message"},
		},
		ExpectedOutput: 2048,
	}

	baseKey := StickyKey(base)

	// Different AffinityKey
	diffAffinity := base
	diffAffinity.AffinityKey = "session-456"
	if StickyKey(diffAffinity) == baseKey {
		t.Error("different AffinityKey should produce different StickyKey")
	}

	// Different Priority
	diffPriority := base
	diffPriority.Priority = PriorityCritical
	if StickyKey(diffPriority) == baseKey {
		t.Error("different Priority should produce different StickyKey")
	}

	// Different UseCase
	diffUseCase := base
	diffUseCase.UseCase = "fim"
	if StickyKey(diffUseCase) == baseKey {
		t.Error("different UseCase should produce different StickyKey")
	}
}

func TestInputBudgetClass(t *testing.T) {
	tests := []struct {
		name   string
		tokens int
		want   string
	}{
		{name: "zero", tokens: 0, want: "small"},
		{name: "1000", tokens: 1000, want: "small"},
		{name: "2047", tokens: 2047, want: "small"},
		{name: "2048 boundary", tokens: 2048, want: "medium"},
		{name: "5000", tokens: 5000, want: "medium"},
		{name: "8191", tokens: 8191, want: "medium"},
		{name: "8192 boundary", tokens: 8192, want: "large"},
		{name: "100000", tokens: 100000, want: "large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inputBudgetClass(tt.tokens)
			if got != tt.want {
				t.Errorf("inputBudgetClass(%d) = %q, want %q", tt.tokens, got, tt.want)
			}
		})
	}
}

func TestOutputBudgetClass(t *testing.T) {
	tests := []struct {
		name   string
		tokens int
		want   string
	}{
		{name: "zero", tokens: 0, want: "small"},
		{name: "200", tokens: 200, want: "small"},
		{name: "511", tokens: 511, want: "small"},
		{name: "512 boundary", tokens: 512, want: "medium"},
		{name: "1000", tokens: 1000, want: "medium"},
		{name: "2047", tokens: 2047, want: "medium"},
		{name: "2048 boundary", tokens: 2048, want: "large"},
		{name: "10000", tokens: 10000, want: "large"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outputBudgetClass(tt.tokens)
			if got != tt.want {
				t.Errorf("outputBudgetClass(%d) = %q, want %q", tt.tokens, got, tt.want)
			}
		})
	}
}

func TestEstimateRoutingInputTokens(t *testing.T) {
	t.Run("messages contribute tokens", func(t *testing.T) {
		req := RoutingRequest{
			Messages: []ChatMessage{
				{Role: "user", Content: "hello world test message here"},
			},
		}
		tokens := estimateRoutingInputTokens(req)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
	})

	t.Run("prompt contributes tokens", func(t *testing.T) {
		req := RoutingRequest{
			Prompt: "generate some code for me please",
		}
		tokens := estimateRoutingInputTokens(req)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
	})

	t.Run("system contributes tokens", func(t *testing.T) {
		req := RoutingRequest{
			System: "you are a helpful assistant",
		}
		tokens := estimateRoutingInputTokens(req)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
	})

	t.Run("suffix contributes tokens", func(t *testing.T) {
		req := RoutingRequest{
			Suffix: "some suffix content here for FIM",
		}
		tokens := estimateRoutingInputTokens(req)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
	})

	t.Run("input strings contribute tokens", func(t *testing.T) {
		req := RoutingRequest{
			Input: []string{"text to embed number one", "text to embed number two"},
		}
		tokens := estimateRoutingInputTokens(req)
		if tokens <= 0 {
			t.Errorf("expected positive token count, got %d", tokens)
		}
	})

	t.Run("empty request returns zero", func(t *testing.T) {
		req := RoutingRequest{}
		tokens := estimateRoutingInputTokens(req)
		if tokens != 0 {
			t.Errorf("expected 0 tokens for empty request, got %d", tokens)
		}
	})

	t.Run("all fields accumulate", func(t *testing.T) {
		req := RoutingRequest{
			Messages: []ChatMessage{
				{Role: "system", Content: "you are helpful"},
				{Role: "user", Content: "hello world"},
			},
			Prompt: "some prompt",
			System: "system text",
			Suffix: "suffix text",
			Input:  []string{"embed this"},
		}
		tokens := estimateRoutingInputTokens(req)

		// Each field alone should contribute less than all combined.
		msgOnly := RoutingRequest{Messages: req.Messages}
		promptOnly := RoutingRequest{Prompt: req.Prompt}

		if tokens <= estimateRoutingInputTokens(msgOnly) {
			t.Error("combined should be greater than messages alone")
		}
		if tokens <= estimateRoutingInputTokens(promptOnly) {
			t.Error("combined should be greater than prompt alone")
		}
	})
}
