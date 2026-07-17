package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Compile-time interface satisfaction checks.
var (
	_ VectorStore       = (*SQLiteStore)(nil)
	_ Exportable        = (*SQLiteStore)(nil)
	_ sourceChunkLoader = (*SQLiteStore)(nil)
	_ sourceHashChecker = (*SQLiteStore)(nil)
)

// SQLiteStore is a VectorStore backed by SQLite with brute-force cosine similarity.
type SQLiteStore struct {
	db                *sql.DB
	behavioral        BehavioralWeighter // optional; nil => behavioral signal inert
	immutable         bool               // set by OpenSQLiteStoreReadOnly; enables the resident snapshot paths
	writeEmbeddingErr error              // non-nil when existing rows are not a homogeneous packed corpus

	// snapshotMu guards resident and snapshotLoad. resident is the published
	// process-resident snapshot (nil until the first successful load);
	// snapshotLoad is the in-flight single-flight load shared by concurrent
	// callers (nil when no load is running).
	snapshotMu   sync.Mutex
	resident     *sqliteSnapshot
	snapshotLoad *sqliteSnapshotLoad
	// recordStage is used by same-package benchmarks to attribute retrieval
	// time without adding a public tracing API. Nil in production. It is not
	// synchronized: it must be set before the store is used and invoked from a
	// single goroutine at a time (the decoupled snapshot load invokes it from
	// the load goroutine while callers are parked on the load). Parallelizing
	// retrieval stages requires revisiting this hook and the benchmark maps
	// it feeds.
	recordStage func(string, time.Duration)
}

// noopStageStop is the shared no-op returned by stageTimer when stage
// recording is disabled, so the disabled path never reads the clock or
// allocates.
var noopStageStop = func() {}

// stageTimer starts timing the named stage and returns a func that records
// the elapsed time via recordStage. Call the returned func at the point the
// stage completes; error returns may skip it, dropping that stage's timing,
// which is acceptable for the benchmark-only hook.
func (s *SQLiteStore) stageTimer(name string) func() {
	if s.recordStage == nil {
		return noopStageStop
	}
	start := time.Now()
	return func() { s.recordStage(name, time.Since(start)) }
}

type replaceSourceOptions struct {
	sourceHash               string
	vectorSpaceID            string
	requireVectorSpaceID     bool
	expectedSourceHash       string
	checkExpectedSourceHash  bool
	checkExistingVectorSpace bool
	replaceVectorSpace       bool
	allowMissingExisting     bool
	allowLegacyUnknown       bool
}

// NewSQLiteStore creates a vector store backed by SQLite.
// Use ":memory:" for dbPath to create an in-memory database (for testing).
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	dsn, err := sqliteReadWriteDSN(dbPath)
	if err != nil {
		return nil, fmt.Errorf("rag: open sqlite: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("rag: open sqlite: %w", err)
	}

	if dbPath == ":memory:" {
		// In-memory databases: constrain to exactly 1 connection.
		// With database/sql's connection pool, multiple connections to :memory:
		// each create a separate database, causing missing schema/data.
		db.SetMaxOpenConns(1)
	} else {
		// File-backed databases: enable WAL mode for better concurrent read performance.
		// WAL is not meaningful for :memory: databases. journal_mode persists in
		// the database file, so one Exec suffices; busy_timeout is per-connection
		// and therefore set via the DSN in sqliteReadWriteDSN.
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("rag: set WAL mode: %w", err)
		}
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &SQLiteStore{db: db}
	format, _, _, err := store.embeddingStorageSummary(context.Background())
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rag: inspect embedding storage: %w", err)
	}
	if format != EmbeddingFormatEmpty && format != EmbeddingFormatPackedFloat32 {
		store.writeEmbeddingErr = fmt.Errorf("rag: write embeddings: existing store uses %s; rebuild or explicitly migrate it before writing packed vectors", format)
	}
	return store, nil
}

