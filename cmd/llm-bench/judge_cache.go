package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
	_ "modernc.org/sqlite"
)

// judgeCacheKeyVersion is bumped to force-invalidate every entry. Increment
// when changing the canonical request shape (new field, semantic shift).
// v3 adds JudgeProvider so frontier (openai-compat:<endpoint-id>) and local
// (ollama) verdicts for an identically-named, digest-less judge model cannot
// alias.
// v4 adds ThinkEnforced and evicts every v3 row: on the openai-compat
// transport, think:false in the envelope used to be recorded-but-dropped
// (the wire carried no enable_thinking), so v3 rows judged by a freely
// thinking judge would otherwise replay into runs judged under the
// enforced regime.
const judgeCacheKeyVersion = 4

// judgeCacheRequest is the canonical envelope hashed to produce the cache
// key. Fields here MUST be limited to inputs that affect judgment semantics.
// KeepAlive, JudgeTimeout, OllamaURL are deliberately excluded — they
// affect execution, not the judge's verdict.
//
// JudgeProvider is the judge provider *instance* identity (e.g. "ollama" or
// "openai-compat:<endpoint-id>"), not the API kind. Two providers serving the
// same model name produce different keys so a frontier-judge verdict never
// reuses another provider's cached score.
type judgeCacheRequest struct {
	Version          int    `json:"version"`
	JudgeProvider    string `json:"judge_provider"`
	JudgeModel       string `json:"judge_model"`
	JudgeModelDigest string `json:"judge_model_digest"`
	SystemPrompt     string `json:"system_prompt"`
	UserPrompt       string `json:"user_prompt"`
	Format           string `json:"format"`
	Think            *bool  `json:"think,omitempty"`
	// ThinkEnforced is whether the transport actually delivered Think to the
	// backend (LLMJudgeScorer.ThinkEnforced). Think alone is insufficient:
	// it records the judge's intent, which the openai-compat transport only
	// enforces when -judge-disable-thinking is set.
	ThinkEnforced bool    `json:"think_enforced"`
	Temperature   float64 `json:"temperature"`
	NumPredict    int     `json:"num_predict"`
}

// canonicalCacheKey hashes the envelope deterministically. Uses
// encoding/json's stable struct-tag ordering (Go's encoding/json marshals
// struct fields in declaration order), then SHA-256s the bytes.
func canonicalCacheKey(r judgeCacheRequest) string {
	raw, err := json.Marshal(r)
	if err != nil {
		// judgeCacheRequest is a fixed-shape struct of primitive types
		// (string/int/float/bool); marshaling cannot fail without a future
		// breaking change. Panic so the cache never silently produces
		// sha256("") and collides every key.
		panic(fmt.Sprintf("canonicalCacheKey: json.Marshal failed on fixed-shape struct: %v", err))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		panic(fmt.Sprintf("canonicalCacheKey: json.Compact failed: %v", err))
	}
	sum := sha256.Sum256(compact.Bytes())
	return hex.EncodeToString(sum[:])
}

// judgeCacheEntry is the value half of a cache row. ResponseContent is the
// content string callJudge returned — normally the raw judge
// Message.Content, but on the openai-compat transport it may be the
// server-separated reasoning_content when the reply's content was empty
// (the adapter's verdict-in-reasoning fallback). AnswerQuality and
// Justification are the parsed verdict (denormalized so cache audits
// don't need to re-parse).
type judgeCacheEntry struct {
	CacheKey         string
	JudgeProvider    string // provider instance identity (e.g. "ollama", "openai-compat:<endpoint-id>")
	JudgeModel       string
	JudgeModelDigest string
	TraceID          string
	CandidateModel   string
	PromptHash       string // sha256 of UserPrompt only, for audit grouping
	RequestJSON      string // canonical request envelope, pretty-printed
	ResponseContent  string
	AnswerQuality    float64
	Justification    string
	CreatedAt        time.Time
	LastUsedAt       time.Time
	HitCount         int64
}

// judgeCacheStore is the abstraction used by LLMJudgeScorer. A nil store is
// a valid "no cache" signal; the SQLite-backed concrete type is constructed
// by openJudgeCache.
type judgeCacheStore interface {
	Get(ctx context.Context, key string) (judgeCacheEntry, bool, error)
	Put(ctx context.Context, e judgeCacheEntry) error
	Stats() (hits, misses int64)
	Close() error
}

// judgeCachePresenceStore is implemented by stores that can perform a
// speculative lookup without counting an absent key as a miss. Hits still
// behave like Get hits: hit counters and last_used_at are updated.
type judgeCachePresenceStore interface {
	GetIfPresent(ctx context.Context, key string) (judgeCacheEntry, bool, error)
}

// Sentinel errors so callers can distinguish the four failure modes per
// feedback_error_granularity. All four are non-fatal: callers log + bypass
// to a direct judge call.
var (
	errJudgeCacheOpen    = errors.New("judge cache: open")
	errJudgeCacheMigrate = errors.New("judge cache: migrate schema")
	errJudgeCacheGet     = errors.New("judge cache: get")
	errJudgeCachePut     = errors.New("judge cache: put")
)

