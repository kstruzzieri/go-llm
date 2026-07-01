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