// sqliteReadWriteDSN builds the read-write DSN for dbPath. File-backed
// databases get busy_timeout via a file: URI _pragma so it applies to every
// pooled connection: modernc.org/sqlite applies PRAGMAs per-connection, so a
// plain Exec would configure only whichever connection served it and
// concurrent writers on the other connections would fail immediately with
// SQLITE_BUSY instead of waiting. _txlock=immediate makes BeginTx issue
// BEGIN IMMEDIATE: with the default deferred BEGIN, the first write inside
// the transaction upgrades a read lock, and SQLite skips the busy handler on
// that upgrade path (deadlock avoidance), so contended writers would still
// fail instantly despite the timeout. The pool is deliberately NOT clamped
// to one connection (the routing-feedback store's alternative) because
// retrieval reads are in the user-facing latency path and rely on WAL read
// concurrency.
// The path is made absolute first: a relative path in url.URL.Path renders
// as file://<path>, which the URI parser reads as a host name, not a file.
// Windows drive-letter/UNC normalization is deferred with the Windows build
// work (issue #303); this mirrors OpenSQLiteStoreReadOnly's exposure.
func sqliteReadWriteDSN(dbPath string) (string, error) {
	if dbPath == ":memory:" {
		return dbPath, nil
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", dbPath, err)
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("_pragma", "busy_timeout(5000)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// OpenSQLiteStoreReadOnly opens an existing SQLite vector store as an immutable
// read-only snapshot without changing journal mode or running migrations. Use
// it for retrieval/probe paths that must not create WAL/SHM files or mutate
// copied/foreign index DBs.
func OpenSQLiteStoreReadOnly(dbPath string) (*SQLiteStore, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("rag: open sqlite read-only: empty path")
	}
	u := url.URL{Scheme: "file", Path: dbPath}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()

	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("rag: open sqlite read-only: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("rag: open sqlite read-only %q: %w", dbPath, err)
	}
	return &SQLiteStore{db: db, immutable: true}, nil
}

// SetBehavioralWeighter installs an optional behavioral weighter consumed by
// SearchMulti as a third RRF list. Startup-only: call once during wiring, before
// any concurrent retrieval. It takes no lock and is not safe to call against
// in-flight SearchMulti calls.
func (s *SQLiteStore) SetBehavioralWeighter(w BehavioralWeighter) {
	s.behavioral = w
}

// DB returns the underlying *sql.DB for packages that need shared access
// to the workspace database (e.g., conversation/ and feedback/ creating
// their own tables).
func (s *SQLiteStore) DB() *sql.DB {
	return s.db
}

// ProbeVectorSpaces summarizes the vector_space_id distribution across the
// chunks table. It returns the lex min and max non-empty vsid (deduped to
// at most two entries) plus a flag indicating any legacy empty-vsid rows.
//
// The bookend (ASC LIMIT 1 / DESC LIMIT 1) form is preferred over
// DISTINCT...LIMIT 2 because the partial index idx_chunks_vector_space_id_nonempty
// is ordered by vsid, so each bookend serves from a single index entry. A
// DISTINCT scan would generally walk the index to prove no second distinct
// value exists. Returned IDs are also deterministic across SQLite versions,
// which matters for tests that assert returned IDs.
func (s *SQLiteStore) ProbeVectorSpaces(ctx context.Context) (VectorSpaceProbe, error) {
	if s.immutable {
		snapshot, err := s.sqliteSnapshot(ctx)
		if err != nil {
			return VectorSpaceProbe{}, err
		}
		return cloneVectorSpaceProbe(snapshot.probe), nil
	}

	stopProbe := s.stageTimer("vector_space_probe_validation")
	var minID string
	err := s.db.QueryRowContext(ctx, `
		SELECT vector_space_id
		  FROM chunks
		 WHERE vector_space_id <> ''
		 ORDER BY vector_space_id ASC
		 LIMIT 1`).Scan(&minID)
	if err != nil && err != sql.ErrNoRows {
		return VectorSpaceProbe{}, fmt.Errorf("rag: probe min vector space: %w", err)
	}
	hasKnown := err != sql.ErrNoRows

	var maxID string
	if hasKnown {
		if err := s.db.QueryRowContext(ctx, `
			SELECT vector_space_id
			  FROM chunks
			 WHERE vector_space_id <> ''
			 ORDER BY vector_space_id DESC
			 LIMIT 1`).Scan(&maxID); err != nil {
			return VectorSpaceProbe{}, fmt.Errorf("rag: probe max vector space: %w", err)
		}
	}

	// EXISTS short-circuits on first match. Worst case is a fully-migrated
	// corpus (no empties) which scans the table; SQLite's sequential scan
	// is acceptable, and the alternative — an index over the empty
	// predicate — would defeat the partial-index design.
	var hasUnknown bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM chunks WHERE vector_space_id = '')
		`).Scan(&hasUnknown); err != nil {
		return VectorSpaceProbe{}, fmt.Errorf("rag: probe unknown vector spaces: %w", err)
	}

	probe := VectorSpaceProbe{HasUnknown: hasUnknown}
	if hasKnown {
		if minID == maxID {
			probe.KnownIDs = []string{minID}
		} else {
			// Min/max bookends are sufficient evidence of ≥2 distinct values.
			probe.KnownIDs = []string{minID, maxID}
		}
	}
	stopProbe()
	return probe, nil
}

func validateStoreInputs(chunks []Chunk, embeddings [][]float64) error {
	if len(chunks) != len(embeddings) {
		return fmt.Errorf("rag: store: chunks/embeddings length mismatch (%d vs %d)", len(chunks), len(embeddings))
	}
	if len(chunks) == 0 {
		return nil
	}
	// Validate all embeddings have the same dimension
	dim := len(embeddings[0])
	if dim == 0 {
		return fmt.Errorf("rag: store: embedding dimension is 0")
	}
	for i, emb := range embeddings {
		if len(emb) != dim {
			return fmt.Errorf("rag: store: embedding dimension mismatch at index %d (expected %d, got %d)", i, dim, len(emb))
		}
	}
	return nil
}

func (s *SQLiteStore) insertChunksTx(ctx context.Context, tx *sql.Tx, chunks []Chunk, embeddings [][]float64, sourceContentHash, vectorSpaceID string) error {
	if s.writeEmbeddingErr != nil {
		return s.writeEmbeddingErr
	}
	// Use ON CONFLICT ... DO UPDATE instead of INSERT OR REPLACE.
	// INSERT OR REPLACE deletes the old row and inserts a new one, but
	// SQLite does not fire DELETE triggers for rows removed by REPLACE
	// conflict resolution (unless recursive_triggers is enabled). This
	// leaves stale entries in the FTS5 index. ON CONFLICT DO UPDATE
	// modifies the row in place (preserving its rowid) and fires the
	// AFTER UPDATE trigger, which correctly maintains FTS5.
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET
				content = excluded.content,
				source = excluded.source,
				start_line = excluded.start_line,
				end_line = excluded.end_line,
				language = excluded.language,
				metadata = excluded.metadata,
				embedding = excluded.embedding,
				indexed_at = excluded.indexed_at,
				stable_key = excluded.stable_key,
				source_content_hash = excluded.source_content_hash,
				vector_space_id = excluded.vector_space_id`)
	if err != nil {
		return fmt.Errorf("rag: prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	now := time.Now().Unix()
	for i, chunk := range chunks {
		metaJSON, err := marshalChunkMetadata(chunk.Metadata, vectorSpaceID)
		if err != nil {
			return fmt.Errorf("rag: marshal metadata: %w", err)
		}
		embedding, err := encodeEmbedding(embeddings[i])
		if err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, chunk.ID, chunk.Content, chunk.Source,
			chunk.StartLine, chunk.EndLine, chunk.Language, string(metaJSON), embedding, now, chunk.StableKey, sourceContentHash, vectorSpaceID); err != nil {
			return fmt.Errorf("rag: insert chunk %q: %w", chunk.ID, err)
		}
	}
	return nil
}

// marshalChunkMetadata returns the JSON-encoded chunk metadata. The
// chunks.vector_space_id column is authoritative; when vectorSpaceID is
// non-empty, the value is also mirrored into metadata for external SQL/debug
// consumers without mutating the caller's map. The store-owned value wins over
// any caller-supplied "vector_space_id" metadata entry.
func marshalChunkMetadata(meta map[string]string, vectorSpaceID string) ([]byte, error) {
	if vectorSpaceID == "" {
		return json.Marshal(meta)
	}
	merged := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		merged[k] = v
	}
	merged["vector_space_id"] = vectorSpaceID
	return json.Marshal(merged)
}

