package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
	modernsqlite "modernc.org/sqlite"
)

func auditRecordStore(t *testing.T) (*sql.DB, *MemoryRecordStore, string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	keyDir := filepath.Join(t.TempDir(), "keys")
	store, err := NewMemoryRecordStore(t.Context(), db, RecordStoreConfig{KeyDir: keyDir})
	if err != nil {
		t.Fatal(err)
	}
	return db, store, keyDir
}

func TestAuditRecordsAllRows(t *testing.T) {
	db, store, keyDir := auditRecordStore(t)
	now := time.Now()
	fixtures := []CreateRecordParams{
		{Kind: KindSemantic, Content: "global", Metadata: []byte(`{"scope":"global"}`)},
		{Kind: KindSemantic, Content: "workspace", WorkspaceID: "workspace-a"},
		{Kind: KindWorking, Content: "session", WorkspaceID: "workspace-a", SessionID: "session-a"},
		{Kind: KindSemantic, Content: "other workspace", WorkspaceID: "workspace-b"},
		{Kind: KindWorking, Content: "other session", WorkspaceID: "workspace-a", SessionID: "session-b"},
		{Kind: KindSemantic, Content: "expired", ExpiresAt: now.Add(-time.Hour)},
		{Kind: KindSemantic, Content: "tombstoned"},
	}
	var tombstoneID string
	for _, fixture := range fixtures {
		record, err := store.Create(t.Context(), fixture)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.Content == "tombstoned" {
			tombstoneID = record.ID
		}
	}
	if err := store.SoftDelete(t.Context(), tombstoneID, RecordAccess{}); err != nil {
		t.Fatal(err)
	}

	legacy := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
	legacy.ID = "signed-legacy"
	legacy.Provenance.TrustClass = TrustLegacyUnreviewed
	if err := signRecord(context.Background(), store.signer, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO memory_records (`+recordColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recordValues(legacy)...); err != nil {
		t.Fatal(err)
	}

	report, err := AuditRecords(t.Context(), db, keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Configured || report.Verified != 8 || report.TotalExtant == nil || *report.TotalExtant != 8 {
		t.Fatalf("AuditRecords report = %+v, want configured and 8 of 8", report)
	}

	for _, tc := range []struct {
		name   string
		params CreateRecordParams
		delete bool
	}{
		{name: "expired", params: CreateRecordParams{Kind: KindSemantic, Content: "expired private payload", ExpiresAt: now.Add(-time.Hour)}},
		{name: "tombstoned", params: CreateRecordParams{Kind: KindSemantic, Content: "deleted private payload"}, delete: true},
		{name: "other workspace", params: CreateRecordParams{Kind: KindSemantic, Content: "other workspace private payload", WorkspaceID: "elsewhere"}},
		{name: "other session", params: CreateRecordParams{Kind: KindWorking, Content: "other session private payload", WorkspaceID: "workspace-a", SessionID: "elsewhere"}},
	} {
		t.Run("corrupt "+tc.name, func(t *testing.T) {
			db, store, keyDir := auditRecordStore(t)
			record, err := store.Create(t.Context(), tc.params)
			if err != nil {
				t.Fatal(err)
			}
			if tc.delete {
				if err := store.SoftDelete(t.Context(), record.ID, RecordAccess{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`UPDATE memory_records SET content='corrupted' WHERE id=?`, record.ID); err != nil {
				t.Fatal(err)
			}
			report, err := AuditRecords(t.Context(), db, keyDir)
			assertAuditCategory(t, err, ErrRecordAuditViolation)
			if !errors.Is(err, signing.ErrInvalidSignature) || report.Verified != 0 || report.TotalExtant == nil || *report.TotalExtant != 1 {
				t.Fatalf("AuditRecords(%s) = %+v, %v", tc.name, report, err)
			}
			if got := err.Error(); got == "" || containsAny(got, tc.params.Content, "corrupted") {
				t.Fatalf("audit error disclosed record content: %q", got)
			}
		})
	}
}

func TestAuditRecordsIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name, mutation string
		cause          error
	}{
		{name: "body", mutation: `content='altered body'`, cause: signing.ErrInvalidSignature},
		{name: "metadata", mutation: `metadata='{"altered":true}'`, cause: signing.ErrInvalidSignature},
		{name: "lifecycle", mutation: `expires_at=expires_at+1`, cause: signing.ErrInvalidSignature},
		{name: "provenance", mutation: `source_hash='altered provenance'`, cause: signing.ErrInvalidSignature},
		{name: "invalid signature", mutation: `signature=X'00'`, cause: signing.ErrInvalidSignature},
		{name: "known key algorithm mismatch", mutation: `signature_alg='hmac-sha256'`, cause: signing.ErrKeyMismatch},
		{name: "removed initialized algorithm", mutation: `signature_alg=''`, cause: errMissingRecordSignature},
		{name: "removed initialized key id", mutation: `signature_key_id=''`, cause: errMissingRecordSignature},
		{name: "removed initialized signature", mutation: `signature=X''`, cause: errMissingRecordSignature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, keyDir := auditRecordStore(t)
			record, err := store.Create(t.Context(), CreateRecordParams{
				Kind: KindSemantic, Content: "private record content",
				Provenance: Provenance{SourceKind: "conversation", SourceID: "private source", Hash: "sha256:fixture"},
				Metadata:   []byte(`{"private":"metadata"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE memory_records SET `+tc.mutation+` WHERE id=?`, record.ID); err != nil {
				t.Fatal(err)
			}
			report, err := AuditRecords(t.Context(), db, keyDir)
			assertAuditCategory(t, err, ErrRecordAuditViolation)
			if !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, tc.cause) {
				t.Fatalf("AuditRecords(%s) cause = %v", tc.name, err)
			}
			if report.Verified != 0 || report.TotalExtant == nil || *report.TotalExtant != 1 {
				t.Fatalf("AuditRecords(%s) report = %+v", tc.name, report)
			}
			if got := err.Error(); containsAny(got, "private record content", "private source", "metadata", "altered") {
				t.Fatalf("audit error disclosed stored data: %q", got)
			}
		})
	}
}

func TestAuditRecordsPartialReport(t *testing.T) {
	t.Run("42 of literal 50", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		for i := 0; i < 50; i++ {
			record := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
			record.ID = fmt.Sprintf("record-%02d", i)
			record.Content = "synthetic fixture"
			record.WorkspaceID, record.SessionID = "", ""
			record.Kind = KindSemantic
			if i == 7 {
				record.ExpiresAt = time.UnixMilli(1)
			}
			if i == 9 {
				record.DeletedAt = time.UnixMilli(2)
			}
			if err := signRecord(t.Context(), store.signer, &record); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO memory_records (`+recordColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recordValues(record)...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`UPDATE memory_records SET content='corrupt private payload' WHERE id='record-42'`); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditViolation)
		if report.Verified != 42 || report.TotalExtant == nil || *report.TotalExtant != 50 || !report.Configured {
			t.Fatalf("AuditRecords = %+v, want configured 42 of 50", report)
		}
		if !strings.Contains(err.Error(), `record "record-42"`) || strings.Contains(err.Error(), "private payload") {
			t.Fatalf("unsafe or missing failing ID: %q", err)
		}
	})

	t.Run("known zero", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		report, err := AuditRecords(t.Context(), db, keyDir)
		if err != nil || report.Verified != 0 || report.TotalExtant == nil || *report.TotalExtant != 0 || !report.Configured {
			t.Fatalf("AuditRecords = %+v, %v", report, err)
		}
	})

	t.Run("unknown before count", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		if _, err := db.Exec(`DROP TABLE memory_schema_version`); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if report.TotalExtant != nil || report.Verified != 0 {
			t.Fatalf("AuditRecords = %+v, want unknown total", report)
		}
	})

	t.Run("key failure retains count", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		if _, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "fixture"}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(keyDir, "current.pem")); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if report.TotalExtant == nil || *report.TotalExtant != 1 || report.Verified != 0 || !report.Configured {
			t.Fatalf("AuditRecords = %+v, want configured 0 of 1", report)
		}
	})
}

type auditSnapshotGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

var (
	auditSnapshotRegister sync.Once
	auditSnapshotMu       sync.Mutex
	auditSnapshotCurrent  *auditSnapshotGate
)

func registerAuditSnapshotGate(t *testing.T) {
	t.Helper()
	var registerErr error
	auditSnapshotRegister.Do(func() {
		registerErr = modernsqlite.RegisterScalarFunction("audit_snapshot_gate_447", 1, func(_ *modernsqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			auditSnapshotMu.Lock()
			gate := auditSnapshotCurrent
			auditSnapshotMu.Unlock()
			if gate != nil {
				gate.once.Do(func() { close(gate.entered) })
				<-gate.release
			}
			return args[0], nil
		})
	})
	if registerErr != nil {
		t.Fatal(registerErr)
	}
}

func TestAuditRecordsCountAndVerificationShareSnapshot(t *testing.T) {
	registerAuditSnapshotGate(t)
	gate := &auditSnapshotGate{entered: make(chan struct{}), release: make(chan struct{})}
	auditSnapshotMu.Lock()
	auditSnapshotCurrent = gate
	auditSnapshotMu.Unlock()
	t.Cleanup(func() {
		auditSnapshotMu.Lock()
		auditSnapshotCurrent = nil
		auditSnapshotMu.Unlock()
	})

	path := filepath.Join(t.TempDir(), "snapshot.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close snapshot database: %v", err)
		}
	})
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	keyDir := filepath.Join(t.TempDir(), "keys")
	store, err := NewMemoryRecordStore(t.Context(), db, RecordStoreConfig{KeyDir: keyDir})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "snapshot original"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE audit_witness (initialized_at INTEGER NOT NULL); INSERT INTO audit_witness VALUES (1); DROP TABLE memory_record_signing; CREATE VIEW memory_record_signing AS SELECT audit_snapshot_gate_447(1) AS id, initialized_at FROM audit_witness`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		report RecordAuditReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := AuditRecords(t.Context(), db, keyDir)
		done <- result{report: report, err: err}
	}()
	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		close(gate.release)
		t.Fatal("audit did not reach post-count witness query")
	}
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		close(gate.release)
		t.Fatal(err)
	}
	if _, err := writer.Exec(`UPDATE memory_records SET content='snapshot corruption' WHERE id=?`, record.ID); err != nil {
		_ = writer.Close()
		close(gate.release)
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		close(gate.release)
		t.Fatal(err)
	}
	close(gate.release)
	got := <-done
	if got.err != nil || got.report.Verified != 1 || got.report.TotalExtant == nil || *got.report.TotalExtant != 1 {
		t.Fatalf("AuditRecords mixed snapshots: %+v, %v", got.report, got.err)
	}
}

func TestAuditRecordsErrorCategories(t *testing.T) {
	t.Run("cancel preserves cause", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := AuditRecords(ctx, db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("AuditRecords cancellation lost cause: %v", err)
		}
	})

	t.Run("future schema is incomplete", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		if _, err := db.Exec(`INSERT INTO memory_schema_version VALUES (4, 'future', 1)`); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if report.TotalExtant != nil {
			t.Fatalf("future schema invented a total: %+v", report)
		}
	})

	t.Run("claimed v3 missing required table is violation", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		if _, err := db.Exec(`DROP TABLE memory_records`); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditViolation)
		if report.TotalExtant != nil {
			t.Fatalf("missing table invented a total: %+v", report)
		}
	})

	t.Run("malformed row preserves meaning cause", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		record, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "private payload"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE memory_records SET kind='invalid-private-kind' WHERE id=?`, record.ID); err != nil {
			t.Fatal(err)
		}
		_, err = AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditViolation)
		if !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, ErrBadKind) || strings.Contains(err.Error(), "invalid-private-kind") {
			t.Fatalf("AuditRecords malformed cause or diagnostic = %v", err)
		}
	})

	t.Run("failing id is escaped", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		record := MemoryRecord{MemoryRecordBody: recordSigningFixture()}
		record.ID = "unsafe\nid"
		record.Kind, record.WorkspaceID, record.SessionID = KindSemantic, "", ""
		if err := signRecord(t.Context(), store.signer, &record); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO memory_records (`+recordColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recordValues(record)...); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE memory_records SET content='corrupt' WHERE id=?`, record.ID); err != nil {
			t.Fatal(err)
		}
		_, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditViolation)
		if strings.Contains(err.Error(), "unsafe\nid") || !strings.Contains(err.Error(), `"unsafe\nid"`) {
			t.Fatalf("failing ID is not safely escaped: %q", err)
		}
	})

	t.Run("nil inputs are incomplete", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		t.Run("context", func(t *testing.T) {
			_, err := AuditRecords(nil, db, keyDir) //nolint:staticcheck // SA1012: intentional public-boundary invalid input.
			assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		})
		t.Run("database", func(t *testing.T) {
			_, err := AuditRecords(t.Context(), nil, keyDir)
			assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		})
	})
}

