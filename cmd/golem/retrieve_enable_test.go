package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// seedIndex builds a per-workspace DB (vsid) + a valid sidecar at the auto paths.
func seedIndex(t *testing.T, dbPath, workspaceID, vsid string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	store, idx := buildTestIndexer(t, dbPath, vsid)
	var out strings.Builder
	executeIndex(context.Background(), indexJob{
		indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: sidecarPath(dbPath), workspaceID: workspaceID,
		requestedModel: vsid, out: &out,
	})
	_ = store.Close()
}

func TestEnableRetrieve_NoRagSuppressesNotice(t *testing.T) {
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{noRag: true})
	if got.reader != nil {
		t.Error("no-rag should register no retrieval generation")
	}
	if !got.suppressNotice {
		t.Error("no-rag should suppress the generic no-index notice")
	}
}

func TestEnableRetrieve_AutoRegistersOnMatch(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	removeSQLiteSidecars(t, dbPath)

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, workspaceID: "workspace:k",
	})
	if got.reader == nil {
		t.Fatalf("auto index with matching vsid should register; warns=%v", got.warns)
	}
	if !strings.Contains(got.line, "auto index") {
		t.Errorf("startup line = %q, want auto-index disclosure", got.line)
	}
	assertNoSQLiteSidecars(t, dbPath)
}

func TestEnableRetrieve_AutoDisablesOnMismatch(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/OLD")

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, workspaceID: "workspace:k",
	})
	if got.reader != nil {
		t.Error("mismatched auto index must not register retrieve")
	}
	if len(got.warns) == 0 || !strings.Contains(got.warns[0], "golem index -full") {
		t.Errorf("auto mismatch warning should suggest -full: %v", got.warns)
	}
	if !got.suppressNotice {
		t.Error("a disabled-but-present auto index must suppress the contradictory generic no-index notice")
	}
}

func TestEnableRetrieve_AutoRejectsIncompleteLegacyIndex(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	// Build the DB but DELETE the sidecar (copied/foreign DB).
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, workspaceID: "workspace:k",
	})
	if got.reader != nil {
		t.Error("auto-discovery without a valid sidecar must not register")
	}
	if len(got.warns) != 1 || !strings.Contains(got.warns[0], "incomplete legacy index") {
		t.Errorf("warning = %v, want incomplete legacy index", got.warns)
	}
	if !got.suppressNotice {
		t.Error("specific invalid-index warning must suppress the contradictory generic notice")
	}
}

func TestEnableRetrieve_ExplicitMismatchHintHasNoFull(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "explicit.db")
	seedIndex(t, dbPath, "workspace:ignored", "ollama/OLD")

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		ragDB: dbPath,
	})
	if got.reader != nil {
		t.Error("explicit -rag-db with mismatched vsid must not register")
	}
	if len(got.warns) == 0 {
		t.Fatal("explicit mismatch should warn")
	}
	if strings.Contains(got.warns[0], "golem index -full") {
		t.Errorf("explicit -rag-db mismatch must NOT suggest golem index -full: %q", got.warns[0])
	}
}

func TestEnableRetrieve_ExplicitLegacyEmbeddingFormatRefusesWithoutMutation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "explicit.db")
	seedIndex(t, dbPath, "workspace:ignored", "ollama/nomic")
	rewriteEmbeddingsAsLegacyJSON(t, dbPath)
	want, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{ragDB: dbPath})
	if got.reader != nil || len(got.warns) != 1 {
		t.Fatalf("explicit legacy result = %+v", got)
	}
	if warning := got.warns[0]; !strings.Contains(warning, "legacy-json-f64") || !strings.Contains(warning, "will not") {
		t.Fatalf("warning = %q, want format and no-migration guidance", warning)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, want) {
		t.Fatal("explicit legacy -rag-db was mutated")
	}
	assertNoSQLiteSidecars(t, dbPath)
}

