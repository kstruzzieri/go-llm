package rag

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Retriever queries the vector store and builds augmented prompts.
type Retriever struct {
	embedder        Embedder
	model           string
	store           VectorStore
	vectorOnly      bool
	readManagedFile func(context.Context, string) ([]byte, error)
	policyEvaluator RetrievalPolicyEvaluator
	policyObserver  RetrievalPolicyObserver
}

// RetrievalScope restricts retrieval to managed documents in a collection and
// carrying every requested tag. An empty scope preserves legacy retrieval.
type RetrievalScope struct {
	Collection string
	Tags       []string
}

// Compile-time check: *Retriever satisfies ScoredRetriever.
var _ ScoredRetriever = (*Retriever)(nil)

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

// WithVectorOnly forces vector-only (cosine) retrieval even when the store
// implements MultiSignalSearcher, disabling the default hybrid path. Use it
// to bypass hybrid retrieval — for example against an index whose FTS5
// keyword table is unavailable (a legacy snapshot opened read-only cannot be
// migrated to add it), or to compare ranking strategies. Hybrid retrieval
// otherwise depends on the chunks_fts table populated at index time.
func WithVectorOnly() RetrieverOption {
	return func(r *Retriever) {
		r.vectorOnly = true
	}
}

// WithRetrievalPolicyEvaluator installs the evaluator used for retrieval policy.
func WithRetrievalPolicyEvaluator(evaluator RetrievalPolicyEvaluator) RetrieverOption {
	return func(r *Retriever) {
		if isNilRetrievalPolicyValue(evaluator) {
			r.policyEvaluator = nil
			return
		}
		r.policyEvaluator = evaluator
	}
}

func isNilRetrievalPolicyValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// WithRetrievalPolicyObserver installs the synchronous, consumer-owned policy
// observer.
func WithRetrievalPolicyObserver(observer RetrievalPolicyObserver) RetrieverOption {
	return func(r *Retriever) {
		if isNilRetrievalPolicyValue(observer) {
			r.policyObserver = nil
			return
		}
		r.policyObserver = observer
	}
}

// PolicyActive reports whether an evaluator is installed; an observer alone
// does not make retrieval policy active.
func (r *Retriever) PolicyActive() bool {
	return r != nil && r.policyEvaluator != nil
}

