package memory

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "memories.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

func TestAddAndList(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws, err := store.Add(ctx, AddParams{Text: "  use table-driven tests  ", Scope: ScopeWorkspace, WorkspaceID: "workspace:aaa", SourceSessionID: "golem:s1"})
	if err != nil {
		t.Fatalf("add workspace: %v", err)
	}
	if ws.Text != "use table-driven tests" {
		t.Errorf("text not trimmed: %q", ws.Text)
	}
	if ws.ID == "" {
		t.Error("empty id")
	}
	if _, err := store.Add(ctx, AddParams{Text: "prefer small diffs", Scope: ScopeGlobal, WorkspaceID: "ignored"}); err != nil {
		t.Fatalf("add global: %v", err)
	}
	if _, err := store.Add(ctx, AddParams{Text: "deploy via make", Scope: ScopeWorkspace, WorkspaceID: "workspace:bbb"}); err != nil {
		t.Fatalf("add other ws: %v", err)
	}

	got, err := store.List(ctx, ListOptions{WorkspaceID: "workspace:aaa"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 visible (workspace:aaa + global), got %d: %+v", len(got), got)
	}
	for _, m := range got {
		if m.Scope == ScopeGlobal && m.WorkspaceID != "" {
			t.Errorf("global has workspace_id %q", m.WorkspaceID)
		}
		if m.WorkspaceID == "workspace:bbb" {
			t.Error("leaked other-workspace memory")
		}
	}
}

func TestAddValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.Add(ctx, AddParams{Text: "   ", Scope: ScopeWorkspace, WorkspaceID: "w"}); !errors.Is(err, ErrEmptyText) {
		t.Errorf("empty text: want ErrEmptyText, got %v", err)
	}
	if _, err := store.Add(ctx, AddParams{Text: "x", Scope: ScopeWorkspace, WorkspaceID: ""}); err == nil {
		t.Error("workspace scope without workspace_id should error")
	}
	if _, err := store.Add(ctx, AddParams{Text: "x", Scope: Scope("bogus"), WorkspaceID: "w"}); !errors.Is(err, ErrBadScope) {
		t.Errorf("bad scope: want ErrBadScope, got %v", err)
	}
}

func TestSearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	a, err := store.Add(ctx, AddParams{Text: "I prefer testing with table-driven style", Scope: ScopeWorkspace, WorkspaceID: "workspace:aaa"})
	if err != nil {
		t.Fatalf("add a: %v", err)
	}
	if _, err := store.Add(ctx, AddParams{Text: "deploy via makefile", Scope: ScopeWorkspace, WorkspaceID: "workspace:bbb"}); err != nil {
		t.Fatalf("add b: %v", err)
	}
	if _, err := store.Add(ctx, AddParams{Text: "keep diffs small", Scope: ScopeGlobal}); err != nil {
		t.Fatalf("add g: %v", err)
	}

	// porter stemming: 'test' matches 'testing'
	got, err := store.Search(ctx, "test", SearchOptions{WorkspaceID: "workspace:aaa"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("porter stem: want only %s, got %+v", a.ID, got)
	}
	// other-workspace memory excluded even if it matches
	if got, _ := store.Search(ctx, "makefile", SearchOptions{WorkspaceID: "workspace:aaa"}); len(got) != 0 {
		t.Errorf("leaked other-workspace match: %+v", got)
	}
	// global visible from any workspace
	if got, _ := store.Search(ctx, "diffs", SearchOptions{WorkspaceID: "workspace:aaa"}); len(got) != 1 {
		t.Errorf("global not visible: %+v", got)
	}
	// id is UNINDEXED: searching the id string returns nothing
	if got, _ := store.Search(ctx, a.ID, SearchOptions{WorkspaceID: "workspace:aaa"}); len(got) != 0 {
		t.Errorf("id should not be searchable, got %+v", got)
	}
	// punctuation/empty query -> empty, no error
	got, err = store.Search(ctx, "  ?? ", SearchOptions{WorkspaceID: "workspace:aaa"})
	if err != nil || len(got) != 0 {
		t.Errorf("punct query: want empty/no-err, got %+v / %v", got, err)
	}
}

