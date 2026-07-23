package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/feedback"
	"github.com/kstruzzieri/go-llm/rag"
)

type behavioralWeighterHandle struct {
	weighter rag.BehavioralWeighter
	db       *sql.DB
}

// feedbackDBPathForWorkspace resolves the per-workspace writable behavioral
// feedback DB path (<base>/golem/retrieval-feedback/<key>.db), mirroring
// indexDBPathForWorkspace. It is validated to live outside the workspace so it
// is never indexed or edited.
func feedbackDBPathForWorkspace(getenv func(string) string, root string) (string, error) {
	base, err := dataDirBase(getenv)
	if err != nil {
		return "", err
	}
	key := strings.TrimPrefix(workspaceID(root), "workspace:")
	dbPath := filepath.Join(base, "golem", "retrieval-feedback", key+".db")
	if err := validatePathOutsideWorkspace(dbPath, root); err != nil {
		return "", err
	}
	return dbPath, nil
}

// openBehavioralWeighter best-effort opens the feedback DB and returns a
// consume-only weighter handle. It NEVER returns an error: on any failure it
// returns a nil handle and a human-readable warning, so retrieval proceeds with
// neutral ranking. It may run feedback schema migrations, but it never records
// retrievals or outcomes. Callers own the returned DB handle and must close it
// during normal shutdown.
func openBehavioralWeighter(ctx context.Context, dbPath string) (*behavioralWeighterHandle, string) {
	if err := prepareDBFile(dbPath); err != nil {
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Sprintf("behavioral feedback disabled: open %q: %v", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Sprintf("behavioral feedback disabled: %s: %v", pragma, err)
		}
	}
	store, err := feedback.NewSignalStore(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Sprintf("behavioral feedback disabled: init %q: %v", dbPath, err)
	}
	if err := chmodDBFiles(dbPath); err != nil {
		_ = db.Close()
		return nil, "behavioral feedback disabled: " + err.Error()
	}
	return &behavioralWeighterHandle{
		weighter: feedback.NewWeightReader(store, feedback.DefaultConfig()),
		db:       db,
	}, ""
}
