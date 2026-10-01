package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

const weightLookupBatchSize = 900

// weightsBatch applies cold-start (warmup) and MinRetrievals gating over a
// store's aggregates. It is the single source of truth shared by Collector and
// WeightReader so the two cannot diverge. Keys below MinRetrievals, unknown
// keys, and every key during warmup return 0.
func weightsBatch(ctx context.Context, store SignalStore, cfg CollectorConfig, chunkKeys []string) (map[string]float64, error) {
	result := make(map[string]float64, len(chunkKeys))
	for _, k := range chunkKeys {
		result[k] = 0
	}

	totalSignals, err := store.SignalCount(ctx)
	if err != nil {
		return nil, err
	}
	if totalSignals < cfg.WarmupSignals {
		return result, nil
	}

	for start := 0; start < len(chunkKeys); start += weightLookupBatchSize {
		end := start + weightLookupBatchSize
		if end > len(chunkKeys) {
			end = len(chunkKeys)
		}
		aggs, err := store.GetAggregatesBatch(ctx, chunkKeys[start:end])
		if err != nil {
			return nil, err
		}
		for k, agg := range aggs {
			if agg.RetrievalCount >= cfg.MinRetrievals {
				result[k] = agg.WeightedScore
			}
		}
	}
	return result, nil
}

// WeightReader exposes read-only behavioral weights without the Collector's
// attribution windows or background sweep goroutine. Use it where ranking only
// consumes weights and must never open attribution windows or emit signals.
type WeightReader struct {
	store  SignalStore
	config CollectorConfig
}

// NewWeightReader builds a read-only reader over store. Negative config fields
// resolve to their defaults (matching Collector); zero values keep their
// meaning (no warmup / no minimum / no decay).
func NewWeightReader(store SignalStore, config CollectorConfig) *WeightReader {
	return &WeightReader{store: store, config: config.withDefaults()}
}

// WeightsBatch returns gated behavioral weights for chunkKeys. Unknown or
// below-threshold keys return 0. During warmup all keys return 0.
func (r *WeightReader) WeightsBatch(ctx context.Context, chunkKeys []string) (map[string]float64, error) {
	return weightsBatch(ctx, r.store, r.config, chunkKeys)
}

// SQLiteWeightReader owns a read-only connection to an existing feedback
// database and serves behavioral weights without running migrations.
type SQLiteWeightReader struct {
	mu     sync.Mutex
	db     *sql.DB
	reader *WeightReader
}

// NewSQLiteWeightReader opens an existing migrated feedback database read-only
// and validates its schema on the handle that then serves reads, so the check
// sees committed WAL pages under SQLite's locks. A relative dbPath is resolved
// against the working directory. A path that is not a regular file, or a file
// smaller than one SQLite page, is rejected before any SQLite open. It never
// writes or deletes the main file or an existing WAL, and never changes schema
// or data, with one exception normal operation never triggers: if the main
// file shrinks below one page between the size check and the open, SQLite may
// delete the WAL beside it. Like any read-only SQLite connection, it may
// create the database's coordination sidecars, a -shm file and an empty -wal
// when a WAL-mode database has none, including beside a database it then
// rejects. A WAL-mode database whose sidecars can be neither opened nor
// created fails to open.
func NewSQLiteWeightReader(ctx context.Context, dbPath string, config CollectorConfig) (*SQLiteWeightReader, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("feedback: open SQLite weight reader: empty path")
	}
	// A relative path would render as file://name, which SQLite rejects as a
	// URI authority.
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("feedback: open SQLite weight reader: resolve path %q: %w", dbPath, err)
	}
	want := migrations[len(migrations)-1].version
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("feedback: open SQLite weight reader: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("feedback: open SQLite weight reader %q: not a regular file", abs)
	}
	// SQLite's minimum page size is 512 bytes, so a shorter file holds no
	// schema. Reject it before any SQLite open: SQLite deletes the WAL beside a
	// main file it sizes at zero pages, and modernc's unix VFS reports a 1-byte
	// file as 0 bytes (SQLite ticket #3260). The stat-to-open window is
	// accepted: a main file never shrinks below one page in normal operation.
	if info.Size() < 512 {
		return nil, fmt.Errorf("feedback: validate SQLite weight reader schema: database file is %d bytes, smaller than one SQLite page; want version %d", info.Size(), want)
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("feedback: open SQLite weight reader: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("feedback: open SQLite weight reader %q: %w", abs, err)
	}
	version, err := currentSchemaVersion(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("feedback: validate SQLite weight reader schema: %w", err)
	}
	if version != want {
		_ = db.Close()
		return nil, fmt.Errorf("feedback: validate SQLite weight reader schema: version %d, want %d", version, want)
	}
	store := &SQLiteSignalStore{db: db}
	return &SQLiteWeightReader{db: db, reader: NewWeightReader(store, config)}, nil
}

// WeightsBatch returns gated behavioral weights from the live database.
func (r *SQLiteWeightReader) WeightsBatch(ctx context.Context, chunkKeys []string) (map[string]float64, error) {
	if r == nil {
		return nil, fmt.Errorf("feedback: SQLite weight reader is closed")
	}
	r.mu.Lock()
	if r.db == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("feedback: SQLite weight reader is closed")
	}
	reader := r.reader
	r.mu.Unlock()
	return reader.WeightsBatch(ctx, chunkKeys)
}

// Close closes the owned database connection. Repeated calls are safe.
func (r *SQLiteWeightReader) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	db := r.db
	r.db = nil
	return db.Close()
}
