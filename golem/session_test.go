package golem

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestCompactThreadCloseWaitsBeforeClosingResources(t *testing.T) {
	t.Parallel()
	db, err := memory.OpenHardenedDB(context.Background(), filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := conversation.NewStore(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), conversation.Conversation{ID: "thread", Messages: compactExchanges(5)}); err != nil {
		t.Fatal(err)
	}
	started, canceled, release, resourcesClosed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	runtime := &Runtime{
		active: make(map[string]*activeRun), activeThreads: make(map[string]*activeRun),
		// New always publishes a snapshot; a hand-built Runtime must too, since
		// CompactThread now reserves the budget alongside the thread (#376).
		snap:      &turnSnapshot{},
		closeDone: make(chan struct{}), compress: true, sessions: &threadStore{store: store},
		summarizer: func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return "", ctx.Err()
		},
		closeOwned: func() error { close(resourcesClosed); return db.Close() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := runtime.CompactThread(ctx, "thread"); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel compaction")
	}
	select {
	case <-resourcesClosed:
		t.Error("Close released resources while the canceled summarizer was still running")
	case <-time.After(50 * time.Millisecond):
	}
	if err := db.Ping(); err != nil {
		t.Errorf("database closed before summarizer exit: %v", err)
	}
	unblock()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not finish")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("CompactThread = %v, want canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compaction did not finish")
	}
	select {
	case <-resourcesClosed:
	default:
		t.Error("Close did not release owned resources")
	}
}

func TestCompressConversationAutomaticThreshold(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		tokens  int
		summary string
		changed bool
	}{
		{name: "raw exact threshold", tokens: 4096},
		{name: "raw above threshold", tokens: 4097, changed: true},
		{name: "summary exact threshold", tokens: 4055, summary: "SUM"},
		{name: "summary above threshold", tokens: 4056, summary: "SUM", changed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := conversation.Conversation{Messages: []conversation.Message{
				{Role: "user", Content: strings.Repeat("x", (tc.tokens-9)*4)},
				{Role: "assistant", Content: "a"},
			}}
			for range 4 {
				current.Messages = append(current.Messages,
					conversation.Message{Role: "user", Content: "q"},
					conversation.Message{Role: "assistant", Content: "a"})
			}
			if tc.summary != "" {
				current.DurableSummary = &conversation.DurableSummary{Content: tc.summary}
			}
			calls := 0
			r := &Runtime{
				summarizer: func(context.Context, string, []conversation.Message) (string, error) {
					calls++
					return "SUM", nil
				},
			}
			_, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, current, false)
			wantCalls := 0
			if tc.changed {
				wantCalls = 1
			}
			if err != nil || changed != tc.changed || calls != wantCalls {
				t.Errorf("compressConversation(%s) = changed %v, calls %d, error %v; want changed %v and %d calls", tc.name, changed, calls, err, tc.changed, wantCalls)
			}
		})
	}
}

func compactExchanges(count int) []conversation.Message {
	var messages []conversation.Message
	for i := range count {
		messages = append(messages,
			conversation.Message{Role: "user", Content: strings.Repeat(string(rune('a'+i)), 40)},
			conversation.Message{Role: "assistant", Content: strings.Repeat(string(rune('A'+i)), 40)})
	}
	return messages
}

func TestCompressConversationForcedBelowThreshold(t *testing.T) {
	t.Parallel()
	current := conversation.Conversation{ID: "thread", Title: "keep title", Messages: compactExchanges(5),
		CreatedAt: time.Unix(10, 0), UpdatedAt: time.Unix(20, 0)}
	calls := 0
	r := &Runtime{
		summarizer: func(_ context.Context, prior string, messages []conversation.Message) (string, error) {
			calls++
			if prior != "" || !reflect.DeepEqual(messages, current.Messages[:2]) {
				t.Errorf("summarizer input = %q, %+v; want empty prior and oldest exchange %+v", prior, messages, current.Messages[:2])
			}
			return "SUM", nil
		},
	}
	automatic, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, current, false)
	if err != nil || changed || calls != 0 || !reflect.DeepEqual(automatic, current) {
		t.Fatalf("automatic compression = %+v, changed %v, calls %d, error %v; want unchanged input and no call", automatic, changed, calls, err)
	}
	compacted, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, current, true)
	want := current
	want.Messages = current.Messages[2:]
	want.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 2}
	if err != nil || !changed || calls != 1 || !reflect.DeepEqual(compacted, want) {
		t.Fatalf("forced compression = %+v, changed %v, calls %d, error %v; want %+v, changed true, one call", compacted, changed, calls, err, want)
	}
	if before, after := estimateStoredHistory(current), estimateStoredHistory(compacted); before != 100 || after != 121 {
		t.Errorf("stored history estimates = %d -> %d, want 100 -> 121", before, after)
	}
	if current.DurableSummary != nil || len(current.Messages) != 10 {
		t.Errorf("input mutated: %+v", current)
	}
}

func TestEstimateStoredHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		current conversation.Conversation
		want    int
	}{
		{name: "empty", want: 0},
		{name: "four exchanges", current: conversation.Conversation{Messages: compactExchanges(4)}, want: 80},
		{name: "summary", current: conversation.Conversation{Messages: compactExchanges(4), DurableSummary: &conversation.DurableSummary{Content: "SUM"}}, want: 121},
		{name: "trim summary", current: conversation.Conversation{DurableSummary: &conversation.DurableSummary{Content: " \n SUM \t"}}, want: 41},
		{name: "blank summary", current: conversation.Conversation{DurableSummary: &conversation.DurableSummary{Content: " \n\t "}}, want: 0},
		{name: "multibyte", current: conversation.Conversation{Messages: []conversation.Message{{Role: "user", Content: "雪雪雪雪雪"}}}, want: 2},
		// Was 10 before compression counted only replayed text: the tool-call
		// frame and the tool result are stored but never sent back, and the
		// system message was always excluded, so nothing here costs anything.
		{name: "tool metadata is not replayed", current: conversation.Conversation{Messages: []conversation.Message{
			{Role: "system", Content: "ignored", ToolCalls: json.RawMessage(`{"x":123}`), ToolName: "skip", ToolCallID: "skip"},
			{Role: "assistant", ToolCalls: json.RawMessage(`{"x":123}`)},
			{Role: "tool", Content: "雪雪雪雪雪", ToolName: "read_file", ToolCallID: "call1"},
		}}, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := estimateStoredHistory(tc.current); got != tc.want {
				t.Errorf("estimateStoredHistory(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestEstimateStoredHistoryCountsOnlyReplayed pins the trigger to what
// history() replays: user/assistant text. Tool results and tool-call frames
// are stored but never sent back, so they must not trip compression.
func TestEstimateStoredHistoryCountsOnlyReplayed(t *testing.T) {
	t.Parallel()
	frame := json.RawMessage(`{"x":123}`)
	for _, tc := range []struct {
		name string
		msg  conversation.Message
		want int
	}{
		{"tool result", conversation.Message{Role: "tool", Content: strings.Repeat("t", 8000), ToolName: "read_file", ToolCallID: "c1"}, 0},
		{"tool-call frame without text", conversation.Message{Role: "assistant", ToolCalls: frame}, 0},
		{"tool-call frame counts text only", conversation.Message{Role: "assistant", Content: strings.Repeat("a", 40), ToolCalls: frame}, 10},
		{"user text", conversation.Message{Role: "user", Content: strings.Repeat("u", 40)}, 10},
		{"system", conversation.Message{Role: "system", Content: strings.Repeat("s", 40)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := estimateStoredHistory(conversation.Conversation{Messages: []conversation.Message{tc.msg}}); got != tc.want {
				t.Errorf("estimateStoredHistory(%s) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

// TestCompressionDoesNotRewriteSummaryEveryTurn replays 60 tool-heavy turns
// through saveThread. Each turn stores 200 replayed tokens plus a 2000-token
// tool result that history() never sends back. Compaction must evict down to
// the low-water mark so the summary (messages[0] of every request) stays
// still between compactions, instead of being rewritten almost every turn.
func TestCompressionDoesNotRewriteSummaryEveryTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := memory.OpenHardenedDB(ctx, filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	store, err := conversation.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	estimate := conversation.CharRatioEstimator(4)
	runtime := &Runtime{
		onWarning: func(err error) { t.Errorf("compression warning: %v", err) },
		sessions:  &threadStore{store: store},
		compress:  true,
		summarizer: func(_ context.Context, _ string, old []conversation.Message) (string, error) {
			calls++
			// A fold batch spans a dozen turns now that tool results cost
			// nothing for the trigger, so the summarize request must carry
			// replayed history only, never the stored tool output.
			for _, message := range old {
				if !replayed(message) {
					t.Errorf("summarizer call %d received a message history() never replays: role %q, %d content bytes", calls, message.Role, len(message.Content))
				}
			}
			// Bound the request by the unfiltered estimate (what the summarize
			// transcript writes); visibleCost would be 0 for the tool results
			// this guards against. Trigger 4096 plus one 200-token turn.
			if tokens := conversation.EstimateMessagesTokens(old, estimate); tokens > 4096+200 {
				t.Errorf("summarizer call %d input = %d tokens, want at most the trigger plus one turn (4296)", calls, tokens)
			}
			return strings.Repeat("s", 200), nil
		},
	}
	state := &threadState{conversation: conversation.Conversation{ID: "thrash"}}
	question := strings.Repeat("q", 400)
	minReplayed := -1
	for turn := range 60 {
		result := agent.Result{Messages: []provider.ChatMessage{
			{Role: "user", Content: question},
			{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{}`)}}}},
			{Role: "tool", ToolName: "read_file", ToolCallID: "c1", Content: strings.Repeat("t", 8000)},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
		}}
		if err := runtime.saveThread(ctx, &activeRun{}, agent.Budget{InputCeiling: 8192}, state, question, result); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if state.conversation.DurableSummary != nil {
			if exchanges := len(state.history()) / 2; minReplayed < 0 || exchanges < minReplayed {
				minReplayed = exchanges
			}
		}
	}
	if calls < 1 || calls > 6 {
		t.Errorf("summarizer calls over 60 turns = %d, want 1 to 6", calls)
	}
	// Low-water 2048 less the summary reserve leaves room for about 7 exchanges
	// of 200 tokens; a fit still counting the invisible tool results keeps only
	// the 4-exchange floor.
	if minReplayed < 6 {
		t.Errorf("fewest replayed exchanges after the first summary = %d, want at least 6", minReplayed)
	}
}

// TestCompressConversationSummarizerGetsRoleAndTextOnly covers an assistant
// message that carries text beside a tool call. history() replays it as role
// and text, so the summarizer must not receive its 8000-byte tool-call JSON,
// tool name or call ID either: those are unpriced by the trigger and would
// break the request bound the fold relies on.
func TestCompressConversationSummarizerGetsRoleAndTextOnly(t *testing.T) {
	t.Parallel()
	old := []conversation.Message{
		{Role: "user", Content: "write it"},
		{Role: "assistant", Content: "writing the file", ToolName: "write_file", ToolCallID: "c1",
			ToolCalls: json.RawMessage(`[{"id":"c1","function":{"name":"write_file","arguments":{"content":"` + strings.Repeat("w", 8000) + `"}}}]`)},
		{Role: "tool", ToolName: "write_file", ToolCallID: "c1", Content: "ok"},
		{Role: "assistant", Content: "done"},
	}
	// Four 1100-token exchanges (4400 tokens) cross the 4096 trigger and sit at
	// the retention floor, so only the exchange above folds.
	var recent []conversation.Message
	for range 4 {
		recent = append(recent,
			conversation.Message{Role: "user", Content: strings.Repeat("q", 2200)},
			conversation.Message{Role: "assistant", Content: strings.Repeat("a", 2200)})
	}
	current := conversation.Conversation{Messages: append(append([]conversation.Message(nil), old...), recent...)}
	want := []conversation.Message{
		{Role: "user", Content: "write it"},
		{Role: "assistant", Content: "writing the file"},
		{Role: "assistant", Content: "done"},
	}
	calls := 0
	r := &Runtime{summarizer: func(_ context.Context, prior string, messages []conversation.Message) (string, error) {
		calls++
		if prior != "" || !reflect.DeepEqual(messages, want) {
			t.Errorf("summarizer input = %q, %+v; want empty prior and role and text only %+v", prior, messages, want)
		}
		return "SUM", nil
	}}
	got, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, current, false)
	if err != nil || !changed || calls != 1 || !reflect.DeepEqual(got.Messages, recent) || summaryMessageCount(got) != 4 {
		t.Errorf("compression = %d retained, summary %+v, changed %v, calls %d, error %v; want the four recent exchanges, count 4, changed true, one call", len(got.Messages), got.DurableSummary, changed, calls, err)
	}
}

// TestCompressConversationSkipsSummarizerWhenNothingReplayedFolds covers a fold
// batch holding only stored tool traffic (here an exchange whose question and
// answer are empty): the prior summary has nothing new to absorb, so it is kept
// as is and the summarizer, whose rewrite would bust the prompt cache, is not
// called. The folded messages still count toward the summary.
func TestCompressConversationSkipsSummarizerWhenNothingReplayedFolds(t *testing.T) {
	t.Parallel()
	unreplayed := []conversation.Message{
		{Role: "user"},
		{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"c1"}]`)},
		{Role: "tool", ToolName: "read_file", ToolCallID: "c1", Content: strings.Repeat("t", 8000)},
		{Role: "assistant"},
	}
	// Four 1100-token exchanges (550 tokens per 2200-char message) sit at the
	// retention floor and cost 4400 plus the 41-token "OLD" summary prompt,
	// over the 4096 trigger, so compression runs and folds only the first.
	var recent []conversation.Message
	for range 4 {
		recent = append(recent,
			conversation.Message{Role: "user", Content: strings.Repeat("q", 2200)},
			conversation.Message{Role: "assistant", Content: strings.Repeat("a", 2200)})
	}
	current := conversation.Conversation{
		Messages:       append(append([]conversation.Message(nil), unreplayed...), recent...),
		DurableSummary: &conversation.DurableSummary{Content: "OLD", MessageCount: 7},
	}
	calls := 0
	r := &Runtime{summarizer: func(context.Context, string, []conversation.Message) (string, error) {
		calls++
		return "REWRITTEN", nil
	}}
	got, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, current, false)
	if err != nil || !changed || calls != 0 || currentSummary(got) != "OLD" || summaryMessageCount(got) != 11 || !reflect.DeepEqual(got.Messages, recent) {
		t.Errorf("compression = %d retained, summary %+v, changed %v, calls %d, error %v; want the four recent exchanges, summary OLD with count 11, changed true, no summarizer call", len(got.Messages), got.DurableSummary, changed, calls, err)
	}
}

// Eviction now targets the low-water mark (half the 4096 trigger), so the raw
// budget is 2048 less the 552-token reserve (512 output + 40 for the empty
// summary prompt) = 1496. A 280-token exchange (two 140-token messages) fits
// five times (1400) and not six (1680), keeping the newest ten messages; with
// no reserve the budget would be 2048 and seven would fit. 15 exchanges
// (4200 tokens) cross the trigger, so the oldest 30-10 = 20 messages fold.
func TestCompressConversationAutomaticSummaryReserve(t *testing.T) {
	t.Parallel()
	var messages []conversation.Message
	for range 15 {
		messages = append(messages,
			conversation.Message{Role: "user", Content: strings.Repeat("q", 560)},
			conversation.Message{Role: "assistant", Content: strings.Repeat("a", 560)})
	}
	calls := 0
	r := &Runtime{summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
		calls++
		if prior != "" || !reflect.DeepEqual(old, messages[:20]) {
			t.Errorf("automatic summary inputs = %q, %d messages; want empty prior and oldest twenty messages", prior, len(old))
		}
		return "SUM", nil
	}}
	got, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 8192}, conversation.Conversation{Messages: messages}, false)
	if err != nil || !changed || calls != 1 || !reflect.DeepEqual(got.Messages, messages[20:]) || summaryMessageCount(got) != 20 {
		t.Errorf("automatic compression = %d retained, summary %+v, changed %v, calls %d, error %v; want newest ten messages, count 20, changed true, one call", len(got.Messages), got.DurableSummary, changed, calls, err)
	}
}

// Ceiling 16384 puts the trigger at 8192 and the low-water mark at 4096, the
// mark these cases used when eviction still stopped at the trigger of an 8192
// ceiling. 17 exchanges of 512 tokens (8704) cross the trigger. The plain
// summary renders to 552 tokens, so the first pass keeps six exchanges
// (3072 <= 4096-552) and folds 22 messages. The escaped one renders to 1064,
// which leaves 4136 stored > 4096, so the retry's 3032-token budget keeps five
// and folds two more (24 in all).
func TestCompressConversationReservesRenderedSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, summary string
		wantCalls     int
	}{
		{"plain", strings.Repeat("s", 2048), 1},
		{"escaped", strings.Repeat("\\", 2048), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var messages []conversation.Message
			for range 17 {
				messages = append(messages,
					conversation.Message{Role: "user", Content: strings.Repeat("q", 1024)},
					conversation.Message{Role: "assistant", Content: strings.Repeat("a", 1024)})
			}
			calls, summarized := 0, 0
			r := &Runtime{summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
				calls++
				if !reflect.DeepEqual(old, messages[summarized:summarized+len(old)]) || (calls > 1 && prior != tc.summary) {
					t.Errorf("summary pass %d = %q, %+v; want prior summary and next oldest messages", calls, prior, old)
				}
				summarized += len(old)
				return tc.summary, nil
			}}
			current := conversation.Conversation{Messages: messages}
			got, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 16384}, current, false)
			if err != nil || !changed || calls != tc.wantCalls || estimateStoredHistory(got) > 4096 {
				t.Fatalf("compression = changed %v, %v, calls %d, tokens %d; want changed, %d calls and <=4096", changed, err, calls, estimateStoredHistory(got), tc.wantCalls)
			}
			if len(got.Messages) < 8 || !reflect.DeepEqual(got.Messages, messages[summarized:]) || summaryMessageCount(got) != summarized {
				t.Fatalf("compressed history = %+v; want retained suffix and summary count %d", got, summarized)
			}
			again, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 16384}, got, false)
			if err != nil || changed || calls != tc.wantCalls || !reflect.DeepEqual(again, got) {
				t.Errorf("immediate repeat = changed %v, %v, calls %d; want unchanged with no further summary", changed, err, calls)
			}
		})
	}
}