// Store saves chunks with their embeddings to SQLite.
func (s *SQLiteStore) Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error {
	if err := validateStoreInputs(chunks, embeddings); err != nil {
		return err
	}
	if len(chunks) == 0 {
		return nil
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("rag: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.validateWriteEmbeddingDimensionTx(ctx, tx, len(embeddings[0])); err != nil {
		return err
	}

	if err := s.insertChunksTx(ctx, tx, chunks, embeddings, "", ""); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: commit: %w", err)
	}
	return nil
}

// ReplaceSource atomically replaces all chunks for a source path.
// If insertion fails, the delete is rolled back and existing data is preserved.
func (s *SQLiteStore) ReplaceSource(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64) error {
	return s.ReplaceSourceWithHash(ctx, source, chunks, embeddings, "")
}

// ReplaceSourceWithHash atomically replaces all chunks for a source path
// and stores the given source signature on each chunk for fast file-level
// invalidation during subsequent incremental indexes.
func (s *SQLiteStore) ReplaceSourceWithHash(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash string) error {
	return s.replaceSource(ctx, source, chunks, embeddings, replaceSourceOptions{
		sourceHash: sourceHash,
	})
}

// ReplaceSourceWithHashAndVectorSpaceID atomically replaces all chunks for a
// source path, stores the source signature, and persists the resolved vector
// space identity on each chunk. When vectorSpaceID is non-empty it is written
// to the chunks.vector_space_id column AND mirrored into the per-chunk
// metadata JSON under the "vector_space_id" key. Non-empty chunk batches
// require a non-empty vectorSpaceID; use ReplaceSourceWithHash for legacy
// replacement that intentionally leaves vector_space_id empty. If this source
// already has known vector-space rows, their id must match vectorSpaceID.
func (s *SQLiteStore) ReplaceSourceWithHashAndVectorSpaceID(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID string) error {
	return s.replaceSource(ctx, source, chunks, embeddings, replaceSourceOptions{
		sourceHash:               sourceHash,
		vectorSpaceID:            vectorSpaceID,
		requireVectorSpaceID:     true,
		checkExistingVectorSpace: true,
		allowMissingExisting:     true,
		allowLegacyUnknown:       true,
	})
}

