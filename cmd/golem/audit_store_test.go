package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func auditStoreFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("PRAGMA page_size=4096; CREATE TABLE evidence (value TEXT); INSERT INTO evidence VALUES ('fixture')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Missing preflight guards would let these paths reach the scanner.
func TestAuditStoreUnavailable(t *testing.T) {
	for _, tc := range []struct{ name, suffix, kind, outcome, code, message string }{
		{"missing", "", "missing", "not-present", "store-not-present", "Store is not present."},
		{"directory", "", "directory", "incomplete", "store-unavailable", "Store is unavailable or unsafe."},
		{"symlink", "", "symlink", "incomplete", "store-unavailable", "Store is unavailable or unsafe."},
		{"wal-symlink", "-wal", "symlink", "incomplete", "store-unavailable", "Store is unavailable or unsafe."},
		{"shm-directory", "-shm", "directory", "incomplete", "store-unavailable", "Store is unavailable or unsafe."},
		{"journal-symlink", "-journal", "symlink", "incomplete", "store-unavailable", "Store is unavailable or unsafe."},
		{"wal", "-wal", "nonempty", "incomplete", "store-active-journal", "Close the writer cleanly and retry; a nonempty journal is present."},
		{"journal", "-journal", "nonempty", "incomplete", "store-active-journal", "Close the writer cleanly and retry; a nonempty journal is present."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := auditStoreFixture(t)
			target := path + tc.suffix
			if tc.suffix == "" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch tc.kind {
			case "directory":
				err = os.Mkdir(target, 0700)
			case "symlink":
				err = os.Symlink("absent-target", target)
			case "nonempty":
				err = os.WriteFile(target, []byte("journal contents"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := auditStoreSnapshot(t, filepath.Dir(path))
			got := withAuditStore(context.Background(), path, func(*sql.DB) auditResult {
				t.Error("unavailable source reached scanner")
				return auditResult{outcome: "valid"}
			})
			want := auditResult{outcome: tc.outcome, diagnostics: []auditDiagnostic{{code: tc.code, target: path, message: tc.message}}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %+v; want %+v", got, want)
			}
			if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Fatal("unavailable source was changed")
			}
		})
	}
	t.Run("permission", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix permission semantics")
		}
		path := auditStoreFixture(t)
		before := auditStoreSnapshot(t, filepath.Dir(path))
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0600) })
		if f, err := os.Open(path); err == nil {
			_ = f.Close()
			t.Skip("process can bypass file permissions")
		}
		got := withAuditStore(context.Background(), path, func(*sql.DB) auditResult {
			t.Error("unreadable source reached scanner")
			return auditResult{outcome: "valid"}
		})
		if got.outcome != "incomplete" || len(got.diagnostics) != 1 || got.diagnostics[0].code != "store-unavailable" {
			t.Fatalf("result = %+v", got)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0 || !info.ModTime().Equal(before["audit.db"].mtime) {
			t.Fatal("unreadable source repaired")
		}
		if err := os.Chmod(path, before["audit.db"].mode); err != nil {
			t.Fatal(err)
		}
		if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
			t.Fatal("unreadable source changed")
		}
	})
}

