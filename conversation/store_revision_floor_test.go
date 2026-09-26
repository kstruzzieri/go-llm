package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestRevisionFloor_ReopenCycles(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cycles.db")
	var stale []Conversation
	for cycle := int64(1); cycle <= 4; cycle++ {
		db := openMigrationDB(t, path)
		store, err := NewStore(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		candidate := Conversation{ID: "reused", Title: fmt.Sprintf("title%d", cycle), Messages: []Message{{Role: "user", Content: fmt.Sprintf("message%d", cycle)}}, DurableSummary: &DurableSummary{Content: fmt.Sprintf("summary%d", cycle), MessageCount: 3}}
		input, _ := json.Marshal(candidate)
		revision, err := store.Save(ctx, candidate)
		if err != nil || revision != cycle {
			t.Fatalf("create = %d, %v; want %d", revision, err, cycle)
		}
		after, _ := json.Marshal(candidate)
		if string(after) != string(input) {
			t.Fatal("Save mutated input")
		}
		loaded, err := store.Load(ctx, candidate.ID)
		if err != nil || loaded.Revision != revision {
			t.Fatalf("Load = %+v, %v; want revision %d", loaded, err, revision)
		}
		before := storedCASState(t, store)
		for _, old := range stale {
			revision, err := store.Save(ctx, old)
			requireConflict(t, err, old.ID, old.Revision)
			if revision != 0 || storedCASState(t, store) != before {
				t.Fatal("stale save changed durable state or returned a revision")
			}
		}
		for n := int64(1); n <= cycle; n++ {
			for _, prefix := range []string{"title", "message", "summary"} {
				term := fmt.Sprintf("%s%d", prefix, n)
				hits, err := store.Search(ctx, term, SearchOptions{})
				want := 0
				if n == cycle {
					want = 1
				}
				if err != nil || len(hits) != want {
					t.Fatalf("Search(%s) = %v, %v; want %d", term, hits, err, want)
				}
			}
		}
		stale = append(stale, *loaded)
		if err := store.Delete(ctx, candidate.ID); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRevisionFloor_DeleteRollbackAndMaximum(t *testing.T) {
	store := openCASHandles(t)[0]
	// A second connection makes an accidental s.db.Exec deletion observable.
	store.db.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for _, id := range []string{"high", "low"} {
		if _, err := store.Save(ctx, Conversation{ID: id, Messages: []Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
	}
	execMigrationSQL(t, store.db, `UPDATE conversations SET revision = 17 WHERE id = 'high'`)
	before := storedCASState(t, store)
	execMigrationSQL(t, store.db, `CREATE TRIGGER refuse_delete_search BEFORE DELETE ON conversation_search BEGIN SELECT RAISE(ABORT, 'projection deletion failed'); END`)
	if err := store.Delete(ctx, "high"); err == nil || !strings.Contains(err.Error(), "projection deletion failed") {
		t.Fatalf("Delete = %v, want projection failure", err)
	}
	if storedCASState(t, store) != before {
		t.Fatal("failed Delete changed snapshot, floor, or search")
	}
	execMigrationSQL(t, store.db, `DROP TRIGGER refuse_delete_search`)
	for _, id := range []string{"high", "low", "low", "absent"} {
		if err := store.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		assertMigrationSQL(t, store.db, `SELECT value FROM conversation_revision_floor`, "17")
	}
	assertMigrationSQL(t, store.db, `SELECT COUNT(*) FROM conversation_revision_floor`, "1")
	if revision, err := store.Save(ctx, Conversation{ID: "high"}); err != nil || revision != 18 {
		t.Fatalf("recreate = %d, %v; want 18", revision, err)
	}
}

func TestRevisionFloor_Exhaustion(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if _, err := store.Save(ctx, Conversation{ID: "low"}); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, store.db, `UPDATE conversation_revision_floor SET value = 9223372036854775806`)
	if revision, err := store.Save(ctx, Conversation{ID: "max"}); err != nil || revision != math.MaxInt64 {
		t.Fatalf("create = %d, %v; want max", revision, err)
	}
	max, err := store.Load(ctx, "max")
	if err != nil || max.Revision != math.MaxInt64 {
		t.Fatalf("Load(max) = %+v, %v", max, err)
	}
	before := storedCASState(t, store)
	if revision, err := store.Save(ctx, *max); revision != 0 || err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "invalid revision") {
		t.Fatalf("update max = %d, %v; want validation error", revision, err)
	}
	if storedCASState(t, store) != before {
		t.Fatal("invalid update changed storage")
	}
	if err := store.Delete(ctx, "max"); err != nil {
		t.Fatal(err)
	}
	assertMigrationSQL(t, store.db, `SELECT value FROM conversation_revision_floor`, "9223372036854775807")
	before = storedCASState(t, store)
	if revision, err := store.Save(ctx, Conversation{ID: "absent"}); revision != 0 || err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "revision exhausted") {
		t.Fatalf("exhausted create = %d, %v; want exhaustion", revision, err)
	}
	revision, err := store.Save(ctx, Conversation{ID: "low"})
	requireConflict(t, err, "low", 0)
	if revision != 0 || storedCASState(t, store) != before {
		t.Fatal("failed creation changed storage or returned revision")
	}
	assertMigrationSQL(t, store.db, `SELECT typeof(value) FROM conversation_revision_floor`, "integer")
	if revision, err := store.Save(ctx, Conversation{ID: "low", Revision: 1}); err != nil || revision != 2 {
		t.Fatalf("lower update = %d, %v; want 2", revision, err)
	}
}

func TestRevisionFloor_MissingMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if _, err := store.Save(ctx, Conversation{ID: "live"}); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, store.db, `DELETE FROM conversation_revision_floor`)
	before := storedCASState(t, store)
	if revision, err := store.Save(ctx, Conversation{ID: "new"}); revision != 0 || err == nil || errors.Is(err, ErrConflict) {
		t.Fatalf("Save without floor = %d, %v", revision, err)
	}
	if err := store.Delete(ctx, "live"); err == nil || !strings.Contains(err.Error(), "revision floor missing") {
		t.Fatalf("Delete without floor = %v", err)
	}
	if _, err := store.db.Exec(legacyV03Save, "new", "obsolete", `[]`, "", 0, 1234, 2345); err == nil || !strings.Contains(err.Error(), "revision floor missing") {
		t.Fatalf("legacy insert without floor = %v", err)
	}
	if storedCASState(t, store) != before {
		t.Fatal("missing floor was repaired or live data changed")
	}
}