// sqliteJudgeCache is the SQLite-backed judgeCacheStore. Use openJudgeCache
// to construct one (it handles migrations); callers MUST treat all returned
// errors as non-fatal and degrade to an uncached judge call.
type sqliteJudgeCache struct {
	db     *sql.DB
	hits   atomic.Int64
	misses atomic.Int64
}

// resolveJudgeCachePath gives the opener and output guards the same target.
// Convert plain paths to URIs before Resolve so symlink/.. is not cleaned.
func resolveJudgeCachePath(path string) (dsn, diskPath string, err error) {
	if strings.TrimSpace(path) == "" {
		return "", "", nil
	}
	if path == ":memory:" {
		return path, "", nil
	}
	u, err := sqlitedsn.FileURLPreservingPath(path)
	if err != nil {
		return "", "", err
	}
	// SQLite ignores fragments; modernc would include one in the last pragma.
	u.Fragment, u.RawFragment = "", ""
	return sqlitedsn.Resolve(u.String(), 5*time.Second)
}

// openJudgeCache opens (and migrates) the judge cache DB at path. Empty
// path returns (nil, nil) so callers can disable the cache by passing "".
// Callers MUST check for a nil concrete pointer before assigning to a
// judgeCacheStore interface variable to avoid the typed-nil interface trap.
func openJudgeCache(path string) (*sqliteJudgeCache, error) {
	dsn, diskPath, err := resolveJudgeCachePath(path)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", errJudgeCacheOpen, path, err)
	}
	if dsn == "" {
		return nil, nil
	}
	if diskPath != "" {
		if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
			return nil, fmt.Errorf("%w %q: mkdir parent: %w", errJudgeCacheOpen, path, err)
		}
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("%w %q: %w", errJudgeCacheOpen, path, err)
	}
	db.SetMaxOpenConns(1) // SQLite does not support concurrent writers; serialize all ops.
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w %q: %w", errJudgeCacheOpen, path, err)
	}
	if err := sqlitedsn.CheckMainFile(context.Background(), db, diskPath); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w %q: %w", errJudgeCacheOpen, path, err)
	}
	if err := migrateJudgeCache(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: %w", errJudgeCacheMigrate, err)
	}
	return &sqliteJudgeCache{db: db}, nil
}

// migrateJudgeCache applies pending migrations, tracking applied versions in
// judge_cache_schema_version, with the claim-first runner of
// conversation/migration.go and feedback/migration.go. The version read here
// only skips steps already applied; each step's own claim decides whether it
// runs, so two llm-bench runs opening one cache at once apply every step
// exactly once.
func migrateJudgeCache(db *sql.DB) error {
	const createVersionTable = `CREATE TABLE IF NOT EXISTS judge_cache_schema_version (
        version     INTEGER PRIMARY KEY,
        description TEXT    NOT NULL,
        applied_at  INTEGER NOT NULL
    );`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("create version table: %w", err)
	}
	var current int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM judge_cache_schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for _, m := range judgeCacheMigrations {
		if m.version <= current {
			continue
		}
		if err := applyJudgeCacheMigration(db, m); err != nil {
			return err
		}
	}
	return nil
}

// judgeCacheMigration is one schema step. ddl may hold several statements and
// need not be idempotent: applyJudgeCacheMigration runs it at most once.
type judgeCacheMigration struct {
	version     int
	description string
	ddl         string
}

var judgeCacheMigrations = []judgeCacheMigration{
	{
		version:     1,
		description: "initial judge_cache table",
		ddl: `
            CREATE TABLE judge_cache (
                cache_key          TEXT PRIMARY KEY,
                judge_model        TEXT NOT NULL,
                judge_model_digest TEXT NOT NULL DEFAULT '',
                trace_id           TEXT NOT NULL,
                candidate_model    TEXT NOT NULL,
                prompt_hash        TEXT NOT NULL,
                request_json       TEXT NOT NULL,
                response_content   TEXT NOT NULL,
                answer_quality     REAL NOT NULL,
                justification      TEXT NOT NULL,
                created_at         INTEGER NOT NULL,
                last_used_at       INTEGER NOT NULL,
                hit_count          INTEGER NOT NULL DEFAULT 0
            );
            CREATE INDEX idx_judge_cache_model_trace ON judge_cache(judge_model, trace_id);
            `,
	},
	{
		version:     2,
		description: "add judge_provider column for provider-instance provenance",
		ddl: `
            ALTER TABLE judge_cache ADD COLUMN judge_provider TEXT NOT NULL DEFAULT '';
            `,
	},
}

