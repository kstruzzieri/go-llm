package conversation

import (
	"context"
	"strings"
	"testing"
)

func TestFallbackSummarizer_FoldsPriorThenMessages(t *testing.T) {
	out, err := FallbackSummarizer(context.Background(), "PRIOR",
		[]Message{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}})
	if err != nil {
		t.Fatalf("FallbackSummarizer: %v", err)
	}
	if !strings.HasPrefix(out, "PRIOR") {
		t.Fatalf("want prior first, got %q", out)
	}
	if !strings.Contains(out, "user: hello") || !strings.Contains(out, "assistant: hi") {
		t.Fatalf("want rendered messages, got %q", out)
	}
}

func TestFallbackSummarizer_EmptyPriorOmitsLeadingBlank(t *testing.T) {
	out, err := FallbackSummarizer(context.Background(), "",
		[]Message{{Role: "user", Content: "only"}})
	if err != nil {
		t.Fatalf("FallbackSummarizer: %v", err)
	}
	if strings.HasPrefix(out, "\n") || out != "user: only" {
		t.Fatalf("want %q, got %q", "user: only", out)
	}
}

func TestEstimateMessagesTokens_SkipsSystem(t *testing.T) {
	est := func(s string) int { return len(s) }
	msgs := []Message{
		{Role: "system", Content: "SYSTEM-IGNORED"},
		{Role: "user", Content: "abc"},
	}
	if got := EstimateMessagesTokens(msgs, est); got != len("abc") {
		t.Fatalf("EstimateMessagesTokens = %d, want %d", got, len("abc"))
	}
}