func TestRevisionFloor_CreateRollback(t *testing.T) {
	store := newTestStore(t)
	ctx := t.Context()
	if _, err := store.Save(ctx, Conversation{ID: "reused"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	before := storedCASState(t, store)
	execMigrationSQL(t, store.db, `CREATE TRIGGER refuse_create_search BEFORE INSERT ON conversation_search BEGIN SELECT RAISE(ABORT, 'projection creation failed'); END`)
	if revision, err := store.Save(ctx, Conversation{ID: "reused"}); revision != 0 || err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "projection creation failed") {
		t.Fatalf("Save = %d, %v; want projection failure", revision, err)
	}
	if storedCASState(t, store) != before {
		t.Fatal("failed creation changed durable state")
	}
	execMigrationSQL(t, store.db, `DROP TRIGGER refuse_create_search`)
	if revision, err := store.Save(ctx, Conversation{ID: "reused"}); err != nil || revision != 2 {
		t.Fatalf("retry = %d, %v; want 2", revision, err)
	}
	before = storedCASState(t, store)
	// An obsolete retained caller increments 0 to 1 after creation.
	revision, err := store.Save(ctx, Conversation{ID: "reused", Revision: 1, Title: "obsolete"})
	requireConflict(t, err, "reused", 1)
	if revision != 0 || storedCASState(t, store) != before {
		t.Fatal("obsolete caller changed winner")
	}
}

// Copied verbatim from v0.2.0 conversation/store.go:66.
const legacyV02Save = `INSERT INTO conversations (id, title, messages, summary_content, summary_message_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title                 = excluded.title,
			messages              = excluded.messages,
			summary_content       = excluded.summary_content,
			summary_message_count = excluded.summary_message_count,
			updated_at            = excluded.updated_at`

// Copied verbatim from v0.3.0 conversation/store.go:85.
const legacyV03Save = `INSERT INTO conversations (id, title, messages, summary_content, summary_message_count, revision, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, 1, ?, ?)
			 ON CONFLICT(id) DO NOTHING`

// Copied verbatim from v0.3.0 conversation/store.go:92.
const legacyV03Update = `UPDATE conversations SET title = ?, messages = ?, summary_content = ?, summary_message_count = ?,
			 revision = revision + 1, updated_at = ?
			 WHERE id = ? AND revision = ?`

// Copied verbatim from v0.3.0 conversation/store.go:236.
const legacyV03Delete = `DELETE FROM conversations WHERE id = ?`

func TestRevisionFloor_LegacyWriter(t *testing.T) {
	for _, floor := range []int64{17, math.MaxInt64} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) {
			store := newTestStore(t)
			ctx := t.Context()
			if _, err := store.Save(ctx, Conversation{ID: "live", Title: "winner"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE conversations SET revision = ?`, floor); err != nil {
				t.Fatal(err)
			}
			// Released Delete SQL must capture the old revision even without the new Go caller.
			if _, err := store.db.Exec(legacyV03Delete, "live"); err != nil {
				t.Fatal(err)
			}
			assertMigrationSQL(t, store.db, `SELECT value FROM conversation_revision_floor`, fmt.Sprint(floor))
			// Recreate the ID so obsolete upserts
			// must fail before ON CONFLICT can overwrite its content.
			if floor < math.MaxInt64 {
				if _, err := store.Save(ctx, Conversation{ID: "live", Title: "winner"}); err != nil {
					t.Fatal(err)
				}
			}
			before := storedCASState(t, store)
			for _, query := range []string{legacyV02Save, legacyV03Save} {
				for _, id := range []string{"live", "absent"} {
					_, err := store.db.Exec(query, id, "obsolete", `[]`, "", 0, 1234, 2345)
					if err == nil || !strings.Contains(err.Error(), "conversation store upgraded (#542): upgrade go-llm/golem to write") {
						t.Fatalf("legacy write = %v; want upgrade error", err)
					}
					if storedCASState(t, store) != before {
						t.Fatal("legacy write changed durable state")
					}
				}
			}
			if floor < math.MaxInt64 {
				result, err := store.db.Exec(legacyV03Update, "updated", `[]`, "", 0, 3456, "live", floor+1)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatalf("legacy CAS update = %d, %v", n, err)
				}
			}
		})
	}
}
