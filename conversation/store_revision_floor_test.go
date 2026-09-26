package conversation

import (
	"context"
	"testing"
)

func TestSaveCAS_DeleteRecreate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.Save(ctx, Conversation{ID: "reused", Title: "oldtitle", Messages: []Message{{Role: "user", Content: "oldtoken"}}}); err != nil {
		t.Fatal(err)
	}
	stale, err := store.Load(ctx, "reused")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, Conversation{ID: "reused", Title: "newtitle", Messages: []Message{{Role: "user", Content: "newtoken"}}, DurableSummary: &DurableSummary{Content: "newsummary", MessageCount: 7}}); err != nil {
		t.Fatal(err)
	}
	before := storedCASState(t, store)
	stale.Messages = append(stale.Messages, Message{Role: "assistant", Content: "staletoken"})
	_, err = store.Save(ctx, *stale)
	requireConflict(t, err, stale.ID, stale.Revision)
	if got := storedCASState(t, store); got != before {
		t.Fatalf("stale save changed winner: %s, want %s", got, before)
	}
	for term, want := range map[string]int{"newtitle": 1, "newtoken": 1, "newsummary": 1, "oldtitle": 0, "oldtoken": 0, "staletoken": 0} {
		hits, err := store.Search(ctx, term, SearchOptions{})
		if err != nil || len(hits) != want {
			t.Errorf("Search(%q) = %v, %v; want %d hits", term, hits, err, want)
		}
	}
}
