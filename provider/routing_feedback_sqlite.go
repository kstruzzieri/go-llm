// routing_feedback_sqlite.go provides the SQLite-backed RoutingFeedbackStore.
// Identical semantics to MemoryStore (validation, retention, atomic batch)
// but persistent and shareable across processes (e.g., the same workspace
// *sql.DB used by rag/, conversation/, fingerprint/).
//
// Constructor shape: caller-owned *sql.DB is the primary path
// (NewSQLiteFeedbackStore); OpenSQLiteFeedbackStore is a convenience for
// quick setup and tests that requires an explicit path argument
// (":memory:" is the only zero-arg-equivalent that must be spelled out).
package provider

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Compile-time interface check.
var _ RoutingFeedbackStore = (*SQLiteFeedbackStore)(nil)

// SQLiteFeedbackStoreConfig configures a SQLiteFeedbackStore. Per-key
// retention knobs mirror MemoryStoreConfig so an operator switching
// stores does not change observable Get/Record/RecordBatch behavior.
type SQLiteFeedbackStoreConfig struct {
	// NeutralScore is returned by Get() when ScoredCount == 0. Zero value
	// selects DefaultNeutralScore (0.5). Out-of-range fails construction.
	NeutralScore float64

	// MaxRetainedSamples bounds per-key retention. 0 -> default 1000;
	// -1 -> unbounded; >0 -> explicit cap with FIFO eviction on overflow.
	MaxRetainedSamples int

	// MaxMetaKeys / MaxMetaValueBytes bound Meta. 0 -> defaults.
	MaxMetaKeys       int
	MaxMetaValueBytes int

	// ownDB is set internally by OpenSQLiteFeedbackStore; users of the
	// caller-owned constructor leave it false. Controls whether Close
	// closes the underlying *sql.DB.
	ownDB bool
}

// SQLiteFeedbackStore is the persistent RoutingFeedbackStore. Safe for
// concurrent use; SQLite serializes writes via the connection pool, and
// reads use a single SELECT per call.
type SQLiteFeedbackStore struct {
	db    *sql.DB
	cfg   resolvedConfig
	ownDB bool

	// closeOnce guards the conditional Close-the-DB path so that calling
	// Close twice on a store opened via OpenSQLiteFeedbackStore does not
	// double-close.
	closeOnce sync.Once
}

// NewSQLiteFeedbackStore wraps a caller-owned *sql.DB. Runs the schema
// migrations; the caller retains responsibility for closing the DB.
//
// PRAGMA expectations for the caller-owned path: the routing-feedback
// workload assumes WAL journal mode (better concurrent-read latency)
// and a positive busy_timeout (bounded wait on contended write locks).
// Because PRAGMAs in modernc.org/sqlite apply per-connection, callers
// who want both to take effect across the whole pool must either
//   - clamp `db.SetMaxOpenConns(1)` (simplest; serializes writes), or
//   - apply the PRAGMAs via a `sql.Connector` / DSN `_pragma=` form
//     before passing the *sql.DB in.
//
// OpenSQLiteFeedbackStore does the former internally. Caller-owned
// users sharing a workspace DB with rag/ inherit whatever that
// package configured.
func NewSQLiteFeedbackStore(ctx context.Context, db *sql.DB, cfg SQLiteFeedbackStoreConfig) (*SQLiteFeedbackStore, error) {
	if db == nil {
		return nil, errors.New("provider: SQLiteFeedbackStore requires non-nil *sql.DB")
	}
	resolved, err := resolveSQLiteConfig(cfg)
	if err != nil {
		return nil, err
	}
	if err := runFeedbackMigrations(db); err != nil {
		return nil, err
	}
	return &SQLiteFeedbackStore{db: db, cfg: resolved, ownDB: cfg.ownDB}, nil
}