// Each mutation isolates an observation; removing that comparison must fail.
func TestAuditStoreChangedDuringScan(t *testing.T) {
	for _, change := range []string{"identity", "size", "mtime", "mode", "wal-created", "journal-created", "shm-created", "shm-removed", "shm-replaced", "main-removed", "main-symlink"} {
		for _, outcome := range []string{"valid", "violation"} {
			t.Run(change+"/"+outcome, func(t *testing.T) {
				path := auditStoreFixture(t)
				if err := os.WriteFile(path+"-shm", []byte("shm"), 0600); err != nil {
					t.Fatal(err)
				}
				if change == "shm-created" {
					if err := os.Remove(path + "-shm"); err != nil {
						t.Fatal(err)
					}
				}
				var afterChange map[string]auditFileSnapshot
				got := withAuditStore(context.Background(), path, func(db *sql.DB) auditResult {
					var err error
					info, err := os.Stat(path)
					if err != nil {
						t.Fatal(err)
					}
					switch change {
					case "identity", "shm-replaced":
						target := path
						if change == "shm-replaced" {
							target += "-shm"
						}
						old, e := os.Stat(target)
						if e != nil {
							t.Fatal(e)
						}
						body, e := os.ReadFile(target)
						if e != nil {
							t.Fatal(e)
						}
						if e = os.WriteFile(target+".replacement", body, old.Mode()); e != nil {
							t.Fatal(e)
						}
						if e = os.Chtimes(target+".replacement", old.ModTime(), old.ModTime()); e != nil {
							t.Fatal(e)
						}
						err = os.Rename(target+".replacement", target)
					case "size":
						f, e := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
						if e != nil {
							t.Fatal(e)
						}
						if _, e = f.Write([]byte{0}); e != nil {
							t.Fatal(e)
						}
						if e = f.Close(); e != nil {
							t.Fatal(e)
						}
						err = os.Chtimes(path, info.ModTime(), info.ModTime())
					case "mtime":
						err = os.Chtimes(path, info.ModTime(), info.ModTime().Add(time.Hour))
					case "mode":
						if runtime.GOOS == "windows" {
							t.Skip("Unix permission semantics")
						}
						err = os.Chmod(path, info.Mode()^0040)
					case "wal-created":
						err = os.WriteFile(path+"-wal", nil, 0600)
					case "journal-created":
						err = os.WriteFile(path+"-journal", nil, 0600)
					case "shm-created":
						err = os.WriteFile(path+"-shm", nil, 0600)
					case "shm-removed":
						err = os.Remove(path + "-shm")
					case "main-removed":
						err = os.Remove(path)
					case "main-symlink":
						if err = os.Rename(path, path+".original"); err != nil {
							t.Fatal(err)
						}
						err = os.Symlink(path+".original", path)
					}
					if err != nil {
						t.Fatal(err)
					}
					afterChange = auditStoreSnapshot(t, filepath.Dir(path))
					return auditResult{outcome: outcome, checked: 7, diagnostics: []auditDiagnostic{{code: "transient", message: "Transient finding."}}}
				})
				want := auditResult{outcome: "incomplete", diagnostics: []auditDiagnostic{{code: "store-changed", target: path, message: "Store changed during verification; close the writer cleanly and retry."}}}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("result = %+v; want %+v", got, want)
				}
				if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(afterChange, after) {
					t.Fatal("audit changed the mutated source")
				}
			})
		}
	}
}

// Raw URI concatenation can redirect reads or override read-only flags.
func TestAuditStoreURI(t *testing.T) {
	if runtime.GOOS != "windows" {
		got, err := auditStoreURI("/tmp/a space%?#&mode=rw.db")
		if err != nil {
			t.Fatal(err)
		}
		if want := "file:///tmp/a%20space%25%3F%23&mode=rw.db?cache=private&immutable=1&mode=ro"; got != want {
			t.Fatalf("URI = %q; want %q", got, want)
		}
	}
	for _, path := range []string{"//remote/share/audit.db", `\\remote\share\audit.db`} {
		if got, err := auditStoreURI(path); err == nil {
			t.Fatalf("nonlocal URI accepted: %q", got)
		}
		got := withAuditStore(context.Background(), path, func(*sql.DB) auditResult {
			t.Error("nonlocal path reached scanner")
			return auditResult{outcome: "valid"}
		})
		if got.outcome != "incomplete" {
			t.Fatalf("nonlocal result = %+v", got)
		}
	}
	if runtime.GOOS == "windows" {
		return
	} // '?' and '#' path cases are Unix-only.
	path := auditStoreFixture(t)
	intended := filepath.Join(filepath.Dir(path), "a space%?#&mode=rw.db")
	if err := os.Rename(path, intended); err != nil {
		t.Fatal(err)
	}
	before := auditStoreSnapshot(t, filepath.Dir(path))
	got := withAuditStore(context.Background(), intended, func(db *sql.DB) auditResult {
		var value string
		if err := db.QueryRow("SELECT value FROM evidence").Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value != "fixture" {
			t.Fatalf("wrong file value = %q", value)
		}
		if _, err := db.Exec("DELETE FROM evidence"); err == nil {
			t.Fatal("URI permitted write")
		}
		return auditResult{outcome: "valid"}
	})
	if got.outcome != "valid" {
		t.Fatalf("result = %+v", got)
	}
	if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
		t.Fatal("URI opened or changed another path")
	}
}