// buildRetriever is the single private path constructing a Retriever; both
// the legacy ollama-backed shim NewRetriever and the new
// NewRetrieverWithEmbedder route through it.
func buildRetriever(emb Embedder, store VectorStore, opts ...RetrieverOption) *Retriever {
	r := &Retriever{
		embedder:        emb,
		model:           "nomic-embed-text",
		store:           store,
		readManagedFile: readManagedRegularFile,
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
	response, err := r.RetrieveRequest(ctx, RetrievalRequest{Query: query, K: k})
	if err != nil {
		return nil, err
	}
	return semanticSearchResults(response.Results, r.usesDenseSearch(), true), nil
}

// RetrieveScoped finds top-k relevant managed chunks after applying scope.
func (r *Retriever) RetrieveScoped(ctx context.Context, query string, k int, scope RetrievalScope) ([]SearchResult, error) {
	response, err := r.RetrieveRequest(ctx, RetrievalRequest{Query: query, K: k, Scope: scope})
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeRetrievalScope(scope)
	if err != nil {
		return nil, err
	}
	return semanticSearchResults(response.Results, r.usesDenseSearch(), normalized.empty()), nil
}

func (r *Retriever) embedQuery(ctx context.Context, query string) ([]float64, error) {
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
	return embedding, nil
}

func semanticSearchResults(scored []ScoredResult, dense, collapseEmpty bool) []SearchResult {
	if scored == nil {
		return nil
	}
	// Dense results pass through verbatim: the legacy dense path returned the
	// store's Score and shape untouched. Only hybrid ranking rewrites Score
	// back to its semantic-similarity contract (1 - Distance). Hybrid empty
	// shapes follow the legacy flatten: the unscoped path collapsed every
	// empty slice to nil (collapseEmpty), while the scoped path collapsed only
	// empty-at-source — post-filter results here — where a surviving non-nil
	// empty slice means filtered-to-empty and stays non-nil.
	if !dense && collapseEmpty && len(scored) == 0 {
		return nil
	}
	results := make([]SearchResult, len(scored))
	for i, result := range scored {
		results[i] = result.SearchResult
		if !dense {
			results[i].Score = 1 - results[i].Distance
		}
	}
	return results
}

// RetrieveScored is the signal-scored retrieval surface. Like Retrieve it embeds
// the query and validates the vector space, but it returns ScoredResult with the
// per-signal breakdown preserved instead of flattening to SearchResult. On stores
// that implement MultiSignalSearcher (and when WithVectorOnly is not set) it
// returns SearchMulti's hybrid results directly; otherwise it falls back to dense
// vector search wrapped as single-signal ("semantic") scored results.
func (r *Retriever) RetrieveScored(ctx context.Context, query string, k int, qCtx QueryContext) ([]ScoredResult, error) {
	response, err := r.RetrieveRequest(ctx, RetrievalRequest{Query: query, K: k, QueryContext: qCtx})
	if err == nil && response.Results == nil && r.usesDenseSearch() {
		response.Results = []ScoredResult{}
	}
	return response.Results, err
}

func (r *Retriever) retrieveScoredBase(ctx context.Context, req RetrievalRequest, owned, policyActive bool, candidateLimit int) ([]ScoredResult, []retrievalFreshness, error) {
	scope, err := normalizeRetrievalScope(req.Scope)
	if err != nil {
		return nil, nil, err
	}
	searchLimit := req.K
	if candidateLimit > 0 {
		searchLimit = candidateLimit
	}
	if scope.empty() {
		results, err := r.retrieveScored(ctx, req.Query, searchLimit, req.QueryContext)
		if err != nil {
			return nil, nil, err
		}
		limit := 0
		if candidateLimit > 0 {
			limit = candidateLimit
		} else if policyActive {
			limit = req.K
		}
		results, freshness, err := r.prepareUnscopedScoredResults(ctx, results, limit, owned)
		if err != nil {
			return nil, nil, err
		}
		return results, freshness, nil
	}
	return r.retrieveScoredScopedBase(ctx, req.Query, searchLimit, scope, req.QueryContext, owned, policyActive, candidateLimit)
}

func (r *Retriever) prepareUnscopedScoredResults(ctx context.Context, results []ScoredResult, k int, owned bool) ([]ScoredResult, []retrievalFreshness, error) {
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	results = ownScoredResults(results, owned)
	freshness, err := refreshManagedScoredResults(ctx, r.store, r.readManagedFile, results)
	return results, freshness, err
}

func (r *Retriever) retrieveScoredScopedBase(ctx context.Context, query string, k int, scope RetrievalScope, qCtx QueryContext, owned, policyActive bool, candidateLimit int) ([]ScoredResult, []retrievalFreshness, error) {
	store, ok := r.store.(*SQLiteStore)
	if !ok {
		return nil, nil, fmt.Errorf("rag: scoped retrieval requires SQLiteStore")
	}
	if store.immutable {
		registry, err := managedRegistrySnapshotForScope(ctx, store, scope)
		if err != nil {
			return nil, nil, fmt.Errorf("rag: scoped managed registry: %w", err)
		}
		results, err := r.retrieveScoredImmutableScoped(ctx, query, k, qCtx, store, registry)
		if err != nil {
			return nil, nil, err
		}
		limit := 0
		if candidateLimit > 0 {
			limit = candidateLimit
		} else if policyActive {
			limit = k
		}
		results = filterScoredResults(results, scope, registry, limit)
		results = ownScoredResults(results, owned)
		freshness, err := refreshManagedScoredResultsWithRegistry(ctx, r.readManagedFile, results, registry)
		if err != nil {
			return nil, nil, err
		}
		return results, freshness, nil
	}
	results, err := r.retrieveScored(ctx, query, 0, qCtx)
	if err != nil {
		return nil, nil, err
	}
	registry, _, err := managedRegistrySnapshot(ctx, store, scoredResultChunks(results))
	if err != nil {
		return nil, nil, fmt.Errorf("rag: scoped managed registry: %w", err)
	}
	results = filterScoredResults(results, scope, registry, k)
	results = ownScoredResults(results, owned)
	freshness, err := refreshManagedScoredResultsWithRegistry(ctx, r.readManagedFile, results, registry)
	if err != nil {
		return nil, nil, err
	}
	return results, freshness, nil
}

// RetrieveScoredScoped is RetrieveScored with managed collection/tag filtering.
func (r *Retriever) RetrieveScoredScoped(ctx context.Context, query string, k int, scope RetrievalScope, qCtx QueryContext) ([]ScoredResult, error) {
	response, err := r.RetrieveRequest(ctx, RetrievalRequest{Query: query, K: k, Scope: scope, QueryContext: qCtx})
	// The immutable snapshot's zero-candidate hybrid search returned a non-nil
	// empty slice on the legacy scored surface (the canonical pipeline
	// collapses it to nil internally); dense empties keep their legacy
	// restored shape as on RetrieveScored. Mutable hybrid emptiness stays nil,
	// matching SearchMulti's legacy passthrough.
	if err == nil && response.Results == nil {
		if s, ok := r.store.(*SQLiteStore); r.usesDenseSearch() || (ok && s.immutable && !scopeEmptyForRestore(scope)) {
			response.Results = []ScoredResult{}
		}
	}
	return response.Results, err
}

// scopeEmptyForRestore reports whether the caller's scope normalizes to empty,
// treating an invalid scope as empty; it exists only to pick the legacy empty
// shape and never affects errors or filtering.
func scopeEmptyForRestore(scope RetrievalScope) bool {
	normalized, err := normalizeRetrievalScope(scope)
	return err != nil || normalized.empty()
}

func (r *Retriever) usesDenseSearch() bool {
	_, hybrid := r.store.(MultiSignalSearcher)
	return r.vectorOnly || !hybrid
}

func (r *Retriever) retrieveScored(ctx context.Context, query string, k int, qCtx QueryContext) ([]ScoredResult, error) {
	embedding, err := r.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}

	if ms, ok := r.store.(MultiSignalSearcher); ok && !r.vectorOnly {
		scored, err := ms.SearchMulti(ctx, embedding, query, k, qCtx)
		if err != nil {
			return nil, fmt.Errorf("rag: hybrid search: %w: %w", ErrStoreOperation, err)
		}
		return scored, nil
	}

	return r.denseScored(ctx, embedding, k)
}

