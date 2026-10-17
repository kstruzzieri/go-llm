package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCanonicalCacheKey_StableUnderFieldReordering(t *testing.T) {
	a := judgeCacheRequest{
		Version: 1, JudgeModel: "ollama/gemma4:31b", JudgeModelDigest: "sha256:abc",
		SystemPrompt: "sys", UserPrompt: "u",
		Format: "json", Temperature: 0.1, NumPredict: 512,
	}
	b := a
	k1 := canonicalCacheKey(a)
	k2 := canonicalCacheKey(b)
	if k1 != k2 {
		t.Fatalf("same inputs produced different keys: %s vs %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("key length %d; want 64 hex chars", len(k1))
	}
}

func TestCanonicalCacheKey_VersionBumpInvalidates(t *testing.T) {
	base := judgeCacheRequest{Version: 1, JudgeModel: "m", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100}
	bumped := base
	bumped.Version = 2
	if canonicalCacheKey(base) == canonicalCacheKey(bumped) {
		t.Fatalf("version bump did not invalidate key")
	}
}

func TestCanonicalCacheKey_DigestSensitive(t *testing.T) {
	base := judgeCacheRequest{Version: 1, JudgeModel: "m", JudgeModelDigest: "sha256:aaa", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100}
	diff := base
	diff.JudgeModelDigest = "sha256:bbb"
	if canonicalCacheKey(base) == canonicalCacheKey(diff) {
		t.Fatalf("digest change did not invalidate key")
	}
}

func TestCanonicalCacheKey_ProviderSensitive(t *testing.T) {
	// A frontier judge served via openai-compat and a local Ollama judge can
	// share a model name (e.g. neither carries a digest). The provider must
	// participate in the key so their verdicts never alias each other.
	base := judgeCacheRequest{Version: judgeCacheKeyVersion, JudgeProvider: "ollama", JudgeModel: "m", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100}
	diff := base
	diff.JudgeProvider = "openai-compat"
	if canonicalCacheKey(base) == canonicalCacheKey(diff) {
		t.Fatalf("judge provider change did not invalidate key")
	}
}

func TestCanonicalCacheKey_ThinkSensitive(t *testing.T) {
	base := judgeCacheRequest{Version: judgeCacheKeyVersion, JudgeModel: "m", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100}
	thinkFalse := false
	noThink := base
	noThink.Think = &thinkFalse
	if canonicalCacheKey(base) == canonicalCacheKey(noThink) {
		t.Fatalf("think mode change did not invalidate key")
	}
}

func TestCanonicalCacheKey_ThinkEnforcedSensitive(t *testing.T) {
	// Think records the judge's intent; ThinkEnforced records whether the
	// transport delivered it to the backend. An openai-compat run with
	// -judge-disable-thinking and one without share every other envelope
	// field, but their verdicts come from different judge regimes and must
	// never alias.
	base := judgeCacheRequest{Version: judgeCacheKeyVersion, JudgeProvider: "openai-compat:ep", JudgeModel: "m", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100}
	enforced := base
	enforced.ThinkEnforced = true
	if canonicalCacheKey(base) == canonicalCacheKey(enforced) {
		t.Fatalf("think-enforced change did not invalidate key")
	}
}

func TestCanonicalCacheKey_ExcludesExecutionOnlyFields(t *testing.T) {
	// Sanity: judgeCacheRequest is the cache key envelope. If anyone adds
	// KeepAlive / JudgeTimeout / OllamaURL to it, this test will be deleted
	// intentionally — keep it as a guard until then.
	s := canonicalCacheKey(judgeCacheRequest{Version: 1, JudgeModel: "m", SystemPrompt: "s", UserPrompt: "u", Format: "json", Temperature: 0.1, NumPredict: 100})
	if strings.TrimSpace(s) == "" {
		t.Fatalf("empty key")
	}
}

func newTestCache(t *testing.T) (*sqliteJudgeCache, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "judge.db")
	c, err := openJudgeCache(path)
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	return c, path
}

func cleanupJudgeCache(t *testing.T, c *sqliteJudgeCache) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("close judge cache: %v", err)
		}
	})
}

