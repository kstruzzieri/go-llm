package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/memory"
)

func TestAuditMemoryPathDiscovery(t *testing.T) {
	root := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	path := filepath.Join(dataHome, "golem", "memories.db")
	got := auditMemory(t.Context(), root)
	want := auditResult{
		scope: "memory", assurance: "signed", outcome: "not-present",
		diagnostics: []auditDiagnostic{{code: "store-not-present", target: path, message: "Store is not present."}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auditMemory(absent) = %+v, want %+v", got, want)
	}

	t.Setenv("XDG_DATA_HOME", root)
	got = auditMemory(t.Context(), root)
	want = auditResult{
		scope: "memory", assurance: "signed", outcome: "incomplete",
		diagnostics: []auditDiagnostic{{code: "store-unavailable", message: "Store is unavailable or unsafe."}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auditMemory(inside workspace) = %+v, want %+v", got, want)
	}
}

func TestAuditMemoryOutcomesAndPartialProgress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *sql.DB, *memory.MemoryRecordStore) (int64, int64)
		outcome string
		code    string
	}{
		{name: "initialized empty", outcome: "valid"},
		{
			name: "partial violation", outcome: "violation", code: "memory-record-invalid",
			prepare: func(t *testing.T, db *sql.DB, store *memory.MemoryRecordStore) (int64, int64) {
				ids := make([]string, 2)
				for i, content := range []string{"first private body", "second private body"} {
					record, err := store.Create(t.Context(), memory.CreateRecordParams{Kind: memory.KindSemantic, Content: content})
					if err != nil {
						t.Fatal(err)
					}
					ids[i] = record.ID
				}
				sort.Strings(ids)
				if _, err := db.Exec(`UPDATE memory_records SET content='corrupt private body' WHERE id=?`, ids[1]); err != nil {
					t.Fatal(err)
				}
				return 1, 2
			},
		},
		{
			name: "unknown verifier", outcome: "incomplete", code: "memory-record-incomplete",
			prepare: func(t *testing.T, db *sql.DB, store *memory.MemoryRecordStore) (int64, int64) {
				record, err := store.Create(t.Context(), memory.CreateRecordParams{Kind: memory.KindSemantic, Content: "unknown-key private body"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE memory_records SET signature_key_id='private-unknown-key' WHERE id=?`, record.ID); err != nil {
					t.Fatal(err)
				}
				return 0, 1
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dataHome := t.TempDir()
			t.Setenv("XDG_DATA_HOME", dataHome)
			path := filepath.Join(dataHome, "golem", "memories.db")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			store, err := memory.NewMemoryRecordStore(t.Context(), db, memory.RecordStoreConfig{KeyDir: path + ".keys"})
			if err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			var wantChecked, wantTotal int64
			if tc.prepare != nil {
				wantChecked, wantTotal = tc.prepare(t, db, store)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			got := auditMemory(t.Context(), root)
			if got.scope != "memory" || got.assurance != "signed" || got.outcome != tc.outcome || got.checked != wantChecked || got.total == nil || *got.total != wantTotal {
				t.Fatalf("auditMemory = %+v, want %s with %d of %d", got, tc.outcome, wantChecked, wantTotal)
			}
			if got.earlyStop != (tc.code != "") {
				t.Fatalf("earlyStop = %v", got.earlyStop)
			}
			if tc.code == "" {
				if len(got.diagnostics) != 0 {
					t.Fatalf("valid diagnostics = %+v", got.diagnostics)
				}
				return
			}
			if len(got.diagnostics) != 1 || got.diagnostics[0].code != tc.code || got.diagnostics[0].target != path {
				t.Fatalf("diagnostics = %+v", got.diagnostics)
			}
			for _, private := range []string{"private body", "private-unknown-key", "corrupt"} {
				if strings.Contains(fmt.Sprint(got), private) {
					t.Fatalf("audit result disclosed %q: %+v", private, got)
				}
			}
		})
	}
}

func TestAuditMemoryUserOnlyDatabaseIsNotConfigured(t *testing.T) {
	root := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	path := filepath.Join(dataHome, "golem", "memories.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.NewStore(t.Context(), db); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got := auditMemory(t.Context(), root)
	if got.scope != "memory" || got.assurance != "signed" || got.outcome != "not-configured" || got.checked != 0 || got.total == nil || *got.total != 0 || len(got.diagnostics) != 0 {
		t.Fatalf("auditMemory(user only) = %+v", got)
	}
	if _, err := os.Stat(path + ".keys"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("audit created signing keys: %v", err)
	}
}

func TestAuditMemoryUsesOnlyPublicErrorTags(t *testing.T) {
	total := int64(50)
	report := memory.RecordAuditReport{Verified: 42, TotalExtant: &total, Configured: true}
	for _, tc := range []struct {
		name, outcome, code string
		err                 error
	}{
		{name: "violation", outcome: "violation", code: "memory-record-invalid", err: fmt.Errorf("private cause text: %w", memory.ErrRecordAuditViolation)},
		{name: "incomplete", outcome: "incomplete", code: "memory-record-incomplete", err: fmt.Errorf("private cause text: %w", memory.ErrRecordAuditIncomplete)},
		{name: "unclassified", outcome: "incomplete", code: "memory-record-incomplete", err: errors.New("private unexpected error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := auditMemoryResult(report, tc.err, "/safe/memories.db")
			if got.outcome != tc.outcome || got.checked != 42 || got.total == nil || *got.total != 50 || !got.earlyStop {
				t.Fatalf("auditMemoryResult = %+v", got)
			}
			if len(got.diagnostics) != 1 || got.diagnostics[0].code != tc.code || strings.Contains(fmt.Sprint(got), "private") {
				t.Fatalf("unsafe or incorrect diagnostic = %+v", got.diagnostics)
			}
		})
	}
	unconfirmed := report
	unconfirmed.Configured = false
	if got := auditMemoryResult(unconfirmed, memory.ErrRecordAuditIncomplete, "/safe/memories.db"); got.outcome != "incomplete" {
		t.Fatalf("configured=false error was reported as %q", got.outcome)
	}
}