func TestEnableRetrieve_ManagedPointerAdmission(t *testing.T) {
	for _, mode := range []string{"repoint", "retire", "missing", "malformed", "directory", "workspace", "schema", "legacy", "legacy symlink", "explicit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "k.db")
			gen := publishTestGeneration(t, dbPath, "workspace:k", strings.Repeat("a", 32))
			opts := retrieveOpts{autoDBPath: dbPath, workspaceID: "workspace:k"}
			if strings.HasPrefix(mode, "legacy") {
				if err := os.Remove(activePointerPath(dbPath)); err != nil {
					t.Fatal(err)
				}
				seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
				removeSQLiteSidecars(t, dbPath)
			}
			if mode == "explicit" {
				opts.ragDB = gen.dbPath
			}
			got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, opts)
			if got.reader == nil {
				t.Fatal("reader not discovered")
			}
			delegate := &countingTool{content: "chunks"}
			got.reader.tool = delegate
			ready := newReadyRetrieve(warmingRetrieveMessage)
			ready.install(got.reader, got.line)
			t.Cleanup(func() { _ = ready.close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if result, err := ready.Invoke(ctx, nil); err != nil || result.Content != "chunks" {
				t.Fatal("valid reader not admitted")
			}
			switch mode {
			case "repoint":
				// No SQLite generation exists for this pointer: admission reads only JSON.
				err := publishActiveGeneration(ctx, dbPath, activeGenerationPointer{SchemaVersion: 1, WorkspaceID: "workspace:k", Generation: strings.Repeat("b", 32)}, nil)
				if err != nil {
					t.Fatal(err)
				}
			case "retire", "explicit":
				if err := retireActiveGeneration(ctx, dbPath, "workspace:k"); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(activePointerPath(dbPath)); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(activePointerPath(dbPath)); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(activePointerPath(dbPath), 0o700); err != nil {
					t.Fatal(err)
				}
			case "legacy symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), activePointerPath(dbPath)); err != nil {
					t.Fatal(err)
				}
			case "workspace", "schema":
				p := activeGenerationPointer{SchemaVersion: 1, WorkspaceID: "workspace:k", Generation: gen.id}
				if mode == "workspace" {
					p.WorkspaceID = "other"
				} else {
					p.SchemaVersion = 2
				}
				data, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(activePointerPath(dbPath), data, 0o600); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			default:
				if err := os.WriteFile(activePointerPath(dbPath), []byte("bad JSON"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := ready.Invoke(ctx, nil)
			if err != nil {
				t.Fatal("admission returned an error instead of an observation")
			}
			if mode == "explicit" || mode == "cancel" {
				if result.Content != "chunks" || !ready.hasReader() {
					t.Error("unaffected reader was detached")
				}
			} else {
				if delegate.calls.Load() != 1 || ready.hasReader() {
					t.Error("stale managed reader admitted a call")
				}
				assertNamesFileTools(t, result.Content)
			}
		})
	}
}

func TestRun_ManagedReaderAdmissionWithNoAutoIndex(t *testing.T) {
	for _, mode := range []string{"startup", "no-auto-index", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			configPath, root := writeGroundingRunConfig(t, false)
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, root)
			if err != nil {
				t.Fatal(err)
			}
			seedIndex(t, dbPath, workspaceID, "test/qwen3-embedding:8b")
			removeSQLiteSidecars(t, dbPath)
			args := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-project-context"}
			if mode == "no-auto-index" {
				args = append(args, "-no-auto-index")
			}
			if mode == "explicit" {
				args = append(args, "-rag-db", dbPath)
			}
			stdin, stdout, stderr := runTestFiles(t)
			stop := errors.New("startup admission checked")
			err = run(args, stdin, stdout, stderr, runHooks{
				startAutoIndex: func() func() { return func() {} },
				afterSessionReady: func(sess *replSession) error {
					for _, tool := range sess.tools {
						ready, ok := tool.(*readyRetrieve)
						if !ok {
							continue
						}
						delegate := &countingTool{content: "chunks"}
						ready.mu.Lock()
						if ready.reader == nil {
							ready.mu.Unlock()
							t.Error("startup reader missing")
							return stop
						}
						ready.reader.tool = delegate
						ready.mu.Unlock()
						if err := retireActiveGeneration(context.Background(), dbPath, workspaceID); err != nil {
							t.Error("retirement setup failed")
							return stop
						}
						result, invokeErr := ready.Invoke(context.Background(), nil)
						if invokeErr != nil {
							t.Error("admission did not return an observation")
						}
						if mode == "explicit" {
							if result.Content != "chunks" {
								t.Error("explicit database was bound to managed pointer")
							}
						} else if delegate.calls.Load() != 0 || result.Content != unavailableRetrieveMessage {
							t.Error("managed startup reader ignored pointer retirement")
						}
						return stop
					}
					t.Error("startup did not register reader wrapper")
					return stop
				},
			})
			if !errors.Is(err, stop) {
				t.Error("run did not reach startup admission check")
			}
		})
	}
}
