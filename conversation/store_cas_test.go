package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func requireConflict(t *testing.T, err error, id string, revision int64) {
	t.Helper()
	wrapped := fmt.Errorf("caller: %w", err)
	var conflict *ConflictError
	if !errors.Is(wrapped, ErrConflict) || !errors.As(wrapped, &conflict) {
		t.Fatalf("Save(%q, revision %d) error = %v, want wrapped typed conflict", id, revision, err)
	}
	if conflict.ID != id || conflict.ExpectedRevision != revision {
		t.Errorf("ConflictError = %+v, want ID %q and ExpectedRevision %d", conflict, id, revision)
	}
}

// Capture all durable data, including projection timestamps and FTS rows.
func storedCASState(t *testing.T, store *SQLiteStore) string {
	t.Helper()
	rows, err := store.db.Query(`SELECT json_array(id,title,messages,summary_content,summary_message_count,revision,created_at,updated_at) FROM conversations
 UNION ALL SELECT json_array(id,title,body,message_count,created_at,updated_at) FROM conversation_search
 UNION ALL SELECT json_array(id,title,body) FROM conversation_fts`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var data []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		data = append(data, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(data, "\n")
}

func TestConflictError(t *testing.T) {
	err := &ConflictError{ID: "workspace:alpha", ExpectedRevision: 7}
	requireConflict(t, err, "workspace:alpha", 7)
	const want = `conversation: save "workspace:alpha": revision 7 conflict; snapshot not saved`
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestSaveCAS_RevisionsAndCreationTimes(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	initial := Conversation{ID: "cas", Title: "original", Messages: []Message{{Role: "user", Content: "originaltoken"}}, DurableSummary: &DurableSummary{Content: "summarytoken", MessageCount: 4}}
	beforeInput, _ := json.Marshal(initial)
	if err := store.Save(ctx, initial); err != nil {
		t.Fatal(err)
	}
	afterInput, _ := json.Marshal(initial)
	if string(afterInput) != string(beforeInput) {
		t.Errorf("Save mutated input: %s, want %s", afterInput, beforeInput)
	}
	loaded, err := store.Load(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 {
		t.Fatalf("create revision = %d, want 1", loaded.Revision)
	}
	// Pin old timestamps without clock sleeps so accidental rewrites cannot pass.
	for _, table := range []string{"conversations", "conversation_search"} {
		if _, err := store.db.Exec(`UPDATE ` + table + ` SET created_at = 1234, updated_at = 2345`); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []struct {
		created      time.Time
		wantRevision int64
	}{{time.Time{}, 2}, {time.UnixMilli(9999), 3}} {
		loaded, err = store.Load(ctx, "cas")
		if err != nil {
			t.Fatal(err)
		}
		loaded.CreatedAt = step.created
		beforeInput, _ = json.Marshal(loaded)
		if err := store.Save(ctx, *loaded); err != nil {
			t.Fatal(err)
		}
		afterInput, _ = json.Marshal(loaded)
		if string(afterInput) != string(beforeInput) {
			t.Errorf("Save mutated loaded input: %s, want %s", afterInput, beforeInput)
		}
		got, err := store.Load(ctx, "cas")
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != step.wantRevision {
			t.Errorf("identical-content save revision = %d, want %d", got.Revision, step.wantRevision)
		}
		if got.CreatedAt.UnixMilli() != 1234 || got.UpdatedAt.UnixMilli() <= 2345 {
			t.Errorf("saved timestamps = %v/%v, want original creation and advanced update", got.CreatedAt, got.UpdatedAt)
		}
		hits, err := store.Search(ctx, "summarytoken", SearchOptions{})
		if err != nil || len(hits) != 1 {
			t.Fatalf("Search(summarytoken) = %+v, %v, want one hit", hits, err)
		}
		if hits[0].CreatedAt.UnixMilli() != 1234 || !hits[0].UpdatedAt.Equal(got.UpdatedAt) {
			t.Errorf("search timestamps = %v/%v, want 1234 and %v", hits[0].CreatedAt, hits[0].UpdatedAt, got.UpdatedAt)
		}
	}
}

func TestSaveCAS_ConflictsPreserveState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision int64
		remove   bool
	}{
		{"duplicate create", 0, false}, {"stale update", 1, false}, {"absent update", 1, true}, {"deleted loaded snapshot", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			candidate := &Conversation{ID: "cas", Revision: 1}
			if tc.name != "absent update" {
				if err := store.Save(ctx, Conversation{ID: "cas", Title: "winner", Messages: []Message{{Role: "user", Content: "winnertoken"}}, DurableSummary: &DurableSummary{Content: "winnersummary", MessageCount: 3}}); err != nil {
					t.Fatal(err)
				}
				var err error
				candidate, err = store.Load(ctx, "cas")
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.remove {
				if err := store.Delete(ctx, "cas"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "stale update" {
				if _, err := store.db.Exec(`UPDATE conversations SET revision = 2`); err != nil {
					t.Fatal(err)
				}
			}
			candidate.Revision = tc.revision
			candidate.Title = "losertitle"
			candidate.Messages = []Message{{Role: "user", Content: "losertoken"}}
			candidate.DurableSummary = &DurableSummary{Content: "losersummary", MessageCount: 9}
			candidate.CreatedAt = time.UnixMilli(9999)
			before := storedCASState(t, store)
			// Any projection write before classification must fail with SQL, not conflict.
			if _, err := store.db.Exec(`CREATE TRIGGER refuse_search BEFORE INSERT ON conversation_search BEGIN SELECT RAISE(ABORT, 'unexpected projection write'); END`); err != nil {
				t.Fatal(err)
			}
			requireConflict(t, store.Save(ctx, *candidate), "cas", tc.revision)
			if got := storedCASState(t, store); got != before {
				t.Errorf("conflicting Save changed storage:\n%s\nwant:\n%s", got, before)
			}
		})
	}
}

func TestSaveCAS_RevisionLimits(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Conversation{ID: "cas", Title: "original"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cas", "absent"} {
		for _, revision := range []int64{-1, math.MaxInt64} {
			before := storedCASState(t, store)
			err := store.Save(ctx, Conversation{ID: id, Revision: revision, Title: "invalid"})
			if err == nil || errors.Is(err, ErrConflict) {
				t.Errorf("Save(%q, %d) = %v, want non-conflict validation error", id, revision, err)
			}
			if got := storedCASState(t, store); got != before {
				t.Errorf("invalid Save changed state: %s, want %s", got, before)
			}
		}
	}
	if _, err := store.db.Exec(`UPDATE conversations SET revision = ? WHERE id = 'cas'`, int64(math.MaxInt64-1)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, *loaded); err != nil {
		t.Fatalf("Save(max-minus-one) = %v", err)
	}
	loaded, err = store.Load(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != math.MaxInt64 {
		t.Fatalf("Load revision = %d, want max int64", loaded.Revision)
	}
	before := storedCASState(t, store)
	if err := store.Save(ctx, *loaded); err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("Save(max) = %v, want validation error", err)
	}
	if got := storedCASState(t, store); got != before {
		t.Errorf("Save(max) changed state: %s, want %s", got, before)
	}
}

func openCASHandles(t *testing.T) [2]*SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cas.db")
	var stores [2]*SQLiteStore
	for i := range stores {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
			if _, err := db.Exec(pragma); err != nil {
				t.Fatal(err)
			}
		}
		stores[i], err = NewStore(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
	}
	return stores
}

func TestSaveCAS_TwoHandles(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprintf("create=%t", create), func(t *testing.T) {
			stores := openCASHandles(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if !create {
				if err := stores[0].Save(ctx, Conversation{ID: "race", Title: "base", Messages: []Message{{Role: "user", Content: "base"}}}); err != nil {
					t.Fatal(err)
				}
			}
			candidates := [2]Conversation{
				{ID: "race", Title: "ambertitle", Messages: []Message{{Role: "user", Content: "ambermessage"}, {Role: "assistant", Content: "amberanswer"}}, DurableSummary: &DurableSummary{Content: "ambersummary", MessageCount: 5}},
				{ID: "race", Title: "violettitle", Messages: []Message{{Role: "user", Content: "violetmessage"}, {Role: "assistant", Content: "violetanswer"}}, DurableSummary: &DurableSummary{Content: "violetsummary", MessageCount: 7}},
			}
			ready := make(chan error, 2)
			release := make(chan struct{})
			var releaseOnce sync.Once
			start := func() { releaseOnce.Do(func() { close(release) }) }
			type outcome struct {
				worker int
				err    error
			}
			outcomes := make(chan outcome, 2)
			var workers sync.WaitGroup
			workers.Add(2)
			done := make(chan struct{})
			t.Cleanup(func() {
				cancel()
				start()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("workers failed to drain")
				}
			})
			for i, store := range stores {
				go func() {
					defer workers.Done()
					loaded, err := store.Load(ctx, "race")
					candidate := candidates[i]
					if create {
						if !errors.Is(err, ErrNotFound) {
							ready <- fmt.Errorf("Load before create = %+v, %v, want ErrNotFound", loaded, err)
							return
						}
					} else {
						if err != nil {
							ready <- err
							return
						}
						if loaded.Revision != 1 {
							ready <- fmt.Errorf("loaded revision = %d, want 1", loaded.Revision)
							return
						}
						candidate.Revision = loaded.Revision
					}
					ready <- nil
					select {
					case <-release:
					case <-ctx.Done():
						return
					}
					outcomes <- outcome{i, store.Save(ctx, candidate)}
				}()
			}
			go func() { workers.Wait(); close(done) }()
			for range 2 {
				select {
				case err := <-ready:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("workers did not load before deadline")
				}
			}
			start()
			winner, successes, conflicts := -1, 0, 0
			for range 2 {
				select {
				case result := <-outcomes:
					if result.err == nil {
						winner = result.worker
						successes++
					} else {
						expected := int64(1)
						if create {
							expected = 0
						}
						requireConflict(t, result.err, "race", expected)
						conflicts++
					}
				case <-ctx.Done():
					t.Fatal("workers did not save before deadline")
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatalf("Save outcomes = %d successes/%d conflicts, want 1/1", successes, conflicts)
			}
			got, err := stores[0].Load(ctx, "race")
			if err != nil {
				t.Fatal(err)
			}
			expectedRevision := int64(2)
			if create {
				expectedRevision = 1
			}
			if got.Revision != expectedRevision {
				t.Errorf("persisted revision = %d, want %d", got.Revision, expectedRevision)
			}
			// Independent literals pin the whole winner, not values passed into Save.
			want := Conversation{Title: "ambertitle", Messages: []Message{{Role: "user", Content: "ambermessage"}, {Role: "assistant", Content: "amberanswer"}}, DurableSummary: &DurableSummary{Content: "ambersummary", MessageCount: 5}}
			if winner == 1 {
				want = Conversation{Title: "violettitle", Messages: []Message{{Role: "user", Content: "violetmessage"}, {Role: "assistant", Content: "violetanswer"}}, DurableSummary: &DurableSummary{Content: "violetsummary", MessageCount: 7}}
			}
			if got.Title != want.Title || !reflect.DeepEqual(got.Messages, want.Messages) || !reflect.DeepEqual(got.DurableSummary, want.DurableSummary) {
				t.Errorf("persisted winner = %+v, want complete %+v", got, want)
			}
			for i, terms := range [2][]string{{"ambertitle", "ambermessage", "amberanswer", "ambersummary"}, {"violettitle", "violetmessage", "violetanswer", "violetsummary"}} {
				for _, term := range terms {
					hits, err := stores[0].Search(ctx, term, SearchOptions{})
					if err != nil {
						t.Fatal(err)
					}
					wantCount := 0
					if i == winner {
						wantCount = 1
					}
					if len(hits) != wantCount {
						t.Errorf("Search(%q) = %+v, want %d hits", term, hits, wantCount)
					}
					if len(hits) == 1 && (hits[0].Title != want.Title || hits[0].MessageCount != 2) {
						t.Errorf("Search(%q) metadata = %+v, want winner title and 2 messages", term, hits[0])
					}
				}
			}
		})
	}
}

func TestSaveCAS_IndexFailureRollsBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Save(ctx, Conversation{ID: "cas", Title: "originaltitle", Messages: []Message{{Role: "user", Content: "originalmessage"}}, DurableSummary: &DurableSummary{Content: "originalsummary", MessageCount: 3}}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"conversations", "conversation_search"} {
		if _, err := store.db.Exec(`UPDATE ` + table + ` SET created_at = 1234, updated_at = 2345`); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := store.Load(ctx, "cas")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Title = "replacementtitle"
	loaded.Messages = []Message{{Role: "assistant", Content: "replacementmessage"}}
	loaded.DurableSummary = &DurableSummary{Content: "replacementsummary", MessageCount: 9}
	before := storedCASState(t, store)
	if _, err := store.db.Exec(`CREATE TRIGGER fail_search BEFORE UPDATE ON conversation_search BEGIN SELECT RAISE(ABORT, 'forced metadata failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = store.Save(ctx, *loaded)
	if err == nil || errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "forced metadata failure") {
		t.Fatalf("Save with failing search trigger = %v, want original SQL failure", err)
	}
	if got := storedCASState(t, store); got != before {
		t.Errorf("failed Save did not roll back:\n%s\nwant:\n%s", got, before)
	}
	for _, term := range []string{"originalmessage", "originalsummary", "replacementmessage", "replacementsummary"} {
		hits, err := store.Search(ctx, term, SearchOptions{})
		want := 0
		if strings.HasPrefix(term, "original") {
			want = 1
		}
		if err != nil || len(hits) != want {
			t.Errorf("Search(%q) after rollback = %+v, %v, want %d hits", term, hits, err, want)
		}
	}
}

func TestSaveCAS_NonConflictErrors(t *testing.T) {
	stores := openCASHandles(t)
	ctx := context.Background()
	if err := stores[0].Save(ctx, Conversation{ID: "cas"}); err != nil {
		t.Fatal(err)
	}
	before := storedCASState(t, stores[0])
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := stores[0].Save(canceled, Conversation{ID: "cas", Revision: 1})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrConflict) {
		t.Errorf("canceled Save = %v, want context.Canceled only", err)
	}
	err = stores[0].Save(ctx, Conversation{ID: "cas", Revision: 1, Messages: []Message{{ToolCalls: json.RawMessage(`{`)}}})
	var encodingError *json.MarshalerError
	if !errors.As(err, &encodingError) || errors.Is(err, ErrConflict) {
		t.Errorf("invalid JSON Save = %v, want encoding error only", err)
	}
	if _, err := stores[1].db.Exec(`PRAGMA busy_timeout=1`); err != nil {
		t.Fatal(err)
	}
	tx, err := stores[0].db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE conversations SET title = 'locked' WHERE id = 'cas'`); err != nil {
		t.Fatal(err)
	}
	err = stores[1].Save(ctx, Conversation{ID: "cas", Revision: 1})
	var sqlError interface{ Code() int }
	if err == nil || errors.Is(err, ErrConflict) || !errors.As(err, &sqlError) || sqlError.Code() != 5 {
		t.Errorf("busy Save = %v, want SQLITE_BUSY only", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := storedCASState(t, stores[0]); got != before {
		t.Errorf("failed saves changed state: %s, want %s", got, before)
	}
}
