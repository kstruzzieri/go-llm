package conversation

import (
	"context"
	"errors"
	"testing"
	"time"
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
