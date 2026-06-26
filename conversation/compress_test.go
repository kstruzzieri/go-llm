package conversation

import (
	"context"
	"errors"
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

func okSummarizer(_ context.Context, prior string, msgs []Message) (string, error) {
	return FallbackSummarizer(context.Background(), prior, msgs)
}

func userAsst(u, a string) []Message {
	return []Message{{Role: "user", Content: u}, {Role: "assistant", Content: a}}
}

func TestCompressMessages_NoOpWhenUnderBudget(t *testing.T) {
	conv := Conversation{ID: "c", Messages: userAsst("hi", "yo")}
	out, err := CompressMessages(context.Background(), conv, 1000, 1, lenEstimator, okSummarizer)
	if err != nil {
		t.Fatalf("CompressMessages: %v", err)
	}
	if len(out.Messages) != 2 || out.DurableSummary != nil {
		t.Fatalf("expected unchanged, got msgs=%d summary=%v", len(out.Messages), out.DurableSummary)
	}
}

func TestCompressMessages_EvictsOldestBeyondFloor(t *testing.T) {
	var msgs []Message
	for i := 0; i < 6; i++ { // 6 exchanges
		msgs = append(msgs, userAsst("question that is fairly long", "answer that is fairly long")...)
	}
	conv := Conversation{ID: "c", Messages: msgs}
	// Tight budget + floor of 2 exchanges forces eviction of the older 4.
	out, err := CompressMessages(context.Background(), conv, 40, 2, lenEstimator, okSummarizer)
	if err != nil {
		t.Fatalf("CompressMessages: %v", err)
	}
	if out.DurableSummary == nil {
		t.Fatalf("expected a durable summary")
	}
	if len(out.Messages) >= len(msgs) {
		t.Fatalf("expected eviction, kept %d of %d", len(out.Messages), len(msgs))
	}
	if out.Messages[0].Role != "user" {
		t.Fatalf("retained history must start on a user message, got %q", out.Messages[0].Role)
	}
	if out.DurableSummary.MessageCount != len(msgs)-len(out.Messages) {
		t.Fatalf("MessageCount = %d, want %d", out.DurableSummary.MessageCount, len(msgs)-len(out.Messages))
	}
}

func TestCompressMessages_HardFloorPreservedEvenWhenOverBudget(t *testing.T) {
	var msgs []Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, userAsst("xxxxxxxxxx", "yyyyyyyyyy")...)
	}
	conv := Conversation{ID: "c", Messages: msgs}
	// maxTokens far below the cost of even 3 exchanges; floor must still be kept.
	out, err := CompressMessages(context.Background(), conv, 1, 3, lenEstimator, okSummarizer)
	if err != nil {
		t.Fatalf("CompressMessages: %v", err)
	}
	// Floor = 3 exchanges = 6 messages retained.
	if len(out.Messages) != 6 {
		t.Fatalf("floor not preserved: kept %d, want 6", len(out.Messages))
	}
}

func TestCompressMessages_NeverSplitsToolChain(t *testing.T) {
	// Exchange 1 contains a tool chain; exchange 2 is plain. A boundary that
	// would land mid-chain must instead drop the whole exchange.
	msgs := []Message{
		{Role: "user", Content: "use a tool please now"},
		{Role: "assistant", ToolCalls: []byte(`[{"id":"t1"}]`)},
		{Role: "tool", ToolCallID: "t1", Content: "tool result body"},
		{Role: "assistant", Content: "done with the tool"},
		{Role: "user", Content: "second question here"},
		{Role: "assistant", Content: "second answer here"},
	}
	conv := Conversation{ID: "c", Messages: msgs}
	out, err := CompressMessages(context.Background(), conv, 30, 1, lenEstimator, okSummarizer)
	if err != nil {
		t.Fatalf("CompressMessages: %v", err)
	}
	// The retained span must not begin with an orphan tool/assistant-result.
	if out.Messages[0].Role != "user" && out.Messages[0].Role != "system" {
		t.Fatalf("retained span starts mid-chain: %q", out.Messages[0].Role)
	}
	// If the tool chain is retained at all, its result must be present with it.
	hasToolCall, hasToolResult := false, false
	for _, m := range out.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			hasToolCall = true
		}
		if m.Role == "tool" {
			hasToolResult = true
		}
	}
	if hasToolCall != hasToolResult {
		t.Fatalf("tool chain split: call=%v result=%v", hasToolCall, hasToolResult)
	}
}

func TestCompressMessages_NilSummarizerReturnsError(t *testing.T) {
	conv := Conversation{ID: "c", Messages: userAsst("a", "b")}
	if _, err := CompressMessages(context.Background(), conv, 0, 0, lenEstimator, nil); err == nil {
		t.Fatal("expected error for nil summarizer")
	}
}

func TestCompressMessages_NilEstimatorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for nil estimator")
		}
	}()
	conv := Conversation{ID: "c", Messages: userAsst("a", "b")}
	_, _ = CompressMessages(context.Background(), conv, 0, 0, nil, okSummarizer)
}

func TestCompressMessages_SummarizerErrorLeavesNoLoss(t *testing.T) {
	conv := Conversation{ID: "c", Messages: append(userAsst("a", "b"), userAsst("c", "d")...)}
	boom := func(context.Context, string, []Message) (string, error) {
		return "", errors.New("model down")
	}
	out, err := CompressMessages(context.Background(), conv, 1, 1, lenEstimator, boom)
	if err == nil {
		t.Fatal("expected summarizer error")
	}
	if len(out.Messages) != 0 { // zero-value Conversation on error
		t.Fatalf("expected zero Conversation on error, got %d msgs", len(out.Messages))
	}
}

func TestCompressMessages_BlankSummaryIsError(t *testing.T) {
	conv := Conversation{ID: "c", Messages: append(userAsst("a", "b"), userAsst("c", "d")...)}
	blank := func(context.Context, string, []Message) (string, error) { return "   ", nil }
	_, err := CompressMessages(context.Background(), conv, 1, 1, lenEstimator, blank)
	if !errors.Is(err, ErrEmptySummary) {
		t.Fatalf("want ErrEmptySummary, got %v", err)
	}
}

func TestCompressMessages_RollingConsolidationPassesPrior(t *testing.T) {
	conv := Conversation{
		ID:             "c",
		Messages:       append(userAsst("old1", "old1a"), userAsst("recent", "recenta")...),
		DurableSummary: &DurableSummary{Content: "EARLIER", MessageCount: 4},
	}
	var seenPrior string
	cap := func(_ context.Context, prior string, _ []Message) (string, error) {
		seenPrior = prior
		return "NEWSUMMARY", nil
	}
	out, err := CompressMessages(context.Background(), conv, 1, 1, lenEstimator, cap)
	if err != nil {
		t.Fatalf("CompressMessages: %v", err)
	}
	if seenPrior != "EARLIER" {
		t.Fatalf("prior not passed to summarizer: %q", seenPrior)
	}
	if out.DurableSummary.Content != "NEWSUMMARY" {
		t.Fatalf("summary not replaced: %q", out.DurableSummary.Content)
	}
	if out.DurableSummary.MessageCount != 4+2 {
		t.Fatalf("MessageCount = %d, want 6", out.DurableSummary.MessageCount)
	}
}
