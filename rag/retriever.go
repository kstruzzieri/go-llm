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
	results, err := r.retrieve(ctx, query, k)
	if err != nil {
		return nil, err
	}
	if err := refreshManagedSearchResults(ctx, r.store, r.readManagedFile, results); err != nil {
		return nil, err
	}
	return results, nil
}

// RetrieveScoped finds top-k relevant managed chunks after applying scope.
func (r *Retriever) RetrieveScoped(ctx context.Context, query string, k int, scope RetrievalScope) ([]SearchResult, error) {
	scope, err := normalizeRetrievalScope(scope)
	if err != nil {
		return nil, err
	}
	if scope.empty() {
		return r.Retrieve(ctx, query, k)
	}
	if _, ok := r.store.(*SQLiteStore); !ok {
		return nil, fmt.Errorf("rag: scoped retrieval requires SQLiteStore")
	}
	results, err := r.retrieve(ctx, query, 0)
	if err != nil {
		return nil, err
	}
	store := r.store.(*SQLiteStore)
	registry, err := managedRegistrySnapshot(ctx, store, searchResultChunks(results))
	if err != nil {
		return nil, fmt.Errorf("rag: scoped managed registry: %w", err)
	}
	results = filterSearchResults(results, scope, registry)
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	if err := refreshManagedSearchResultsWithRegistry(ctx, r.readManagedFile, results, registry); err != nil {
		return nil, err
	}
	return results, nil
}

func (r *Retriever) retrieve(ctx context.Context, query string, k int) ([]SearchResult, error) {
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

	// Prefer multi-signal (hybrid) retrieval when the store supports it and the
	// caller has not opted out: semantic, keyword, and temporal signals
	// participate in ranking. Retrieve's signature carries no editor context, so
	// the structural scorer stays inert (empty QueryContext); the temporal scorer
	// still works, dating chunks against the newest indexed row. Behavioral does
	// NOT depend on QueryContext: when a weighter is installed (SetBehavioralWeighter)
	// it is active here too, keyed by each chunk's StableKey.
	if ms, ok := r.store.(MultiSignalSearcher); ok && !r.vectorOnly {
		scored, err := ms.SearchMulti(ctx, embedding, query, k, QueryContext{})
		if err != nil {
			return nil, fmt.Errorf("rag: hybrid search: %w: %w", ErrStoreOperation, err)
		}
		if len(scored) == 0 {
			return nil, nil
		}
		// Normalize Score to semantic similarity so the SearchResult contract
		// (Score == 1 - Distance, in [0,1]) holds identically on both retrieval
		// paths. Hybrid ranking is preserved in result order, not in Score:
		// downstream BuildContext renders Score as "similarity", and callers
		// surface it as a 0..1 value, so it must not carry raw RRF magnitudes.
		results := make([]SearchResult, len(scored))
		for i, s := range scored {
			sr := s.SearchResult
			sr.Score = 1 - sr.Distance
			results[i] = sr
		}
		return results, nil
	}

	results, err := r.store.Search(ctx, embedding, k)
	if err != nil {
		return nil, fmt.Errorf("rag: search: %w: %w", ErrStoreOperation, err)
	}

	return results, nil
}

// RetrieveScored is the signal-scored retrieval surface. Like Retrieve it embeds
// the query and validates the vector space, but it returns ScoredResult with the
// per-signal breakdown preserved instead of flattening to SearchResult. On stores
// that implement MultiSignalSearcher (and when WithVectorOnly is not set) it
// returns SearchMulti's hybrid results directly; otherwise it falls back to dense
// vector search wrapped as single-signal ("semantic") scored results.
func (r *Retriever) RetrieveScored(ctx context.Context, query string, k int, qCtx QueryContext) ([]ScoredResult, error) {
	results, err := r.retrieveScored(ctx, query, k, qCtx)
	if err != nil {
		return nil, err
	}
	if err := refreshManagedScoredResults(ctx, r.store, r.readManagedFile, results); err != nil {
		return nil, err
	}
	return results, nil
}

