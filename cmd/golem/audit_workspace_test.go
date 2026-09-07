package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
)

const auditHashABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
const auditHashEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
const auditHashNew = "11507a0e2f5e69d5dfa40a62a1bd7b6ee57e6bcd85c67c9b8431b36fff21c437"

type auditWorkspaceFixture struct {
	t         *testing.T
	root, key string
	store     *checkpointStore
	signer    signing.Signer
}

func newAuditWorkspaceFixture(t *testing.T) *auditWorkspaceFixture {
	t.Helper()
	root, data := t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	s, err := openCheckpointStore(context.Background(), os.Getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	key := filepath.Join(data, "golem", "signing", "agent-ed25519.pem")
	signer, _, err := signing.LoadOrCreateEd25519(key)
	if err != nil {
		t.Fatal(err)
	}
	return &auditWorkspaceFixture{t: t, root: root, key: key, store: s, signer: signer}
}

func (f *auditWorkspaceFixture) body(path, before, after string) agenttools.MutationReceiptBody {
	return agenttools.MutationReceiptBody{Kind: "intent", MutationID: rand.Text(), WorkspaceHash: f.store.workspaceHash, Path: path, BeforeHash: before, AfterHash: after, AgentID: f.signer.KeyID(), Timestamp: "2026-09-06T12:00:00Z"}
}
func (f *auditWorkspaceFixture) raw(body agenttools.MutationReceiptBody) []byte {
	f.t.Helper()
	receipt, err := agenttools.SignMutationReceipt(context.Background(), f.signer, body)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := signing.MarshalCanonical(receipt)
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}
func (f *auditWorkspaceFixture) add(body agenttools.MutationReceiptBody, applied bool) {
	f.t.Helper()
	intent := f.raw(body)
	var after any
	if applied {
		body.Kind = "applied"
		after = f.raw(body)
	}
	checkpointSQL(f.t, f.store.db, `INSERT INTO mutation_receipts(mutation_id,intent_json,applied_json) VALUES(?,?,?)`, body.MutationID, intent, after)
}
func (f *auditWorkspaceFixture) write(path, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, path), []byte(content), 0600); err != nil {
		f.t.Fatal(err)
	}
}
func (f *auditWorkspaceFixture) audit() auditResult {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatal(err)
	}
	return auditWorkspace(context.Background(), f.root)
}
func assertAuditWorkspace(t *testing.T, got auditResult, outcome string, checked, paths int64, code string) {
	t.Helper()
	if got.scope != "workspace" || got.assurance != "signed" || got.outcome != outcome || got.checked != checked || got.paths != paths {
		t.Fatalf("audit = %+v, want %s checked=%d paths=%d", got, outcome, checked, paths)
	}
	if code != "" && (len(got.diagnostics) == 0 || got.diagnostics[len(got.diagnostics)-1].code != code) {
		t.Fatalf("diagnostics = %+v, want %s", got.diagnostics, code)
	}
}

func TestAuditWorkspaceLatestApplied(t *testing.T) {
	f := newAuditWorkspaceFixture(t)
	first := f.body("a.txt", "absent", auditHashABC)
	f.add(first, true)
	f.checkpoint(first, nil, false, nil)
	checkpointSQL(t, f.store.db, `DELETE FROM checkpoints`) // Prune snapshots, retain signed evidence.
	second := f.body("a.txt", auditHashEmpty, auditHashNew) // Authorized edit may incorporate external content.
	second.Timestamp = "2026-09-05T12:00:00Z"
	f.add(second, true)
	checkpointSQL(t, f.store.db, `UPDATE mutation_receipts SET sequence=sequence+10`)
	f.write("a.txt", "new")
	assertAuditWorkspace(t, f.audit(), "valid", 2, 1, "")
}

func (f *auditWorkspaceFixture) checkpoint(body agenttools.MutationReceiptBody, prior []byte, restored bool, inverse any) {
	f.t.Helper()
	checkpointSQL(f.t, f.store.db, `INSERT OR IGNORE INTO checkpoints(id,created_at,goal,state) VALUES(1,'2026-09-06T12:00:00Z','','completed')`)
	var mode any
	if body.AfterMode != nil {
		mode = *body.AfterMode
	}
	checkpointSQL(f.t, f.store.db, `INSERT INTO checkpoint_files(checkpoint_id,path,prior_content,prior_hash,existed,after_hash,summary,at,applied,restored,after_mode,forward_mutation_id,inverse_mutation_id) VALUES(1,?,?,?,?,?,'','2026-09-06T12:00:00Z',1,?,?,?,?)`, body.Path, prior, body.BeforeHash, body.BeforeHash != "absent", body.AfterHash, restored, mode, body.MutationID, inverse)
}

func TestAuditWorkspaceEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, want, code string
		change           func(*auditWorkspaceFixture, agenttools.MutationReceiptBody)
	}{
		{"completed inverse", "valid", "", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			i := storeInverse(b, rand.Text())
			f.add(i, true)
			f.checkpoint(b, []byte{}, true, i.MutationID)
			f.write("a.txt", "")
		}},
		{"undo create to absence", "valid", "", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `DELETE FROM mutation_receipts`)
			b.BeforeHash = "absent"
			f.add(b, true)
			i := storeInverse(b, rand.Text())
			f.add(i, true)
			if err := os.Remove(filepath.Join(f.root, "a.txt")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"wrong workspace", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			b.WorkspaceHash = auditHashABC
			checkpointSQL(f.t, f.store.db, `DELETE FROM mutation_receipts`)
			f.add(b, true)
		}},
		{"wrong mutation identity", "violation", "receipt-invalid", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET mutation_id=?`, rand.Text())
		}},
		{"invalid inverse lineage", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			i := storeInverse(b, rand.Text())
			i.AfterHash = auditHashNew
			f.add(i, true)
		}},
		{"missing original", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			i := storeInverse(b, rand.Text())
			i.UndoOf = rand.Text()
			f.add(i, true)
		}},
		{"duplicate inverse", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.add(storeInverse(b, rand.Text()), true)
			f.add(storeInverse(b, rand.Text()), true)
		}},
		{"multiple unconfirmed inverse", "incomplete", "receipt-unconfirmed", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.add(storeInverse(b, rand.Text()), false)
			f.add(storeInverse(b, rand.Text()), false)
		}},
		{"checkpoint binding", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, []byte{}, false, nil)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoint_files SET path='other.txt'`)
		}},
		{"empty before image altered", "violation", "checkpoint-content", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, []byte("changed"), false, nil)
		}},
		{"nil blob cannot bypass hash", "violation", "checkpoint-content", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `DELETE FROM mutation_receipts`)
			b.BeforeHash = auditHashNew
			f.add(b, true)
			f.checkpoint(b, nil, false, nil)
		}},
		{"restored before image altered", "violation", "checkpoint-content", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			i := storeInverse(b, rand.Text())
			f.add(i, true)
			f.checkpoint(b, []byte("changed"), true, i.MutationID)
			f.write("a.txt", "")
		}},
		{"missing forward reference", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, nil, false, nil)
			checkpointSQL(f.t, f.store.db, `PRAGMA foreign_keys=OFF`)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoint_files SET forward_mutation_id=?`, rand.Text())
		}},
		{"missing inverse reference", "violation", "receipt-binding", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, nil, true, nil)
			checkpointSQL(f.t, f.store.db, `PRAGMA foreign_keys=OFF`)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoint_files SET inverse_mutation_id=?`, rand.Text())
		}},
		{"null forward reference", "incomplete", "checkpoint-unconfirmed", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, nil, false, nil)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoint_files SET forward_mutation_id=NULL`)
		}},
		{"restored without inverse", "incomplete", "checkpoint-unconfirmed", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) { f.checkpoint(b, nil, true, nil) }},
		{"open checkpoint", "incomplete", "checkpoint-unconfirmed", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, nil, false, nil)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoints SET state='open'`)
		}},
		{"undoing checkpoint", "incomplete", "checkpoint-unconfirmed", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.checkpoint(b, nil, false, nil)
			checkpointSQL(f.t, f.store.db, `UPDATE checkpoints SET state='undoing'`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			b := f.body("a.txt", auditHashEmpty, auditHashABC)
			f.add(b, true)
			f.write("a.txt", "abc")
			tc.change(f, b)
			got := f.audit()
			if got.outcome != tc.want || tc.code != "" && (len(got.diagnostics) == 0 || got.diagnostics[len(got.diagnostics)-1].code != tc.code) {
				t.Fatalf("audit = %+v, want %s/%s", got, tc.want, tc.code)
			}
		})
	}
}

