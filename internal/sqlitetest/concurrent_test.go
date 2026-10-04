package sqlitetest_test

import (
	"context"
	"database/sql"
	"io"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

// openWAL opens path the way a converted store opener does.
func openWAL(ctx context.Context, path string) (io.Closer, error) {
	dsn, err := sqlitedsn.WithBusyTimeout(path, 5*time.Second)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := sqlitedsn.EnableWAL(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func TestRunConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 3, openWAL)
}