// RetrieveScoredScoped is RetrieveScored with managed collection/tag filtering.
func (r *Retriever) RetrieveScoredScoped(ctx context.Context, query string, k int, scope RetrievalScope, qCtx QueryContext) ([]ScoredResult, error) {
	scope, err := normalizeRetrievalScope(scope)
	if err != nil {
		return nil, err
	}
	if scope.empty() {
		return r.RetrieveScored(ctx, query, k, qCtx)
	}
	if _, ok := r.store.(*SQLiteStore); !ok {
		return nil, fmt.Errorf("rag: scoped retrieval requires SQLiteStore")
	}
	results, err := r.retrieveScored(ctx, query, 0, qCtx)
	if err != nil {
		return nil, err
	}
	store := r.store.(*SQLiteStore)
	registry, err := managedRegistrySnapshot(ctx, store, scoredResultChunks(results))
	if err != nil {
		return nil, fmt.Errorf("rag: scoped managed registry: %w", err)
	}
	results = filterScoredResults(results, scope, registry)
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	if err := refreshManagedScoredResultsWithRegistry(ctx, r.readManagedFile, results, registry); err != nil {
		return nil, err
	}
	return results, nil
}

func (r *Retriever) retrieveScored(ctx context.Context, query string, k int, qCtx QueryContext) ([]ScoredResult, error) {
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

	if ms, ok := r.store.(MultiSignalSearcher); ok && !r.vectorOnly {
		scored, err := ms.SearchMulti(ctx, embedding, query, k, qCtx)
		if err != nil {
			return nil, fmt.Errorf("rag: hybrid search: %w: %w", ErrStoreOperation, err)
		}
		return scored, nil
	}

	return r.denseScored(ctx, embedding, k)
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

func filterSearchResults(results []SearchResult, scope RetrievalScope, registry map[string]managedRegistryDocument) []SearchResult {
	filtered := results[:0]
	for _, result := range results {
		if document, ok := registry[result.Chunk.Source]; ok && matchesRetrievalScope(result.Chunk, document, scope) {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func filterScoredResults(results []ScoredResult, scope RetrievalScope, registry map[string]managedRegistryDocument) []ScoredResult {
	filtered := results[:0]
	for _, result := range results {
		if document, ok := registry[result.Chunk.Source]; ok && matchesRetrievalScope(result.Chunk, document, scope) {
			filtered = append(filtered, result)
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

func searchResultChunks(results []SearchResult) []Chunk {
	chunks := make([]Chunk, len(results))
	for i := range results {
		chunks[i] = results[i].Chunk
	}
	return chunks
}

func scoredResultChunks(results []ScoredResult) []Chunk {
	chunks := make([]Chunk, len(results))
	for i := range results {
		chunks[i] = results[i].Chunk
	}
	return chunks
}

var errManagedDocumentsTableMissing = errors.New("rag: managed_documents table missing")

func managedRegistrySnapshot(ctx context.Context, store *SQLiteStore, candidates []Chunk) (map[string]managedRegistryDocument, error) {
	const batchSize = 100
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
		return nil, nil
	}
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
	registry := make(map[string]managedRegistryDocument, len(sources))
	for start := 0; start < len(sources); start += batchSize {
		end := start + batchSize
		if end > len(sources) {
			end = len(sources)
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", end-start), ",")
		args := make([]any, end-start)
		for i, source := range sources[start:end] {
			args[i] = source
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, source, title, kind, origin, mime_type, content_hash,
			       source_signature, vector_space_id, chunk_count, collection, tags, state
			  FROM managed_documents
			 WHERE source IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		batchRegistry := make(map[string]managedRegistryDocument, end-start)
		for rows.Next() {
			var document managedRegistryDocument
			if err := rows.Scan(
				&document.id, &document.source, &document.title, &document.kind,
				&document.origin, &document.mimeType, &document.contentHash,
				&document.sourceSignature, &document.vectorSpaceID, &document.chunkCount,
				&document.collection, &document.tagsJSON, &document.state,
			); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !validManagedRegistryDocument(&document) {
				continue
			}
			batchRegistry[document.source] = document
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}

		rows, err = tx.QueryContext(ctx, `
			SELECT source, COUNT(*),
			       COALESCE(MIN(source_content_hash), ''), COALESCE(MAX(source_content_hash), ''),
			       COALESCE(MIN(vector_space_id), ''), COALESCE(MAX(vector_space_id), ''),
			       COALESCE(MIN(CASE WHEN json_valid(metadata)
			                         THEN COALESCE(CAST(json_extract(metadata, '$.managed_document_id') AS TEXT), '')
			                         ELSE '' END), ''),
			       COALESCE(MAX(CASE WHEN json_valid(metadata)
			                         THEN COALESCE(CAST(json_extract(metadata, '$.managed_document_id') AS TEXT), '')
			                         ELSE '' END), '')
			  FROM chunks
			 WHERE source IN (`+placeholders+`)
			 GROUP BY source`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var source string
			var count int
			var minSignature, maxSignature, minVectorSpaceID, maxVectorSpaceID, minDocumentID, maxDocumentID string
			if err := rows.Scan(
				&source, &count, &minSignature, &maxSignature, &minVectorSpaceID,
				&maxVectorSpaceID, &minDocumentID, &maxDocumentID,
			); err != nil {
				_ = rows.Close()
				return nil, err
			}
			document, ok := batchRegistry[source]
			if ok && count > 0 && count == document.chunkCount &&
				minSignature == document.sourceSignature && maxSignature == document.sourceSignature &&
				minVectorSpaceID == document.vectorSpaceID && maxVectorSpaceID == document.vectorSpaceID &&
				minDocumentID == document.id && maxDocumentID == document.id {
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

func refreshManagedSearchResults(ctx context.Context, store VectorStore, readFile func(context.Context, string) ([]byte, error), results []SearchResult) error {
	sqlite, ok := store.(*SQLiteStore)
	if !ok {
		return nil
	}
	registry, err := managedRegistrySnapshot(ctx, sqlite, searchResultChunks(results))
	if errors.Is(err, errManagedDocumentsTableMissing) {
		return nil // Legacy read-only stores without managed_documents stay compatible.
	}
	if err != nil {
		return err
	}
	return refreshManagedSearchResultsWithRegistry(ctx, readFile, results, registry)
}

func refreshManagedSearchResultsWithRegistry(ctx context.Context, readFile func(context.Context, string) ([]byte, error), results []SearchResult, registry map[string]managedRegistryDocument) error {
	cache := make(map[string]managedFreshnessCheck)
	for i := range results {
		if document, ok := registry[results[i].Chunk.Source]; ok {
			if err := refreshManagedChunk(ctx, readFile, &results[i].Chunk, document, cache); err != nil {
				return err
			}
		}
	}
	return nil
}

func refreshManagedScoredResults(ctx context.Context, store VectorStore, readFile func(context.Context, string) ([]byte, error), results []ScoredResult) error {
	sqlite, ok := store.(*SQLiteStore)
	if !ok {
		return nil
	}
	registry, err := managedRegistrySnapshot(ctx, sqlite, scoredResultChunks(results))
	if errors.Is(err, errManagedDocumentsTableMissing) {
		return nil // Legacy read-only stores without managed_documents stay compatible.
	}
	if err != nil {
		return err
	}
	return refreshManagedScoredResultsWithRegistry(ctx, readFile, results, registry)
}

func refreshManagedScoredResultsWithRegistry(ctx context.Context, readFile func(context.Context, string) ([]byte, error), results []ScoredResult, registry map[string]managedRegistryDocument) error {
	cache := make(map[string]managedFreshnessCheck)
	for i := range results {
		if document, ok := registry[results[i].Chunk.Source]; ok {
			if err := refreshManagedChunk(ctx, readFile, &results[i].Chunk, document, cache); err != nil {
				return err
			}
		}
	}
	return nil
}

func refreshManagedChunk(ctx context.Context, readFile func(context.Context, string) ([]byte, error), chunk *Chunk, document managedRegistryDocument, cache map[string]managedFreshnessCheck) error {
	if document.kind != string(DocumentKindFile) || document.state != string(DocumentStateIndexed) || !matchesManagedRegistry(*chunk, document) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	origin := document.origin
	check, ok := cache[origin]
	if !ok {
		data, err := readFile(ctx, origin)
		check = managedFreshnessCheck{err: err}
		if err == nil {
			check.hash = contentHash(string(data))
		}
		cache[origin] = check
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	freshness := DocumentFreshnessStale
	if check.err == nil && check.hash == document.contentHash {
		freshness = DocumentFreshnessFresh
	}
	meta := chunk.Metadata
	cloned := make(map[string]string, len(meta))
	for key, value := range meta {
		cloned[key] = value
	}
	cloned["managed_freshness"] = string(freshness)
	chunk.Metadata = cloned
	return nil
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
	scored := make([]ScoredResult, len(results))
	for i, sr := range results {
		scored[i] = ScoredResult{
			SearchResult: sr,
			RankScore:    sr.Score,
			Signals:      map[string]float64{"semantic": sr.Score},
		}
	}
	return scored, nil
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
		entry := fmt.Sprintf("--- %s (lines %d-%d, similarity: %.2f) ---\n%s\n",
			res.Chunk.Source, res.Chunk.StartLine, res.Chunk.EndLine,
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