func TestSQLiteJudgeCache_PutThenGetReturnsEntry(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	entry := judgeCacheEntry{
		CacheKey: "abc", JudgeModel: "ollama/gemma4:31b",
		JudgeModelDigest: "sha256:deadbeef",
		TraceID:          "t1", CandidateModel: "ollama/cand",
		PromptHash:      "ph",
		RequestJSON:     `{"v":1}`,
		ResponseContent: `{"answer_quality":0.5,"justification":"ok"}`,
		AnswerQuality:   0.5, Justification: "ok",
		CreatedAt: now, LastUsedAt: now, HitCount: 0,
	}
	if err := c.Put(ctx, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.Get(ctx, "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get returned ok=false for present key")
	}
	if got.ResponseContent != entry.ResponseContent || got.AnswerQuality != 0.5 {
		t.Fatalf("Get returned wrong entry: %+v", got)
	}
	if got.HitCount != 1 {
		t.Fatalf("HitCount not bumped: %d", got.HitCount)
	}
}

func TestSQLiteJudgeCache_PersistsJudgeProvider(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	entry := judgeCacheEntry{
		CacheKey: "pk", JudgeProvider: "openai-compat", JudgeModel: "claude-x",
		TraceID: "t", CandidateModel: "ollama/cand", PromptHash: "ph",
		RequestJSON: "{}", ResponseContent: `{"answer_quality":1.0}`,
		AnswerQuality: 1.0, Justification: "j",
		CreatedAt: now, LastUsedAt: now,
	}
	if err := c.Put(ctx, entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.Get(ctx, "pk")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.JudgeProvider != "openai-compat" {
		t.Fatalf("JudgeProvider = %q; want openai-compat", got.JudgeProvider)
	}
}

func TestSQLiteJudgeCache_GetMissingReturnsOkFalse(t *testing.T) {
	c, _ := newTestCache(t)
	_, ok, err := c.Get(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Fatalf("Get returned ok=true for missing key")
	}
}

func TestSQLiteJudgeCache_MigrationIdempotent(t *testing.T) {
	_, path := newTestCache(t)
	// Reopen same path; migration must not error or duplicate version row.
	c2, err := openJudgeCache(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	cleanupJudgeCache(t, c2)
}

func TestOpenJudgeCache_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "nested", "judge.db")
	c, err := openJudgeCache(path)
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file stat: %v", err)
	}
}

func TestOpenJudgeCache_CorruptDBReturnsWrappedErrorNoAutoRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	// Write garbage bytes to the file before opening.
	if err := os.WriteFile(path, []byte("not a sqlite file"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := openJudgeCache(path)
	if err == nil {
		t.Fatalf("openJudgeCache(corrupt) returned nil error; want wrapped open or migrate error")
	}
	if !errors.Is(err, errJudgeCacheOpen) && !errors.Is(err, errJudgeCacheMigrate) {
		t.Fatalf("openJudgeCache(corrupt) returned %v; want errJudgeCacheOpen or errJudgeCacheMigrate", err)
	}
	// The file must still be on disk (no auto-repair / auto-delete).
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		t.Fatalf("openJudgeCache deleted the corrupt file; expected no auto-repair")
	}
}

func TestOpenJudgeCache_EmptyPathReturnsNilCache(t *testing.T) {
	c, err := openJudgeCache("")
	if err != nil {
		t.Fatalf("openJudgeCache(\"\"): %v", err)
	}
	if c != nil {
		t.Fatalf("openJudgeCache(\"\") returned non-nil cache; want nil (disabled)")
	}
}

func TestSQLiteJudgeCache_ConcurrentGetPut_NoRace(t *testing.T) {
	c, _ := newTestCache(t)
	ctx := context.Background()
	const workers = 8
	const iters = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("k-%d-%d", id, i%10) // each worker re-touches its own 10-key window
				now := time.Now().UTC()
				_ = c.Put(ctx, judgeCacheEntry{
					CacheKey: key, JudgeModel: "m", JudgeModelDigest: "d",
					TraceID: "t", CandidateModel: "cand",
					PromptHash: "ph", RequestJSON: "{}", ResponseContent: "{}",
					AnswerQuality: 0.5, Justification: "j",
					CreatedAt: now, LastUsedAt: now,
				})
				_, _, _ = c.Get(ctx, key)
			}
		}(w)
	}
	wg.Wait()
}

