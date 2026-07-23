package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newRecordStore(t *testing.T) *MemoryRecordStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewMemoryRecordStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewMemoryRecordStore: %v", err)
	}
	return s
}

func mustSearch(t *testing.T, s *MemoryRecordStore, query string, opts RecordSearchOptions) []MemoryRecord {
	t.Helper()
	got, err := s.Search(context.Background(), query, opts)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return got
}

func TestCreateValidKinds(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	cases := []CreateRecordParams{
		{Kind: KindWorking, Content: "wm", WorkspaceID: "w1", SessionID: "s1"},
		{Kind: KindSemantic, Content: "sm", WorkspaceID: "w1"},
		{Kind: KindEpisodic, Content: "em"},
	}
	for _, in := range cases {
		m, err := s.Create(ctx, in)
		if err != nil {
			t.Fatalf("Create(%s): %v", in.Kind, err)
		}
		if m.ID == "" {
			t.Fatalf("Create(%s): empty id", in.Kind)
		}
		got, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: in.WorkspaceID, SessionID: in.SessionID})
		if err != nil {
			t.Fatalf("Get(%s): %v", in.Kind, err)
		}
		if got.Content != in.Content || got.Kind != in.Kind {
			t.Fatalf("round-trip mismatch: got %+v want kind=%s content=%s", got, in.Kind, in.Content)
		}
		if string(got.Metadata) != "{}" {
			t.Fatalf("metadata not normalized: %q", got.Metadata)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		in   CreateRecordParams
		want error
	}{
		{"empty content", CreateRecordParams{Kind: KindSemantic, Content: "   "}, ErrEmptyContent},
		{"bad kind", CreateRecordParams{Kind: "bogus", Content: "x"}, ErrBadKind},
		{"session needs workspace", CreateRecordParams{Kind: KindSemantic, Content: "x", SessionID: "s1"}, ErrSessionNeedsWorkspace},
		{"working needs session", CreateRecordParams{Kind: KindWorking, Content: "x", WorkspaceID: "w1"}, ErrWorkingNeedsSession},
		{"bad provenance range", CreateRecordParams{Kind: KindSemantic, Content: "x", Provenance: Provenance{Start: 5, End: 2}}, ErrBadProvenanceRange},
		{"bad metadata", CreateRecordParams{Kind: KindSemantic, Content: "x", Metadata: json.RawMessage("{not json")}, ErrBadMetadata},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Create(ctx, c.in); !errors.Is(err, c.want) {
				t.Fatalf("Create: got %v want %v", err, c.want)
			}
		})
	}
}

func TestCreateProvenanceRoundTrip(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	prov := Provenance{SourceKind: "conversation", SourceID: "c1", Start: 3, End: 9, Hash: "abc"}
	m, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1", Provenance: prov, Metadata: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Provenance != prov {
		t.Fatalf("provenance mismatch: got %+v want %+v", got.Provenance, prov)
	}
	if string(got.Metadata) != `{"a":1}` {
		t.Fatalf("metadata mismatch: %q", got.Metadata)
	}
}

func TestGetMissAndScope(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	if _, err := s.Get(ctx, "nope", RecordAccess{}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Get miss: got %v want ErrRecordNotFound", err)
	}
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1"})
	if _, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w2"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("cross-workspace Get: got %v want ErrRecordNotFound", err)
	}
	sm, _ := s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "x", WorkspaceID: "w1", SessionID: "s1"})
	if _, err := s.Get(ctx, sm.ID, RecordAccess{WorkspaceID: "w1"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("non-session Get of session record: got %v want ErrRecordNotFound", err)
	}
	// positive: a global record (workspace_id="") is visible under any workspace scope.
	g, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "global"})
	if _, err := s.Get(ctx, g.ID, RecordAccess{WorkspaceID: "any"}); err != nil {
		t.Fatalf("global record not visible under workspace scope: %v", err)
	}
}

func TestCreatePartialProvenanceAllowed(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	// End == 0 (unset) with a known Start is partial provenance, not an error.
	m, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1", Provenance: Provenance{SourceKind: "tool", Start: 5}})
	if err != nil {
		t.Fatalf("partial provenance Create: %v", err)
	}
	got, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Provenance.Start != 5 || got.Provenance.End != 0 {
		t.Fatalf("partial provenance round-trip: got %+v", got.Provenance)
	}
}

func TestGetReturnsExpired(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1", ExpiresAt: past})
	got, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Get expired: %v", err)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatalf("expected non-zero ExpiresAt round-trip")
	}
}

func TestSearchFTSAndEmptyQuery(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "alpha beta gamma", WorkspaceID: "w1"})
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "delta epsilon", WorkspaceID: "w1"})

	hits, err := s.Search(ctx, "beta", RecordSearchOptions{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Content != "alpha beta gamma" {
		t.Fatalf("FTS search: got %d %v", len(hits), hits)
	}
	all, err := s.Search(ctx, "", RecordSearchOptions{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("empty-query listing: got %d want 2", len(all))
	}
}

func TestSearchFilters(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "task note", WorkspaceID: "w1", SessionID: "s1", Namespace: "ns1"})
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "fact note", WorkspaceID: "w1", Namespace: "ns2"})

	byKind := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", SessionID: "s1", Kind: KindWorking})
	if len(byKind) != 1 || byKind[0].Kind != KindWorking {
		t.Fatalf("kind filter: got %v", byKind)
	}
	byNS := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", Namespace: "ns2"})
	if len(byNS) != 1 || byNS[0].Namespace != "ns2" {
		t.Fatalf("namespace filter: got %v", byNS)
	}
}