func TestAuditWorkspaceSchema(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"legacy", "PRAGMA user_version=2", "incomplete"},
		{"future", "PRAGMA user_version=4", "incomplete"},
		{"missing checkpoints", "DROP TABLE checkpoints", "violation"},
		{"missing file column", "ALTER TABLE checkpoint_files DROP COLUMN summary", "violation"},
		{"missing ledger", "DROP TABLE mutation_receipts", "violation"},
		{"nonpositive sequence", `UPDATE mutation_receipts SET sequence=0`, "violation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			if tc.name == "nonpositive sequence" {
				f.add(f.body("a.txt", "absent", auditHashABC), true)
				f.write("a.txt", "abc")
			}
			checkpointSQL(t, f.store.db, tc.sql)
			got := f.audit()
			if got.outcome != tc.want {
				t.Fatalf("audit=%+v, want %s", got, tc.want)
			}
		})
	}
}

func TestAuditWorkspaceAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*auditWorkspaceFixture, agenttools.MutationReceiptBody)
	}{
		{"intent signature", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			raw := f.raw(b)
			r, err := agenttools.DecodeMutationReceipt(raw)
			if err != nil {
				f.t.Fatal(err)
			}
			r.Signature.Bytes[0] ^= 1
			raw, err = signing.MarshalCanonical(r)
			if err != nil {
				f.t.Fatal(err)
			}
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET intent_json=?`, raw)
		}},
		{"applied signature", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			b.Kind = "applied"
			r, err := agenttools.DecodeMutationReceipt(f.raw(b))
			if err != nil {
				f.t.Fatal(err)
			}
			r.Signature.Bytes[0] ^= 1
			raw, err := signing.MarshalCanonical(r)
			if err != nil {
				f.t.Fatal(err)
			}
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET applied_json=?`, raw)
		}},
		{"body tamper", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET intent_json=?`, bytes.ReplaceAll(f.raw(b), []byte(auditHashABC), []byte(auditHashNew)))
		}},
		{"noncanonical", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET intent_json=?`, append([]byte(" "), f.raw(b)...))
		}},
		{"phase disagreement", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			b.Kind = "applied"
			b.AfterHash = auditHashNew
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET applied_json=?`, f.raw(b))
		}},
		{"internal identity disagreement", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			r, err := agenttools.DecodeMutationReceipt(f.raw(b))
			if err != nil {
				f.t.Fatal(err)
			}
			r.Body.AgentID = auditHashEmpty
			raw, err := signing.MarshalCanonical(r)
			if err != nil {
				f.t.Fatal(err)
			}
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET intent_json=?`, raw)
		}},
		{"missing key", "incomplete", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			if err := os.Remove(f.key); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"insecure key", "incomplete", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			if err := os.Chmod(f.key, 0644); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"mismatched key", "incomplete", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			if err := os.Remove(f.key); err != nil {
				f.t.Fatal(err)
			}
			if _, _, err := signing.LoadOrCreateEd25519(f.key); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			b := f.body("a.txt", "absent", auditHashABC)
			f.add(b, true)
			f.write("a.txt", "abc")
			tc.change(f, b)
			got := f.audit()
			if got.outcome != tc.want || got.paths != 0 || !got.earlyStop {
				t.Fatalf("audit=%+v, want %s before live checks", got, tc.want)
			}
		})
	}
}