func TestCompressConversationRenderedReserveStopsAtFloor(t *testing.T) {
	t.Parallel()
	current := conversation.Conversation{DurableSummary: &conversation.DurableSummary{Content: "SUM"}}
	for range 4 {
		current.Messages = append(current.Messages,
			conversation.Message{Role: "user", Content: strings.Repeat("q", 2048)},
			conversation.Message{Role: "assistant", Content: strings.Repeat("a", 2048)})
	}
	calls := 0
	r := &Runtime{summarizer: func(context.Context, string, []conversation.Message) (string, error) {
		calls++
		return "SUM", nil
	}}
	got, changed, err := r.compressConversation(context.Background(), agent.Budget{}, current, false)
	if err != nil || changed || calls != 1 || !reflect.DeepEqual(got, current) {
		t.Fatalf("floor compression = %+v, changed %v, %v, calls %d; want unchanged floor and one call", got, changed, err, calls)
	}
}

// Same 16384-ceiling arithmetic as TestCompressConversationReservesRenderedSummary:
// the escaped summary forces a second eviction pass, whose summarizer fails.
func TestCompressConversationRenderedReserveRetryFailure(t *testing.T) {
	t.Parallel()
	var current conversation.Conversation
	for range 17 {
		current.Messages = append(current.Messages,
			conversation.Message{Role: "user", Content: strings.Repeat("q", 1024)},
			conversation.Message{Role: "assistant", Content: strings.Repeat("a", 1024)})
	}
	before := append([]conversation.Message(nil), current.Messages...)
	failure := errors.New("second summary failed")
	calls := 0
	r := &Runtime{summarizer: func(context.Context, string, []conversation.Message) (string, error) {
		calls++
		if calls == 1 {
			return strings.Repeat("\\", 2048), nil
		}
		return "", failure
	}}
	_, changed, err := r.compressConversation(context.Background(), agent.Budget{InputCeiling: 16384}, current, false)
	if !errors.Is(err, failure) || changed || calls != 2 || current.DurableSummary != nil || !reflect.DeepEqual(current.Messages, before) {
		t.Fatalf("retry failure = changed %v, %v, calls %d; want failure with original history intact", changed, err, calls)
	}
}