type auditRecordFileState struct {
	body  string
	mode  os.FileMode
	mtime time.Time
}

func auditRecordTree(t *testing.T, root string) map[string]auditRecordFileState {
	t.Helper()
	state := make(map[string]auditRecordFileState)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var body []byte
		if info.Mode().IsRegular() {
			body, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		state[rel] = auditRecordFileState{body: string(body), mode: info.Mode(), mtime: info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestAuditRecordsReadOnlyAndContentFree(t *testing.T) {
	db, store, keyDir := auditRecordStore(t)
	const privateContent = "content must never leave audit"
	if _, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: privateContent, Metadata: []byte(`{"secret":"metadata"}`)}); err != nil {
		t.Fatal(err)
	}
	var dbPath string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	var schemaBefore string
	if err := db.QueryRow(`SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name)`).Scan(&schemaBefore); err != nil {
		t.Fatal(err)
	}
	dbBefore := auditRecordTree(t, filepath.Dir(dbPath))
	keysBefore := auditRecordTree(t, keyDir)

	report, err := AuditRecords(t.Context(), db, keyDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fmt.Sprintf("%+v", report), privateContent) {
		t.Fatalf("report disclosed record content: %+v", report)
	}
	var schemaAfter string
	if err := db.QueryRow(`SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema WHERE sql IS NOT NULL ORDER BY name)`).Scan(&schemaAfter); err != nil {
		t.Fatal(err)
	}
	if schemaAfter != schemaBefore {
		t.Fatal("audit changed database schema")
	}
	if after := auditRecordTree(t, filepath.Dir(dbPath)); !reflect.DeepEqual(dbBefore, after) {
		t.Fatal("audit changed database bytes, modes, mtimes, or directory entries")
	}
	if after := auditRecordTree(t, keyDir); !reflect.DeepEqual(keysBefore, after) {
		t.Fatal("audit changed key bytes, modes, mtimes, or directory entries")
	}
}

func TestAuditRecordsInitializationAndKeys(t *testing.T) {
	t.Run("user only no key", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "user.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("close user-only database: %v", err)
			}
		})
		if _, err := NewStore(t.Context(), db); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, filepath.Join(t.TempDir(), "absent-keys"))
		if err != nil || report.Configured || report.TotalExtant == nil || *report.TotalExtant != 0 {
			t.Fatalf("AuditRecords(user only) = %+v, %v", report, err)
		}
	})

	for _, version := range []int{1, 2} {
		t.Run("empty schema v"+string(rune('0'+version)), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("close legacy database: %v", err)
				}
			})
			if _, err := db.Exec(`CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO memory_schema_version VALUES (?, 'legacy', 1)`, version); err != nil {
				t.Fatal(err)
			}
			if version == 2 {
				if err := migrateV2ForAuditTest(db); err != nil {
					t.Fatal(err)
				}
			}
			report, err := AuditRecords(t.Context(), db, filepath.Join(t.TempDir(), "absent"))
			if err != nil || report.Configured || report.TotalExtant == nil || *report.TotalExtant != 0 {
				t.Fatalf("AuditRecords(v%d empty) = %+v, %v", version, report, err)
			}
		})
	}

	t.Run("v2 records are unsigned coverage", func(t *testing.T) {
		db := legacyRecordDB(t, filepath.Join(t.TempDir(), "v2.db"), 1)
		report, err := AuditRecords(t.Context(), db, filepath.Join(t.TempDir(), "absent"))
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if report.TotalExtant == nil || *report.TotalExtant != 1 || report.Verified != 0 || report.Configured {
			t.Fatalf("AuditRecords(v2 records) = %+v", report)
		}
	})

	t.Run("empty v3 missing required record column", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "malformed-v3.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Errorf("close malformed-v3 database: %v", err)
			}
		})
		for _, statement := range []string{
			`CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`,
			`INSERT INTO memory_schema_version VALUES (3, 'claimed v3', 1)`,
			`CREATE TABLE memory_records (
			 id TEXT PRIMARY KEY, kind TEXT NOT NULL, content TEXT NOT NULL, namespace TEXT NOT NULL DEFAULT '',
			 workspace_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', source_kind TEXT NOT NULL DEFAULT '',
			 source_id TEXT NOT NULL DEFAULT '', source_start INTEGER NOT NULL DEFAULT 0, source_end INTEGER NOT NULL DEFAULT 0,
			 source_hash TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}', created_at INTEGER NOT NULL,
			 updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0,
			 origin_tool TEXT NOT NULL DEFAULT '', origin_session_id TEXT NOT NULL DEFAULT '', trust_class TEXT NOT NULL DEFAULT '',
			 signature_alg TEXT NOT NULL DEFAULT '', signature BLOB NOT NULL DEFAULT X'')`,
			`CREATE TABLE memory_record_signing (id INTEGER PRIMARY KEY CHECK (id = 1), initialized_at INTEGER NOT NULL)`,
		} {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
		report, err := AuditRecords(t.Context(), db, filepath.Join(t.TempDir(), "absent"))
		assertAuditCategory(t, err, ErrRecordAuditViolation)
		if report.TotalExtant == nil || *report.TotalExtant != 0 || report.Verified != 0 || report.Configured {
			t.Fatalf("AuditRecords(malformed empty v3) = %+v", report)
		}
	})

	for _, tc := range []struct {
		name      string
		signed    bool
		want      error
		wantCause error
	}{
		{name: "missing witness unsigned rows", want: ErrRecordAuditIncomplete},
		{name: "signed rows without witness", signed: true, want: ErrRecordAuditViolation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, store, keyDir := auditRecordStore(t)
			if _, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "fixture"}); err != nil {
				t.Fatal(err)
			}
			if !tc.signed {
				if _, err := db.Exec(`UPDATE memory_records SET signature_alg='', signature_key_id='', signature=X''`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`DELETE FROM memory_record_signing`); err != nil {
				t.Fatal(err)
			}
			report, err := AuditRecords(t.Context(), db, keyDir)
			assertAuditCategory(t, err, tc.want)
			if report.TotalExtant == nil || *report.TotalExtant != 1 || report.Configured {
				t.Fatalf("AuditRecords = %+v", report)
			}
		})
	}

	t.Run("initialized empty missing current key", func(t *testing.T) {
		db, _, keyDir := auditRecordStore(t)
		if err := os.Remove(filepath.Join(keyDir, "current.pem")); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if !errors.Is(err, fs.ErrNotExist) || !report.Configured || report.Verified != 0 || report.TotalExtant == nil || *report.TotalExtant != 0 {
			t.Fatalf("AuditRecords = %+v, %v", report, err)
		}
	})

	t.Run("retained old verifier", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		if _, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "old key"}); err != nil {
			t.Fatal(err)
		}
		old, err := signing.LoadEd25519(filepath.Join(keyDir, "current.pem"))
		if err != nil {
			t.Fatal(err)
		}
		trusted := filepath.Join(keyDir, "trusted")
		if err := os.Mkdir(trusted, 0o700); err != nil {
			t.Fatal(err)
		}
		writeRecordPublicKey(t, filepath.Join(trusted, old.KeyID()+".pem"), old.PublicKey())
		if err := os.Remove(filepath.Join(keyDir, "current.pem")); err != nil {
			t.Fatal(err)
		}
		if _, created, err := signing.LoadOrCreateEd25519(filepath.Join(keyDir, "current.pem")); err != nil || !created {
			t.Fatalf("create replacement key = %v, %v", created, err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		if err != nil || report.Verified != 1 {
			t.Fatalf("AuditRecords(retained) = %+v, %v", report, err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		db, store, keyDir := auditRecordStore(t)
		record, err := store.Create(t.Context(), CreateRecordParams{Kind: KindSemantic, Content: "unknown"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE memory_records SET signature_key_id='unknown' WHERE id=?`, record.ID); err != nil {
			t.Fatal(err)
		}
		report, err := AuditRecords(t.Context(), db, keyDir)
		assertAuditCategory(t, err, ErrRecordAuditIncomplete)
		if !errors.Is(err, signing.ErrUnknownKey) || !errors.Is(err, ErrRecordIntegrity) || report.Verified != 0 {
			t.Fatalf("AuditRecords(unknown key) = %+v, %v", report, err)
		}
	})
}

func migrateV2ForAuditTest(db *sql.DB) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	if err := migrateV2(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func assertAuditCategory(t *testing.T, err, want error) {
	t.Helper()
	if err == nil || !errors.Is(err, want) || errors.Is(err, map[error]error{
		ErrRecordAuditViolation:  ErrRecordAuditIncomplete,
		ErrRecordAuditIncomplete: ErrRecordAuditViolation,
	}[want]) {
		t.Fatalf("error category = %v, want exactly %v", err, want)
	}
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}
