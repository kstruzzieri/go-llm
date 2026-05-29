// transcript/store.go
package transcript

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store persists MCP chat calls: an immutable raw_chat_calls row (source of
// truth) followed by a best-effort canonical conversations projection. Safe for
// concurrent use — a store-level mutex serializes Record so concurrent same-key
// calls cannot interleave their stitch decisions.
type Store struct {
	db    *sql.DB
	ownDB bool

	mu             sync.Mutex // serializes Record (preserves the stitch invariant)
	now            func() time.Time
	leaseWindow    time.Duration
	shortThreshold int

	closeOnce sync.Once
}

// Open opens (or creates) a transcript database at path and runs migrations.
// Pass ":memory:" explicitly for in-process tests (empty string is rejected to
// avoid the default-string footgun). The returned store owns the *sql.DB and
// closes it on Close.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("transcript: Open requires non-empty path; pass \":memory:\" explicitly")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("transcript: open sqlite %q: %w", path, err)
	}
	// modernc.org/sqlite applies PRAGMAs per-connection; clamp the pool to one
	// connection so the WAL + busy_timeout settings hold for every write, and so
	// :memory: does not open a fresh private DB per connection.
	db.SetMaxOpenConns(1)
	if path != ":memory:" {
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("transcript: %s: %w", pragma, err)
			}
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		db:             db,
		ownDB:          true,
		now:            time.Now,
		leaseWindow:    defaultLeaseWindow,
		shortThreshold: defaultShortThreshold,
	}, nil
}

// Close releases the underlying *sql.DB. Safe to call multiple times.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.ownDB {
			err = s.db.Close()
		}
	})
	return err
}

// Record is implemented in Task 9. Stub keeps the package compiling.
func (s *Store) Record(ctx context.Context, in RecordInput) error {
	_ = in
	return nil
}