func TestSearchVisibilityIsolation(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindEpisodic, Content: "global note"})
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "w1 note", WorkspaceID: "w1"})
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "s1 note", WorkspaceID: "w1", SessionID: "s1"})

	fromS1 := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", SessionID: "s1"})
	if len(fromS1) != 3 {
		t.Fatalf("session view: got %d want 3", len(fromS1))
	}
	fromW1 := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1"})
	if len(fromW1) != 2 {
		t.Fatalf("workspace view: got %d want 2", len(fromW1))
	}
	fromW2 := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w2"})
	if len(fromW2) != 1 {
		t.Fatalf("other workspace view: got %d want 1", len(fromW2))
	}
}

func TestSearchExpiryAndLimit(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	now := time.Now()
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "live", WorkspaceID: "w1"})
	_, _ = s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "dead", WorkspaceID: "w1", ExpiresAt: now.Add(-time.Hour)})

	live := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", Now: now})
	if len(live) != 1 || live[0].Content != "live" {
		t.Fatalf("expiry exclusion: got %v", live)
	}
	withExpired := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", Now: now, IncludeExpired: true})
	if len(withExpired) != 2 {
		t.Fatalf("IncludeExpired: got %d want 2", len(withExpired))
	}
	limited := mustSearch(t, s, "", RecordSearchOptions{WorkspaceID: "w1", Now: now, IncludeExpired: true, Limit: 1})
	if len(limited) != 1 {
		t.Fatalf("limit: got %d want 1", len(limited))
	}
}

func TestSearchNoMatchNonNil(t *testing.T) {
	s := newRecordStore(t)
	got, err := s.Search(context.Background(), "zzz", RecordSearchOptions{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want non-nil empty slice, got %v", got)
	}
}

func strptr(s string) *string { return &s }

func TestUpdateFields(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "old", WorkspaceID: "w1", Namespace: "a"})
	exp := time.Now().Add(time.Hour)
	got, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, UpdateRecordParams{
		Content:   strptr("new content"),
		Namespace: strptr("b"),
		Metadata:  json.RawMessage(`{"k":1}`),
		ExpiresAt: &exp,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Content != "new content" || got.Namespace != "b" || string(got.Metadata) != `{"k":1}` || got.ExpiresAt.IsZero() {
		t.Fatalf("Update result: %+v", got)
	}
	hits := mustSearch(t, s, "content", RecordSearchOptions{WorkspaceID: "w1"})
	if len(hits) != 1 {
		t.Fatalf("FTS resync: got %d want 1", len(hits))
	}
	if old := mustSearch(t, s, "old", RecordSearchOptions{WorkspaceID: "w1"}); len(old) != 0 {
		t.Fatalf("stale FTS term still matches: %v", old)
	}
	// re-fetch from the DB to confirm the write landed (not just the in-memory struct).
	reread, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Get after Update: %v", err)
	}
	if reread.Content != "new content" || reread.Namespace != "b" || string(reread.Metadata) != `{"k":1}` || reread.ExpiresAt.IsZero() {
		t.Fatalf("persisted row mismatch: %+v", reread)
	}
}

func TestUpdatePartialLeavesOthers(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "keep", WorkspaceID: "w1", Namespace: "a", Metadata: json.RawMessage(`{"k":1}`)})
	got, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, UpdateRecordParams{Namespace: strptr("b")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Namespace != "b" {
		t.Fatalf("namespace not updated: %q", got.Namespace)
	}
	if got.Content != "keep" || string(got.Metadata) != `{"k":1}` || !got.ExpiresAt.IsZero() {
		t.Fatalf("untouched fields changed: %+v", got)
	}
}

func TestUpdateClearExpiry(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1", ExpiresAt: time.Now().Add(time.Hour)})
	zero := time.Time{}
	got, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, UpdateRecordParams{ExpiresAt: &zero})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("expected cleared expiry, got %v", got.ExpiresAt)
	}
}

func TestUpdateMissScopeAndBadMetadata(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	if _, err := s.Update(ctx, "nope", RecordAccess{}, UpdateRecordParams{Namespace: strptr("x")}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Update miss: got %v", err)
	}
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1"})
	if _, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w2"}, UpdateRecordParams{Namespace: strptr("x")}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("cross-workspace Update: got %v", err)
	}
	if _, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, UpdateRecordParams{Metadata: json.RawMessage("{bad")}); !errors.Is(err, ErrBadMetadata) {
		t.Fatalf("bad metadata: got %v", err)
	}
	if _, err := s.Update(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, UpdateRecordParams{Content: strptr("   ")}); !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("whitespace content: got %v want ErrEmptyContent", err)
	}
}

