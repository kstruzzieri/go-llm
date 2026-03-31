package feedback

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// Aggregate holds the materialized score for a single chunk key.
type Aggregate struct {
	WeightedScore  float64
	RetrievalCount int
}

// SignalStore defines the persistence operations for behavioral feedback.
type SignalStore interface {
	// InsertRetrieval records a retrieval event with its associated chunk keys.
	InsertRetrieval(ctx context.Context, id, query string, chunkKeys []string) error

	// InsertSignal persists a signal event. When createdAt is zero the
	// current time is used.
	InsertSignal(ctx context.Context, retrievalID, chunkKey string, kind SignalKind, strength float64, createdAt time.Time) error

	// SignalCount returns the total number of recorded signals.
	SignalCount(ctx context.Context) (int, error)

	// GetAggregate returns the aggregate for a single chunk key.
	GetAggregate(ctx context.Context, chunkKey string) (Aggregate, error)

	// GetAggregatesBatch returns aggregates for multiple chunk keys.
	GetAggregatesBatch(ctx context.Context, chunkKeys []string) (map[string]Aggregate, error)

	// RecomputeAggregates recalculates weighted_score for all chunks using
	// the provided decay lambda.
	RecomputeAggregates(ctx context.Context, lambda float64) error

	// IncrementRetrievalCount bumps retrieval_count for each chunk key.
	IncrementRetrievalCount(ctx context.Context, chunkKeys []string) error

	// PruneSignals deletes signal events older than the given time and
	// returns the number of deleted rows.
	PruneSignals(ctx context.Context, olderThan time.Time) (int, error)

	// PruneRetrievals deletes retrieval rows that have no remaining signal
	// events referencing them. Returns the number of deleted rows.
	PruneRetrievals(ctx context.Context) (int, error)
}

// SQLiteSignalStore implements SignalStore using a SQLite database.
type SQLiteSignalStore struct {
	db *sql.DB
}

// NewSignalStore creates a SQLiteSignalStore, running migrations on the
// provided database if needed.
func NewSignalStore(ctx context.Context, db *sql.DB) (*SQLiteSignalStore, error) {
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("feedback: init store: %w", err)
	}
	return &SQLiteSignalStore{db: db}, nil
}

// InsertRetrieval records a retrieval event.
func (s *SQLiteSignalStore) InsertRetrieval(ctx context.Context, id, query string, chunkKeys []string) error {
	keys := strings.Join(chunkKeys, "\n")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO feedback_retrievals (retrieval_id, query, chunk_keys, created_at) VALUES (?, ?, ?, ?)`,
		id, query, keys, time.Now().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("feedback: insert retrieval %q: %w", id, err)
	}
	return nil
}

// InsertSignal persists a signal event and updates the last_signal_at
// aggregate timestamp. When createdAt is zero the current time is used.
func (s *SQLiteSignalStore) InsertSignal(ctx context.Context, retrievalID, chunkKey string, kind SignalKind, strength float64, createdAt time.Time) error {
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	now := createdAt.UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: insert signal begin: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feedback_signals (retrieval_id, chunk_key, signal_kind, strength, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		retrievalID, chunkKey, string(kind), strength, now,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("feedback: insert signal: %w", err)
	}

	// Upsert aggregate row to track last_signal_at.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO feedback_aggregates (chunk_key, last_signal_at)
		 VALUES (?, ?)
		 ON CONFLICT(chunk_key) DO UPDATE SET last_signal_at = excluded.last_signal_at`,
		chunkKey, now,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("feedback: upsert aggregate last_signal_at: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback: insert signal commit: %w", err)
	}
	return nil
}

