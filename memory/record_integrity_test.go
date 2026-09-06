package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

func TestRecordCreateStampsAuthoritativeProvenance(t *testing.T) {
	s := newRecordStore(t)
	for _, kind := range []MemoryKind{KindWorking, KindSemantic, KindEpisodic} {
		m, err := s.Create(context.Background(), CreateRecordParams{
			Kind: kind, Content: "claimed fact", WorkspaceID: "workspace", SessionID: "session",
			Provenance: Provenance{SourceKind: "user", SourceID: "claim", OriginTool: "human-review", OriginSessionID: "forged", TrustClass: "user-trusted"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if m.Provenance.OriginTool != "memory.create" || m.Provenance.OriginSessionID != "session" || m.Provenance.TrustClass != "agent-written" {
			t.Errorf("kind %s: authoritative provenance = %+v", kind, m.Provenance)
		}
		if m.Provenance.SourceKind != "user" || m.Provenance.SourceID != "claim" {
			t.Fatal("source claims lost")
		}
		if len(m.Signature.Bytes) == 0 {
			t.Error("created record has no signature")
		}
	}
}

func TestRecordCorruptPreimageFailsClosed(t *testing.T) {
	for _, operation := range []string{"get", "search", "recency", "update", "promote", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s := newRecordStore(t)
			ctx := context.Background()
			m, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "original needle"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE memory_records SET content = 'altered payload' WHERE id = ?`, m.ID); err != nil {
				t.Fatal(err)
			}
			var records []MemoryRecord
			switch operation {
			case "get":
				_, err = s.Get(ctx, m.ID, RecordAccess{})
			case "search":
				records, err = s.Search(ctx, "needle", RecordSearchOptions{})
			case "recency":
				records, err = s.Search(ctx, "", RecordSearchOptions{})
			case "update":
				_, err = s.Update(ctx, m.ID, RecordAccess{}, UpdateRecordParams{})
			case "promote":
				_, err = s.Promote(ctx, m.ID, RecordAccess{}, KindEpisodic)
			case "delete":
				err = s.SoftDelete(ctx, m.ID, RecordAccess{})
			}
			if !errors.Is(err, ErrRecordIntegrity) || len(records) != 0 {
				t.Fatalf("operation returned records=%d err=%v", len(records), err)
			}
		})
	}
}

func TestRecordFilesystemIdentityCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	rt, err := OpenRecordStore(context.Background(), path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func(runtime *RecordRuntime) { _ = runtime.Close() }(rt)
	if _, err := os.Stat(path + ".keys/current.pem"); err != nil {
		t.Fatalf("identity missing: %v", err)
	}
}

func TestRecordSelectedFieldCorruption(t *testing.T) {
	cases := []struct {
		name, set string
		cause     error
	}{
		{"id", `id = 'tampered-id'`, signing.ErrInvalidSignature},
		{"kind", `kind = 'episodic'`, signing.ErrInvalidSignature},
		{"unsupported kind", `kind = 'unsupported'`, ErrBadKind},
		{"content", `content = 'private-payload'`, signing.ErrInvalidSignature},
		{"namespace", `namespace = 'altered'`, signing.ErrInvalidSignature},
		{"workspace", `workspace_id = ''`, ErrSessionNeedsWorkspace},
		{"session", `session_id = ''`, signing.ErrInvalidSignature},
		{"source kind", `source_kind = 'changed'`, signing.ErrInvalidSignature},
		{"source id", `source_id = 'changed'`, signing.ErrInvalidSignature},
		{"source start", `source_start = 1`, signing.ErrInvalidSignature},
		{"source end", `source_end = 2`, signing.ErrInvalidSignature},
		{"source hash", `source_hash = 'changed'`, signing.ErrInvalidSignature},
		{"origin", `origin_tool = 'mcp.agent_memory_create'`, signing.ErrInvalidSignature},
		{"origin session", `origin_session_id = 'changed'`, signing.ErrInvalidSignature},
		{"trust", `trust_class = 'legacy-unreviewed'`, signing.ErrInvalidSignature},
		{"unknown trust", `trust_class = 'user-trusted'`, errInvalidRecordTrust},
		{"metadata", `metadata = '{"changed":true}'`, signing.ErrInvalidSignature},
		{"metadata empty", `metadata = ''`, ErrBadMetadata},
		{"metadata duplicate", `metadata = '{"private-payload":1,"private-payload":2}'`, signing.ErrDuplicateKey},
		{"created", `created_at = 123`, signing.ErrInvalidSignature},
		{"updated", `updated_at = 123`, signing.ErrInvalidSignature},
		{"expires", `expires_at = 123`, signing.ErrInvalidSignature},
		{"algorithm", `signature_alg = 'unknown'`, signing.ErrKeyMismatch},
		{"key", `signature_key_id = 'unknown'`, signing.ErrUnknownKey},
		{"signature", `signature = X'01'`, signing.ErrInvalidSignature},
		{"missing", `signature = X''`, errMissingRecordSignature},
		{"malformed scan", `source_start = 'private-payload'`, nil},
		{"malformed json", `metadata = '{"private-payload":}'`, errInvalidRecordBody},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := newRecordStore(t)
			ctx := context.Background()
			bad, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "needle", WorkspaceID: "ws", SessionID: "ss"})
			if err != nil {
				t.Fatal(err)
			}
			// The valid row must precede the invalid row in both query orderings.
			good, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			good.UpdatedAt = time.UnixMilli(9000000000000).UTC()
			if err := signRecord(ctx, s.signer, &good); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := persistRecord(ctx, tx, good); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE memory_records SET `+tt.set+` WHERE id = ?`, bad.ID); err != nil {
				t.Fatal(err)
			}
			if tt.name == "id" {
				if _, err := s.db.Exec(`UPDATE memory_records_fts SET id = 'tampered-id' WHERE id = ?`, bad.ID); err != nil {
					t.Fatal(err)
				}
				bad.ID = "tampered-id"
			}
			got, err := s.Get(ctx, bad.ID, RecordAccess{WorkspaceID: "ws", SessionID: "ss"})
			if !errors.Is(err, ErrRecordIntegrity) || got.ID != "" || tt.cause != nil && !errors.Is(err, tt.cause) {
				t.Fatalf("get: id=%s err=%v", got.ID, err)
			}
			if strings.Contains(err.Error(), "private-payload") {
				t.Fatalf("get disclosed stored bytes: %v", err)
			}
			for _, query := range []string{"", "needle"} {
				rows, err := s.Search(ctx, query, RecordSearchOptions{WorkspaceID: "ws", SessionID: "ss", IncludeExpired: true})
				if rows != nil || !errors.Is(err, ErrRecordIntegrity) || tt.cause != nil && !errors.Is(err, tt.cause) {
					t.Fatalf("query %q: rows=%d err=%v", query, len(rows), err)
				}
				if strings.Contains(err.Error(), "private-payload") {
					t.Fatalf("search disclosed stored bytes: %v", err)
				}
			}
		})
	}
}

