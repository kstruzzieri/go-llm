package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestFeedbackDBPathForWorkspace(t *testing.T) {
	base := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	root := t.TempDir()
	p, err := feedbackDBPathForWorkspace(getenv, root)
	if err != nil {
		t.Fatalf("feedbackDBPathForWorkspace: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(p), "golem/retrieval-feedback/") || !strings.HasSuffix(p, ".db") {
		t.Errorf("unexpected path %q", p)
	}
}

func TestFeedbackDBPathForWorkspaceRejectsInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return root
		}
		return ""
	}
	if _, err := feedbackDBPathForWorkspace(getenv, root); err == nil {
		t.Fatalf("expected path inside workspace to be rejected")
	}
}

func TestOpenBehavioralWeighterValid(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "fb.db") // parent dir does not exist yet
	h, warn := openBehavioralWeighter(context.Background(), dbPath)
	if h == nil || h.weighter == nil || h.db == nil {
		t.Fatalf("want non-nil handle, warn=%q", warn)
	}
	defer func() { _ = h.db.Close() }()
	if warn != "" {
		t.Errorf("unexpected warn: %q", warn)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Errorf("feedback DB not created: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("feedback DB mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Errorf("feedback DB dir not created: %v", err)
	} else if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("feedback DB dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestOpenBehavioralWeighterFailsOpen(t *testing.T) {
	dir := t.TempDir() // a directory path is not a valid SQLite file target
	h, warn := openBehavioralWeighter(context.Background(), dir)
	if h != nil {
		t.Errorf("want nil handle for bad path, got non-nil")
	}
	if warn == "" {
		t.Errorf("want a warning for bad path")
	}
}

func TestEnableRetrieveFeedbackValid(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	removeSQLiteSidecars(t, dbPath)

	feedbackDB := filepath.Join(dataDir, "feedback", "fb.db")
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath:  dbPath,
		workspaceID: "workspace:k",
		feedbackDB:  feedbackDB,
	})
	if got.tool == nil {
		t.Fatalf("retrieve should register; warns=%v", got.warns)
	}
	if got.reader == nil || got.reader.feedback == nil || got.reader.feedback.db == nil || got.reader.feedback.weighter == nil {
		t.Fatalf("feedback handle not retained by reader: %#v", got.reader)
	}
	defer got.reader.closeAfterDrain()
}

func TestEnableRetrieveFeedbackFailsOpen(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	removeSQLiteSidecars(t, dbPath)

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath:  dbPath,
		workspaceID: "workspace:k",
		feedbackDB:  t.TempDir(), // directory path: invalid SQLite file target
	})
	if got.tool == nil {
		t.Fatalf("retrieve should remain registered when feedback fails open; warns=%v", got.warns)
	}
	if got.reader != nil && got.reader.feedback != nil {
		t.Fatalf("bad feedback DB should not return a handle")
	}
	joined := strings.Join(got.warns, "\n")
	if !strings.Contains(joined, "behavioral feedback disabled") {
		t.Fatalf("missing feedback warning in %v", got.warns)
	}
}