// ForceReplaceSourceWithHashAndVectorSpaceID is the explicit full-reindex path:
// it preserves the atomic replace and non-empty-vsid requirements, but it does
// not reject an existing source whose stored vector space differs from the
// incoming vectorSpaceID. Use this for deliberate full-corpus migrations.
func (s *SQLiteStore) ForceReplaceSourceWithHashAndVectorSpaceID(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID string) error {
	return s.replaceSource(ctx, source, chunks, embeddings, replaceSourceOptions{
		sourceHash:           sourceHash,
		vectorSpaceID:        vectorSpaceID,
		requireVectorSpaceID: true,
	})
}

// ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash atomically replaces all
// chunks for source only if the stored source hash still matches
// expectedSourceHash. The existing per-source vector-space ids are checked in
// the same transaction so incremental writers cannot reuse stale embeddings
// after another writer has changed the source.
func (s *SQLiteStore) ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID, expectedSourceHash string) error {
	return s.replaceSource(ctx, source, chunks, embeddings, replaceSourceOptions{
		sourceHash:               sourceHash,
		vectorSpaceID:            vectorSpaceID,
		requireVectorSpaceID:     true,
		expectedSourceHash:       expectedSourceHash,
		checkExpectedSourceHash:  true,
		checkExistingVectorSpace: true,
	})
}

func (s *SQLiteStore) replaceSource(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, opts replaceSourceOptions) error {
	if err := s.validateReplaceSource(source, chunks, embeddings, opts); err != nil {
		return err
	}
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("%w: replace source %q: begin transaction: %w", ErrStoreOperation, source, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.replaceSourceTx(ctx, tx, source, chunks, embeddings, opts); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: replace source %q: commit: %w", source, err)
	}
	return nil
}

func (s *SQLiteStore) validateReplaceSource(source string, chunks []Chunk, embeddings [][]float64, opts replaceSourceOptions) error {
	if err := validateStoreInputs(chunks, embeddings); err != nil {
		return fmt.Errorf("rag: replace source %q: %w", source, err)
	}
	for i, chunk := range chunks {
		if chunk.Source != source {
			return fmt.Errorf("rag: replace source %q: chunk %d has source %q", source, i, chunk.Source)
		}
	}
	if opts.requireVectorSpaceID && len(chunks) > 0 && opts.vectorSpaceID == "" {
		return fmt.Errorf("%w: replace source %q with non-empty chunks", ErrMissingVectorSpaceID, source)
	}
	return nil
}

func (s *SQLiteStore) replaceSourceTx(ctx context.Context, tx *sql.Tx, source string, chunks []Chunk, embeddings [][]float64, opts replaceSourceOptions) error {
	if !opts.replaceVectorSpace && len(embeddings) > 0 {
		if err := s.validateWriteEmbeddingDimensionTx(ctx, tx, len(embeddings[0])); err != nil {
			return fmt.Errorf("rag: replace source %q: %w", source, err)
		}
	}

	if opts.checkExpectedSourceHash {
		matches, err := sourceHashMatchesTx(ctx, tx, source, opts.expectedSourceHash)
		if err != nil {
			return fmt.Errorf("%w: replace source %q: check source hash: %w", ErrStoreOperation, source, err)
		}
		if !matches {
			return fmt.Errorf("%w: replace source %q expected source hash %q", ErrIncrementalStaleSource, source, opts.expectedSourceHash)
		}
	}
	if opts.checkExistingVectorSpace && len(chunks) > 0 {
		if err := validateExistingSourceVectorSpaceTx(ctx, tx, source, opts.vectorSpaceID, opts.allowMissingExisting, opts.allowLegacyUnknown); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: replace source %q: delete existing chunks: %w", source, err)
	}
	if opts.replaceVectorSpace && len(embeddings) > 0 {
		if err := s.validateMigrationCorpusEmbeddingDimensionTx(ctx, tx, len(embeddings[0])); err != nil {
			return fmt.Errorf("rag: replace source %q: %w", source, err)
		}
	}

	if len(chunks) > 0 {
		if err := s.insertChunksTx(ctx, tx, chunks, embeddings, opts.sourceHash, opts.vectorSpaceID); err != nil {
			return fmt.Errorf("rag: replace source %q: %w", source, err)
		}
	}
	return nil
}

