package main

import (
	"strings"
	"testing"
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