// OpenSQLiteFeedbackStore opens a SQLite database at path (use ":memory:"
// for in-process tests; explicit empty string is rejected to avoid the
// default-string footgun) and returns a store that owns the *sql.DB.
// Close on this store closes the DB.
func OpenSQLiteFeedbackStore(ctx context.Context, path string, cfg SQLiteFeedbackStoreConfig) (*SQLiteFeedbackStore, error) {
	if path == "" {
		return nil, errors.New("provider: OpenSQLiteFeedbackStore requires non-empty path; pass \":memory:\" explicitly")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("provider: open sqlite %q: %w", path, err)
	}
	// Clamp the connection pool to one connection regardless of path.
	// modernc.org/sqlite applies PRAGMAs per-connection; without this,
	// the WAL + busy_timeout PRAGMAs below would only apply to the
	// connection that received them, and other connections in the pool
	// would silently revert to journal_mode=DELETE / busy_timeout=0 —
	// exactly the contention behavior the PRAGMAs exist to prevent.
	// Routing-feedback writes are low-rate; serializing through one
	// connection is acceptable until benchmarks show otherwise.
	// (:memory: also requires MaxOpenConns(1) because each connection
	// opens its own private database otherwise — same outcome via a
	// different mechanism.)
	db.SetMaxOpenConns(1)

	if path != ":memory:" {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("provider: set WAL mode: %w", err)
		}
		// busy_timeout = 5000ms: bounded wait on a contended write lock
		// before giving up with SQLITE_BUSY. Avoids tight retry loops in
		// the calling code while still respecting the feedbackWriteTimeout
		// the caller passes on the outer ctx.
		if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("provider: set busy_timeout: %w", err)
		}
	}
	cfg.ownDB = true
	store, err := NewSQLiteFeedbackStore(ctx, db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the underlying *sql.DB if and only if the store opened
// it (via OpenSQLiteFeedbackStore). Stores constructed against a
// caller-owned *sql.DB leave Close as a no-op so the caller can manage
// its own lifecycle.
func (s *SQLiteFeedbackStore) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.ownDB {
			err = s.db.Close()
		}
	})
	return err
}

func resolveSQLiteConfig(cfg SQLiteFeedbackStoreConfig) (resolvedConfig, error) {
	r := resolvedConfig{
		neutralScore:       cfg.NeutralScore,
		maxRetainedSamples: cfg.MaxRetainedSamples,
		maxMetaKeys:        cfg.MaxMetaKeys,
		maxMetaValueBytes:  cfg.MaxMetaValueBytes,
	}
	if r.neutralScore == 0 {
		r.neutralScore = DefaultNeutralScore
	}
	if r.maxRetainedSamples == 0 {
		r.maxRetainedSamples = DefaultMaxRetainedSamples
	}
	if r.maxMetaKeys == 0 {
		r.maxMetaKeys = DefaultMaxMetaKeys
	}
	if r.maxMetaValueBytes == 0 {
		r.maxMetaValueBytes = DefaultMaxMetaValueBytes
	}
	if err := validateNeutralScore(r.neutralScore); err != nil {
		return r, err
	}
	if r.maxRetainedSamples < -1 {
		return r, fmt.Errorf("provider: MaxRetainedSamples %d out of range; use -1 for unbounded", r.maxRetainedSamples)
	}
	if r.maxMetaKeys < 0 {
		return r, fmt.Errorf("provider: MaxMetaKeys %d out of range", r.maxMetaKeys)
	}
	if r.maxMetaValueBytes < 0 {
		return r, fmt.Errorf("provider: MaxMetaValueBytes %d out of range", r.maxMetaValueBytes)
	}
	return r, nil
}

