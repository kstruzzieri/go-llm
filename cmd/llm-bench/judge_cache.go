package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// judgeCacheKeyVersion is bumped to force-invalidate every entry. Increment
// when changing the canonical request shape (new field, semantic shift).
const judgeCacheKeyVersion = 1

// judgeCacheRequest is the canonical envelope hashed to produce the cache
// key. Fields here MUST be limited to inputs that affect judgment semantics.
// KeepAlive, JudgeTimeout, OllamaURL are deliberately excluded — they
// affect execution, not the judge's verdict.
type judgeCacheRequest struct {
	Version          int     `json:"version"`
	JudgeModel       string  `json:"judge_model"`
	JudgeModelDigest string  `json:"judge_model_digest"`
	SystemPrompt     string  `json:"system_prompt"`
	UserPrompt       string  `json:"user_prompt"`
	Format           string  `json:"format"`
	Temperature      float64 `json:"temperature"`
	NumPredict       int     `json:"num_predict"`
}

// canonicalCacheKey hashes the envelope deterministically. Uses
// encoding/json's stable struct-tag ordering (Go's encoding/json marshals
// struct fields in declaration order), then SHA-256s the bytes.
func canonicalCacheKey(r judgeCacheRequest) string {
	raw, _ := json.Marshal(r) // marshaling a fixed-shape struct cannot fail
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// judgeCacheEntry is the value half of a cache row. ResponseContent is the
// raw judge Message.Content; AnswerQuality and Justification are the
// parsed verdict (denormalized so cache audits don't need to re-parse).
type judgeCacheEntry struct {
	CacheKey         string
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
	Close() error
}