func (r *Retriever) retrieveScoredImmutableScoped(ctx context.Context, query string, k int, qCtx QueryContext, store *SQLiteStore, registry map[string]managedRegistryDocument) ([]ScoredResult, error) {
	embedding, err := r.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	if !r.vectorOnly {
		results, err := store.searchMultiSnapshotScoped(ctx, embedding, query, k, qCtx, registry)
		if err != nil {
			return nil, fmt.Errorf("rag: hybrid search: %w: %w", ErrStoreOperation, err)
		}
		// The snapshot search allocates even for zero candidates — the only
		// hybrid source that produces a non-nil empty slice. Collapse it to
		// nil here (as the legacy pre-filter flatten did) so scope filtering,
		// which preserves nil-ness via results[:0], keeps empty-at-source
		// distinguishable from filtered-to-empty downstream.
		if len(results) == 0 {
			return nil, nil
		}
		return results, nil
	}
	results, err := store.searchSnapshotScoped(ctx, embedding, k, registry)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w: %w", ErrStoreOperation, err)
	}
	return searchResultsToScored(results), nil
}

func (scope RetrievalScope) empty() bool {
	return scope.Collection == "" && len(scope.Tags) == 0
}

func normalizeRetrievalScope(scope RetrievalScope) (RetrievalScope, error) {
	if !utf8.ValidString(scope.Collection) || len(scope.Collection) > MaxManagedMetadataBytes {
		return RetrievalScope{}, fmt.Errorf("rag: retrieval collection exceeds %d-byte limit or is not valid UTF-8", MaxManagedMetadataBytes)
	}
	scope.Collection = strings.TrimSpace(scope.Collection)
	var err error
	scope.Tags, err = normalizeManagedTags(scope.Tags)
	if err != nil {
		return RetrievalScope{}, err
	}
	return scope, nil
}