func TestCompressConversationRetainsToolExchangesAndUnresolvedTail(t *testing.T) {
	t.Parallel()
	old := []conversation.Message{
		{Role: "user", Content: "old question\n"},
		{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"old","function":{"name":"read_file","arguments":{"path":"old.txt"}}}]`)},
		{Role: "tool", ToolName: "read_file", ToolCallID: "old", Content: "old bytes\n"},
		{Role: "assistant", Content: "old answer\n"},
	}
	retained := []conversation.Message{{Role: "system", Content: " preserve this\n"}}
	retained = append(retained, compactExchanges(3)...)
	retained = append(retained,
		conversation.Message{Role: "user", Content: "new question\n"},
		conversation.Message{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"new","function":{"name":"read_file","arguments":{"path":"new.txt"}}}]`)},
		conversation.Message{Role: "tool", ToolName: "read_file", ToolCallID: "new", Content: "new bytes\n"},
		conversation.Message{Role: "assistant", Content: "new answer\n"},
		conversation.Message{Role: "user", Content: "pending question\n"},
		conversation.Message{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"pending","function":{"name":"read_file","arguments":{"path":"pending.txt"}}}]`)},
		conversation.Message{Role: "tool", ToolName: "read_file", ToolCallID: "pending", Content: "pending result\n"})
	current := conversation.Conversation{ID: "thread", Title: "original", Messages: append(append([]conversation.Message(nil), old...), retained...), CreatedAt: time.Unix(10, 0), UpdatedAt: time.Unix(20, 0)}
	calls := 0
	r := &Runtime{summarizer: func(_ context.Context, prior string, messages []conversation.Message) (string, error) {
		calls++
		// The whole old exchange leaves storage together (count 4 below), but
		// the summarizer sees only what history() replayed: the question and
		// the answer, not the tool-call frame or the tool result.
		if replayedOnly := []conversation.Message{old[0], old[3]}; prior != "" || !reflect.DeepEqual(messages, replayedOnly) {
			t.Errorf("summarizer input = %q, %+v; want empty prior and the replayed question and answer %+v", prior, messages, replayedOnly)
		}
		return "SUM", nil
	}}
	want := current
	want.Messages = retained
	want.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 4}
	got, changed, err := r.compressConversation(context.Background(), agent.Budget{}, current, true)
	if err != nil || !changed || calls != 1 || !reflect.DeepEqual(got, want) {
		t.Errorf("forced tool compression = %+v, changed %v, calls %d, error %v; want %+v, changed true, one call", got, changed, calls, err, want)
	}
}

func TestCompressConversationProgressiveSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		exchanges   int
		replacement string
		wantCount   int
		changed     bool
	}{
		{name: "fold oldest pair", exchanges: 5, replacement: "REPLACEMENT", wantCount: 14, changed: true},
		{name: "summary only equal cost", exchanges: 4, replacement: "REVISED", wantCount: 12, changed: true},
		{name: "summary without raw history", replacement: "REPLACEMENT", wantCount: 12, changed: true},
		{name: "identical rewrite", exchanges: 4, replacement: "EARLIER", wantCount: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := conversation.Conversation{Messages: compactExchanges(tc.exchanges), DurableSummary: &conversation.DurableSummary{Content: "EARLIER", MessageCount: 12}}
			calls := 0
			r := &Runtime{summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
				calls++
				if prior != "EARLIER" {
					t.Errorf("summarizer prior = %q, want EARLIER", prior)
				}
				if tc.exchanges == 5 {
					if !reflect.DeepEqual(old, current.Messages[:2]) {
						t.Errorf("summarizer messages = %+v, want oldest pair %+v", old, current.Messages[:2])
					}
				} else if len(old) != 0 {
					t.Errorf("summarizer messages = %+v, want no newly evicted messages", old)
				}
				return tc.replacement, nil
			}}
			got, changed, err := r.compressConversation(context.Background(), agent.Budget{}, current, true)
			if err != nil || changed != tc.changed || calls != 1 {
				t.Fatalf("compressConversation(%s) = changed %v, calls %d, error %v; want changed %v, one call", tc.name, changed, calls, err, tc.changed)
			}
			wantMessages := current.Messages
			if tc.exchanges == 5 {
				wantMessages = current.Messages[2:]
			}
			if !reflect.DeepEqual(got.DurableSummary, &conversation.DurableSummary{Content: tc.replacement, MessageCount: tc.wantCount}) || len(got.Messages) != len(wantMessages) || (len(wantMessages) > 0 && !reflect.DeepEqual(got.Messages, wantMessages)) {
				t.Errorf("compressConversation(%s) = %+v; want retained messages %+v, summary %q, count %d", tc.name, got, wantMessages, tc.replacement, tc.wantCount)
			}
			if current.DurableSummary.Content != "EARLIER" || current.DurableSummary.MessageCount != 12 {
				t.Errorf("input summary mutated: %+v", current.DurableSummary)
			}
		})
	}
}

func TestCompressConversationForcedNoOpAtFloor(t *testing.T) {
	t.Parallel()
	for count := 0; count <= 4; count++ {
		current := conversation.Conversation{Messages: compactExchanges(count)}
		r := &Runtime{summarizer: func(context.Context, string, []conversation.Message) (string, error) {
			t.Errorf("forced compression with %d exchanges called summarizer; want no call", count)
			return "SUM", nil
		}}
		got, changed, err := r.compressConversation(context.Background(), agent.Budget{}, current, true)
		if err != nil || changed || !reflect.DeepEqual(got, current) {
			t.Errorf("forced compression with %d exchanges = %+v, changed %v, error %v; want unchanged %+v", count, got, changed, err, current)
		}
	}
}

func TestCompressConversationSummaryFailure(t *testing.T) {
	t.Parallel()
	modelErr := errors.New("model unavailable")
	for _, tc := range []struct {
		name    string
		output  string
		err     error
		wantErr error
	}{
		{name: "model failure", err: modelErr, wantErr: modelErr},
		{name: "blank output", output: " \t\n ", wantErr: conversation.ErrEmptySummary},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := conversation.Conversation{Messages: compactExchanges(5), DurableSummary: &conversation.DurableSummary{Content: "EARLIER", MessageCount: 12}}
			before, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			r := &Runtime{summarizer: func(context.Context, string, []conversation.Message) (string, error) { return tc.output, tc.err }}
			_, changed, err := r.compressConversation(context.Background(), agent.Budget{}, current, true)
			if !errors.Is(err, tc.wantErr) || changed {
				t.Errorf("compressConversation(%s) = changed %v, error %v; want unchanged and %v", tc.name, changed, err, tc.wantErr)
			}
			after, err := json.Marshal(current)
			if err != nil || string(after) != string(before) {
				t.Errorf("input after failed compression = %s, error %v; want %s", after, err, before)
			}
		})
	}
}

func TestResultMessagesRedactsMemoryData(t *testing.T) {
	const secret = "do not persist this"
	result := agent.Result{Messages: []provider.ChatMessage{
		{Role: "user", Content: "question"},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      agenttools.AgentMemoryCreateToolName,
					Arguments: json.RawMessage(`{"content":"` + secret + `"}`),
				},
			}},
		},
		{Role: "tool", ToolName: agenttools.AgentMemoryCreateToolName, ToolCallID: "call-1", Content: secret},
		{Role: "tool", ToolName: agenttools.MemorySearchToolName, ToolCallID: "call-2", Content: secret},
	}}

	messages, err := resultMessages("question", result)
	if err != nil {
		t.Fatalf("resultMessages: %v", err)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal persisted messages: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("persisted messages contain memory data: %s", raw)
	}
}

func TestRedactAgentMemoryToolCallsPreservesInput(t *testing.T) {
	const secret = "SECRET NOTE CONTENT"
	orig := []provider.ToolCall{
		{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: agenttools.AgentMemoryCreateToolName, Arguments: json.RawMessage(`{"content":"` + secret + `"}`)}},
		{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"keep.txt"}`)}},
	}
	got := redactAgentMemoryToolCalls(orig)
	if string(got[0].Function.Arguments) != agentMemoryArgsRedactedMarker {
		t.Errorf("memory call args = %s", got[0].Function.Arguments)
	}
	if !json.Valid(got[0].Function.Arguments) {
		t.Error("redacted args must remain valid JSON")
	}
	if string(got[1].Function.Arguments) != `{"path":"keep.txt"}` {
		t.Errorf("non-memory call mutated: %s", got[1].Function.Arguments)
	}
	if !strings.Contains(string(orig[0].Function.Arguments), secret) {
		t.Error("input slice was mutated; the live turn owns it")
	}
	// no memory calls => same backing array back, no copy churn
	plain := []provider.ToolCall{{Function: provider.ToolCallFunction{Name: "read_file"}}}
	if out := redactAgentMemoryToolCalls(plain); &out[0] != &plain[0] {
		t.Error("expected pass-through when nothing matches")
	}
}

