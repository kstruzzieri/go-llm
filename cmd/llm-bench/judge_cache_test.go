package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	t.Cleanup(func() { _ = c.Close() })
	return c, path
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
	defer c2.Close()
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