func filterScoredResults(results []ScoredResult, scope RetrievalScope, registry map[string]managedRegistryDocument, k int) []ScoredResult {
	if results == nil {
		return nil
	}
	capacity := len(results)
	if k > 0 && k < capacity {
		capacity = k
	}
	filtered := make([]ScoredResult, 0, capacity)
	for _, result := range results {
		if document, ok := registry[result.Chunk.Source]; ok && matchesRetrievalScope(result.Chunk, document, scope) {
			filtered = append(filtered, result)
			if k > 0 && len(filtered) == k {
				break
			}
		}
	}
	return filtered
}

func matchesRetrievalScope(chunk Chunk, document managedRegistryDocument, scope RetrievalScope) bool {
	if !matchesManagedRegistry(chunk, document) {
		return false
	}
	return (scope.Collection == "" || document.collection == scope.Collection) && containsManagedTags(document.tags, scope.Tags)
}

type managedRegistryDocument struct {
	id, source, title, kind, origin, mimeType, contentHash, sourceSignature string
	vectorSpaceID, collection, tagsJSON, state                              string
	chunkCount                                                              int
	tags                                                                    []string
}

func matchesManagedRegistry(chunk Chunk, document managedRegistryDocument) bool {
	meta := chunk.Metadata
	return chunk.Source == document.source && meta["managed_document_id"] == document.id &&
		meta["managed_title"] == document.title && meta["managed_kind"] == document.kind &&
		meta["managed_origin"] == document.origin && meta["managed_mime_type"] == document.mimeType &&
		meta["managed_content_hash"] == document.contentHash && meta["managed_collection"] == document.collection &&
		meta["managed_tags"] == document.tagsJSON && meta["managed_state"] == document.state
}

func scoredResultChunks(results []ScoredResult) []Chunk {
	chunks := make([]Chunk, len(results))
	for i := range results {
		chunks[i] = results[i].Chunk
	}
	return chunks
}

var errManagedDocumentsTableMissing = errors.New("rag: managed_documents table missing")

func managedRegistrySnapshot(ctx context.Context, store *SQLiteStore, candidates []Chunk) (map[string]managedRegistryDocument, map[string]struct{}, error) {
	sources := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, chunk := range candidates {
		if looksManagedDocumentSource(chunk.Source) {
			if _, ok := seen[chunk.Source]; !ok {
				seen[chunk.Source] = struct{}{}
				sources = append(sources, chunk.Source)
			}
		}
	}
	if len(sources) == 0 {
		return nil, nil, nil
	}
	registeredSources := make(map[string]struct{}, len(sources))
	registry, err := managedRegistrySnapshotRead(ctx, store, sources, nil, registeredSources)
	return registry, registeredSources, err
}

func managedRegistrySnapshotForScope(ctx context.Context, store *SQLiteStore, scope RetrievalScope) (map[string]managedRegistryDocument, error) {
	return managedRegistrySnapshotRead(ctx, store, nil, &scope, nil)
}