func (s *SQLiteStore) validateMigrationCorpusEmbeddingDimensionTx(ctx context.Context, tx *sql.Tx, dimension int) error {
	if s.writeEmbeddingErr != nil {
		return s.writeEmbeddingErr
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, embedding FROM chunks ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("rag: write embeddings: inspect remaining corpus: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var chunkID string
		var encoded []byte
		if err := rows.Scan(&chunkID, &encoded); err != nil {
			return fmt.Errorf("rag: write embeddings: inspect remaining corpus: %w", err)
		}
		format, existingDimension, err := inspectEmbedding(encoded)
		if err != nil {
			return fmt.Errorf("rag: write embeddings: inspect existing chunk %q: %w", chunkID, err)
		}
		if format != embeddingFormatPackedFloat32 {
			return fmt.Errorf("rag: write embeddings: existing store uses %s; rebuild or explicitly migrate it before writing packed vectors", format)
		}
		if dimension != existingDimension {
			return fmt.Errorf("rag: write embeddings: dimension %d does not match existing corpus dimension %d", dimension, existingDimension)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rag: write embeddings: inspect remaining corpus: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("rag: write embeddings: close remaining corpus: %w", err)
	}
	return nil
}

func (s *SQLiteStore) validateWriteEmbeddingDimensionTx(ctx context.Context, tx *sql.Tx, dimension int) error {
	if s.writeEmbeddingErr != nil {
		return s.writeEmbeddingErr
	}
	// Best-effort re-check inside the write transaction: the complete every-row
	// scan runs once at open, so inspect the corpus's first and last rows here
	// to also catch foreign-format rows appended after open (for example an
	// older writer overlapping a deploy always appends at the highest rowid).
	var firstID, lastID string
	var firstEncoded, lastEncoded []byte
	err := tx.QueryRowContext(ctx, `SELECT id, embedding FROM chunks ORDER BY rowid LIMIT 1`).Scan(&firstID, &firstEncoded)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("rag: write embeddings: inspect existing corpus: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id, embedding FROM chunks ORDER BY rowid DESC LIMIT 1`).Scan(&lastID, &lastEncoded); err != nil {
		return fmt.Errorf("rag: write embeddings: inspect existing corpus: %w", err)
	}
	inspect := func(chunkID string, encoded []byte) error {
		format, existingDimension, err := inspectEmbedding(encoded)
		if err != nil {
			return fmt.Errorf("rag: write embeddings: inspect existing chunk %q: %w", chunkID, err)
		}
		if format != embeddingFormatPackedFloat32 {
			return fmt.Errorf("rag: write embeddings: existing store uses %s; rebuild or explicitly migrate it before writing packed vectors", format)
		}
		if dimension != existingDimension {
			return fmt.Errorf("rag: write embeddings: dimension %d does not match existing corpus dimension %d", dimension, existingDimension)
		}
		return nil
	}
	if err := inspect(firstID, firstEncoded); err != nil {
		return err
	}
	if lastID != firstID {
		return inspect(lastID, lastEncoded)
	}
	return nil
}

func (s *SQLiteStore) beginWriteTx(ctx context.Context) (*sql.Tx, error) {
	// TxOptions{ReadOnly: false} alone still issues a deferred BEGIN in
	// modernc.org/sqlite; the write lock at BEGIN time comes from the
	// _txlock=immediate DSN option set in sqliteReadWriteDSN, which keeps CAS
	// reads and writes in one write transaction instead of upgrading after
	// the validation SELECT (an upgrade SQLite fails with SQLITE_BUSY without
	// consulting the busy handler).
	return s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: false})
}

