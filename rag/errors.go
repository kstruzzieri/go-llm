// Package rag — sentinel errors.
//
// These two sentinels are kept distinct because the recovery action diverges:
//
//   - ErrVectorSpaceMismatch: query-side fix — the corpus is consistent but
//     the embedder produced an embedding in a different vector space.
//     Reconfigure the embedder (or its model) to match the corpus.
//
//   - ErrCorpusMixedVectorSpaces: corpus-side fix — chunks from multiple
//     incompatible vector spaces coexist (or legacy unknown-vsid rows
//     coexist with known ones). The only safe recovery is a full re-index.
package rag

import (
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

// SensitiveKind identifies content covered by filesystem indexing policy.
type SensitiveKind string

// Supported filesystem sensitive-content kinds; all default to skipping the file.
const (
	SensitiveOpenAIToken      SensitiveKind = SensitiveKind(secretscan.OpenAIToken)
	SensitiveGitHubToken      SensitiveKind = SensitiveKind(secretscan.GitHubToken)
	SensitiveGitLabToken      SensitiveKind = SensitiveKind(secretscan.GitLabToken)
	SensitiveSlackToken       SensitiveKind = SensitiveKind(secretscan.SlackToken)
	SensitiveNPMToken         SensitiveKind = SensitiveKind(secretscan.NPMToken)
	SensitiveBearerToken      SensitiveKind = SensitiveKind(secretscan.BearerToken)
	SensitiveSecretAssignment SensitiveKind = SensitiveKind(secretscan.SecretAssignment)
	SensitivePrivateKey       SensitiveKind = SensitiveKind(secretscan.PrivateKey)
	SensitivePaymentCard      SensitiveKind = SensitiveKind(secretscan.PaymentCard)
)

// IndexPolicyAction is the filesystem indexing action for detected content.
type IndexPolicyAction string

const (
	// IndexPolicySkip removes the source instead of indexing detected content.
	IndexPolicySkip IndexPolicyAction = "skip"
	// IndexPolicyRedact indexes sanitized content after removing detected spans.
	IndexPolicyRedact IndexPolicyAction = "redact"
)

// IndexPolicyOutcome describes a file affected by sensitive-content policy.
// Path is the caller's original identifier and may itself contain sensitive
// content. Unsafe means clearing the previous indexed source failed.
type IndexPolicyOutcome struct {
	Path   string
	Action IndexPolicyAction
	Kinds  []SensitiveKind
	Unsafe bool
}

// IndexPolicyError reports a skipped file or a failure after policy detection.
// Err preserves the cause for inspection; its contents are never rendered.
type IndexPolicyError struct {
	Outcome IndexPolicyOutcome
	Err     error
}

func (e *IndexPolicyError) Error() string {
	return fmt.Sprintf("rag: index policy %s for %q (kinds=%v, unsafe=%t)",
		e.Outcome.Action, safeIndexPath(e.Outcome.Path), e.Outcome.Kinds, e.Outcome.Unsafe)
}

// Unwrap returns the underlying indexing or cleanup failure, if any.
func (e *IndexPolicyError) Unwrap() error { return e.Err }

func safeIndexPath(path string) string {
	return secretscan.Redact(path, secretscan.Scan(path))
}

// IsSafeIndexSkip reports whether every branch of a non-nil error tree is a
// successfully cleared policy skip. Ordinary errors and unsafe outcomes fail.
func IsSafeIndexSkip(err error) bool {
	if err == nil {
		return false
	}
	if policy, ok := err.(*IndexPolicyError); ok {
		return policy != nil && policy.Outcome.Action == IndexPolicySkip && !policy.Outcome.Unsafe && policy.Err == nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !IsSafeIndexSkip(child) {
				return false
			}
		}
		return true
	}
	if child := errors.Unwrap(err); child != nil {
		return IsSafeIndexSkip(child)
	}
	return false
}

var (
	// ErrPolicyDenied indicates that retrieval policy denied a request.
	ErrPolicyDenied = errors.New("rag: retrieval policy denied")

	// ErrPolicyEvaluatorFailed indicates that retrieval policy evaluation failed.
	ErrPolicyEvaluatorFailed = errors.New("rag: retrieval policy evaluator failed")

	// ErrPolicyDecisionInvalid indicates an invalid retrieval policy decision.
	ErrPolicyDecisionInvalid = errors.New("rag: retrieval policy decision invalid")

	// ErrFreshnessUnknown indicates that retrieval freshness could not be determined.
	ErrFreshnessUnknown = errors.New("rag: retrieval freshness unknown")

	// ErrVectorSpaceMismatch indicates the query embedding's vector-space
	// identifier does not match the (single) vector space found in the
	// corpus, or the query embedder did not produce a VectorSpaceID at all.
	ErrVectorSpaceMismatch = errors.New("rag: vector-space mismatch")

	// ErrCorpusMixedVectorSpaces indicates the corpus contains chunks from
	// more than one vector space, or a partially-migrated corpus where
	// known-vsid rows coexist with legacy unknown-vsid rows.
	ErrCorpusMixedVectorSpaces = errors.New("rag: corpus contains mixed vector spaces")

	// ErrVectorSpaceDrift indicates an incremental index attempted to mix
	// freshly embedded chunks with cached chunks from a different vector space.
	ErrVectorSpaceDrift = errors.New("rag: vector-space drift")

	// ErrMissingVectorSpaceID indicates an embedder or write path produced a
	// non-empty batch without a VectorSpaceID/Provider/Model identity.
	ErrMissingVectorSpaceID = errors.New("rag: missing vector-space id")

	// ErrIncrementalRebuildRequired indicates the incremental path cannot
	// safely reuse cached embeddings and the caller should do a full re-embed.
	ErrIncrementalRebuildRequired = errors.New("rag: incremental index requires full re-embed")

	// ErrIncrementalStaleSource indicates the source changed in the store
	// between the incremental read/diff and the transactional replace.
	ErrIncrementalStaleSource = errors.New("rag: incremental source changed during indexing")

	// ErrEmbedderFailed indicates an embedding call failed during indexing.
	ErrEmbedderFailed = errors.New("rag: embedder failed")

	// ErrEmbeddingCountMismatch indicates an embedder returned the wrong number
	// of vectors for the requested input batch.
	ErrEmbeddingCountMismatch = errors.New("rag: embedding count mismatch")

	// ErrStoreOperation indicates the vector store failed an operation needed
	// by indexing or retrieval.
	ErrStoreOperation = errors.New("rag: vector store operation failed")
)