// Both SQLite corruption errors and integrity-check rows must block scanning.
func TestAuditStoreCorrupt(t *testing.T) {
	for _, kind := range []string{"not-database", "damaged-page", "integrity-check"} {
		t.Run(kind, func(t *testing.T) {
			path := auditStoreFixture(t)
			switch kind {
			case "not-database":
				if err := os.WriteFile(path, []byte("not a sqlite database; sensitive contents"), 0600); err != nil {
					t.Fatal(err)
				}
			case "damaged-page":
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteAt([]byte{0xff}, 4096); err != nil {
					t.Fatal(err)
				}
				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
			case "integrity-check":
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("CREATE INDEX evidence_value ON evidence(value)"); err != nil {
					t.Fatal(err)
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
				// Page 3 is the index root. Clear its two-byte cell count, leaving
				// the table row present and the index entry missing.
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = f.WriteAt([]byte{0, 0}, 8195); err != nil {
					t.Fatal(err)
				}
				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before := auditStoreSnapshot(t, filepath.Dir(path))
			got := withAuditStore(context.Background(), path, func(*sql.DB) auditResult {
				t.Error("corrupt source reached scanner")
				return auditResult{outcome: "valid"}
			})
			want := auditResult{outcome: "violation", diagnostics: []auditDiagnostic{{code: "store-corrupt", target: path, message: "SQLite integrity verification failed."}}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("result = %+v; want %+v", got, want)
			}
			if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Fatal("corrupt source changed")
			}
		})
	}
}

func TestAuditStoreCancellation(t *testing.T) {
	for _, stage := range []string{"before", "during"} {
		t.Run(stage, func(t *testing.T) {
			path := auditStoreFixture(t)
			before := auditStoreSnapshot(t, filepath.Dir(path))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before" {
				cancel()
			}
			got := withAuditStore(ctx, path, func(*sql.DB) auditResult {
				if stage == "before" {
					t.Error("canceled source reached scanner")
				}
				cancel()
				return auditResult{outcome: "valid"}
			})
			if got.outcome != "incomplete" {
				t.Fatalf("canceled result = %+v", got)
			}
			if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Fatal("canceled scan changed source")
			}
		})
	}
}

type auditFileSnapshot struct {
	bytes string
	mode  os.FileMode
	mtime time.Time
}

func auditStoreSnapshot(t *testing.T, dir string) map[string]auditFileSnapshot {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]auditFileSnapshot)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		var body []byte
		if info.Mode().IsRegular() {
			body, err = os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
		}
		result[entry.Name()] = auditFileSnapshot{string(body), info.Mode(), info.ModTime()}
	}
	return result
}

// Opening a writable connection or touching sidecars must fail this check.
func TestAuditStoreReadOnly(t *testing.T) {
	for _, sidecars := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent-sidecars", true: "existing-sidecars"}[sidecars], func(t *testing.T) {
			path := auditStoreFixture(t)
			if sidecars {
				for _, suffix := range []string{"-wal", "-journal", "-shm"} {
					body := []byte(nil)
					if suffix == "-shm" {
						body = []byte("existing shared memory")
					}
					if err := os.WriteFile(path+suffix, body, 0640); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := auditStoreSnapshot(t, filepath.Dir(path))
			var handle *sql.DB
			got := withAuditStore(context.Background(), path, func(db *sql.DB) auditResult {
				handle = db
				var value string
				if err := db.QueryRow("SELECT value FROM evidence").Scan(&value); err != nil {
					t.Fatal(err)
				}
				if value != "fixture" {
					t.Fatalf("value = %q", value)
				}
				if _, err := db.Exec("INSERT INTO evidence VALUES ('forbidden')"); err == nil {
					t.Fatal("write succeeded")
				}
				if db.Stats().MaxOpenConnections != 1 {
					t.Fatal("connection pool is not restricted")
				}
				return auditResult{outcome: "valid", checked: 1}
			})
			if got.outcome != "valid" || got.checked != 1 {
				t.Fatalf("result = %+v", got)
			}
			if handle == nil {
				t.Fatal("scan was not called")
			}
			if err := handle.Ping(); err == nil {
				t.Fatal("connection remained open")
			}
			if after := auditStoreSnapshot(t, filepath.Dir(path)); !reflect.DeepEqual(before, after) {
				t.Fatal("audit altered source names, bytes, modes, or modification times")
			}
		})
	}
}