func sourceHashMatchesTx(ctx context.Context, tx *sql.Tx, source, expected string) (bool, error) {
	var count int
	var minHash, maxHash string
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MIN(source_content_hash), ''), COALESCE(MAX(source_content_hash), '')
		  FROM chunks
		 WHERE source = ?`, source).Scan(&count, &minHash, &maxHash); err != nil {
		return false, err
	}
	return count > 0 && minHash == expected && maxHash == expected, nil
}

func validateExistingSourceVectorSpaceTx(ctx context.Context, tx *sql.Tx, source, vectorSpaceID string, allowMissing, allowLegacyUnknown bool) error {
	var count int
	var minID, maxID string
	var hasUnknown bool
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MIN(NULLIF(vector_space_id, '')), ''),
		       COALESCE(MAX(NULLIF(vector_space_id, '')), ''),
		       EXISTS(SELECT 1 FROM chunks WHERE source = ? AND vector_space_id = '')
		  FROM chunks
		 WHERE source = ?`, source, source).Scan(&count, &minID, &maxID, &hasUnknown); err != nil {
		return fmt.Errorf("%w: replace source %q: check vector space: %w", ErrStoreOperation, source, err)
	}
	if count == 0 {
		if allowMissing {
			return nil
		}
		return fmt.Errorf("%w: replace source %q has no existing chunks", ErrIncrementalStaleSource, source)
	}
	hasKnown := minID != ""
	if hasUnknown && hasKnown {
		return fmt.Errorf("%w: replace source %q has known vector space %q plus legacy unknown rows", ErrCorpusMixedVectorSpaces, source, minID)
	}
	if hasUnknown {
		if allowLegacyUnknown {
			return nil
		}
		return fmt.Errorf("%w: %w: replace source %q has only legacy unknown vector-space rows", ErrIncrementalRebuildRequired, ErrMissingVectorSpaceID, source)
	}
	if minID != maxID {
		return fmt.Errorf("%w: replace source %q has mixed vector spaces %q and %q", ErrCorpusMixedVectorSpaces, source, minID, maxID)
	}
	if hasKnown && minID != vectorSpaceID {
		return fmt.Errorf("%w: replace source %q existing vector space %q differs from incoming %q", ErrVectorSpaceDrift, source, minID, vectorSpaceID)
	}
	return nil
}