func TestRecordSoftDelete(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "gone soon", WorkspaceID: "w1"})
	if err := s.SoftDelete(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if _, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("Get after delete: got %v", err)
	}
	if hits := mustSearch(t, s, "gone", RecordSearchOptions{WorkspaceID: "w1"}); len(hits) != 0 {
		t.Fatalf("Search after delete: got %v", hits)
	}
	// prove the FTS row itself is gone, not merely hidden by the deleted_at join
	// filter (otherwise orphaned FTS rows would accumulate unbounded).
	var ftsRows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_records_fts WHERE id = ?`, m.ID).Scan(&ftsRows); err != nil {
		t.Fatalf("count fts rows: %v", err)
	}
	if ftsRows != 0 {
		t.Fatalf("orphaned FTS row after delete: count=%d", ftsRows)
	}
	if err := s.SoftDelete(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("double delete: got %v want ErrRecordNotFound", err)
	}
}

func TestRecordSoftDeleteScope(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "x", WorkspaceID: "w1"})
	if err := s.SoftDelete(ctx, m.ID, RecordAccess{WorkspaceID: "w2"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("cross-workspace delete: got %v want ErrRecordNotFound", err)
	}
	if _, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}); err != nil {
		t.Fatalf("record wrongly deleted: %v", err)
	}
	// session isolation: a session-scoped record is not deletable without the session.
	sm, _ := s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "y", WorkspaceID: "w1", SessionID: "s1"})
	if err := s.SoftDelete(ctx, sm.ID, RecordAccess{WorkspaceID: "w1"}); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("cross-session delete: got %v want ErrRecordNotFound", err)
	}
	if _, err := s.Get(ctx, sm.ID, RecordAccess{WorkspaceID: "w1", SessionID: "s1"}); err != nil {
		t.Fatalf("session record wrongly deleted: %v", err)
	}
}

func TestPromote(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	for _, to := range []MemoryKind{KindSemantic, KindEpisodic} {
		m, _ := s.Create(ctx, CreateRecordParams{
			Kind: KindWorking, Content: "candidate", WorkspaceID: "w1", SessionID: "s1",
			Provenance: Provenance{SourceKind: "conversation", SourceID: "c1", Start: 1, End: 4},
			ExpiresAt:  time.Now().Add(time.Hour),
		})
		got, err := s.Promote(ctx, m.ID, RecordAccess{WorkspaceID: "w1", SessionID: "s1"}, to)
		if err != nil {
			t.Fatalf("Promote(%s): %v", to, err)
		}
		if got.Kind != to {
			t.Fatalf("kind: got %s want %s", got.Kind, to)
		}
		if got.SessionID != "" {
			t.Fatalf("session not cleared: %q", got.SessionID)
		}
		if !got.ExpiresAt.IsZero() {
			t.Fatalf("expiry not cleared: %v", got.ExpiresAt)
		}
		if got.WorkspaceID != "w1" {
			t.Fatalf("workspace changed: %q", got.WorkspaceID)
		}
		if got.Provenance.SourceID != "c1" {
			t.Fatalf("provenance lost: %+v", got.Provenance)
		}
		// now visible to a non-session caller in w1 (session binding shed)
		if _, err := s.Get(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}); err != nil {
			t.Fatalf("promoted record not workspace-visible: %v", err)
		}
	}
}

func TestPromoteBadTargetAndMiss(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "x", WorkspaceID: "w1", SessionID: "s1"})
	if _, err := s.Promote(ctx, m.ID, RecordAccess{WorkspaceID: "w1", SessionID: "s1"}, KindWorking); !errors.Is(err, ErrBadPromotion) {
		t.Fatalf("promote to working: got %v want ErrBadPromotion", err)
	}
	if _, err := s.Promote(ctx, "nope", RecordAccess{}, KindSemantic); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("promote miss: got %v", err)
	}
	if _, err := s.Promote(ctx, m.ID, RecordAccess{WorkspaceID: "w1"}, KindSemantic); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("promote of session record without session scope: got %v want ErrRecordNotFound", err)
	}
}

func TestMemoryRecordsMigrationKeepsMemoriesWorking(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Open BOTH stores on the same DB (shared migration chain, v1 + v2).
	notes, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := NewMemoryRecordStore(ctx, db); err != nil {
		t.Fatalf("NewMemoryRecordStore: %v", err)
	}

	// #237 Memory still round-trips: Add, Search, List, ResolveVisible.
	m, err := notes.Add(ctx, AddParams{Text: "user preference note", Scope: ScopeWorkspace, WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if hits, err := notes.Search(ctx, "preference", SearchOptions{WorkspaceID: "w1"}); err != nil || len(hits) != 1 {
		t.Fatalf("Search: err=%v n=%d", err, len(hits))
	}
	if ms, err := notes.List(ctx, ListOptions{WorkspaceID: "w1"}); err != nil || len(ms) != 1 {
		t.Fatalf("List: err=%v n=%d", err, len(ms))
	}
	if got, err := notes.ResolveVisible(ctx, m.ID, "w1"); err != nil || got.ID != m.ID {
		t.Fatalf("ResolveVisible: err=%v id=%s", err, got.ID)
	}
}
