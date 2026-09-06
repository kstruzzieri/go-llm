package memory

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

func recordTestConfig(t *testing.T) RecordStoreConfig {
	t.Helper()
	signer, err := signing.NewEd25519Signer(ed25519.NewKeyFromSeed(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	ring, err := signing.NewKeyring(signer.Verifier())
	if err != nil {
		t.Fatal(err)
	}
	return RecordStoreConfig{Signer: signer, Verifiers: ring, Initialize: true}
}

// This literal predates signing; using migrateV2 here would let changes to the
// migration redefine the historical input this regression is meant to pin.
func legacyRecordDB(t *testing.T, path string, count int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`,
		`INSERT INTO memory_schema_version VALUES (2, 'literal legacy schema', 1)`,
		`CREATE TABLE memory_records (
		 id TEXT PRIMARY KEY, kind TEXT NOT NULL, content TEXT NOT NULL, namespace TEXT NOT NULL DEFAULT '',
		 workspace_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
		 source_kind TEXT NOT NULL DEFAULT '', source_id TEXT NOT NULL DEFAULT '', source_start INTEGER NOT NULL DEFAULT 0,
		 source_end INTEGER NOT NULL DEFAULT 0, source_hash TEXT NOT NULL DEFAULT '', metadata TEXT NOT NULL DEFAULT '{}',
		 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, expires_at INTEGER NOT NULL DEFAULT 0, deleted_at INTEGER NOT NULL DEFAULT 0)`,
		`CREATE VIRTUAL TABLE memory_records_fts USING fts5(id UNINDEXED, content, tokenize='porter')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for i := range count {
		id := fmt.Sprintf("legacy-%04d", i)
		if _, err := db.Exec(`INSERT INTO memory_records (id,kind,content,workspace_id,session_id,source_kind,source_id,metadata,created_at,updated_at,expires_at,deleted_at) VALUES (?, 'working', 'legacy needle', 'workspace', 'historical-session', 'user', 'historical-claim', '  ', 1234, 5678, 9, ?)`, id, i%2); err != nil {
			t.Fatal(err)
		}
		if i%2 == 0 {
			if _, err := db.Exec(`INSERT INTO memory_records_fts (id,content) VALUES (?, 'legacy needle')`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	return db
}

func TestRecordLegacyBatchesAndUserOnlyMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "memories.db")
	db := legacyRecordDB(t, path, 257)
	if _, err := NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".keys"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("user-only open created keys: %v", err)
	}
	store, err := NewMemoryRecordStore(ctx, db, RecordStoreConfig{KeyDir: path + ".keys"})
	if err != nil {
		t.Fatal(err)
	}
	if len(store.CreatedKeyID()) != 64 {
		t.Fatalf("missing created key ID: %q", store.CreatedKeyID())
	}
	rows, err := db.QueryContext(ctx, `SELECT `+recordColumns+` FROM memory_records ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for rows.Next() {
		m, err := scanRecord(rows)
		if err != nil {
			t.Fatal(err)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate %s", m.ID)
		}
		seen[m.ID] = true
		if m.Content != "legacy needle" || m.WorkspaceID != "workspace" || m.SessionID != "historical-session" || toMs(m.DeletedAt) != int64((len(seen)-1)%2) {
			t.Fatalf("legacy body or tombstone changed: %+v", m.MemoryRecordBody)
		}
		if err := verifyRecord(ctx, store.verifiers, m); err != nil {
			t.Fatal(err)
		}
		if m.Provenance.OriginTool != "legacy-migration" || m.Provenance.OriginSessionID != "historical-session" || m.Provenance.TrustClass != "legacy-unreviewed" || m.Provenance.SourceKind != "user" || m.Provenance.SourceID != "historical-claim" {
			t.Fatalf("incorrect legacy provenance: %+v", m.Provenance)
		}
		if m.CreatedAt.UnixMilli() != 1234 || m.UpdatedAt.UnixMilli() != 5678 || m.ExpiresAt.UnixMilli() != 9 || string(m.Metadata) != "{}" {
			t.Fatalf("legacy values changed: %+v", m.MemoryRecordBody)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 257 {
		t.Fatalf("records imported = %d", len(seen))
	}
	for i := range 257 {
		if !seen[fmt.Sprintf("legacy-%04d", i)] {
			t.Fatalf("legacy ID %d missing", i)
		}
	}
	var ftsCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_records_fts`).Scan(&ftsCount); err != nil || ftsCount != 129 {
		t.Fatalf("fts=%d err=%v", ftsCount, err)
	}
	reopened, err := NewMemoryRecordStore(ctx, db, RecordStoreConfig{KeyDir: path + ".keys"})
	if err != nil || reopened.CreatedKeyID() != "" {
		t.Fatalf("reopen: %v", err)
	}
}

type countingRecordSigner struct {
	signing.Signer
	calls, failAt int
	cancel        context.CancelFunc
	beforeSign    func()
}

func (s *countingRecordSigner) Sign(ctx context.Context, domain string, payload []byte) (signing.Signature, error) {
	s.calls++
	if s.beforeSign != nil {
		s.beforeSign()
	}
	if s.calls == s.failAt {
		if s.cancel != nil {
			s.cancel()
			return signing.Signature{}, ctx.Err()
		}
		return signing.Signature{}, errors.New("synthetic signer unavailable")
	}
	return s.Signer.Sign(ctx, domain, payload)
}

func TestRecordLegacyRollback(t *testing.T) {
	for _, failure := range []string{"later signer", "cancel", "invalid kind", "empty id", "invalid scope", "invalid range", "invalid json", "scan", "oversized content", "oversized whitespace", "sql"} {
		t.Run(failure, func(t *testing.T) {
			db := legacyRecordDB(t, filepath.Join(t.TempDir(), "records.db"), 257)
			config := recordTestConfig(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			signer := &countingRecordSigner{Signer: config.Signer}
			config.Signer = signer
			var statement string
			var arg any
			switch failure {
			case "later signer":
				signer.failAt = 130 // challenge + first closed batch + one
			case "cancel":
				signer.failAt, signer.cancel = 130, cancel
			case "empty id":
				statement, arg = `UPDATE memory_records SET id = ? WHERE id = 'legacy-0256'`, ""
			case "invalid kind":
				statement, arg = `UPDATE memory_records SET kind = ? WHERE id = 'legacy-0256'`, "unsupported"
			case "invalid scope":
				statement, arg = `UPDATE memory_records SET workspace_id = ? WHERE id = 'legacy-0256'`, ""
			case "invalid range":
				statement, arg = `UPDATE memory_records SET source_start = 9, source_end = ? WHERE id = 'legacy-0256'`, 2
			case "invalid json":
				statement, arg = `UPDATE memory_records SET metadata = ? WHERE id = 'legacy-0256'`, `{"private-payload":}`
			case "scan":
				statement, arg = `UPDATE memory_records SET source_start = ? WHERE id = 'legacy-0256'`, "private-payload"
			case "oversized content":
				statement, arg = `UPDATE memory_records SET content = ? WHERE id = 'legacy-0256'`, strings.Repeat("x", 32769)
			case "oversized whitespace":
				statement, arg = `UPDATE memory_records SET metadata = ? WHERE id = 'legacy-0256'`, strings.Repeat(" ", 32769)
			case "sql":
				statement = `CREATE TRIGGER fail_import BEFORE UPDATE ON memory_records WHEN OLD.id = 'legacy-0256' BEGIN SELECT RAISE(ABORT, 'synthetic SQL failure'); END`
			}
			if statement != "" {
				args := []any{}
				if arg != nil {
					args = append(args, arg)
				}
				if _, err := db.Exec(statement, args...); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewMemoryRecordStore(ctx, db, config)
			if err == nil {
				t.Fatal("import succeeded")
			}
			if strings.Contains(err.Error(), "private-payload") {
				t.Fatalf("payload diagnostic: %v", err)
			}
			if failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			var signed, marker, fts int
			if err := db.QueryRow(`SELECT COUNT(*) FROM memory_records WHERE length(signature) != 0 OR origin_tool != '' OR metadata != '  '`).Scan(&signed); err != nil {
				t.Fatal(err)
			}
			want := 0
			if failure == "invalid json" || failure == "oversized whitespace" {
				want = 1
			}
			if signed != want {
				t.Fatalf("earlier batches changed: count=%d want=%d", signed, want)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM memory_record_signing`).Scan(&marker); err != nil || marker != 0 {
				t.Fatalf("marker=%d err=%v", marker, err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM memory_records_fts`).Scan(&fts); err != nil || fts != 129 {
				t.Fatalf("fts=%d err=%v", fts, err)
			}
		})
	}
}

func TestRecordInitializationAuthorization(t *testing.T) {
	for _, state := range []string{"empty injected denied", "initialized injected denied", "partial signer", "partial ring", "conflict", "filesystem initialize", "writer", "wrong ring", "wrong binding", "signed row", "mixed rows", "algorithm only", "key only", "bytes only", "marker changed"} {
		t.Run(state, func(t *testing.T) {
			db := legacyRecordDB(t, ":memory:", 0)
			config := recordTestConfig(t)
			ctx := context.Background()
			switch state {
			case "empty injected denied":
				config.Initialize = false
			case "initialized injected denied":
				if _, err := NewMemoryRecordStore(ctx, db, config); err != nil {
					t.Fatal(err)
				}
			case "partial signer":
				config.Verifiers = nil
			case "partial ring":
				config.Signer = nil
			case "conflict":
				config.KeyDir = filepath.Join(t.TempDir(), "keys")
			case "filesystem initialize":
				config.Signer, config.Verifiers, config.KeyDir = nil, nil, filepath.Join(t.TempDir(), "keys")
			case "writer":
				config.Writer = 255
			case "wrong ring":
				config.Verifiers = &signing.Keyring{}
			case "wrong binding":
				config.Signer = wrongBindingSigner{config.Signer}
			case "algorithm only", "key only", "bytes only":
				if _, err := NewStore(ctx, db); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO memory_records (id,kind,content,created_at,updated_at) VALUES ('partial','semantic','unsigned',1,1)`); err != nil {
					t.Fatal(err)
				}
				statement := `UPDATE memory_records SET signature_alg = 'ed25519'`
				if state == "key only" {
					statement = `UPDATE memory_records SET signature_key_id = 'some-key'`
				}
				if state == "bytes only" {
					statement = `UPDATE memory_records SET signature = X'01'`
				}
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			case "signed row", "mixed rows":
				s, err := NewMemoryRecordStore(ctx, db, config)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "signed"}); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DELETE FROM memory_record_signing`); err != nil {
					t.Fatal(err)
				}
				if state == "mixed rows" {
					if _, err := db.Exec(`INSERT INTO memory_records (id,kind,content,created_at,updated_at) VALUES ('unsigned','semantic','unsigned',1,1)`); err != nil {
						t.Fatal(err)
					}
				}
			case "marker changed":
				if _, err := NewStore(ctx, db); err != nil {
					t.Fatal(err)
				}
				config.Signer = &countingRecordSigner{Signer: config.Signer, beforeSign: func() {
					if _, err := db.Exec(`INSERT INTO memory_record_signing VALUES (1, 1)`); err != nil {
						t.Fatal(err)
					}
				}}
			}
			if _, err := NewMemoryRecordStore(ctx, db, config); err == nil {
				t.Fatal("invalid initialization allowed")
			} else if state == "marker changed" && err.Error() != "memory: record initialization changed during import" {
				t.Fatalf("marker was not rechecked before import: %v", err)
			}
		})
	}
}

type wrongBindingSigner struct{ signing.Signer }

func (s wrongBindingSigner) KeyID() string { return "different-identity" }

func writeRecordPublicKey(t *testing.T, path string, key ed25519.PublicKey) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRecordKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "records.db")
	rt, err := OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	oldID := rt.Store().CreatedKeyID()
	oldRecord, err := rt.Store().Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "old-key needle"})
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	currentPath := path + ".keys/current.pem"
	old, err := signing.LoadEd25519(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	if old.KeyID() != oldID {
		t.Fatal("created identity mismatch")
	}
	trusted := path + ".keys/trusted"
	if err := os.Mkdir(trusted, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRecordPublicKey(t, filepath.Join(trusted, oldID+".pem"), old.PublicKey())
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatalf("exact current duplicate failed: %v", err)
	}
	if rt.Store().CreatedKeyID() != "" {
		t.Fatal("identity recreated")
	}
	rt.Close()
	if err := os.Rename(currentPath, currentPath+".retired"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRecordStore(ctx, path, RecordStoreConfig{}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(currentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing initialized key was recreated")
	}
	// Operator rotation: a new current identity, with the old public verifier retained.
	newKey, _, err := signing.LoadOrCreateEd25519(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	newRecord, err := rt.Store().Create(ctx, CreateRecordParams{Kind: KindSemantic, Content: "new-key needle"})
	if err != nil {
		t.Fatal(err)
	}
	if newRecord.Signature.KeyID != newKey.KeyID() {
		t.Fatal("rotation did not change writer")
	}
	if _, err := rt.Store().Get(ctx, oldRecord.ID, RecordAccess{}); err != nil {
		t.Fatal(err)
	}
	rt.Close()
	if err := os.Remove(filepath.Join(trusted, oldID+".pem")); err != nil {
		t.Fatal(err)
	}
	rt, err = OpenRecordStore(ctx, path, RecordStoreConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rt.Store().Get(ctx, newRecord.ID, RecordAccess{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Store().Get(ctx, oldRecord.ID, RecordAccess{}); !errors.Is(err, ErrRecordIntegrity) || !errors.Is(err, signing.ErrUnknownKey) {
		t.Fatalf("old key not classified: %v", err)
	}
	if got, err := rt.Store().Search(ctx, "needle", RecordSearchOptions{}); got != nil || !errors.Is(err, ErrRecordIntegrity) {
		t.Fatalf("partial result after retired verifier loss: count=%d err=%v", len(got), err)
	}
}

func TestRecordRetainedVerifierValidation(t *testing.T) {
	for _, failure := range []string{"invalid current duplicate", "misnamed", "loose file", "loose directory", "empty loose directory", "symlink directory"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "records.db")
			rt, err := OpenRecordStore(context.Background(), path, RecordStoreConfig{})
			if err != nil {
				t.Fatal(err)
			}
			rt.Close()
			key, err := signing.LoadEd25519(path + ".keys/current.pem")
			if err != nil {
				t.Fatal(err)
			}
			trusted := path + ".keys/trusted"
			if err := os.Mkdir(trusted, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(trusted, key.KeyID()+".pem")
			writeRecordPublicKey(t, file, key.PublicKey())
			switch failure {
			case "invalid current duplicate":
				err = os.WriteFile(file, []byte("invalid PEM"), 0o600)
			case "misnamed":
				err = os.Rename(file, filepath.Join(trusted, "wrong.pem"))
			case "loose file":
				err = os.Chmod(file, 0o644)
			case "loose directory":
				err = os.Chmod(trusted, 0o755)
			case "empty loose directory":
				if err = os.Remove(file); err == nil {
					err = os.Chmod(trusted, 0o755)
				}
			case "symlink directory":
				if err = os.Rename(trusted, trusted+"-target"); err == nil {
					err = os.Symlink(trusted+"-target", trusted)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRecordStore(context.Background(), path, RecordStoreConfig{}); err == nil {
				t.Fatal("invalid retained verifier accepted")
			}
		})
	}
}

func TestRecordFailedBootstrapWitnessAndResetMarker(t *testing.T) {
	for _, failure := range []string{"failed import", "reset marker"} {
		t.Run(failure, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "records.db")
			db := legacyRecordDB(t, path, 1)
			if failure == "failed import" {
				if _, err := db.Exec(`UPDATE memory_records SET kind = 'unknown'`); err != nil {
					t.Fatal(err)
				}
			}
			_, err := NewMemoryRecordStore(context.Background(), db, RecordStoreConfig{KeyDir: path + ".keys"})
			if failure == "failed import" && err == nil {
				t.Fatal("bad import succeeded")
			}
			if failure == "reset marker" && err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`DELETE FROM memory_record_signing`); err != nil {
				t.Fatal(err)
			}
			if _, err := NewMemoryRecordStore(context.Background(), db, RecordStoreConfig{KeyDir: path + ".keys"}); !errors.Is(err, ErrIncompleteInitialization) {
				t.Fatalf("witness lost: %v", err)
			}
			if failure == "failed import" {
				// Explicit operator recovery via injected credentials authorizes
				// the repaired unsigned snapshot; ordinary filesystem reopen did not.
				if _, err := db.Exec(`UPDATE memory_records SET kind = 'working'`); err != nil {
					t.Fatal(err)
				}
				if _, err := NewMemoryRecordStore(context.Background(), db, recordTestConfig(t)); err != nil {
					t.Fatalf("explicit repaired import: %v", err)
				}
			}
		})
	}
}