func validateCorpusVectorSpaceTx(ctx context.Context, tx *sql.Tx, excludedSource, vectorSpaceID string) error {
	var minID, maxID string
	var hasUnknown bool
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MIN(NULLIF(vector_space_id, '')), ''),
		       COALESCE(MAX(NULLIF(vector_space_id, '')), ''),
		       EXISTS(SELECT 1 FROM chunks WHERE source <> ? AND vector_space_id = '')
		  FROM chunks
		 WHERE source <> ?`, excludedSource, excludedSource).Scan(&minID, &maxID, &hasUnknown); err != nil {
		return fmt.Errorf("%w: validate corpus vector space: %w", ErrStoreOperation, err)
	}
	if minID != maxID {
		return fmt.Errorf("%w: existing corpus mixes vector spaces %q and %q; reindex existing sources into one embedding space before managed ingest", ErrCorpusMixedVectorSpaces, minID, maxID)
	}
	if hasUnknown && minID != "" {
		return fmt.Errorf("%w: existing corpus mixes vector space %q with legacy chunks missing vector-space identity; reindex legacy sources before managed ingest", ErrCorpusMixedVectorSpaces, minID)
	}
	if hasUnknown {
		return fmt.Errorf("%w: incoming vector space %q would mix with legacy chunks missing vector-space identity; reindex legacy sources before managed ingest", ErrCorpusMixedVectorSpaces, vectorSpaceID)
	}
	if minID != "" && minID != vectorSpaceID {
		return fmt.Errorf("%w: incoming vector space %q differs from corpus vector space %q", ErrVectorSpaceDrift, vectorSpaceID, minID)
	}
	return nil
}

// Search finds the top-k most similar chunks using brute-force cosine similarity.
func (s *SQLiteStore) Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	if s.immutable {
		return s.searchSnapshot(ctx, queryEmbedding, k)
	}

	stopScan := s.stageTimer("dense_scan_decode_hydrate_semantic")
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language, metadata, embedding, stable_key FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("rag: query chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var results []SearchResult
	var decoder corpusEmbeddingDecoder
	for rows.Next() {
		var chunk Chunk
		var metaJSON string
		var encodedEmbedding []byte
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language, &metaJSON, &encodedEmbedding, &chunk.StableKey); err != nil {
			return nil, fmt.Errorf("rag: scan chunk: %w", err)
		}

		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metaJSON), &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
		}

		embedding, err := decoder.decode(encodedEmbedding, chunk.ID)
		if err != nil {
			return nil, err
		}
		if err := validateSearchEmbeddingDimension(chunk.ID, queryEmbedding, embedding); err != nil {
			return nil, err
		}

		score := cosineSimilarity(queryEmbedding, embedding)
		results = append(results, SearchResult{
			Chunk:    chunk,
			Score:    score,
			Distance: 1 - score,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate chunks: %w", err)
	}
	stopScan()

	// Sort by score descending
	stopRank := s.stageTimer("ranking_top_k")
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if k > 0 && len(results) > k {
		results = results[:k]
	}
	stopRank()
	return results, nil
}

// DeleteBySource removes all chunks with the given source path.
func (s *SQLiteStore) DeleteBySource(ctx context.Context, source string) error {
	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return fmt.Errorf("rag: delete by source %q: begin transaction: %w", source, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE source = ?`, source); err != nil {
		return fmt.Errorf("rag: delete by source %q: %w", source, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE managed_documents
		   SET state = 'failed', freshness = 'stale',
		       last_error = 'chunks deleted by low-level source deletion', updated_at = ?
		 WHERE source = ?`, time.Now().Unix(), source); err != nil {
		return fmt.Errorf("rag: mark managed source %q deleted: %w", source, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rag: delete by source %q: commit: %w", source, err)
	}
	return nil
}

// ListSources returns the distinct source paths currently stored, sorted
// ascending. It supports deleted-source pruning during directory indexing.
func (s *SQLiteStore) ListSources(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT source FROM chunks ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("rag: list sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []string
	for rows.Next() {
		var src string
		if err := rows.Scan(&src); err != nil {
			return nil, fmt.Errorf("rag: scan source: %w", err)
		}
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate sources: %w", err)
	}
	return sources, nil
}

// GetBySource returns all chunks for a given source path, ordered by start_line,
// with their embeddings. This supports incremental indexing by allowing comparison
// of existing chunks against newly generated ones.
func (s *SQLiteStore) GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language,
		        metadata, embedding, stable_key, vector_space_id
		   FROM chunks
		  WHERE source = ?
		  ORDER BY start_line`, source)
	if err != nil {
		return nil, fmt.Errorf("rag: get by source %q: %w", source, err)
	}
	defer func() { _ = rows.Close() }()

	var results []ChunkWithEmbedding
	var decoder corpusEmbeddingDecoder
	for rows.Next() {
		var chunk Chunk
		var metaJSON string
		var encodedEmbedding []byte
		var vectorSpaceID string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language,
			&metaJSON, &encodedEmbedding, &chunk.StableKey, &vectorSpaceID); err != nil {
			return nil, fmt.Errorf("rag: scan chunk for source %q: %w", source, err)
		}

		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metaJSON), &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
		}

		embedding, err := decoder.decode(encodedEmbedding, chunk.ID)
		if err != nil {
			return nil, err
		}

		results = append(results, ChunkWithEmbedding{
			Chunk:         chunk,
			Embedding:     embedding,
			VectorSpaceID: vectorSpaceID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate chunks for source %q: %w", source, err)
	}
	return results, nil
}

// GetSourceHash returns the stored source signature for a source path, or
// empty string if the source has no chunks or no signature is stored. This
// enables fast file-level invalidation during incremental indexing.
func (s *SQLiteStore) GetSourceHash(ctx context.Context, source string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT source_content_hash FROM chunks WHERE source = ? LIMIT 1`,
		source).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("rag: get source hash for %q: %w", source, err)
	}
	return hash, nil
}

// UpdateSourceHash updates the source_content_hash for all chunks belonging
// to the given source. Used to backfill the hash on databases migrated from
// V3 where chunks exist but the hash column is empty.
func (s *SQLiteStore) UpdateSourceHash(ctx context.Context, source, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE chunks SET source_content_hash = ? WHERE source = ?`, hash, source)
	if err != nil {
		return fmt.Errorf("rag: update source hash for %q: %w", source, err)
	}
	return nil
}