func TestAuditWorkspaceCurrentState(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(*auditWorkspaceFixture, agenttools.MutationReceiptBody)
	}{
		{"external edit", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) { f.write("a.txt", "new") }},
		{"deletion", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			if err := os.Remove(filepath.Join(f.root, "a.txt")); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"recreation after undo", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.add(storeInverse(b, rand.Text()), true)
		}},
		{"directory replacement", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			p := filepath.Join(f.root, "a.txt")
			if err := os.Remove(p); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Mkdir(p, 0700); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"outside symlink", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			p := filepath.Join(f.root, "a.txt")
			if err := os.Remove(p); err != nil {
				f.t.Fatal(err)
			}
			outside := filepath.Join(f.t.TempDir(), "secret")
			if err := os.WriteFile(outside, []byte("abc"), 0600); err != nil {
				f.t.Fatal(err)
			}
			if err := os.Symlink(outside, p); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"later unconfirmed suppresses mismatch", "incomplete", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			f.add(f.body("a.txt", auditHashABC, auditHashNew), false)
			f.write("a.txt", "new")
		}},
		{"intent bytes match still incomplete", "incomplete", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			checkpointSQL(f.t, f.store.db, `UPDATE mutation_receipts SET applied_json=NULL`)
		}},
		{"tracked mode survives overwrite and undo", "violation", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			mode := uint32(0600)
			b.AfterMode = &mode
			checkpointSQL(f.t, f.store.db, `DELETE FROM mutation_receipts`)
			f.add(b, true)
			next := f.body("a.txt", auditHashABC, auditHashNew)
			f.add(next, true)
			f.add(storeInverse(next, rand.Text()), true)
			if err := os.Chmod(filepath.Join(f.root, "a.txt"), 0644); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"mode reset on untracked create", "valid", func(f *auditWorkspaceFixture, b agenttools.MutationReceiptBody) {
			mode := uint32(0600)
			b.AfterMode = &mode
			checkpointSQL(f.t, f.store.db, `DELETE FROM mutation_receipts`)
			f.add(b, true)
			f.add(storeInverse(b, rand.Text()), true)
			f.add(f.body("a.txt", "absent", auditHashABC), true)
			if err := os.Chmod(filepath.Join(f.root, "a.txt"), 0644); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			b := f.body("a.txt", "absent", auditHashABC)
			f.add(b, true)
			f.write("a.txt", "abc")
			tc.change(f, b)
			got := f.audit()
			if got.outcome != tc.want {
				t.Fatalf("audit=%+v, want %s", got, tc.want)
			}
		})
	}
}

func TestAuditWorkspaceUniquePathChecks(t *testing.T) {
	for _, scenario := range []string{"valid", "late invalid", "late unconfirmed", "later applied", "retained old intent"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			for i := 0; i < 60; i++ {
				for _, path := range []string{"b.txt", "a.txt"} {
					b := f.body(path, auditHashEmpty, auditHashABC)
					f.add(b, true)
					f.add(storeInverse(b, rand.Text()), true)
				}
			}
			f.write("a.txt", "")
			f.write("b.txt", "")
			if scenario == "late invalid" {
				b := f.body("a.txt", auditHashEmpty, auditHashABC)
				f.add(b, true)
				checkpointSQL(t, f.store.db, `UPDATE mutation_receipts SET intent_json='{}' WHERE mutation_id=?`, b.MutationID)
			}
			if scenario == "late unconfirmed" || scenario == "later applied" || scenario == "retained old intent" {
				b := f.body("a.txt", auditHashEmpty, auditHashABC)
				f.add(b, false)
				f.write("a.txt", "abc")
				if scenario == "retained old intent" {
					f.checkpoint(b, nil, false, nil)
				}
				if scenario != "late unconfirmed" {
					f.add(f.body("a.txt", auditHashABC, auditHashNew), true)
					f.write("a.txt", "new")
				}
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			var calls []string
			observations := 0
			result := withAuditStore(context.Background(), f.store.dbPath, func(db *sql.DB) auditResult {
				store := &checkpointStore{db: db, dbPath: f.store.dbPath, workspaceHash: f.store.workspaceHash}
				return scanAuditWorkspace(context.Background(), f.root, store, func(root, path string, between func()) (fileState, error) {
					calls = append(calls, path)
					state, err := readWorkspaceFileStable(root, path, func() {
						observations++
						if between != nil {
							between()
						}
					})
					return state, err
				})
			})
			wantCalls, wantOutcome, wantChecked := "[a.txt b.txt]", "valid", int64(240)
			switch scenario {
			case "late invalid":
				wantCalls, wantOutcome, wantChecked = "[]", "violation", 200
			case "late unconfirmed":
				wantCalls, wantOutcome, wantChecked = "[b.txt]", "incomplete", 241
			case "later applied", "retained old intent":
				wantOutcome, wantChecked = "incomplete", 242
			}
			if fmt.Sprint(calls) != wantCalls || observations != 2*len(calls) || result.outcome != wantOutcome || result.checked != wantChecked {
				t.Fatalf("result=%+v calls=%v observations=%d, want %s %s checked=%d", result, calls, observations, wantCalls, wantOutcome, wantChecked)
			}
		})
	}
}