// applyJudgeCacheMigration runs m in one transaction whose first statement
// claims m's version row. Being first, the claim takes the write lock without
// upgrading a read lock, so it waits out another writer under busy_timeout
// instead of failing at once. A claim that inserts nothing means another
// opener already committed m, and the step is skipped. The claim commits with
// the DDL, so a failed step leaves no version row behind.
func applyJudgeCacheMigration(db *sql.DB, m judgeCacheMigration) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("rollback v%d: %w", m.version, rbErr))
		}
	}()
	res, err := tx.Exec(
		`INSERT INTO judge_cache_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("claim v%d: %w", m.version, err)
	}
	claimed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim v%d rows: %w", m.version, err)
	}
	if claimed == 0 {
		return nil
	}
	if _, err := tx.Exec(m.ddl); err != nil {
		return fmt.Errorf("apply v%d: %w", m.version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit v%d: %w", m.version, err)
	}
	return nil
}

// Stats returns the running hit/miss counters since this cache was
// opened. Counters are atomic and safe under concurrent Get callers.
// Per spec §5.2 these participate in the benchmark provenance block.
func (c *sqliteJudgeCache) Stats() (int64, int64) {
	return c.hits.Load(), c.misses.Load()
}

// Get returns the entry for key plus ok=true on hit, ok=false on miss.
// On a hit, hit_count is bumped and last_used_at is set to now; the
// returned entry reflects the post-bump values so caller telemetry sees
// the same state the DB now holds.
func (c *sqliteJudgeCache) Get(ctx context.Context, key string) (judgeCacheEntry, bool, error) {
	return c.get(ctx, key, true)
}

// GetIfPresent returns the entry for key when present, but an absent key does
// not increment the miss counter. Use this only for speculative alternate-key
// probes where a miss should not affect benchmark cache-hit provenance.
func (c *sqliteJudgeCache) GetIfPresent(ctx context.Context, key string) (judgeCacheEntry, bool, error) {
	return c.get(ctx, key, false)
}

func (c *sqliteJudgeCache) get(ctx context.Context, key string, countMiss bool) (judgeCacheEntry, bool, error) {
	var e judgeCacheEntry
	var createdNs, lastUsedNs int64
	err := c.db.QueryRowContext(ctx, `
        SELECT cache_key, judge_provider, judge_model, judge_model_digest, trace_id, candidate_model,
               prompt_hash, request_json, response_content, answer_quality, justification,
               created_at, last_used_at, hit_count
        FROM judge_cache WHERE cache_key = ?`, key,
	).Scan(
		&e.CacheKey, &e.JudgeProvider, &e.JudgeModel, &e.JudgeModelDigest, &e.TraceID, &e.CandidateModel,
		&e.PromptHash, &e.RequestJSON, &e.ResponseContent, &e.AnswerQuality, &e.Justification,
		&createdNs, &lastUsedNs, &e.HitCount,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if countMiss {
			c.misses.Add(1)
		}
		return judgeCacheEntry{}, false, nil
	}
	if err != nil {
		return judgeCacheEntry{}, false, fmt.Errorf("%w key %s: %v", errJudgeCacheGet, key, err)
	}
	e.CreatedAt = time.Unix(0, createdNs).UTC()
	e.LastUsedAt = time.Unix(0, lastUsedNs).UTC()

	nowNs := time.Now().UTC().UnixNano()
	if _, err := c.db.ExecContext(ctx,
		`UPDATE judge_cache SET hit_count = hit_count + 1, last_used_at = ? WHERE cache_key = ?`,
		nowNs, key,
	); err != nil {
		// Hit-count is observational; surface to stderr but keep the verdict
		// so the caller is not forced to re-invoke the judge.
		fmt.Fprintf(os.Stderr, "llm-bench: judge cache hit-count update failed (key=%s): %v\n", key, err)
	} else {
		e.HitCount++
		e.LastUsedAt = time.Unix(0, nowNs).UTC()
	}
	c.hits.Add(1)
	return e, true, nil
}

// Put upserts e by cache_key. On conflict, only last_used_at and hit_count
// are updated — the original verdict (answer_quality, justification,
// response_content) is preserved so a re-Put of the same key behaves like
// a Get-hit.
func (c *sqliteJudgeCache) Put(ctx context.Context, e judgeCacheEntry) error {
	_, err := c.db.ExecContext(ctx, `
        INSERT INTO judge_cache (cache_key, judge_provider, judge_model, judge_model_digest, trace_id, candidate_model,
            prompt_hash, request_json, response_content, answer_quality, justification,
            created_at, last_used_at, hit_count)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(cache_key) DO UPDATE SET
            last_used_at = excluded.last_used_at,
            hit_count    = judge_cache.hit_count + 1`,
		e.CacheKey, e.JudgeProvider, e.JudgeModel, e.JudgeModelDigest, e.TraceID, e.CandidateModel,
		e.PromptHash, e.RequestJSON, e.ResponseContent, e.AnswerQuality, e.Justification,
		e.CreatedAt.UnixNano(), e.LastUsedAt.UnixNano(), e.HitCount,
	)
	if err != nil {
		return fmt.Errorf("%w key %s: %v", errJudgeCachePut, e.CacheKey, err)
	}
	return nil
}

// Close releases the underlying DB handle. Safe to call on a nil receiver
// so callers can defer cache.Close() even when openJudgeCache("") returned
// (nil, nil).
func (c *sqliteJudgeCache) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}