// Get aggregates signals for key via a single SELECT. Returns a neutral
// aggregate when no rows exist.
func (s *SQLiteFeedbackStore) Get(ctx context.Context, key FeedbackKey) (agg Aggregate, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, strength, at_ns FROM routing_feedback_signals
		   WHERE provider = ? AND model = ? AND use_case = ?
		   ORDER BY at_ns ASC, id ASC`,
		key.Provider, key.Model, key.UseCase,
	)
	if err != nil {
		return Aggregate{}, fmt.Errorf("provider: SQLiteFeedbackStore.Get: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("provider: SQLiteFeedbackStore.Get close rows: %w", closeErr)
		}
	}()

	var (
		sum         float64
		scored      int
		sampleCount int
		latestAtNs  int64
	)
	for rows.Next() {
		var (
			kind     string
			strength sql.NullFloat64
			atNs     int64
		)
		if err := rows.Scan(&kind, &strength, &atNs); err != nil {
			return Aggregate{}, fmt.Errorf("provider: SQLiteFeedbackStore.Get scan: %w", err)
		}
		sampleCount++
		if atNs > latestAtNs {
			latestAtNs = atNs
		}
		var eff float64
		if strength.Valid {
			eff = strength.Float64
		} else {
			eff = defaultStrength[RoutingSignalKind(kind)]
		}
		if eff == 0 {
			continue
		}
		if eff < -1 {
			eff = -1
		} else if eff > 1 {
			eff = 1
		}
		sum += eff
		scored++
	}
	if err := rows.Err(); err != nil {
		return Aggregate{}, fmt.Errorf("provider: SQLiteFeedbackStore.Get rows: %w", err)
	}
	if sampleCount == 0 {
		return Aggregate{Score: s.cfg.neutralScore}, nil
	}
	updatedAt := time.Unix(0, latestAtNs)
	if scored == 0 {
		return Aggregate{Score: s.cfg.neutralScore, SampleCount: sampleCount, UpdatedAt: updatedAt}, nil
	}
	return Aggregate{
		Score:       0.5 + 0.5*(sum/float64(scored)),
		SampleCount: sampleCount,
		ScoredCount: scored,
		UpdatedAt:   updatedAt,
	}, nil
}

// Record persists a single signal. Validates the key/signal, defaults At
// to time.Now() when zero, and applies retention inside a transaction so
// reads cannot observe an over-cap state.
func (s *SQLiteFeedbackStore) Record(ctx context.Context, key FeedbackKey, sig FeedbackSignal) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateSignal(sig, s.cfg.maxMetaKeys, s.cfg.maxMetaValueBytes); err != nil {
		return err
	}
	stored := cloneSignal(sig)
	if stored.At.IsZero() {
		stored.At = time.Now()
	}
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		if err := insertSignalTx(ctx, tx, key, stored); err != nil {
			return err
		}
		return applyRetentionTx(ctx, tx, key, s.cfg.maxRetainedSamples)
	})
}

// RecordBatch persists multiple signals as one atomic transition.
// Per-key FIFO retention is applied once per touched key after all
// inserts complete, still inside the same transaction.
func (s *SQLiteFeedbackStore) RecordBatch(ctx context.Context, items []FeedbackItem) error {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]FeedbackItem, len(items))
	for i, item := range items {
		if err := validateKey(item.Key); err != nil {
			return err
		}
		if err := validateSignal(item.Signal, s.cfg.maxMetaKeys, s.cfg.maxMetaValueBytes); err != nil {
			return err
		}
		cloned[i] = FeedbackItem{Key: item.Key, Signal: cloneSignal(item.Signal)}
	}
	now := time.Now()
	for i := range cloned {
		if cloned[i].Signal.At.IsZero() {
			cloned[i].Signal.At = now
		}
	}
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		touched := make(map[FeedbackKey]struct{}, len(cloned))
		for i := range cloned {
			if err := insertSignalTx(ctx, tx, cloned[i].Key, cloned[i].Signal); err != nil {
				return err
			}
			touched[cloned[i].Key] = struct{}{}
		}
		for k := range touched {
			if err := applyRetentionTx(ctx, tx, k, s.cfg.maxRetainedSamples); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *SQLiteFeedbackStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("provider: SQLiteFeedbackStore begin: %w", err)
	}
	// Defensive rollback covers the panic path (return early on success
	// commit). database/sql treats Rollback after Commit as a no-op, so
	// this is safe.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("provider: SQLiteFeedbackStore commit: %w", err)
	}
	return nil
}

// insertSignalTx writes one signal row. Meta is marshalled to JSON; the
// caller has already enforced the key-count / value-byte limits via
// validateSignal so json.Marshal cannot exceed the column expectations.
func insertSignalTx(ctx context.Context, tx *sql.Tx, key FeedbackKey, sig FeedbackSignal) error {
	var strength sql.NullFloat64
	if sig.Strength != nil {
		strength = sql.NullFloat64{Float64: *sig.Strength, Valid: true}
	}
	meta := []byte("{}")
	if len(sig.Meta) > 0 {
		m, err := json.Marshal(sig.Meta)
		if err != nil {
			return fmt.Errorf("provider: marshal meta: %w", err)
		}
		meta = m
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO routing_feedback_signals
		   (provider, model, use_case, kind, strength, at_ns,
		    latency_ms, error_class, route_id, completion_id, meta)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Provider, key.Model, key.UseCase,
		string(sig.Kind), strength, sig.At.UnixNano(),
		sig.LatencyMs, sig.ErrorClass, sig.RouteID, sig.CompletionID, string(meta),
	)
	if err != nil {
		return fmt.Errorf("provider: insert signal: %w", err)
	}
	return nil
}

// applyRetentionTx deletes all but the newest maxRetained inserted rows for
// key, ordered by id DESC to match MemoryStore's FIFO retention even when a
// caller records backdated or delayed signals. No-op when maxRetained <= 0
// (the "unbounded" semantics from MemoryStoreConfig).
func applyRetentionTx(ctx context.Context, tx *sql.Tx, key FeedbackKey, maxRetained int) error {
	if maxRetained <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM routing_feedback_signals
		   WHERE provider = ? AND model = ? AND use_case = ?
		     AND id NOT IN (
		       SELECT id FROM routing_feedback_signals
		         WHERE provider = ? AND model = ? AND use_case = ?
		         ORDER BY id DESC
		         LIMIT ?
		     )`,
		key.Provider, key.Model, key.UseCase,
		key.Provider, key.Model, key.UseCase, maxRetained,
	)
	if err != nil {
		return fmt.Errorf("provider: retention DELETE: %w", err)
	}
	return nil
}