func TestAuditWorkspaceChangingFile(t *testing.T) {
	f := newAuditWorkspaceFixture(t)
	f.add(f.body("a.txt", "absent", auditHashABC), true)
	f.write("a.txt", "abc")
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	got := withAuditStore(context.Background(), f.store.dbPath, func(db *sql.DB) auditResult {
		return scanAuditWorkspace(context.Background(), f.root, &checkpointStore{db: db, dbPath: f.store.dbPath, workspaceHash: f.store.workspaceHash}, func(root, path string, _ func()) (fileState, error) {
			return readWorkspaceFileStable(root, path, func() { f.write(path, "new") })
		})
	})
	if got.outcome != "incomplete" || got.paths != 0 || len(got.diagnostics) != 1 || got.diagnostics[0].message != "Workspace state could not be observed consistently and safely." {
		t.Fatalf("changing file=%+v", got)
	}
}

func TestAuditWorkspaceCanceledDuringObservation(t *testing.T) {
	f := newAuditWorkspaceFixture(t)
	f.add(f.body("a.txt", "absent", auditHashABC), true)
	f.write("a.txt", "new")
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := withAuditStore(ctx, f.store.dbPath, func(db *sql.DB) auditResult {
		return scanAuditWorkspace(ctx, f.root, &checkpointStore{db: db, dbPath: f.store.dbPath, workspaceHash: f.store.workspaceHash}, func(root, path string, _ func()) (fileState, error) {
			return readWorkspaceFileStable(root, path, func() { cancel() })
		})
	})
	if got.outcome != "incomplete" {
		t.Fatalf("canceled scan=%+v", got)
	}
}

func TestAuditWorkspaceEmptyDoesNotRequireKey(t *testing.T) {
	f := newAuditWorkspaceFixture(t)
	if err := os.Remove(f.key); err != nil {
		t.Fatal(err)
	}
	assertAuditWorkspace(t, f.audit(), "valid", 0, 0, "")
}

func TestAuditWorkspaceTypeRace(t *testing.T) {
	for _, kind := range []string{"directory to regular", "regular to symlink", "ancestor symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := "a.txt"
			target := filepath.Join(root, path)
			if kind == "ancestor symlink" {
				path = "dir/a.txt"
				target = filepath.Join(root, "dir")
				if err := os.Symlink(t.TempDir(), target); err != nil {
					t.Fatal(err)
				}
			} else if kind == "directory to regular" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(target, []byte("abc"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			state, err := readWorkspaceFileStable(root, path, func() {
				calls++
				if calls != 1 || kind == "ancestor symlink" {
					return
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if kind == "directory to regular" {
					if err := os.WriteFile(target, []byte("abc"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), target); err != nil {
					t.Fatal(err)
				}
			})
			if kind == "ancestor symlink" {
				if err != nil || state.hash != "nonregular:dir" || calls != 2 {
					t.Fatalf("contained type=%+v, err=%v calls=%d", state, err, calls)
				}
			} else if err == nil || calls != 2 {
				t.Fatalf("type race=%+v, err=%v calls=%d", state, err, calls)
			}
		})
	}
}

func TestAuditWorkspaceUnconfirmedCreateClearsPreservedMode(t *testing.T) {
	f := newAuditWorkspaceFixture(t)
	b := f.body("a.txt", "absent", auditHashABC)
	mode := uint32(0600)
	b.AfterMode = &mode
	f.add(b, true)
	f.add(f.body("a.txt", "absent", auditHashABC), false)
	f.add(f.body("a.txt", auditHashABC, auditHashNew), true)
	f.write("a.txt", "new")
	if err := os.Chmod(filepath.Join(f.root, "a.txt"), 0644); err != nil {
		t.Fatal(err)
	}
	assertAuditWorkspace(t, f.audit(), "incomplete", 3, 1, "receipt-unconfirmed")
}

func TestAuditWorkspaceRejectsInvalidStoredFlags(t *testing.T) {
	for _, column := range []string{"existed", "applied", "restored", "after_mode"} {
		t.Run(column, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			b := f.body("a.txt", auditHashEmpty, auditHashABC)
			f.add(b, true)
			f.checkpoint(b, nil, false, nil)
			f.write("a.txt", "abc")
			checkpointSQL(t, f.store.db, `PRAGMA ignore_check_constraints=ON`)
			checkpointSQL(t, f.store.db, "UPDATE checkpoint_files SET "+column+"=999")
			got := f.audit()
			if got.outcome != "violation" || got.paths != 0 {
				t.Fatalf("invalid %s=%+v", column, got)
			}
		})
	}
}

func TestAuditWorkspaceInterruptedCheckpointSuppressesUnknownState(t *testing.T) {
	for _, state := range []string{"open", "undoing"} {
		t.Run(state, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			b := f.body("a.txt", auditHashEmpty, auditHashABC)
			f.add(b, true)
			f.checkpoint(b, nil, false, nil)
			checkpointSQL(t, f.store.db, `UPDATE checkpoints SET state=?`, state)
			f.write("a.txt", "new")
			assertAuditWorkspace(t, f.audit(), "incomplete", 1, 0, "checkpoint-unconfirmed")
		})
	}
}