func TestRecordMutationAtomicityAndReopen(t *testing.T) {
	for _, operation := range []string{"update", "promote", "delete"} {
		for _, failure := range []string{"signer", "sql"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				s := newRecordStore(t)
				before, err := s.Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "original needle", WorkspaceID: "ws", SessionID: "original-session"})
				if err != nil {
					t.Fatal(err)
				}
				if failure == "signer" {
					s.signer = &countingRecordSigner{Signer: s.signer, failAt: 1}
				}
				if failure == "sql" {
					// Update the base row first, then fail the FTS operation. This
					// catches a missing rollback rather than just an early rejection.
					if _, err := s.db.Exec(`DROP TABLE memory_records_fts; CREATE TABLE memory_records_fts (id TEXT, content TEXT CHECK(content != 'replacement')); INSERT INTO memory_records_fts VALUES (?, 'original needle')`, before.ID); err != nil {
						t.Fatal(err)
					}
					statement := `CREATE TRIGGER fail_write AFTER UPDATE ON memory_records BEGIN SELECT RAISE(FAIL, 'synthetic SQL failure'); END`
					if operation == "delete" {
						statement = `CREATE TRIGGER fail_write BEFORE DELETE ON memory_records_fts BEGIN SELECT RAISE(ABORT, 'synthetic SQL failure'); END`
					}
					if operation != "update" {
						if _, err := s.db.Exec(statement); err != nil {
							t.Fatal(err)
						}
					}
				}
				acc := RecordAccess{WorkspaceID: "ws", SessionID: "original-session"}
				replacement := "replacement"
				switch operation {
				case "update":
					_, err = s.Update(ctx, before.ID, acc, UpdateRecordParams{Content: &replacement})
				case "promote":
					_, err = s.Promote(ctx, before.ID, acc, KindSemantic)
				case "delete":
					err = s.SoftDelete(ctx, before.ID, acc)
				}
				if err == nil {
					t.Fatal("write unexpectedly succeeded")
				}
				after, err := s.Get(ctx, before.ID, acc)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("failed write changed row: before=%+v after=%+v", before, after)
				}
				var ftsID, ftsContent string
				if err := s.db.QueryRow(`SELECT id, content FROM memory_records_fts`).Scan(&ftsID, &ftsContent); err != nil || ftsID != before.ID || ftsContent != "original needle" {
					t.Fatalf("failed write changed FTS: id=%q content=%q err=%v", ftsID, ftsContent, err)
				}
			})
		}
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "records.db")
	rt, err := OpenRecordStore(ctx, path, RecordStoreConfig{Writer: WriterGolem})
	if err != nil {
		t.Fatal(err)
	}
	m, err := rt.Store().Create(ctx, CreateRecordParams{Kind: KindWorking, Content: "original", WorkspaceID: "ws", SessionID: "original-session"})
	if err != nil {
		t.Fatal(err)
	}
	content := "replacement needle"
	m, err = rt.Store().Update(ctx, m.ID, RecordAccess{WorkspaceID: "ws", SessionID: "original-session"}, UpdateRecordParams{Content: &content})
	if err != nil {
		t.Fatal(err)
	}
	m, err = rt.Store().Promote(ctx, m.ID, RecordAccess{WorkspaceID: "ws", SessionID: "original-session"}, KindEpisodic)
	if err != nil {
		t.Fatal(err)
	}
	if m.SessionID != "" || m.Provenance.OriginSessionID != "original-session" || m.Provenance.OriginTool != "golem.agent_memory_create" || m.Provenance.TrustClass != "agent-written" {
		t.Fatalf("promotion changed origin/trust: %+v", m)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{Writer: WriterMCP})
	if err != nil {
		t.Fatal(err)
	}
	defer func(runtime *RecordRuntime) { _ = runtime.Close() }(rt)
	got, err := rt.Store().Get(ctx, m.ID, RecordAccess{WorkspaceID: "ws"})
	if err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("reopen changed record: err=%v", err)
	}
	if err := rt.Store().SoftDelete(ctx, m.ID, RecordAccess{WorkspaceID: "ws"}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func(runtime *RecordRuntime) { _ = runtime.Close() }(rt)
	tombstone, err := scanRecord(rt.db.QueryRow(`SELECT `+recordColumns+` FROM memory_records WHERE id = ?`, m.ID))
	if err != nil {
		t.Fatal(err)
	}
	if tombstone.DeletedAt.IsZero() {
		t.Fatal("deletion not persisted")
	}
	if err := verifyRecord(ctx, rt.Store().verifiers, tombstone); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.db.Exec(`UPDATE memory_records SET deleted_at = 0 WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Store().Get(ctx, m.ID, RecordAccess{WorkspaceID: "ws"}); !errors.Is(err, ErrRecordIntegrity) {
		t.Fatalf("resurrected tombstone verified: %v", err)
	}
	if _, err := rt.db.Exec(`INSERT INTO memory_records_fts (id,content) VALUES (?, ?)`, m.ID, m.Content); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"", "needle"} {
		if hits, err := rt.Store().Search(ctx, query, RecordSearchOptions{WorkspaceID: "ws"}); hits != nil || !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, signing.ErrInvalidSignature) {
			t.Fatalf("resurrected search: hits=%d err=%v", len(hits), err)
		}
	}
}

func TestRecordMissingSignatureNotRepairedOnReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "records.db")
	rt, err := OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := rt.Store().Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.db.Exec(`UPDATE memory_records SET signature = X'' WHERE id = ?`, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func(runtime *RecordRuntime) { _ = runtime.Close() }(rt)
	if _, err := rt.Store().Get(ctx, m.ID, RecordAccess{}); !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, errMissingRecordSignature) {
		t.Fatalf("reopen repaired signature: %v", err)
	}
}

func TestRecordLegacyContentLimit(t *testing.T) {
	db := legacyRecordDB(t, ":memory:", 1)
	if _, err := db.Exec(`UPDATE memory_records SET content = ?, deleted_at = 0`, strings.Repeat("x", 4097)); err != nil {
		t.Fatal(err)
	}
	s, err := NewMemoryRecordStore(context.Background(), db, recordTestConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	acc := RecordAccess{WorkspaceID: "workspace", SessionID: "historical-session"}
	ctx := context.Background()
	if _, err := s.Update(ctx, "legacy-0000", acc, UpdateRecordParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Promote(ctx, "legacy-0000", acc, KindSemantic); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 4097)
	if _, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: content}); !errors.Is(err, ErrContentTooLong) {
		t.Fatalf("create content limit: %v", err)
	}
	if _, err := s.Update(ctx, "legacy-0000", acc, UpdateRecordParams{Content: &content}); !errors.Is(err, ErrContentTooLong) {
		t.Fatalf("replacement content limit: %v", err)
	}
	if err := s.SoftDelete(ctx, "legacy-0000", acc); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRawMetadataBoundBeforeNormalization(t *testing.T) {
	s := newRecordStore(t)
	ctx := context.Background()
	m, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{strings.Repeat(" ", 32769), strings.Repeat("{", 32769)} {
		if _, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "needle", Metadata: []byte(raw)}); !errors.Is(err, ErrRecordTooLarge) {
			t.Errorf("create raw size: %v", err)
		}
		if _, err := s.Update(ctx, m.ID, RecordAccess{}, UpdateRecordParams{Metadata: []byte(raw)}); !errors.Is(err, ErrRecordTooLarge) {
			t.Errorf("update raw size: %v", err)
		}
	}
}
