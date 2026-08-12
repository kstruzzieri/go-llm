package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
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

// NewSQLiteWeightReader opens an existing migrated feedback database read-only.
func NewSQLiteWeightReader(ctx context.Context, dbPath string, config CollectorConfig) (*SQLiteWeightReader, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("feedback: open SQLite weight reader: empty path")
	}
	u := url.URL{Scheme: "file", Path: dbPath}
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
		return nil, fmt.Errorf("feedback: open SQLite weight reader %q: %w", dbPath, err)
	}
	version, err := currentSchemaVersion(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("feedback: validate SQLite weight reader schema: %w", err)
	}
	if want := migrations[len(migrations)-1].version; version != want {
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