func TestResolveVisible(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	a, _ := store.Add(ctx, AddParams{Text: "alpha", Scope: ScopeWorkspace, WorkspaceID: "workspace:aaa"})
	other, _ := store.Add(ctx, AddParams{Text: "beta", Scope: ScopeWorkspace, WorkspaceID: "workspace:bbb"})
	g, _ := store.Add(ctx, AddParams{Text: "gamma", Scope: ScopeGlobal})

	if got, err := store.ResolveVisible(ctx, a.ID, "workspace:aaa"); err != nil || got.ID != a.ID {
		t.Fatalf("resolve own: %v / %+v", err, got)
	}
	if _, err := store.ResolveVisible(ctx, g.ID, "workspace:aaa"); err != nil {
		t.Errorf("resolve global from any ws: %v", err)
	}
	if _, err := store.ResolveVisible(ctx, other.ID, "workspace:aaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("foreign workspace memory: want ErrNotFound, got %v", err)
	}
	if _, err := store.ResolveVisible(ctx, "zzzzzzzz", "workspace:aaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown prefix: want ErrNotFound, got %v", err)
	}
}

func TestResolveVisiblePrefix(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	for _, id := range []string{"abc1", "abc2"} {
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO memories (id, text, scope, workspace_id, source_session_id, created_at, updated_at, deleted_at)
			 VALUES (?, 'x', 'workspace', 'workspace:aaa', '', ?, ?, 0)`, id, now, now); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	if _, err := store.ResolveVisible(ctx, "abc", "workspace:aaa"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("prefix abc: want ErrAmbiguous, got %v", err)
	}
	if got, err := store.ResolveVisible(ctx, "abc1", "workspace:aaa"); err != nil || got.ID != "abc1" {
		t.Errorf("exact abc1 should win: got %+v / %v", got, err)
	}
}

func TestSoftDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.Add(ctx, AddParams{Text: "ephemeral note", Scope: ScopeWorkspace, WorkspaceID: "workspace:aaa"})
	if err := store.SoftDelete(ctx, m.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if got, _ := store.List(ctx, ListOptions{WorkspaceID: "workspace:aaa"}); len(got) != 0 {
		t.Errorf("still listed: %+v", got)
	}
	if got, _ := store.Search(ctx, "ephemeral", SearchOptions{WorkspaceID: "workspace:aaa"}); len(got) != 0 {
		t.Errorf("still searchable: %+v", got)
	}
	if _, err := store.ResolveVisible(ctx, m.ID, "workspace:aaa"); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolvable after delete: %v", err)
	}
	if err := store.SoftDelete(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: want ErrNotFound, got %v", err)
	}
}

func TestSetScopePromoteLocalize(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	m, _ := store.Add(ctx, AddParams{Text: "promote me", Scope: ScopeWorkspace, WorkspaceID: "workspace:aaa"})

	if err := store.SetScope(ctx, m.ID, ScopeGlobal, ""); err != nil {
		t.Fatalf("promote: %v", err)
	}
	got, err := store.ResolveVisible(ctx, m.ID, "workspace:zzz") // visible from any ws now
	if err != nil || got.Scope != ScopeGlobal || got.WorkspaceID != "" {
		t.Errorf("after promote: %+v / %v", got, err)
	}

	if err := store.SetScope(ctx, m.ID, ScopeWorkspace, "workspace:bbb"); err != nil {
		t.Fatalf("localize: %v", err)
	}
	got, err = store.ResolveVisible(ctx, m.ID, "workspace:bbb")
	if err != nil || got.Scope != ScopeWorkspace || got.WorkspaceID != "workspace:bbb" {
		t.Errorf("after localize: %+v / %v", got, err)
	}

	if err := store.SetScope(ctx, m.ID, ScopeWorkspace, ""); err == nil {
		t.Error("localize without workspace_id should error")
	}
	if err := store.SetScope(ctx, "nope", ScopeGlobal, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing id: want ErrNotFound, got %v", err)
	}
	if err := store.SetScope(ctx, m.ID, Scope("bogus"), ""); !errors.Is(err, ErrBadScope) {
		t.Errorf("bad scope: want ErrBadScope, got %v", err)
	}
}