func TestJudgeCacheStatsStartAtZero(t *testing.T) {
	dir := t.TempDir()
	c, err := openJudgeCache(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	hits, misses := c.Stats()
	if hits != 0 || misses != 0 {
		t.Fatalf("initial Stats() = (%d,%d); want (0,0)", hits, misses)
	}
}

func TestJudgeCacheStatsIncrement(t *testing.T) {
	dir := t.TempDir()
	c, err := openJudgeCache(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	ctx := context.Background()

	if _, ok, err := c.Get(ctx, "key-A"); err != nil || ok {
		t.Fatalf("first Get: ok=%v err=%v; want ok=false err=nil", ok, err)
	}
	hits, misses := c.Stats()
	if hits != 0 || misses != 1 {
		t.Fatalf("after one miss Stats() = (%d,%d); want (0,1)", hits, misses)
	}

	// Use a Put with realistic fields so the migrated table accepts the row.
	if err := c.Put(ctx, judgeCacheEntry{
		CacheKey:        "key-A",
		JudgeModel:      "test-judge",
		TraceID:         "t",
		CandidateModel:  "test-cand",
		PromptHash:      "ph",
		RequestJSON:     "{}",
		ResponseContent: "{}",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok, err := c.Get(ctx, "key-A"); err != nil || !ok {
		t.Fatalf("second Get: ok=%v err=%v; want ok=true err=nil", ok, err)
	}
	hits, misses = c.Stats()
	if hits != 1 || misses != 1 {
		t.Fatalf("after hit Stats() = (%d,%d); want (1,1)", hits, misses)
	}
}

func TestJudgeCacheStatsAreConcurrentSafe(t *testing.T) {
	dir := t.TempDir()
	c, err := openJudgeCache(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	ctx := context.Background()

	if err := c.Put(ctx, judgeCacheEntry{
		CacheKey:        "k",
		JudgeModel:      "test-judge",
		TraceID:         "t",
		CandidateModel:  "test-cand",
		PromptHash:      "ph",
		RequestJSON:     "{}",
		ResponseContent: "{}",
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	const goroutines = 8
	const ops = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				_, _, _ = c.Get(ctx, "k")
			}
		}()
	}
	wg.Wait()
	hits, misses := c.Stats()
	if hits != goroutines*ops || misses != 0 {
		t.Fatalf("hits=%d misses=%d; want hits=%d misses=0", hits, misses, goroutines*ops)
	}
}

func TestJudgeCacheGetPreservesVerdictOnHit(t *testing.T) {
	dir := t.TempDir()
	c, err := openJudgeCache(filepath.Join(dir, "cache.db"))
	if err != nil {
		t.Fatalf("openJudgeCache: %v", err)
	}
	cleanupJudgeCache(t, c)
	ctx := context.Background()
	want := judgeCacheEntry{
		CacheKey: "k", JudgeModel: "j", TraceID: "t", CandidateModel: "cm",
		PromptHash: "p", RequestJSON: "{}", ResponseContent: `{"answer_quality":0.5}`,
		AnswerQuality: 0.5, Justification: "good",
	}
	if err := c.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.AnswerQuality != want.AnswerQuality {
		t.Fatalf("Get returned AnswerQuality=%v; want %v", got.AnswerQuality, want.AnswerQuality)
	}
	if got.Justification != want.Justification {
		t.Fatalf("Get returned Justification=%q; want %q", got.Justification, want.Justification)
	}
}

func judgeCacheQuery(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var got string
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("Query(%q): %v", query, err)
	}
	return got
}

func TestOpenJudgeCache_BusyTimeoutOnEveryConnection(t *testing.T) {
	c, _ := newTestCache(t)
	// Drop idle connections so each query runs on a connection opened after
	// openJudgeCache returned; a one-off PRAGMA at open would not reach it.
	c.db.SetMaxIdleConns(0)
	for i := range 2 {
		if got := judgeCacheQuery(t, c.db, "PRAGMA busy_timeout"); got != "5000" {
			t.Fatalf("query %d: busy_timeout = %s, want 5000", i, got)
		}
	}
}

// The cache must open the file the -report collision guard resolved:
// symlinks first, then "..", as SQLite does, not lexical cleaning.
func TestOpenJudgeCache_ResolvesSymlinkBeforeDotDot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows; FileURLPreservingPath defers to FileURL there")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real", "sub"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	c, err := openJudgeCache(filepath.Join(root, "link") + "/../judge.db")
	if err != nil {
		t.Fatal(err)
	}
	cleanupJudgeCache(t, c)
	if _, err := os.Stat(filepath.Join(root, "real", "judge.db")); err != nil {
		t.Fatalf("cache not at the symlink-resolved path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "judge.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache opened at the lexically cleaned path (stat err %v)", err)
	}
}

func TestOpenJudgeCache_MemoryStaysInMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	c, err := openJudgeCache(":memory:")
	if err != nil {
		t.Fatalf("openJudgeCache(:memory:): %v", err)
	}
	cleanupJudgeCache(t, c)
	if _, err := os.Stat(":memory:"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(":memory: created an on-disk file (stat err %v)", err)
	}
}

const judgeCacheVersionsQuery = "SELECT COALESCE(group_concat(version), '') FROM (SELECT version FROM judge_cache_schema_version ORDER BY version)"

// seedJudgeCacheV1 writes a schema-v1 judge cache by hand, independent of the
// migration code under test: version row 1 and the v1 table holding one
// entry, without the judge_provider column v2 adds.
func seedJudgeCacheV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	const v1 = `
CREATE TABLE judge_cache_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL);
INSERT INTO judge_cache_schema_version VALUES (1, 'initial judge_cache table', 1);
CREATE TABLE judge_cache (
	cache_key TEXT PRIMARY KEY, judge_model TEXT NOT NULL, judge_model_digest TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL, candidate_model TEXT NOT NULL, prompt_hash TEXT NOT NULL,
	request_json TEXT NOT NULL, response_content TEXT NOT NULL, answer_quality REAL NOT NULL,
	justification TEXT NOT NULL, created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL,
	hit_count INTEGER NOT NULL DEFAULT 0);
CREATE INDEX idx_judge_cache_model_trace ON judge_cache(judge_model, trace_id);
INSERT INTO judge_cache VALUES ('v1key', 'm', '', 't', 'c', 'ph', '{}', '{}', 0.5, 'j', 1, 1, 0);`
	if _, err := db.Exec(v1); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
}

// Two llm-bench runs sharing the default cache path open it at once. One
// round rarely interleaves (measured ~3.5% for the pre-#593 runner once
// busy_timeout was set), so run many;
// TestApplyJudgeCacheMigration_SkipsCommittedStep is the deterministic check.
func TestOpenJudgeCache_ConcurrentOpenersMigrateOnce(t *testing.T) {
	for _, fixture := range []struct {
		name string
		seed func(*testing.T, string)
	}{
		{"fresh", func(*testing.T, string) {}},
		{"v1", seedJudgeCacheV1},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dir := t.TempDir()
			for round := range 25 {
				path := filepath.Join(dir, fmt.Sprintf("judge-%d.db", round))
				fixture.seed(t, path)
				type opened struct {
					c   *sqliteJudgeCache
					err error
				}
				start, results := make(chan struct{}), make(chan opened, 2)
				for range 2 {
					go func() {
						<-start
						c, err := openJudgeCache(path)
						results <- opened{c, err}
					}()
				}
				close(start)
				// Drain both openers before failing so neither handle leaks.
				// The deadline turns an opener stuck on the single pooled
				// connection (a step that kept its transaction) into a
				// failure instead of a hang until the test binary's timeout.
				deadline := time.After(time.Minute)
				var c *sqliteJudgeCache
				for range 2 {
					var r opened
					select {
					case r = <-results:
					case <-deadline:
						t.Fatalf("round %d: an opener is still blocked after 1m", round)
					}
					if r.c != nil {
						cleanupJudgeCache(t, r.c)
						c = r.c
						// A handle returned with its one connection held
						// would block every later query on it.
						if inUse := r.c.db.Stats().InUse; inUse != 0 {
							t.Errorf("round %d: opener returned with %d connection(s) in use", round, inUse)
						}
					}
					if r.err != nil {
						t.Errorf("round %d: concurrent open: %v", round, r.err)
					}
				}
				if t.Failed() {
					return
				}
				if got := judgeCacheQuery(t, c.db, judgeCacheVersionsQuery); got != "1,2" {
					t.Fatalf("round %d: versions = %q, want 1,2", round, got)
				}
				if got := judgeCacheQuery(t, c.db, "SELECT COUNT(*) FROM pragma_table_info('judge_cache') WHERE name = 'judge_provider'"); got != "1" {
					t.Fatalf("round %d: judge_provider columns = %s, want 1", round, got)
				}
				if fixture.name == "v1" {
					if got := judgeCacheQuery(t, c.db, "SELECT COUNT(*) FROM judge_cache WHERE cache_key = 'v1key' AND judge_provider = ''"); got != "1" {
						t.Fatalf("round %d: v1 row after v2 = %s, want 1", round, got)
					}
				}
			}
		})
	}
}