func (s *SQLiteStore) embeddingStorageSummary(ctx context.Context) (format string, dimension int, storageBytes int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, embedding FROM chunks ORDER BY rowid`)
	if err != nil {
		return "", 0, 0, fmt.Errorf("rag: inspect embedding storage: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var firstFormat embeddingFormat
	mixed := false
	for rows.Next() {
		var chunkID string
		var encoded []byte
		if err := rows.Scan(&chunkID, &encoded); err != nil {
			return "", 0, storageBytes, fmt.Errorf("rag: inspect embedding storage row: %w", err)
		}
		storageBytes += int64(len(encoded))
		rowFormat, rowDimension, err := inspectEmbedding(encoded)
		if err != nil {
			return "", 0, storageBytes, fmt.Errorf("rag: inspect embedding for chunk %q: %w", chunkID, err)
		}
		if firstFormat == "" {
			firstFormat, dimension = rowFormat, rowDimension
			continue
		}
		if rowFormat != firstFormat || rowDimension != dimension {
			mixed = true
		}
	}
	if err := rows.Err(); err != nil {
		return "", 0, storageBytes, fmt.Errorf("rag: inspect embedding storage rows: %w", err)
	}
	if firstFormat == "" {
		return EmbeddingFormatEmpty, 0, 0, nil
	}
	if mixed {
		return EmbeddingFormatMixed, 0, storageBytes, nil
	}
	return string(firstFormat), dimension, storageBytes, nil
}

// Stats returns index statistics.
func (s *SQLiteStore) Stats(ctx context.Context) (StoreStats, error) {
	var stats StoreStats

	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&stats.TotalChunks)
	if err != nil {
		return stats, fmt.Errorf("rag: count chunks: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT source) FROM chunks`).Scan(&stats.TotalSources)
	if err != nil {
		return stats, fmt.Errorf("rag: count sources: %w", err)
	}

	stats.EmbeddingFormat, stats.EmbeddingDim, stats.EmbeddingBytes, err = s.embeddingStorageSummary(ctx)
	if err != nil {
		return stats, err
	}

	// Get database page count and page size for storage estimate
	var pageCount, pageSize int64
	if s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount) == nil {
		if s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize) == nil {
			stats.StorageBytes = pageCount * pageSize
		}
	}

	return stats, nil
}

// Close releases the database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// cosineSimilarity computes cosine similarity between two vectors.
// Returns 0 if either vector has zero magnitude.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, magA, magB float64
	for i := range a {
		dot += a[i] * b[i]
		magA += a[i] * a[i]
		magB += b[i] * b[i]
	}
	mag := math.Sqrt(magA) * math.Sqrt(magB)
	if mag == 0 {
		return 0
	}
	return dot / mag
}

// detectLanguageFromPath is available in chunker_code.go as detectLanguage.

// ExportChunks implements Exportable by streaming all matching chunks from SQLite.
// The returned iterator yields one ExportedChunk at a time, with cleanup handled
// automatically when iteration stops.
func (s *SQLiteStore) ExportChunks(ctx context.Context, filter *ExportFilter) (iter.Seq2[ExportedChunk, error], error) {
	query, args := buildExportQuery(filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rag: export chunks: %w", err)
	}
	return func(yield func(ExportedChunk, error) bool) {
		defer func() { _ = rows.Close() }()
		var decoder corpusEmbeddingDecoder
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				yield(ExportedChunk{}, err)
				return
			}
			chunk, embedding, err := scanExportRow(rows, &decoder)
			if err != nil {
				yield(ExportedChunk{}, err)
				return
			}
			if !yield(ExportedChunk{Chunk: chunk, Embedding: embedding}, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(ExportedChunk{}, fmt.Errorf("rag: iterate export chunks: %w", err))
		}
	}, nil
}

// buildExportQuery constructs the SELECT with optional WHERE clauses for export filtering.
func buildExportQuery(filter *ExportFilter) (string, []any) {
	base := `SELECT id, content, source, start_line, end_line, language, embedding, stable_key FROM chunks`
	var conditions []string
	var args []any

	if filter != nil {
		if filter.SourcePattern != "" {
			conditions = append(conditions, "source GLOB ?")
			args = append(args, filter.SourcePattern)
		}
		if filter.Language != "" {
			conditions = append(conditions, "language = ?")
			args = append(args, filter.Language)
		}
	}

	if len(conditions) > 0 {
		base += " WHERE " + strings.Join(conditions, " AND ")
	}
	base += " ORDER BY source, start_line"
	return base, args
}

// scanExportRow scans a row from the export query into a Chunk and embedding.
// Metadata is intentionally not selected — it's excluded from the Parquet schema.
func scanExportRow(rows *sql.Rows, decoder *corpusEmbeddingDecoder) (Chunk, []float64, error) {
	var chunk Chunk
	var encodedEmbedding []byte
	if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
		&chunk.StartLine, &chunk.EndLine, &chunk.Language, &encodedEmbedding, &chunk.StableKey); err != nil {
		return Chunk{}, nil, fmt.Errorf("rag: scan export row: %w", err)
	}

	embedding, err := decoder.decode(encodedEmbedding, chunk.ID)
	if err != nil {
		return Chunk{}, nil, err
	}

	return chunk, embedding, nil
}