func managedRegistrySnapshotRead(ctx context.Context, store *SQLiteStore, sources []string, scope *RetrievalScope, registeredSources map[string]struct{}) (map[string]managedRegistryDocument, error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'managed_documents')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, errManagedDocumentsTableMissing
	}

	scopedDocuments := make(map[string]managedRegistryDocument)
	if scope != nil {
		query := `
			SELECT id, source, title, kind, origin, mime_type, content_hash,
			       source_signature, vector_space_id, chunk_count, collection, tags, state
			  FROM managed_documents
			 WHERE state = ?`
		args := []any{string(DocumentStateIndexed)}
		if scope.Collection != "" {
			query += ` AND collection = ?`
			args = append(args, scope.Collection)
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var document managedRegistryDocument
			if err := scanManagedRegistryDocument(rows, &document); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if validManagedRegistryDocument(&document) && containsManagedTags(document.tags, scope.Tags) {
				sources = append(sources, document.source)
				scopedDocuments[document.source] = document
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	registry := make(map[string]managedRegistryDocument, len(sources))
	const batchSize = 100
	for start := 0; start < len(sources); start += batchSize {
		end := min(start+batchSize, len(sources))
		placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, end-start)
		for i, source := range sources[start:end] {
			args[i] = source
		}
		batchRegistry := make(map[string]managedRegistryDocument, end-start)
		if scope == nil {
			rows, err := tx.QueryContext(ctx, `
				SELECT id, source, title, kind, origin, mime_type, content_hash,
				       source_signature, vector_space_id, chunk_count, collection, tags, state
				  FROM managed_documents
				 WHERE source IN (`+placeholders+`)`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var document managedRegistryDocument
				if err := scanManagedRegistryDocument(rows, &document); err != nil {
					_ = rows.Close()
					return nil, err
				}
				if registeredSources != nil {
					registeredSources[document.source] = struct{}{}
				}
				if validManagedRegistryDocument(&document) {
					batchRegistry[document.source] = document
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		} else {
			for _, source := range sources[start:end] {
				batchRegistry[source] = scopedDocuments[source]
			}
		}

		rows, err := tx.QueryContext(ctx, `
			SELECT c.source, COUNT(*),
			       COALESCE(MIN(c.source_content_hash), ''), COALESCE(MAX(c.source_content_hash), ''),
			       COALESCE(MIN(c.vector_space_id), ''), COALESCE(MAX(c.vector_space_id), ''),
			       MIN(CASE WHEN json_valid(c.metadata) THEN
			           CASE WHEN
			             json_type(c.metadata, '$.managed_document_id') = 'text' AND json_extract(c.metadata, '$.managed_document_id') = d.id AND
			             json_type(c.metadata, '$.managed_title') = 'text' AND json_extract(c.metadata, '$.managed_title') = d.title AND
			             json_type(c.metadata, '$.managed_kind') = 'text' AND json_extract(c.metadata, '$.managed_kind') = d.kind AND
			             json_type(c.metadata, '$.managed_origin') = 'text' AND json_extract(c.metadata, '$.managed_origin') = d.origin AND
			             json_type(c.metadata, '$.managed_mime_type') = 'text' AND json_extract(c.metadata, '$.managed_mime_type') = d.mime_type AND
			             json_type(c.metadata, '$.managed_content_hash') = 'text' AND json_extract(c.metadata, '$.managed_content_hash') = d.content_hash AND
			             json_type(c.metadata, '$.managed_collection') = 'text' AND json_extract(c.metadata, '$.managed_collection') = d.collection AND
			             json_type(c.metadata, '$.managed_tags') = 'text' AND json_extract(c.metadata, '$.managed_tags') = d.tags AND
			             json_type(c.metadata, '$.managed_state') = 'text' AND json_extract(c.metadata, '$.managed_state') = d.state AND
			             NOT EXISTS (
			               SELECT 1 FROM json_each(c.metadata) AS metadata_entry
			                GROUP BY metadata_entry.key
			               HAVING COUNT(*) > 1 OR MIN(metadata_entry.type) <> 'text'
			             )
			           THEN 1 ELSE 0 END
			         ELSE 0 END)
			  FROM chunks AS c
			  JOIN managed_documents AS d ON d.source = c.source
			 WHERE c.source IN (`+placeholders+`)
			 GROUP BY c.source`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source string
			var count int
			var minSignature, maxSignature, minVectorSpaceID, maxVectorSpaceID string
			var metadataMatches int
			if err := rows.Scan(
				&source, &count, &minSignature, &maxSignature, &minVectorSpaceID,
				&maxVectorSpaceID, &metadataMatches,
			); err != nil {
				_ = rows.Close()
				return nil, err
			}
			document, ok := batchRegistry[source]
			if ok && count > 0 && count == document.chunkCount &&
				minSignature == document.sourceSignature && maxSignature == document.sourceSignature &&
				minVectorSpaceID == document.vectorSpaceID && maxVectorSpaceID == document.vectorSpaceID &&
				metadataMatches == 1 {
				registry[source] = document
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return registry, nil
}

func scanManagedRegistryDocument(scanner interface{ Scan(...any) error }, document *managedRegistryDocument) error {
	return scanner.Scan(
		&document.id, &document.source, &document.title, &document.kind,
		&document.origin, &document.mimeType, &document.contentHash,
		&document.sourceSignature, &document.vectorSpaceID, &document.chunkCount,
		&document.collection, &document.tagsJSON, &document.state,
	)
}

func looksManagedDocumentSource(source string) bool {
	if !strings.HasPrefix(source, managedSourcePrefix) {
		return false
	}
	rest := strings.TrimPrefix(source, managedSourcePrefix)
	if len(rest) < 32 || !isLowerHex(rest[:32], 32) {
		return false
	}
	return true
}

// Source shape alone is not ownership: legacy indexer callers may use the
// managed prefix and arbitrary metadata. Managed chunks carry lifecycle keys.
func chunkClaimsManagedDocument(chunk Chunk) bool {
	if !looksManagedDocumentSource(chunk.Source) {
		return false
	}
	_, hasDocumentID := chunk.Metadata["managed_document_id"]
	_, hasFreshness := chunk.Metadata["managed_freshness"]
	return hasDocumentID || hasFreshness
}

func validManagedRegistryDocument(document *managedRegistryDocument) bool {
	if !isLowerHex(document.id, 32) || !validGeneratedManagedSource(document.source, document.id) || !isLowerHex(document.contentHash, 64) {
		return false
	}
	if document.kind != string(DocumentKindText) && document.kind != string(DocumentKindFile) || document.state != string(DocumentStateIndexed) {
		return false
	}
	for _, value := range []string{document.title, document.mimeType, document.collection} {
		if !utf8.ValidString(value) || len(value) > MaxManagedMetadataBytes {
			return false
		}
	}
	if !utf8.ValidString(document.tagsJSON) {
		return false
	}
	if document.title == "" || document.mimeType == "" {
		return false
	}
	if document.kind == string(DocumentKindText) {
		if document.origin != "" {
			return false
		}
	} else if !utf8.ValidString(document.origin) || len(document.origin) > MaxManagedMetadataBytes || !filepath.IsAbs(document.origin) || filepath.Clean(document.origin) != document.origin {
		return false
	}
	if err := json.Unmarshal([]byte(document.tagsJSON), &document.tags); err != nil {
		return false
	}
	normalized, err := normalizeManagedTags(document.tags)
	return err == nil && reflect.DeepEqual(document.tags, normalized)
}

func validGeneratedManagedSource(source, id string) bool {
	prefix := managedSourcePrefix + id
	if !strings.HasPrefix(source, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(source, prefix)
	return suffix == "" || strings.HasPrefix(suffix, ".") && suffix == strings.ToLower(suffix) && !strings.Contains(suffix, "/")
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

type managedFreshnessCheck struct {
	hash string
	err  error
}

type retrievalFreshness struct {
	known    bool
	value    DocumentFreshness
	managed  bool
	document managedRegistryDocument
}

func unknownFreshness(count int) []retrievalFreshness {
	return make([]retrievalFreshness, count)
}

func trustedManagedFreshness(document managedRegistryDocument, live DocumentFreshness) retrievalFreshness {
	if document.state != string(DocumentStateIndexed) {
		return retrievalFreshness{known: true, value: DocumentFreshnessStale, managed: true, document: document}
	}
	if document.kind == string(DocumentKindText) {
		return retrievalFreshness{known: true, value: DocumentFreshnessFresh, managed: true, document: document}
	}
	if document.kind == string(DocumentKindFile) {
		return retrievalFreshness{known: true, value: live, managed: true, document: document}
	}
	return retrievalFreshness{known: true, value: DocumentFreshnessStale, managed: true, document: document}
}

const maxManagedFreshnessReads = 100

func refreshManagedScoredResults(ctx context.Context, store VectorStore, readFile func(context.Context, string) ([]byte, error), results []ScoredResult) ([]retrievalFreshness, error) {
	sqlite, ok := store.(*SQLiteStore)
	if !ok {
		return unknownFreshness(len(results)), nil
	}
	registry, registeredSources, err := managedRegistrySnapshot(ctx, sqlite, scoredResultChunks(results))
	if errors.Is(err, errManagedDocumentsTableMissing) {
		return unknownFreshness(len(results)), nil // Legacy read-only stores without managed_documents stay compatible.
	}
	if err != nil {
		return nil, err
	}
	return refreshManagedScoredResultsWithRegistryLimit(ctx, readFile, results, registry, registeredSources, maxManagedFreshnessReads)
}

func refreshManagedScoredResultsWithRegistry(ctx context.Context, readFile func(context.Context, string) ([]byte, error), results []ScoredResult, registry map[string]managedRegistryDocument) ([]retrievalFreshness, error) {
	// Apply the same bound to scoped paths so large or zero k cannot trigger
	// unbounded file I/O.
	return refreshManagedScoredResultsWithRegistryLimit(ctx, readFile, results, registry, nil, maxManagedFreshnessReads)
}

func refreshManagedScoredResultsWithRegistryLimit(ctx context.Context, readFile func(context.Context, string) ([]byte, error), results []ScoredResult, registry map[string]managedRegistryDocument, registeredSources map[string]struct{}, maxReads int) ([]retrievalFreshness, error) {
	freshness := unknownFreshness(len(results))
	cache := make(map[string]managedFreshnessCheck)
	for i := range results {
		if document, ok := registry[results[i].Chunk.Source]; ok {
			trusted, err := refreshManagedChunk(ctx, readFile, &results[i].Chunk, document, cache, maxReads)
			if err != nil {
				return nil, err
			}
			freshness[i] = trusted
		} else if _, registered := registeredSources[results[i].Chunk.Source]; registered {
			// Registry-miss managed chunks must not claim their baked freshness.
			stampManagedChunkStale(&results[i].Chunk)
			freshness[i] = retrievalFreshness{known: true, value: DocumentFreshnessStale}
		} else if chunkClaimsManagedDocument(results[i].Chunk) {
			// Preserve legacy metadata without trusting forged provenance.
			stampManagedChunkStale(&results[i].Chunk)
		}
	}
	return freshness, nil
}

// stampManagedChunkStale clones the chunk metadata and marks it stale: the
// chunk no longer corresponds to a live, matching registry document (deleted
// or reindexed between the chunk read and the registry snapshot). Cloning
// keeps shared resident-snapshot maps unmutated.
func stampManagedChunkStale(chunk *Chunk) {
	cloned := make(map[string]string, len(chunk.Metadata)+1)
	for key, value := range chunk.Metadata {
		cloned[key] = value
	}
	cloned["managed_freshness"] = string(DocumentFreshnessStale)
	chunk.Metadata = cloned
}

func refreshManagedChunk(ctx context.Context, readFile func(context.Context, string) ([]byte, error), chunk *Chunk, document managedRegistryDocument, cache map[string]managedFreshnessCheck, maxReads int) (retrievalFreshness, error) {
	if !matchesManagedRegistry(*chunk, document) {
		// Provenance drift: the registry row moved on (reindex) while this
		// chunk still carries the old content. Its baked freshness is a lie.
		stampManagedChunkStale(chunk)
		return retrievalFreshness{known: true, value: DocumentFreshnessStale}, nil
	}
	if document.kind != string(DocumentKindFile) || document.state != string(DocumentStateIndexed) {
		return trustedManagedFreshness(document, ""), nil
	}
	if err := ctx.Err(); err != nil {
		return retrievalFreshness{}, err
	}
	origin := document.origin
	check, ok := cache[origin]
	if !ok {
		if maxReads > 0 && len(cache) >= maxReads {
			return retrievalFreshness{}, fmt.Errorf("rag: managed file freshness read limit exceeded (%d); reduce results or use scoped retrieval", maxReads)
		}
		data, err := readFile(ctx, origin)
		check = managedFreshnessCheck{err: err}
		if err == nil {
			check.hash = contentHash(string(data))
		}
		cache[origin] = check
	}
	if err := ctx.Err(); err != nil {
		return retrievalFreshness{}, err
	}
	if check.err != nil {
		stampManagedChunkStale(chunk)
		return retrievalFreshness{managed: true, document: document}, nil
	}
	freshness := DocumentFreshnessStale
	if check.hash == document.contentHash {
		freshness = DocumentFreshnessFresh
	}
	meta := chunk.Metadata
	cloned := make(map[string]string, len(meta))
	for key, value := range meta {
		cloned[key] = value
	}
	cloned["managed_freshness"] = string(freshness)
	chunk.Metadata = cloned
	return trustedManagedFreshness(document, freshness), nil
}

// denseScored runs dense vector search and wraps each result as a ScoredResult
// carrying a single "semantic" signal, with RankScore == Score. It is the
// graceful fallback for stores without MultiSignalSearcher and for the
// WithVectorOnly path.
func (r *Retriever) denseScored(ctx context.Context, embedding []float64, k int) ([]ScoredResult, error) {
	results, err := r.store.Search(ctx, embedding, k)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w: %w", ErrStoreOperation, err)
	}
	return searchResultsToScored(results), nil
}

func searchResultsToScored(results []SearchResult) []ScoredResult {
	if results == nil {
		return nil
	}
	scored := make([]ScoredResult, len(results))
	for i, sr := range results {
		scored[i] = ScoredResult{
			SearchResult: sr,
			RankScore:    sr.Score,
			Signals:      map[string]float64{"semantic": sr.Score},
		}
	}
	return scored
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
//
// Each chunk's contents are rendered with stable line numbers anchored at the
// chunk's StartLine, so the model can cite exact lines and the user can
// inspect the supporting evidence. The header retains source, line range, and
// similarity attribution, and the existing max-token truncation is preserved.
func (r *Retriever) BuildContext(results []SearchResult, maxTokens int) string {
	if len(results) == 0 {
		return ""
	}

	maxChars := maxTokens * 4 // rough approximation
	var b strings.Builder
	b.WriteString("Relevant code context:\n\n")

	for _, res := range results {
		// The source sits inside a line-start block delimiter and is untrusted:
		// newlines are legal in POSIX filenames, nothing in the write path rejects
		// control characters on chunks.source, and the managed-document path takes
		// source straight from the caller. Flatten it to exactly one line or a path
		// can forge a second block with fabricated attribution. Content needs no
		// such treatment here -- numberLines' per-line prefix already blocks it.
		entry := fmt.Sprintf("--- %s (lines %d-%d, similarity: %.2f) ---\n%s\n",
			normalizeOrientationValue(res.Chunk.Source), res.Chunk.StartLine, res.Chunk.EndLine,
			res.Score, numberLines(res.Chunk.Content, res.Chunk.StartLine))

		if maxChars > 0 && b.Len()+len(entry) > maxChars {
			break
		}
		b.WriteString(entry)
	}

	return b.String()
}

// numberLines prefixes each line of content with a 1-based source line number
// starting at startLine, producing line-anchored output (e.g. "42| code").
// A single trailing newline is dropped so it does not yield a phantom empty
// numbered line. The returned string ends with a newline when non-empty.
//
// Security note: the "%d| " prefix is load-bearing, not just cosmetic — it
// stops each LF-delimited content line from forging a "--- " block header.
// Callers rendering untrusted text with other line separators must canonicalize
// them first; progressive evidenceText does so without changing BuildContext.
func numberLines(content string, startLine int) string {
	lines := strings.Split(content, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // ignore the empty tail from a trailing newline
	}

	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%d| %s\n", startLine+i, line)
	}
	return b.String()
}
