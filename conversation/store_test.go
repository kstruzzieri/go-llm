package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db := openTestDB(t)
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	return store
}

func TestSave_And_Load_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{
		ID:    NewID(),
		Title: "test conversation",
		Messages: []Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
		},
	}

	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(ctx, conv.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if loaded.ID != conv.ID {
		t.Errorf("ID = %q, want %q", loaded.ID, conv.ID)
	}
	if loaded.Title != conv.Title {
		t.Errorf("Title = %q, want %q", loaded.Title, conv.Title)
	}
	if len(loaded.Messages) != len(conv.Messages) {
		t.Fatalf("Messages len = %d, want %d", len(loaded.Messages), len(conv.Messages))
	}
	for i, msg := range loaded.Messages {
		if msg.Role != conv.Messages[i].Role || msg.Content != conv.Messages[i].Content {
			t.Errorf("Messages[%d] = {%s, %q}, want {%s, %q}",
				i, msg.Role, msg.Content, conv.Messages[i].Role, conv.Messages[i].Content)
		}
	}
	if loaded.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if loaded.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

func TestSave_And_Load_RoundTripWithDurableSummary(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{
		ID:    NewID(),
		Title: "compressed conversation",
		Messages: []Message{
			{Role: "user", Content: "recent question"},
			{Role: "assistant", Content: "recent answer"},
		},
		DurableSummary: &DurableSummary{
			Content:      "Earlier conversation covered repository setup and Golem goals.",
			MessageCount: 6,
		},
	}

	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(ctx, conv.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.DurableSummary == nil {
		t.Fatal("DurableSummary is nil")
	}
	if loaded.DurableSummary.Content != conv.DurableSummary.Content {
		t.Fatalf("DurableSummary.Content = %q, want %q", loaded.DurableSummary.Content, conv.DurableSummary.Content)
	}
	if loaded.DurableSummary.MessageCount != conv.DurableSummary.MessageCount {
		t.Fatalf("DurableSummary.MessageCount = %d, want %d", loaded.DurableSummary.MessageCount, conv.DurableSummary.MessageCount)
	}
	if len(loaded.Messages) != len(conv.Messages) {
		t.Fatalf("Messages len = %d, want %d", len(loaded.Messages), len(conv.Messages))
	}
}

func TestSave_EmptyID_ReturnsError(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.Save(ctx, Conversation{ID: "", Title: "no id"})
	if err == nil {
		t.Fatal("Save() with empty ID should return error")
	}
}

func TestSave_NilMessages_NormalizesToEmptyArray(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{ID: NewID(), Title: "empty"}
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(ctx, conv.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.Messages == nil {
		t.Error("Messages is nil, want empty slice")
	}
	if len(loaded.Messages) != 0 {
		t.Errorf("Messages len = %d, want 0", len(loaded.Messages))
	}
}

func TestSave_Upsert_PreservesCreatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := NewID()
	conv := Conversation{
		ID:       id,
		Title:    "original",
		Messages: []Message{{Role: "user", Content: "v1"}},
	}
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	first, err := store.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	conv.Title = "updated"
	conv.Messages = append(conv.Messages, Message{Role: "assistant", Content: "v2"})
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() update error: %v", err)
	}

	second, err := store.Load(ctx, id)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("CreatedAt changed: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if !second.UpdatedAt.After(first.UpdatedAt) {
		t.Errorf("UpdatedAt did not advance: %v -> %v", first.UpdatedAt, second.UpdatedAt)
	}
	if second.Title != "updated" {
		t.Errorf("Title = %q, want %q", second.Title, "updated")
	}
	if len(second.Messages) != 2 {
		t.Errorf("Messages len = %d, want 2", len(second.Messages))
	}
}

func TestLoad_NotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, err := store.Load(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("Load() should return error for nonexistent ID")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestList_OrderedByUpdatedAtDesc(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ids := make([]string, 3)
	for i := range ids {
		ids[i] = NewID()
		err := store.Save(ctx, Conversation{
			ID:       ids[i],
			Title:    string(rune('A' + i)),
			Messages: []Message{{Role: "user", Content: "msg"}},
		})
		if err != nil {
			t.Fatalf("Save() error: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	summaries, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(summaries) != 3 {
		t.Fatalf("List() len = %d, want 3", len(summaries))
	}

	if summaries[0].ID != ids[2] {
		t.Errorf("summaries[0].ID = %q, want %q", summaries[0].ID, ids[2])
	}
	if summaries[2].ID != ids[0] {
		t.Errorf("summaries[2].ID = %q, want %q", summaries[2].ID, ids[0])
	}

	for _, s := range summaries {
		if s.MessageCount != 1 {
			t.Errorf("summary %q MessageCount = %d, want 1", s.ID, s.MessageCount)
		}
	}
}

func TestDelete_Idempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := NewID()
	if err := store.Save(ctx, Conversation{ID: id, Title: "delete me"}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete() second call error: %v", err)
	}
	_, err := store.Load(ctx, id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after Delete: error = %v, want ErrNotFound", err)
	}
}

func TestSearch_FindsMessageTextWithoutLoadingBlobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{
		ID:    "workspace:alpha",
		Title: "Golem session",
		Messages: []Message{
			{Role: "user", Content: "How do approval prompts work?"},
			{Role: "assistant", Content: "The approval prompt gates writes and exec."},
		},
	}
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if err := store.Save(ctx, Conversation{
		ID:       "workspace:beta",
		Title:    "Unrelated",
		Messages: []Message{{Role: "user", Content: "quantum trading notes"}},
	}); err != nil {
		t.Fatalf("Save() unrelated error: %v", err)
	}

	got, err := store.Search(ctx, "approval prompt", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() len = %d, want 1: %+v", len(got), got)
	}
	if got[0].ID != conv.ID || got[0].Title != conv.Title || got[0].MessageCount != 2 {
		t.Fatalf("Search() result = %+v, want alpha metadata", got[0])
	}
	if got[0].Snippet == "" {
		t.Fatal("Search() result missing snippet")
	}
	if got[0].CreatedAt.IsZero() || got[0].UpdatedAt.IsZero() {
		t.Fatalf("Search() timestamps missing: %+v", got[0])
	}
}

func TestSearch_FindsDurableSummaryText(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	conv := Conversation{
		ID:       "workspace:compressed",
		Title:    "Compressed session",
		Messages: []Message{{Role: "user", Content: "recent turn only"}},
		DurableSummary: &DurableSummary{
			Content:      "Earlier turns discussed frobnicator calibration.",
			MessageCount: 8,
		},
	}
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Search(ctx, "frobnicator", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(got) != 1 || got[0].ID != conv.ID {
		t.Fatalf("Search() = %+v, want compressed session", got)
	}
}

func TestSearch_FindsToolCallPayloads(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.Save(ctx, Conversation{
		ID:    "workspace:toolcall",
		Title: "Tool call session",
		Messages: []Message{
			{Role: "user", Content: "read it"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"read_file","arguments":{"path":"secret.go"}}}]`)},
			{Role: "tool", Content: "package secret", ToolName: "read_file", ToolCallID: "c1"},
			{Role: "assistant", Content: "found it"},
		},
	}); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Search(ctx, "secret.go", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "workspace:toolcall" {
		t.Fatalf("Search() = %+v, want tool-call session", got)
	}
}

func TestSearch_UpdateAndDeleteStayInSync(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	id := "workspace:sync"
	if err := store.Save(ctx, Conversation{
		ID:       id,
		Title:    "Sync",
		Messages: []Message{{Role: "user", Content: "alpha needle"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, Conversation{
		ID:       id,
		Title:    "Sync",
		Messages: []Message{{Role: "user", Content: "bravo needle"}},
	}); err != nil {
		t.Fatal(err)
	}

	oldResults, err := store.Search(ctx, "alpha", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(old) error: %v", err)
	}
	if len(oldResults) != 0 {
		t.Fatalf("old term still indexed after update: %+v", oldResults)
	}
	newResults, err := store.Search(ctx, "bravo", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(new) error: %v", err)
	}
	if len(newResults) != 1 || newResults[0].ID != id {
		t.Fatalf("new term results = %+v, want %q", newResults, id)
	}

	if err := store.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.Search(ctx, "bravo", SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search(after delete) error: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted conversation still indexed: %+v", deleted)
	}
}

func TestSearch_IDPrefixScopeAndLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, conv := range []Conversation{
		{ID: "workspace:a", Title: "A", Messages: []Message{{Role: "user", Content: "shared term"}}},
		{ID: "user:b", Title: "B", Messages: []Message{{Role: "user", Content: "shared term"}}},
		{ID: "user:c", Title: "C", Messages: []Message{{Role: "user", Content: "shared term"}}},
	} {
		if err := store.Save(ctx, conv); err != nil {
			t.Fatalf("Save(%s): %v", conv.ID, err)
		}
	}

	got, err := store.Search(ctx, "shared", SearchOptions{IDPrefix: "user:", Limit: 1})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() len = %d, want 1: %+v", len(got), got)
	}
	if !strings.HasPrefix(got[0].ID, "user:") {
		t.Fatalf("Search() = %+v, want user: scoped result", got)
	}
}

func TestSave_And_Load_WithToolCalls(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	original := []ollama.ChatMessage{
		{Role: "user", Content: "Find files"},
		{
			Role: "assistant",
			ToolCalls: []ollama.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: ollama.ToolCallFunction{
						Index: 0,
						Name:  "find",
						Arguments: map[string]any{
							"pattern": "*.go",
							"depth":   float64(3),
						},
					},
				},
			},
		},
		{
			Role:       "tool",
			Content:    `["main.go", "util.go"]`,
			ToolName:   "find",
			ToolCallID: "call_1",
		},
		{Role: "assistant", Content: "Found 2 Go files."},
	}

	msgs, err := FromChatMessages(original)
	if err != nil {
		t.Fatalf("FromChatMessages() error: %v", err)
	}

	conv := Conversation{
		ID:       NewID(),
		Title:    "tool call test",
		Messages: msgs,
	}
	if err := store.Save(ctx, conv); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	loaded, err := store.Load(ctx, conv.ID)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	restored, err := ToChatMessages(loaded.Messages)
	if err != nil {
		t.Fatalf("ToChatMessages() error: %v", err)
	}

	if len(restored[1].ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(restored[1].ToolCalls))
	}
	tc := restored[1].ToolCalls[0]
	if tc.Function.Name != "find" {
		t.Errorf("Function.Name = %q, want %q", tc.Function.Name, "find")
	}
	args := tc.Function.Arguments
	if args["pattern"] != "*.go" {
		t.Errorf("Arguments[pattern] = %v, want %q", args["pattern"], "*.go")
	}
	if args["depth"] != float64(3) {
		t.Errorf("Arguments[depth] = %v, want %v", args["depth"], float64(3))
	}
	if restored[2].ToolCallID != "call_1" {
		t.Errorf("ToolCallID = %q, want %q", restored[2].ToolCallID, "call_1")
	}
}