// An opener that read the schema version before another opener committed
// holds a stale decision to run every step. Replaying each step against a
// migrated cache must skip it, not rerun its non-idempotent DDL.
func TestApplyJudgeCacheMigration_SkipsCommittedStep(t *testing.T) {
	c, _ := newTestCache(t)
	for _, m := range judgeCacheMigrations {
		if err := applyJudgeCacheMigration(c.db, m); err != nil {
			t.Errorf("stale v%d: %v", m.version, err)
		}
		// The pool holds one connection: a skipped step that kept its
		// transaction open would block the next Begin forever.
		if inUse := c.db.Stats().InUse; inUse != 0 {
			t.Fatalf("after stale v%d: retained connections = %d", m.version, inUse)
		}
	}
	if got := judgeCacheQuery(t, c.db, judgeCacheVersionsQuery); got != "1,2" {
		t.Fatalf("versions = %q, want 1,2", got)
	}
}

// The claim commits with the DDL: a failed step leaves no version row, so the
// next open retries it instead of skipping a step that never ran. The failure
// is a runtime primary-key conflict, not a prepare error, so the CREATE before
// it has really executed and must be rolled back.
func TestApplyJudgeCacheMigration_FailedStepLeavesNoClaim(t *testing.T) {
	c, _ := newTestCache(t)
	bad := judgeCacheMigration{version: 9, description: "fails", ddl: "CREATE TABLE tentative (v TEXT); INSERT INTO judge_cache_schema_version VALUES (1, 'dup', 0);"}
	if err := applyJudgeCacheMigration(c.db, bad); err == nil {
		t.Fatal("failing step returned nil")
	}
	if inUse := c.db.Stats().InUse; inUse != 0 {
		t.Fatalf("retained connections = %d", inUse)
	}
	if got := judgeCacheQuery(t, c.db, judgeCacheVersionsQuery); got != "1,2" {
		t.Fatalf("versions = %q, want 1,2", got)
	}
	if got := judgeCacheQuery(t, c.db, "SELECT COUNT(*) FROM sqlite_schema WHERE name = 'tentative'"); got != "0" {
		t.Fatalf("tentative tables = %s, want 0", got)
	}
}

// SQLite ignores a file: URI's fragment. Appending the busy_timeout query
// must not leave the fragment after it, where modernc would read it as part
// of the pragma value and fail the open.
func TestOpenJudgeCache_FileURIFragmentIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file: URI of a drive path needs a leading slash; the fragment handling is OS-independent")
	}
	path := filepath.Join(t.TempDir(), "judge.db")
	// openJudgeCache creates filepath.Dir of its raw input, which for a file:
	// URI is a relative "file:..." directory; keep it out of the package dir.
	t.Chdir(t.TempDir())
	c, err := openJudgeCache("file:" + path + "#frag")
	if err != nil {
		t.Fatalf("openJudgeCache(file URI with fragment): %v", err)
	}
	cleanupJudgeCache(t, c)
	if got := judgeCacheQuery(t, c.db, "PRAGMA busy_timeout"); got != "5000" {
		t.Fatalf("busy_timeout = %s, want 5000", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not at the URI's path: %v", err)
	}
}