func TestResultMessagesRedactsAgentMemoryMarkers(t *testing.T) {
	const retrieved = "RETRIEVED RECORD ROWS"
	const created = "CREATED NOTE ARGS"
	res := agent.Result{
		Answer: "ok",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "q"},
			{Role: "assistant", ToolCalls: []provider.ToolCall{
				{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: agenttools.AgentMemoryCreateToolName, Arguments: json.RawMessage(`{"content":"` + created + `"}`)}},
				{ID: "c2", Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"keep.txt"}`)}},
			}},
			{Role: "tool", ToolName: agenttools.AgentMemoryCreateToolName, ToolCallID: "c1", Content: "recorded rec1 (working)"},
			{Role: "tool", ToolName: "read_file", ToolCallID: "c2", Content: "file body"},
			{Role: "tool", ToolName: agenttools.AgentMemorySearchToolName, ToolCallID: "c3", Content: retrieved},
			{Role: "tool", ToolName: agenttools.MemorySearchToolName, ToolCallID: "c4", Content: retrieved},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultMessages("q", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	var searchRedacted, createRedacted, argsRedacted, memSearchRedacted bool
	for _, m := range msgs {
		if strings.Contains(m.Content, retrieved) || strings.Contains(string(m.ToolCalls), created) {
			t.Fatalf("memory payload leaked into persisted history: %+v", m)
		}
		if m.ToolName == agenttools.AgentMemorySearchToolName && m.Content == agentMemoryRedactedMarker {
			searchRedacted = true
		}
		if m.ToolName == agenttools.AgentMemoryCreateToolName && m.Content == agentMemoryRedactedMarker {
			createRedacted = true
		}
		if m.ToolName == agenttools.MemorySearchToolName && m.Content == memorySearchRedactedMarker {
			memSearchRedacted = true
		}
		if len(m.ToolCalls) > 0 {
			if !strings.Contains(string(m.ToolCalls), "keep.txt") {
				t.Errorf("non-memory tool call args lost: %s", m.ToolCalls)
			}
			if strings.Contains(string(m.ToolCalls), "agent memory arguments omitted") {
				argsRedacted = true
			}
		}
		if m.ToolName == "read_file" && m.Content != "file body" {
			t.Errorf("non-memory tool result mutated: %q", m.Content)
		}
	}
	if !searchRedacted || !createRedacted || !argsRedacted || !memSearchRedacted {
		t.Errorf("markers missing: agentSearch=%v create=%v args=%v memSearch=%v",
			searchRedacted, createRedacted, argsRedacted, memSearchRedacted)
	}
	// live Result untouched
	if !strings.Contains(string(res.Messages[1].ToolCalls[0].Function.Arguments), created) {
		t.Error("live agent.Result mutated by persistence mapping")
	}
}

// TestResultMessagesReplacesGoalWithRawUserMessage pins that host-supplied
// ContextItems (embedded into the orchestrator goal) never persist: the first
// user message is stored as the raw Turn.Message, not the composed goal.
func TestResultMessagesReplacesGoalWithRawUserMessage(t *testing.T) {
	const contextValue = "CONTEXT FILE CONTENTS"
	goal := "question" + contextDelimiter + `[{"description":"file","value":"` + contextValue + `"}]`
	res := agent.Result{
		Answer: "ok",
		Messages: []provider.ChatMessage{
			{Role: "user", Content: goal},
			{Role: "assistant", Content: "ok"},
		},
	}
	msgs, err := resultMessages("question", res)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if msgs[0].Content != "question" {
		t.Fatalf("persisted user message = %q, want raw message", msgs[0].Content)
	}
	raw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), contextValue) {
		t.Fatalf("turn context leaked into persisted history: %s", raw)
	}
}

func TestDefaultSessionDBPathRejectsSymlinkIntoWorkspace(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink data directory: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", link)

	if _, err := defaultSessionDBPath(root); err == nil {
		t.Fatal("want symlinked session database inside workspace to be rejected")
	}
}

func TestConcurrentCloseWaitsForResourceShutdown(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	runtime := &Runtime{
		active:        make(map[string]*activeRun),
		activeThreads: make(map[string]*activeRun),
		closeOwned: func() error {
			close(started)
			<-release
			return nil
		},
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- runtime.Close() }()
	<-started

	secondDone := make(chan error, 1)
	go func() { secondDone <- runtime.Close() }()
	select {
	case err := <-secondDone:
		t.Fatalf("second Close returned before resources closed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSaveThreadRetainsCommittedRevision(t *testing.T) {
	t.Parallel()
	for _, compress := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "compressed"}[compress], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db, err := memory.OpenHardenedDB(ctx, filepath.Join(t.TempDir(), "sessions.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			store, err := conversation.NewStore(ctx, db)
			if err != nil {
				t.Fatal(err)
			}
			current := conversation.Conversation{ID: "retained"}
			for range 5 {
				current.Messages = append(current.Messages, conversation.Message{Role: "user", Content: "old question"}, conversation.Message{Role: "assistant", Content: "old answer"})
			}
			if _, err := store.Save(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE conversations SET revision = 17 WHERE id = 'retained'`); err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(ctx, current.ID); err != nil {
				t.Fatal(err)
			}
			state := &threadState{conversation: current}
			var warnings []error
			runtime := &Runtime{onWarning: func(err error) { warnings = append(warnings, err) }, sessions: &threadStore{store: store}, compress: compress, summarizer: func(context.Context, string, []conversation.Message) (string, error) { return "summary", nil }}
			if err := runtime.saveThread(ctx, &activeRun{}, agent.Budget{InputCeiling: 2}, state, "new question", agent.Result{Answer: "new answer"}); err != nil {
				t.Fatal(err)
			}
			want := int64(18)
			if compress {
				want = 19
			}
			if len(warnings) != 0 {
				t.Fatalf("compression warnings: %v", warnings)
			}
			saved, err := store.Load(ctx, current.ID)
			if err != nil || state.conversation.Revision != want || saved.Revision != want || !reflect.DeepEqual(state.conversation.Messages, saved.Messages) || !reflect.DeepEqual(state.conversation.DurableSummary, saved.DurableSummary) {
				t.Fatalf("retained = %+v, persisted %+v, %v; want revision %d", state.conversation, saved, err, want)
			}
			// Reusing the retained value must be safe; fetching a new token onto stale content is forbidden.
			if err := runtime.saveThread(ctx, &activeRun{}, agent.Budget{InputCeiling: 2}, state, "later question", agent.Result{Answer: "later answer"}); err != nil {
				t.Fatalf("save retained snapshot: %v", err)
			}
			if len(warnings) != 0 {
				t.Fatalf("retained save warnings: %v", warnings)
			}
		})
	}
}