func TestAuditWorkspaceUnsequencedUndoRemainsUncertain(t *testing.T) {
	for _, scenario := range []string{"restored content", "undoing content", "restored tracked mode", "undoing tracked mode", "ordered inverse intent"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			first := f.body("a.txt", auditHashEmpty, auditHashABC)
			modeCase := scenario == "restored tracked mode" || scenario == "undoing tracked mode"
			if modeCase {
				mode := uint32(0600)
				first.BeforeHash = "absent"
				first.AfterMode = &mode
			}
			f.add(first, true)
			var inverseID any
			if scenario == "ordered inverse intent" {
				inverse := storeInverse(first, rand.Text())
				f.add(inverse, false)
				inverseID = inverse.MutationID
			}
			f.add(f.body("a.txt", auditHashABC, auditHashNew), true)
			restored := scenario != "undoing content" && scenario != "undoing tracked mode"
			f.checkpoint(first, nil, restored, inverseID)
			if !restored {
				checkpointSQL(t, f.store.db, `UPDATE checkpoints SET state='undoing'`)
			}
			content := ""
			if modeCase || scenario == "ordered inverse intent" {
				content = "new"
			}
			f.write("a.txt", content)
			if modeCase {
				if err := os.Chmod(filepath.Join(f.root, "a.txt"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			checked, paths := int64(2), int64(0)
			if scenario == "ordered inverse intent" {
				checked, paths = 3, 1
			}
			assertAuditWorkspace(t, f.audit(), "incomplete", checked, paths, "checkpoint-unconfirmed")
		})
	}
}

func TestAuditWorkspaceReferencedOriginalVerifierMismatch(t *testing.T) {
	for _, scenario := range []string{"different key", "different algorithm", "internal identity disagreement"} {
		t.Run(scenario, func(t *testing.T) {
			f := newAuditWorkspaceFixture(t)
			trusted := f.signer
			var other signing.Signer
			var err error
			if scenario == "different algorithm" {
				other, err = signing.NewHMAC(bytes.Repeat([]byte{0x24}, 32))
			} else {
				other, _, err = signing.LoadOrCreateEd25519(filepath.Join(t.TempDir(), "signing", "other.pem"))
			}
			if err != nil {
				t.Fatal(err)
			}
			original := f.body("a.txt", auditHashEmpty, auditHashABC)
			original.AgentID = other.KeyID()
			inverse := storeInverse(original, rand.Text())
			inverse.AgentID = trusted.KeyID()
			f.add(inverse, true) // The retained database order need not put the original first.
			// Force original decoding through reference lookup, beyond the first page.
			for i := 0; i < 99; i++ {
				f.add(f.body("b.txt", "absent", auditHashABC), true)
			}
			f.signer = other
			f.add(original, true)
			if scenario == "internal identity disagreement" {
				receipt, err := agenttools.DecodeMutationReceipt(f.raw(original))
				if err != nil {
					t.Fatal(err)
				}
				receipt.Body.AgentID = trusted.KeyID()
				raw, err := signing.MarshalCanonical(receipt)
				if err != nil {
					t.Fatal(err)
				}
				checkpointSQL(t, f.store.db, `UPDATE mutation_receipts SET intent_json=? WHERE mutation_id=?`, raw, original.MutationID)
			}
			outcome, code := "incomplete", "workspace-key"
			if scenario == "internal identity disagreement" {
				outcome, code = "violation", "receipt-binding"
			}
			got := f.audit()
			assertAuditWorkspace(t, got, outcome, 0, 0, code)
			if !got.earlyStop {
				t.Fatalf("referenced verifier mismatch did not stop authentication: %+v", got)
			}
		})
	}
}
