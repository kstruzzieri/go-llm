package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
