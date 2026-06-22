package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
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
	store.Close()
}

func TestEnableRetrieve_NoRagSuppressesNotice(t *testing.T) {
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{noRag: true})
	if got.tool != nil {
		t.Error("no-rag should yield no tool")
	}
	if !got.suppressNotice {
		t.Error("no-rag should suppress the generic no-index notice")
	}
}

func TestEnableRetrieve_AutoRegistersOnMatch(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, autoSidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k",
	})
	if got.tool == nil {
		t.Fatalf("auto index with matching vsid should register; warns=%v", got.warns)
	}
	if !strings.Contains(got.line, "auto index") {
		t.Errorf("startup line = %q, want auto-index disclosure", got.line)
	}
}

func TestEnableRetrieve_AutoDisablesOnMismatch(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/OLD")

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, autoSidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k",
	})
	if got.tool != nil {
		t.Error("mismatched auto index must not register retrieve")
	}
	if len(got.warns) == 0 || !strings.Contains(got.warns[0], "golem index -full") {
		t.Errorf("auto mismatch warning should suggest -full: %v", got.warns)
	}
}

func TestEnableRetrieve_AutoRequiresSidecar(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	// Build the DB but DELETE the sidecar (copied/foreign DB).
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath: dbPath, autoSidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k",
	})
	if got.tool != nil {
		t.Error("auto-discovery without a valid sidecar must not register")
	}
	if got.suppressNotice {
		t.Error("missing index should NOT suppress the generic notice")
	}
}

func TestEnableRetrieve_ExplicitMismatchHintHasNoFull(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "explicit.db")
	seedIndex(t, dbPath, "workspace:ignored", "ollama/OLD")

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		ragDB: dbPath,
	})
	if got.tool != nil {
		t.Error("explicit -rag-db with mismatched vsid must not register")
	}
	if len(got.warns) == 0 {
		t.Fatal("explicit mismatch should warn")
	}
	if strings.Contains(got.warns[0], "golem index -full") {
		t.Errorf("explicit -rag-db mismatch must NOT suggest golem index -full: %q", got.warns[0])
	}
}

var _ rag.StoreStats // confirm rag.StoreStats is referenced (type existence check)
