package rag

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Retriever queries the vector store and builds augmented prompts.
type Retriever struct {
	embedder Embedder
	model    string
	store    VectorStore
}

// VectorSpaceProbe summarizes the vector-space-id distribution across a
// vector store's chunks: the distinct known (non-empty) vector-space IDs and
// whether any legacy empty-vsid rows remain.
type VectorSpaceProbe struct {
	KnownIDs   []string
	HasUnknown bool
}

// vectorSpaceProber is the optional capability a VectorStore can implement
// to expose its vsid distribution. Stores that don't implement it skip the
// drift check.
type vectorSpaceProber interface {
	ProbeVectorSpaces(ctx context.Context) (VectorSpaceProbe, error)
}

// RetrieverOption configures a Retriever.
type RetrieverOption func(*Retriever)

// WithRetrieverModel sets the embedding model for query embedding (default:
// "nomic-embed-text"). The retriever model must match the model used when
// the corpus was indexed. Stores that expose vector-space provenance reject
// mismatches before search. Legacy/non-prober stores only retain Search's
// dimension guard, so same-dimension vector-space drift remains undetectable
// there until the store opts into vector-space probing.
func WithRetrieverModel(model string) RetrieverOption {
	return func(r *Retriever) {
		r.model = model
	}
}

// buildRetriever is the single private path constructing a Retriever; both
// the legacy ollama-backed shim NewRetriever and the new
// NewRetrieverWithEmbedder route through it.
func buildRetriever(emb Embedder, store VectorStore, opts ...RetrieverOption) *Retriever {
	r := &Retriever{
		embedder: emb,
		model:    "nomic-embed-text",
		store:    store,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// NewRetriever is the *ollama.Client-backed compat shim. Existing consumers
// continue to use this constructor unchanged; new code should prefer
// NewRetrieverWithEmbedder.
//
// Nil-passthrough is preserved: passing a nil client yields a Retriever
// whose embedder is nil and which will nil-deref on first Retrieve call,
// matching the pre-Embedder behaviour.
func NewRetriever(client *ollama.Client, store VectorStore, opts ...RetrieverOption) *Retriever {
	return buildRetriever(embedderFromOllamaClient(client), store, opts...)
}

// NewRetrieverWithEmbedder is the router-aware constructor. See
// NewIndexerWithEmbedder for the design rationale.
//
// Returns an error if embedder is nil.
func NewRetrieverWithEmbedder(embedder Embedder, store VectorStore, opts ...RetrieverOption) (*Retriever, error) {
	if embedder == nil {
		return nil, fmt.Errorf("rag: NewRetrieverWithEmbedder: embedder is required")
	}
	if isNilVectorStore(store) {
		return nil, fmt.Errorf("rag: NewRetrieverWithEmbedder: store is required")
	}
	return buildRetriever(embedder, store, opts...), nil
}

// Retrieve finds the top-k most relevant chunks for a query.
func (r *Retriever) Retrieve(ctx context.Context, query string, k int) ([]SearchResult, error) {
	res, err := r.embedder.Embed(ctx, r.model, []string{query})
	if err != nil {
		return nil, fmt.Errorf("%w: embed query: %w", ErrEmbedderFailed, err)
	}
	if len(res.Embeddings) != 1 {
		return nil, fmt.Errorf("%w: embed query: expected 1 embedding, got %d", ErrEmbeddingCountMismatch, len(res.Embeddings))
	}
	embedding := res.Embeddings[0]

	if err := r.validateVectorSpace(ctx, res); err != nil {
		return nil, err
	}

	results, err := r.store.Search(ctx, embedding, k)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w: %w", ErrStoreOperation, err)
	}

	return results, nil
}

func (r *Retriever) validateVectorSpace(ctx context.Context, res EmbedResult) error {
	prober, ok := r.store.(vectorSpaceProber)
	if !ok {
		return nil
	}
	probe, err := prober.ProbeVectorSpaces(ctx)
	if err != nil {
		return fmt.Errorf("%w: probe vector spaces: %w", ErrStoreOperation, err)
	}
	return validateQueryVectorSpace(resolveVectorSpaceID(res), probe)
}

func validateQueryVectorSpace(queryVectorSpaceID string, probe VectorSpaceProbe) error {
	if len(probe.KnownIDs) > 1 {
		return fmt.Errorf("%w: corpus has multiple known vector spaces %v", ErrCorpusMixedVectorSpaces, probe.KnownIDs)
	}
	if len(probe.KnownIDs) == 1 && probe.HasUnknown {
		return fmt.Errorf("%w: corpus has known vector space %q plus chunks with unknown legacy vector space", ErrCorpusMixedVectorSpaces, probe.KnownIDs[0])
	}
	if len(probe.KnownIDs) == 0 {
		// Empty and fully-legacy corpora cannot be validated by vsid. Preserve
		// pre-v5 behaviour until rows are re-indexed with a known vector space.
		return nil
	}

	corpusVectorSpaceID := probe.KnownIDs[0]
	if queryVectorSpaceID == "" {
		return fmt.Errorf("%w: query embedder produced no VectorSpaceID for corpus vector space %q", ErrVectorSpaceMismatch, corpusVectorSpaceID)
	}
	if queryVectorSpaceID != corpusVectorSpaceID {
		return fmt.Errorf("%w: query vector space %q differs from corpus vector space %q", ErrVectorSpaceMismatch, queryVectorSpaceID, corpusVectorSpaceID)
	}
	return nil
}

// BuildContext constructs a context string from retrieved chunks,
// formatted for LLM consumption with source attribution.
// maxTokens provides a rough character limit (4 chars per token).
func (r *Retriever) BuildContext(results []SearchResult, maxTokens int) string {
	if len(results) == 0 {
		return ""
	}

	maxChars := maxTokens * 4 // rough approximation
	var b strings.Builder
	b.WriteString("Relevant code context:\n\n")

	for _, res := range results {
		entry := fmt.Sprintf("--- %s (lines %d-%d, similarity: %.2f) ---\n%s\n\n",
			res.Chunk.Source, res.Chunk.StartLine, res.Chunk.EndLine,
			res.Score, res.Chunk.Content)

		if maxChars > 0 && b.Len()+len(entry) > maxChars {
			break
		}
		b.WriteString(entry)
	}

	return b.String()
}