// SignalCount returns the total number of recorded signals.
func (s *SQLiteSignalStore) SignalCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback_signals`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("feedback: signal count: %w", err)
	}
	return count, nil
}

// GetAggregate returns the materialized aggregate for a single chunk key.
func (s *SQLiteSignalStore) GetAggregate(ctx context.Context, chunkKey string) (Aggregate, error) {
	var agg Aggregate
	err := s.db.QueryRowContext(ctx,
		`SELECT weighted_score, retrieval_count FROM feedback_aggregates WHERE chunk_key = ?`,
		chunkKey,
	).Scan(&agg.WeightedScore, &agg.RetrievalCount)
	if err == sql.ErrNoRows {
		return Aggregate{}, nil
	}
	if err != nil {
		return Aggregate{}, fmt.Errorf("feedback: get aggregate %q: %w", chunkKey, err)
	}
	return agg, nil
}

// GetAggregatesBatch returns aggregates for multiple chunk keys in a single
// query.
func (s *SQLiteSignalStore) GetAggregatesBatch(ctx context.Context, chunkKeys []string) (map[string]Aggregate, error) {
	if len(chunkKeys) == 0 {
		return map[string]Aggregate{}, nil
	}

	placeholders := make([]string, len(chunkKeys))
	args := make([]any, len(chunkKeys))
	for i, k := range chunkKeys {
		placeholders[i] = "?"
		args[i] = k
	}

	query := fmt.Sprintf(
		`SELECT chunk_key, weighted_score, retrieval_count FROM feedback_aggregates WHERE chunk_key IN (%s)`,
		strings.Join(placeholders, ","),
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("feedback: get aggregates batch: %w", err)
	}
	defer rows.Close()

	result := make(map[string]Aggregate, len(chunkKeys))
	for rows.Next() {
		var key string
		var agg Aggregate
		if err := rows.Scan(&key, &agg.WeightedScore, &agg.RetrievalCount); err != nil {
			return nil, fmt.Errorf("feedback: get aggregates batch scan: %w", err)
		}
		result[key] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feedback: get aggregates batch iterate: %w", err)
	}
	return result, nil
}

// RecomputeAggregates recalculates the weighted_score for every chunk key
// from raw signal events, applying exponential time-decay.
//
// Formula: SUM(strength * exp(-lambda * days_since_signal))
func (s *SQLiteSignalStore) RecomputeAggregates(ctx context.Context, lambda float64) error {
	nowMs := time.Now().UnixMilli()

	rows, err := s.db.QueryContext(ctx,
		`SELECT chunk_key, strength, created_at FROM feedback_signals ORDER BY chunk_key`)
	if err != nil {
		return fmt.Errorf("feedback: recompute query signals: %w", err)
	}
	defer rows.Close()

	scores := make(map[string]float64)
	for rows.Next() {
		var key string
		var strength float64
		var createdMs int64
		if err := rows.Scan(&key, &strength, &createdMs); err != nil {
			return fmt.Errorf("feedback: recompute scan: %w", err)
		}
		daysSince := float64(nowMs-createdMs) / (24 * 60 * 60 * 1000)
		if daysSince < 0 {
			daysSince = 0
		}
		scores[key] += strength * math.Exp(-lambda*daysSince)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("feedback: recompute iterate: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: recompute begin: %w", err)
	}

	// Zero out all weighted_scores so that chunks whose signals were fully
	// pruned don't retain stale non-zero scores.
	if _, err := tx.ExecContext(ctx,
		`UPDATE feedback_aggregates SET weighted_score = 0, recomputed_at = ?`,
		nowMs,
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("feedback: recompute zero scores: %w", err)
	}

	for key, score := range scores {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feedback_aggregates (chunk_key, weighted_score, recomputed_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT(chunk_key) DO UPDATE SET
				weighted_score = excluded.weighted_score,
				recomputed_at  = excluded.recomputed_at`,
			key, score, nowMs,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("feedback: recompute upsert %q: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback: recompute commit: %w", err)
	}
	return nil
}

// IncrementRetrievalCount bumps retrieval_count for each provided chunk key.
func (s *SQLiteSignalStore) IncrementRetrievalCount(ctx context.Context, chunkKeys []string) error {
	if len(chunkKeys) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("feedback: increment retrieval count begin: %w", err)
	}

	for _, key := range chunkKeys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO feedback_aggregates (chunk_key, retrieval_count)
			 VALUES (?, 1)
			 ON CONFLICT(chunk_key) DO UPDATE SET
				retrieval_count = feedback_aggregates.retrieval_count + 1`,
			key,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("feedback: increment retrieval count %q: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("feedback: increment retrieval count commit: %w", err)
	}
	return nil
}

// PruneSignals deletes signals older than olderThan and returns the number
// of deleted rows.
func (s *SQLiteSignalStore) PruneSignals(ctx context.Context, olderThan time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM feedback_signals WHERE created_at < ?`,
		olderThan.UnixMilli(),
	)
	if err != nil {
		return 0, fmt.Errorf("feedback: prune signals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("feedback: prune signals rows affected: %w", err)
	}
	return int(n), nil
}

// PruneRetrievals deletes retrieval rows with no remaining signal events.
func (s *SQLiteSignalStore) PruneRetrievals(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM feedback_retrievals
		 WHERE retrieval_id NOT IN (SELECT DISTINCT retrieval_id FROM feedback_signals)`,
	)
	if err != nil {
		return 0, fmt.Errorf("feedback: prune retrievals: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("feedback: prune retrievals rows affected: %w", err)
	}
	return int(n), nil
}
