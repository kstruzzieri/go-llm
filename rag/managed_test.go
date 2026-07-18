package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type managedTestEmbedder struct {
	vectorSpaceID string
	dimension     int
	err           error
	cancel        context.CancelFunc
}

type blockingManagedTestEmbedder struct {
	started chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (e *blockingManagedTestEmbedder) Embed(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-e.release:
	case <-ctx.Done():
		return EmbedResult{}, ctx.Err()
	}
	if e.err != nil {
		return EmbedResult{}, e.err
	}
	embeddings := make([][]float64, len(inputs))
	for i := range embeddings {
		embeddings[i] = []float64{1, float64(i + 1)}
	}
	return EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/v1"}, nil
}

func (e *managedTestEmbedder) Embed(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
	if e.cancel != nil {
		e.cancel()
		return EmbedResult{}, ctx.Err()
	}
	if e.err != nil {
		return EmbedResult{}, e.err
	}
	dimension := e.dimension
	if dimension == 0 {
		dimension = 2
	}
	embeddings := make([][]float64, len(inputs))
	for i := range inputs {
		embeddings[i] = make([]float64, dimension)
		embeddings[i][0] = 1
		if dimension > 1 {
			embeddings[i][1] = float64(i + 1)
		}
	}
	return EmbedResult{Embeddings: embeddings, VectorSpaceID: e.vectorSpaceID}, nil
}

func newManagedTestService(t *testing.T, embedder Embedder) (*ManagedSources, *Indexer, *SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	managed, idx := newManagedTestServiceOnStore(t, embedder, store)
	return managed, idx, store
}

func newManagedTestServiceOnStore(t *testing.T, embedder Embedder, store *SQLiteStore) (*ManagedSources, *Indexer) {
	t.Helper()
	idx, err := NewIndexerWithEmbedder(embedder, store, WithEmbeddingModel("test"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		return []Chunk{makeChunk(source, content, 1, strings.Count(content, "\n")+1, "")}, nil
	})
	managed, err := NewManagedSources(idx, store)
	if err != nil {
		t.Fatalf("NewManagedSources() error: %v", err)
	}
	return managed, idx
}

func requireManagedDocument(t *testing.T, managed *ManagedSources, id string) Document {
	t.Helper()
	documents, err := managed.ListDocuments(context.Background(), DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments() error: %v", err)
	}
	for _, document := range documents {
		if document.ID == id {
			return document
		}
	}
	t.Fatalf("document %q not found in %#v", id, documents)
	return Document{}
}

func requireManagedChunks(t *testing.T, store *SQLiteStore, source string) []ChunkWithEmbedding {
	t.Helper()
	chunks, err := store.GetBySource(context.Background(), source)
	if err != nil {
		t.Fatalf("GetBySource(%q) error: %v", source, err)
	}
	return chunks
}

func requireManagedStatus(t *testing.T, store *SQLiteStore, id string, state DocumentState, freshness DocumentFreshness) {
	t.Helper()
	var gotState DocumentState
	var gotFreshness DocumentFreshness
	if err := store.db.QueryRow(
		`SELECT state, freshness FROM managed_documents WHERE id = ?`, id,
	).Scan(&gotState, &gotFreshness); err != nil {
		t.Fatalf("query managed status: %v", err)
	}
	if gotState != state || gotFreshness != freshness {
		t.Fatalf("status = %s/%s, want %s/%s", gotState, gotFreshness, state, freshness)
	}
}

func requireManagedVectorSpaceID(t *testing.T, store *SQLiteStore, id, want string) {
	t.Helper()
	var got string
	if err := store.db.QueryRow(`SELECT vector_space_id FROM managed_documents WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("query managed vector space: %v", err)
	}
	if got != want {
		t.Fatalf("registry vector_space_id = %q, want %q", got, want)
	}
}

func requireManagedRevision(t *testing.T, store *SQLiteStore, id string, want int64) {
	t.Helper()
	var got int64
	if err := store.db.QueryRow(`SELECT revision FROM managed_documents WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("query managed revision: %v", err)
	}
	if got != want {
		t.Fatalf("managed revision = %d, want %d", got, want)
	}
}

func TestManagedSourcesTextRoundTripAndRepeatCreatesDistinctIDs(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()

	first, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatalf("first IngestText() error: %v", err)
	}
	second, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatalf("second IngestText() error: %v", err)
	}
	if first.ID == "" || second.ID == "" || first.ID == second.ID {
		t.Fatalf("IDs = %q and %q, want distinct non-empty IDs", first.ID, second.ID)
	}
	if first.Title != "runbook.md" || first.Kind != DocumentKindText || first.State != DocumentStateIndexed || first.Freshness != DocumentFreshnessFresh {
		t.Fatalf("first document = %#v", first)
	}
	if first.ContentHash != contentHash("restart safely") || first.SourceSignature == "" || first.VectorSpaceID != "test/v1" || first.IndexedAt == 0 {
		t.Fatalf("first provenance = %#v", first)
	}
	if first.ChunkCount != 1 {
		t.Fatalf("first ChunkCount = %d, want 1", first.ChunkCount)
	}

	documents, err := managed.ListDocuments(ctx, DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments() error: %v", err)
	}
	if len(documents) != 2 {
		t.Fatalf("documents = %#v, want 2", documents)
	}
	if documents[0].ID > documents[1].ID {
		t.Fatalf("documents not ordered by ID: %#v", documents)
	}
}

func TestManagedSourcesAcceptedLifecycleMutationsIncrementRevision(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/v1"}
	managed, _, store := newManagedTestService(t, embedder)
	document, err := managed.IngestText(context.Background(), "runbook.md", "initial", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireManagedRevision(t, store, document.ID, 2)

	if _, err := managed.ReindexDocument(context.Background(), document.ID); err != nil {
		t.Fatal(err)
	}
	requireManagedRevision(t, store, document.ID, 3)

	embedder.err = errors.New("embed unavailable")
	if _, err := managed.ReindexDocument(context.Background(), document.ID); err == nil {
		t.Fatal("ReindexDocument() succeeded despite embed failure")
	}
	requireManagedRevision(t, store, document.ID, 4)

	if err := store.DeleteBySource(context.Background(), document.source); err != nil {
		t.Fatal(err)
	}
	requireManagedRevision(t, store, document.ID, 5)
}

func TestManagedSourcesReconciliationIncrementsRevision(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(context.Background(), path, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireManagedRevision(t, store, document.ID, 2)
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.ListDocuments(context.Background(), DocumentFilter{}); err != nil {
		t.Fatal(err)
	}
	requireManagedRevision(t, store, document.ID, 3)
}

func TestManagedSourcesFileRoundTripAndStableReindexID(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "Guide.MD")
	if err := os.WriteFile(path, []byte("first version"), 0o600); err != nil {
		t.Fatalf("write first file: %v", err)
	}

	document, err := managed.IngestFile(ctx, path, DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestFile() error: %v", err)
	}
	abs, _ := filepath.Abs(path)
	if document.Kind != DocumentKindFile || document.Origin != filepath.Clean(abs) || document.Title != "Guide.MD" {
		t.Fatalf("document = %#v", document)
	}
	if !strings.HasSuffix(document.source, ".md") {
		t.Fatalf("internal source = %q, want lowercase original extension", document.source)
	}
	originalID := document.ID
	originalHash := document.ContentHash

	if err := os.WriteFile(path, []byte("second version"), 0o600); err != nil {
		t.Fatalf("write second file: %v", err)
	}
	document, err = managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatalf("ReindexDocument() error: %v", err)
	}
	if document.ID != originalID || document.ContentHash == originalHash || document.Freshness != DocumentFreshnessFresh {
		t.Fatalf("reindexed document = %#v", document)
	}
	chunks := requireManagedChunks(t, store, document.source)
	if len(chunks) != 1 || chunks[0].Chunk.Content != "second version" {
		t.Fatalf("reindexed chunks = %#v", chunks)
	}
}

func TestManagedSourcesEmptyFileRemainsFresh(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "empty.md")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(ctx, path, DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestFile() error: %v", err)
	}
	if document.State != DocumentStateIndexed || document.Freshness != DocumentFreshnessFresh {
		t.Fatalf("ingested empty document = %#v", document)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("empty document chunks = %#v, want none", chunks)
	}
	listed := requireManagedDocument(t, managed, document.ID)
	if listed.State != DocumentStateIndexed || listed.Freshness != DocumentFreshnessFresh {
		t.Fatalf("listed empty document = %#v", listed)
	}
	reindexed, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatalf("ReindexDocument() error: %v", err)
	}
	if reindexed.ID != document.ID || reindexed.State != DocumentStateIndexed || reindexed.Freshness != DocumentFreshnessFresh {
		t.Fatalf("reindexed empty document = %#v", reindexed)
	}
}

func TestManagedSourcesNonEmptyZeroChunkDocumentRemainsFresh(t *testing.T) {
	managed, idx, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	idx.chunker = chunkerFunc(func(string, string) ([]Chunk, error) { return nil, nil })
	document, err := managed.IngestText(context.Background(), "filtered.md", "non-empty but intentionally filtered", DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("zero-chunk document chunks = %#v", chunks)
	}
	listed := requireManagedDocument(t, managed, document.ID)
	if listed.State != DocumentStateIndexed || listed.Freshness != DocumentFreshnessFresh {
		t.Fatalf("listed zero-chunk document = %#v", listed)
	}
	if listed.ChunkCount != 0 {
		t.Fatalf("listed ChunkCount = %d, want 0", listed.ChunkCount)
	}
	var chunkCount int
	if err := store.db.QueryRow(`SELECT chunk_count FROM managed_documents WHERE id = ?`, document.ID).Scan(&chunkCount); err != nil {
		t.Fatalf("query committed chunk count: %v", err)
	}
	if chunkCount != 0 {
		t.Fatalf("committed chunk_count = %d, want 0", chunkCount)
	}
	// chunk_count is part of the public document JSON so consumers can tell
	// an indexed-empty document from one with content.
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"chunk_count":0`) {
		t.Fatalf("public document JSON missing chunk_count: %s", encoded)
	}
}

func TestManagedSourcesListMarksPartialChunkLossFailedAndStale(t *testing.T) {
	managed, idx, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	idx.chunker = chunkerFunc(func(source, _ string) ([]Chunk, error) {
		return []Chunk{
			makeChunk(source, "alpha", 1, 1, ""),
			makeChunk(source, "beta", 2, 2, ""),
			makeChunk(source, "gamma", 3, 3, ""),
		}, nil
	})
	document, err := managed.IngestText(context.Background(), "partial.md", "alpha\nbeta\ngamma", DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
	chunks := requireManagedChunks(t, store, document.source)
	if len(chunks) != 3 {
		t.Fatalf("ingested chunks = %#v, want 3", chunks)
	}
	result, err := store.db.Exec(`DELETE FROM chunks WHERE id = ?`, chunks[0].Chunk.ID)
	if err != nil {
		t.Fatalf("delete one chunk: %v", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("deleted rows = %d, error = %v; want 1", affected, err)
	}

	listed := requireManagedDocument(t, managed, document.ID)
	if listed.State != DocumentStateFailed || listed.Freshness != DocumentFreshnessStale {
		t.Fatalf("listed partial document = %#v", listed)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
	remaining := requireManagedChunks(t, store, document.source)
	if len(remaining) != 2 {
		t.Fatalf("remaining chunks = %#v, want 2", remaining)
	}
	for _, chunk := range remaining {
		if chunk.Chunk.Metadata["managed_state"] != string(DocumentStateFailed) || chunk.Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
			t.Fatalf("remaining chunk metadata = %#v", chunk.Chunk.Metadata)
		}
	}
}

func TestManagedSourcesIngestTextRejectsWhitespaceOnlyContent(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	if _, err := managed.IngestText(context.Background(), "doc.md", " \n\t", DocumentOptions{}); err == nil || !strings.Contains(err.Error(), "content is required") {
		t.Fatalf("IngestText(whitespace-only) error = %v, want content-required error", err)
	}
}

func TestManagedSourcesActiveIndexingVisibleAcrossServices(t *testing.T) {
	embedStarted := make(chan struct{})
	releaseEmbed := make(chan struct{})
	first, idx, store := newManagedTestService(t, EmbedderFunc(func(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
		close(embedStarted)
		select {
		case <-releaseEmbed:
		case <-ctx.Done():
			return EmbedResult{}, ctx.Err()
		}
		embeddings := make([][]float64, len(inputs))
		for i := range embeddings {
			embeddings[i] = []float64{1, float64(i + 1)}
		}
		return EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/v1"}, nil
	}))
	second, err := NewManagedSources(idx, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-releaseEmbed:
		default:
			close(releaseEmbed)
		}
	})

	ingestDone := make(chan error, 1)
	go func() {
		_, err := first.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
		ingestDone <- err
	}()
	select {
	case <-embedStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not reach blocking embedder")
	}

	documents, err := second.ListDocuments(context.Background(), DocumentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].State != DocumentStateIndexing {
		t.Fatalf("documents = %#v, want active indexing row", documents)
	}

	close(releaseEmbed)
	if err := <-ingestDone; err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
}

func TestIndexerManagedPrefixAllowsUnregisteredSource(t *testing.T) {
	_, idx, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	source := managedSourcePrefix + "notes.md"

	if err := idx.indexText(ctx, source, "content"); err != nil {
		t.Fatalf("IndexText(unregistered managed prefix) error = %v", err)
	}
	if err := idx.replaceSourceWithProvenanceIfSourceHash(ctx, source, nil, nil, "", "", idx.currentSourceSignature("content").String()); err != nil {
		t.Fatalf("replaceSourceWithProvenanceIfSourceHash(unregistered managed prefix) error = %v", err)
	}
}

func TestIndexerRejectsManagedDocumentSourceBeforeEmbedding(t *testing.T) {
	managed, idx, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	embedCalls := 0
	idx.embedder = EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
		embedCalls++
		return EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}, nil
	})

	err = idx.indexText(context.Background(), document.source, "replacement content")
	if err == nil || !strings.Contains(err.Error(), "belongs to a managed document") {
		t.Fatalf("IndexText(managed source) error = %v, want ownership error", err)
	}
	if embedCalls != 0 {
		t.Fatalf("Embed calls = %d, want 0", embedCalls)
	}
}

func TestIndexerRejectsManagedDocumentSourceBeforeIncrementalRead(t *testing.T) {
	managed, idx, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = idx.IndexFileIncremental(context.Background(), document.source)
	if err == nil || !strings.Contains(err.Error(), "belongs to a managed document") {
		t.Fatalf("IndexFileIncremental(managed source) error = %v, want ownership error before read", err)
	}
}

func TestIndexerRejectsManagedDocumentSourceBeforeFileRead(t *testing.T) {
	managed, idx, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	err = idx.IndexFile(context.Background(), document.source)
	if err == nil || !strings.Contains(err.Error(), "belongs to a managed document") {
		t.Fatalf("IndexFile(managed source) error = %v, want ownership error before read", err)
	}
}

func TestManagedSourcesReconciliationSkipsStaleSnapshotAfterReindex(t *testing.T) {
	managed, idx, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stale := requireManagedDocument(t, managed, document.ID)
	if _, err := store.db.Exec(`UPDATE managed_documents SET stored_text = ? WHERE id = ?`, "new content", document.ID); err != nil {
		t.Fatalf("update retained text: %v", err)
	}
	second, err := NewManagedSources(idx, store)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := second.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != DocumentStateIndexed || updated.ContentHash == stale.ContentHash {
		t.Fatalf("reindexed document = %#v, want updated indexed document", updated)
	}

	changed, err := managed.reconcileDocument(ctx, &stale)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("stale reconciliation mutated reindexed document")
	}
	if stale.ContentHash != updated.ContentHash {
		t.Fatalf("reconciled snapshot = %#v, want reindexed document", stale)
	}
	restored := requireManagedDocument(t, managed, document.ID)
	if restored.State != DocumentStateIndexed || restored.Freshness != DocumentFreshnessFresh || restored.ContentHash != updated.ContentHash {
		t.Fatalf("document after stale reconciliation = %#v, want reindexed document", restored)
	}
}

func TestManagedSourcesStaleReindexSuccessDoesNotReplaceNewerIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	managed, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, store)
	file := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(file, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(context.Background(), file, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	otherStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })
	blocking := &blockingManagedTestEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	stale, _ := newManagedTestServiceOnStore(t, blocking, store)
	newer, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, otherStore)

	if err := os.WriteFile(file, []byte("stale content"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := stale.ReindexDocument(context.Background(), document.ID)
		done <- err
	}()
	<-blocking.started
	if err := os.WriteFile(file, []byte("newer content"), 0o600); err != nil {
		t.Fatal(err)
	}
	newerDocument, err := newer.ReindexDocument(context.Background(), document.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	if err := <-done; !errors.Is(err, ErrDocumentChanged) {
		t.Fatalf("stale ReindexDocument() error = %v, want ErrDocumentChanged", err)
	}

	got := requireManagedDocument(t, newer, document.ID)
	if got.ContentHash != newerDocument.ContentHash || got.State != DocumentStateIndexed || got.Freshness != DocumentFreshnessFresh {
		t.Fatalf("document after stale success = %#v, want newer %#v", got, newerDocument)
	}
	chunks := requireManagedChunks(t, otherStore, document.source)
	if len(chunks) != 1 || chunks[0].Chunk.Content != "newer content" {
		t.Fatalf("chunks after stale success = %#v, want newer content", chunks)
	}
}

func TestManagedSourcesStaleReindexFailureDoesNotMarkNewerIndexFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	managed, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, store)
	file := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(file, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(context.Background(), file, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	otherStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })
	blocking := &blockingManagedTestEmbedder{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("stale embed failure"),
	}
	stale, _ := newManagedTestServiceOnStore(t, blocking, store)
	newer, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, otherStore)

	done := make(chan error, 1)
	go func() {
		_, err := stale.ReindexDocument(context.Background(), document.ID)
		done <- err
	}()
	<-blocking.started
	if err := os.WriteFile(file, []byte("newer content"), 0o600); err != nil {
		t.Fatal(err)
	}
	newerDocument, err := newer.ReindexDocument(context.Background(), document.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	if err := <-done; !errors.Is(err, ErrDocumentChanged) {
		t.Fatalf("stale failed ReindexDocument() error = %v, want ErrDocumentChanged", err)
	}

	got := requireManagedDocument(t, newer, document.ID)
	if got.ContentHash != newerDocument.ContentHash || got.State != DocumentStateIndexed || got.Freshness != DocumentFreshnessFresh || got.LastError != "" {
		t.Fatalf("document after stale failure = %#v, want newer %#v", got, newerDocument)
	}
	chunks := requireManagedChunks(t, otherStore, document.source)
	if len(chunks) != 1 || chunks[0].Chunk.Content != "newer content" || chunks[0].Chunk.Metadata["managed_state"] != string(DocumentStateIndexed) {
		t.Fatalf("chunks after stale failure = %#v, want newer indexed content", chunks)
	}
}

func TestManagedSourcesDeleteWinsConcurrentReindex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	managed, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, store)
	document, err := managed.IngestText(context.Background(), "runbook.md", "initial", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	otherStore, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherStore.Close() })
	blocking := &blockingManagedTestEmbedder{started: make(chan struct{}), release: make(chan struct{})}
	stale, _ := newManagedTestServiceOnStore(t, blocking, store)
	deleter, _ := newManagedTestServiceOnStore(t, &managedTestEmbedder{vectorSpaceID: "test/v1"}, otherStore)

	done := make(chan error, 1)
	go func() {
		_, err := stale.ReindexDocument(context.Background(), document.ID)
		done <- err
	}()
	<-blocking.started
	if err := deleter.DeleteDocument(context.Background(), document.ID); err != nil {
		t.Fatal(err)
	}
	close(blocking.release)
	if err := <-done; !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("stale ReindexDocument() error = %v, want ErrDocumentNotFound", err)
	}
	if _, err := deleter.loadDocument(context.Background(), document.ID); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("deleted registry lookup error = %v, want ErrDocumentNotFound", err)
	}
	if chunks := requireManagedChunks(t, otherStore, document.source); len(chunks) != 0 {
		t.Fatalf("deleted chunks resurrected: %#v", chunks)
	}
}

func TestManagedSourcesReconcileReadsFileBeforeWriterTransaction(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("restart safely"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(context.Background(), path, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	managed.readFile = func(context.Context, string) ([]byte, error) {
		close(readStarted)
		<-releaseRead
		return []byte("restart safely"), nil
	}
	reconcileDone := make(chan error, 1)
	go func() {
		_, err := managed.reconcileDocument(context.Background(), &document)
		reconcileDone <- err
	}()

	released := false
	reconciled := false
	defer func() {
		if !released {
			close(releaseRead)
		}
		if !reconciled {
			select {
			case err := <-reconcileDone:
				if err != nil {
					t.Errorf("reconcileDocument() error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("reconcileDocument() did not finish after file read release")
			}
		}
	}()

	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcileDocument() did not reach file read")
	}

	writerCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	tx, err := store.beginWriteTx(writerCtx)
	if err != nil {
		t.Fatalf("writer transaction was blocked by file read: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	close(releaseRead)
	released = true
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	reconciled = true
}

func TestManagedSteadyStateCommitSkipsMigrationScan(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()

	document, err := managed.IngestText(ctx, "doc.md", "hello", DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
	if cachedErr, scan := store.writeEmbeddingState(); cachedErr != nil || scan {
		t.Fatalf("writeEmbeddingState after first ingest = (%v, %v), want clean (no migration scan scheduled)", cachedErr, scan)
	}
	if _, err := managed.ReindexDocument(ctx, document.ID); err != nil {
		t.Fatalf("ReindexDocument() error: %v", err)
	}
	if cachedErr, scan := store.writeEmbeddingState(); cachedErr != nil || scan {
		t.Fatalf("writeEmbeddingState after same-space reindex = (%v, %v), want clean (no migration scan scheduled)", cachedErr, scan)
	}
}

func TestManagedListDocumentsWorksOnReadOnlyStore(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "rag.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	idx, err := NewIndexerWithEmbedder(&managedTestEmbedder{vectorSpaceID: "test/v1"}, store, WithEmbeddingModel("test"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		return []Chunk{makeChunk(source, content, 1, 1, "")}, nil
	})
	managed, err := NewManagedSources(idx, store)
	if err != nil {
		t.Fatalf("NewManagedSources() error: %v", err)
	}
	document, err := managed.IngestText(ctx, "doc.md", "hello", DocumentOptions{})
	if err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	readOnly, err := OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly() error: %v", err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	readOnlyManaged, err := NewManagedSources(nil, readOnly)
	if err != nil {
		t.Fatalf("NewManagedSources(read-only) error: %v", err)
	}
	// A consistent registry must list without needing the write transaction
	// the reconcile path takes only when something actually changed.
	documents, err := readOnlyManaged.ListDocuments(ctx, DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments(read-only) error: %v", err)
	}
	if len(documents) != 1 || documents[0].ID != document.ID || documents[0].State != DocumentStateIndexed {
		t.Fatalf("read-only documents = %#v, want the ingested indexed document", documents)
	}
}

func TestManagedListDocumentsTrimsCollectionFilter(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	if _, err := managed.IngestText(ctx, "doc.md", "hello", DocumentOptions{Collection: "ops"}); err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
	// Scoped retrieval trims the collection before matching; listing must
	// agree on the same filter value.
	documents, err := managed.ListDocuments(ctx, DocumentFilter{Collection: " ops "})
	if err != nil {
		t.Fatalf("ListDocuments() error: %v", err)
	}
	if len(documents) != 1 || documents[0].Collection != "ops" {
		t.Fatalf("documents = %#v, want the ops-collection document", documents)
	}
}

func TestManagedSourcesListFiltersAndOrdersByID(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	first, err := managed.IngestText(ctx, "one.md", "one", DocumentOptions{Collection: "ops", Tags: []string{"beta", "alpha", " beta "}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := managed.IngestText(ctx, "two.md", "two", DocumentOptions{Collection: "ops", Tags: []string{"beta"}})
	if err != nil {
		t.Fatal(err)
	}
	third, err := managed.IngestText(ctx, "three.md", "three", DocumentOptions{Collection: "policy", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE managed_documents SET state = 'failed', freshness = 'stale' WHERE id = ?`, third.ID); err != nil {
		t.Fatalf("mark filter fixture failed: %v", err)
	}

	documents, err := managed.ListDocuments(ctx, DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments() error: %v", err)
	}
	ids := []string{first.ID, second.ID, third.ID}
	sort.Strings(ids)
	if len(documents) != len(ids) {
		t.Fatalf("documents = %#v", documents)
	}
	for i := range ids {
		if documents[i].ID != ids[i] {
			t.Fatalf("documents[%d].ID = %q, want %q", i, documents[i].ID, ids[i])
		}
	}

	documents, err = managed.ListDocuments(ctx, DocumentFilter{Collection: "ops", Tags: []string{" beta ", "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].ID != first.ID {
		t.Fatalf("collection/all-tags filter = %#v, want first", documents)
	}
	documents, err = managed.ListDocuments(ctx, DocumentFilter{State: DocumentStateFailed, Freshness: DocumentFreshnessStale})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].ID != third.ID {
		t.Fatalf("state/freshness filter = %#v, want third", documents)
	}
}

func TestManagedSourcesListFiltersImmutableMetadataBeforeFileRead(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("restart safely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.IngestFile(context.Background(), path, DocumentOptions{Collection: "ops", Tags: []string{"alpha"}}); err != nil {
		t.Fatal(err)
	}
	called := false
	managed.readFile = func(context.Context, string) ([]byte, error) {
		called = true
		return nil, errors.New("filtered document must not be read")
	}
	for name, filter := range map[string]DocumentFilter{
		"collection": {Collection: "other", Tags: []string{"alpha"}},
		"all tags":   {Collection: "ops", Tags: []string{"beta"}},
	} {
		t.Run(name, func(t *testing.T) {
			called = false
			documents, err := managed.ListDocuments(context.Background(), filter)
			if err != nil {
				t.Fatal(err)
			}
			if len(documents) != 0 || called {
				t.Fatalf("filtered documents = %#v, file read = %v, want empty without read", documents, called)
			}
		})
	}
}

func TestManagedSourcesListPaginatesAfterID(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	for _, name := range []string{"one.md", "two.md", "three.md"} {
		if _, err := managed.IngestText(ctx, name, name, DocumentOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := managed.ListDocuments(ctx, DocumentFilter{Limit: 2})
	if err != nil || len(first) != 2 {
		t.Fatalf("first page = %#v, err = %v", first, err)
	}
	second, err := managed.ListDocuments(ctx, DocumentFilter{AfterID: first[1].ID, Limit: 2})
	if err != nil || len(second) != 1 || second[0].ID <= first[1].ID {
		t.Fatalf("second page = %#v, err = %v", second, err)
	}
}

func TestManagedSourcesListReturnsResumeCursorAtScanLimit(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	for i := 1; i <= maxManagedListScan+1; i++ {
		state := DocumentStateIndexed
		if i == 50 || i == maxManagedListScan+1 {
			state = DocumentStateFailed
		}
		id := fmt.Sprintf("%03d", i)
		if _, err := store.db.Exec(`
			INSERT INTO managed_documents (
				id, source, title, kind, origin, mime_type, stored_text,
				content_hash, source_signature, indexed_at, vector_space_id,
				chunk_count, collection, tags, state, freshness, last_error, created_at, updated_at
			) VALUES (?, ?, 'title', 'file', ?, 'text/plain', '', ?, 'signature', 0, '', 0, '', '[]', ?, 'fresh', '', 0, 0)`,
			id, managedSourcePrefix+id, "/fixture/"+id, contentHash("content"), state); err != nil {
			t.Fatalf("insert fixture %q: %v", id, err)
		}
	}
	readCalls := 0
	managed.readFile = func(context.Context, string) ([]byte, error) {
		readCalls++
		return []byte("content"), nil
	}

	first, err := managed.ListDocuments(context.Background(), DocumentFilter{State: DocumentStateFailed, Limit: 3})
	if !errors.Is(err, ErrManagedListScanLimit) {
		t.Fatalf("first page error = %v, want ErrManagedListScanLimit", err)
	}
	var limitErr *ManagedListScanLimitError
	if !errors.As(err, &limitErr) {
		t.Fatalf("first page error type = %T, want *ManagedListScanLimitError", err)
	}
	if len(first) != 1 || first[0].ID != "050" {
		t.Fatalf("first page = %#v, want partial document 050", first)
	}
	if limitErr.AfterID != "100" || limitErr.Scanned != maxManagedListScan {
		t.Fatalf("scan limit error = %#v, want cursor 100 after %d rows", limitErr, maxManagedListScan)
	}
	if readCalls > maxManagedListScan {
		t.Fatalf("file reads = %d, want at most %d", readCalls, maxManagedListScan)
	}

	second, err := managed.ListDocuments(context.Background(), DocumentFilter{
		State:   DocumentStateFailed,
		AfterID: limitErr.AfterID,
		Limit:   3,
	})
	if err != nil || len(second) != 1 || second[0].ID != "101" {
		t.Fatalf("resumed page = %#v, err = %v, want document 101", second, err)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("resumed page duplicated %q", second[0].ID)
	}
}

func TestManagedSourcesListExactlyAtScanLimitDoesNotReturnResumeError(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	for i := 1; i <= maxManagedListScan; i++ {
		id := fmt.Sprintf("%03d", i)
		if _, err := store.db.Exec(`
			INSERT INTO managed_documents (
				id, source, title, kind, origin, mime_type, stored_text,
				content_hash, source_signature, indexed_at, vector_space_id,
				chunk_count, collection, tags, state, freshness, last_error, created_at, updated_at
			) VALUES (?, ?, 'title', 'text', '', 'text/plain', '', 'hash', 'signature', 0, '', 0, '', '[]', 'failed', 'stale', '', 0, 0)`,
			id, managedSourcePrefix+id); err != nil {
			t.Fatalf("insert fixture %q: %v", id, err)
		}
	}

	documents, err := managed.ListDocuments(context.Background(), DocumentFilter{State: DocumentStateIndexed, Limit: 1})
	if err != nil || len(documents) != 0 {
		t.Fatalf("exact scan-limit page = %#v, err = %v, want empty success", documents, err)
	}
}

func TestManagedSourcesListDoesNotDecodeLookaheadDocument(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	for i := 1; i <= maxManagedListScan+1; i++ {
		id := fmt.Sprintf("%03d", i)
		tags := "[]"
		if i == maxManagedListScan+1 {
			tags = "{"
		}
		if _, err := store.db.Exec(`
			INSERT INTO managed_documents (
				id, source, title, kind, origin, mime_type, stored_text,
				content_hash, source_signature, indexed_at, vector_space_id,
				chunk_count, collection, tags, state, freshness, last_error, created_at, updated_at
			) VALUES (?, ?, 'title', 'text', '', 'text/plain', '', 'hash', 'signature', 0, '', 0, '', ?, 'failed', 'stale', '', 0, 0)`,
			id, managedSourcePrefix+id, tags); err != nil {
			t.Fatalf("insert fixture %q: %v", id, err)
		}
	}

	documents, err := managed.ListDocuments(context.Background(), DocumentFilter{State: DocumentStateIndexed, Limit: 1})
	if len(documents) != 0 || !errors.Is(err, ErrManagedListScanLimit) {
		t.Fatalf("documents = %#v, err = %v, want bounded empty partial result", documents, err)
	}
}

func TestManagedSourcesListPaginatesAcrossFilteredBatch(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	const filteredCount = maxManagedListScan + 4
	for i := 1; i <= filteredCount+3; i++ {
		collection := "filtered"
		if i > filteredCount {
			collection = "wanted"
		}
		id := fmt.Sprintf("%03d", i)
		if _, err := store.db.Exec(`
			INSERT INTO managed_documents (
				id, source, title, kind, origin, mime_type, stored_text,
				content_hash, source_signature, indexed_at, vector_space_id,
				chunk_count, collection, tags, state, freshness, last_error, created_at, updated_at
			) VALUES (?, ?, 'title', 'text', '', 'text/plain', 'retained text', 'hash', 'signature', 0, '', 0, ?, '[]', 'indexed', 'fresh', '', 0, 0)`,
			id, managedSourcePrefix+id, collection); err != nil {
			t.Fatalf("insert fixture %q: %v", id, err)
		}
	}

	first, err := managed.ListDocuments(context.Background(), DocumentFilter{Collection: "wanted", Limit: 2})
	if err != nil || len(first) != 2 || first[0].ID != "105" || first[1].ID != "106" {
		t.Fatalf("first page = %#v, err = %v", first, err)
	}
	if first[0].storedText != "" || first[1].storedText != "" {
		t.Fatalf("list hydrated retained text: %#v", first)
	}
	second, err := managed.ListDocuments(context.Background(), DocumentFilter{Collection: "wanted", AfterID: first[1].ID, Limit: 2})
	if err != nil || len(second) != 1 || second[0].ID != "107" {
		t.Fatalf("second page = %#v, err = %v", second, err)
	}
	empty, err := managed.ListDocuments(context.Background(), DocumentFilter{Collection: "missing", Limit: 2})
	if err != nil || len(empty) != 0 {
		t.Fatalf("all-filtered page = %#v, err = %v", empty, err)
	}
}

func TestManagedSourcesListRejectsLimitOverMaximum(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	if _, err := managed.ListDocuments(context.Background(), DocumentFilter{Limit: MaxManagedListLimit + 1}); err == nil {
		t.Fatalf("ListDocuments() accepted limit %d", MaxManagedListLimit+1)
	}
}

func TestManagedSourcesRejectsOversizeInputBeforeRegistration(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	content := strings.Repeat("x", MaxManagedDocumentBytes+1)
	if _, err := managed.IngestText(context.Background(), "large.md", content, DocumentOptions{}); err == nil {
		t.Fatalf("IngestText() accepted %d-byte content", len(content))
	}
	if documents, err := managed.ListDocuments(context.Background(), DocumentFilter{}); err != nil || len(documents) != 0 {
		t.Fatalf("documents after rejected input = %#v, err = %v", documents, err)
	}
}

func TestManagedSourcesRejectsRawOversizeTextNameBeforeTrim(t *testing.T) {
	calls := 0
	managed, _, store := newManagedTestService(t, EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		calls++
		return EmbedResult{Embeddings: make([][]float64, len(inputs)), VectorSpaceID: "test/v1"}, nil
	}))
	name := strings.Repeat(" ", MaxManagedMetadataBytes) + "name"
	if _, err := managed.IngestText(context.Background(), name, "content", DocumentOptions{}); err == nil {
		t.Fatal("IngestText() accepted raw oversized padded name")
	}
	if calls != 0 {
		t.Fatalf("Embed() calls = %d, want 0", calls)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM managed_documents`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("managed registry count = %d, err = %v, want 0", count, err)
	}
}

func TestManagedSourcesReindexRejectsInvalidRetainedTextBeforeEmbedding(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{name: "oversize", text: strings.Repeat("x", MaxManagedDocumentBytes+1)},
		{name: "invalid utf8", text: string([]byte{0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			managed, _, store := newManagedTestService(t, EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
				calls++
				embeddings := make([][]float64, len(inputs))
				for i := range embeddings {
					embeddings[i] = []float64{1, float64(i + 1)}
				}
				return EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/v1"}, nil
			}))
			document, err := managed.IngestText(context.Background(), "runbook.md", "valid", DocumentOptions{})
			if err != nil {
				t.Fatal(err)
			}
			calls = 0
			if _, err := store.db.Exec(`UPDATE managed_documents SET stored_text = ? WHERE id = ?`, tc.text, document.ID); err != nil {
				t.Fatalf("tamper retained text: %v", err)
			}

			got, err := managed.ReindexDocument(context.Background(), document.ID)
			if err == nil {
				t.Fatal("ReindexDocument() accepted invalid retained text")
			}
			if calls != 0 {
				t.Fatalf("Embed() calls = %d, want 0", calls)
			}
			if got.State != DocumentStateFailed || got.Freshness != DocumentFreshnessStale {
				t.Fatalf("document = %s/%s, want failed/stale", got.State, got.Freshness)
			}
			requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
		})
	}
}

func TestManagedSourcesRejectsOversizeMetadataAndTagsBeforeRegistration(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	tooManyTags := make([]string, MaxManagedTags+1)
	for i := range tooManyTags {
		tooManyTags[i] = fmt.Sprintf("tag-%d", i)
	}
	for name, opts := range map[string]DocumentOptions{
		"metadata": {Title: strings.Repeat("t", MaxManagedMetadataBytes+1)},
		"tags":     {Tags: tooManyTags},
		"tag":      {Tags: []string{strings.Repeat("t", MaxManagedTagBytes+1)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := managed.IngestText(context.Background(), "doc.md", "content", opts); err == nil {
				t.Fatal("IngestText() accepted oversized managed metadata")
			}
		})
	}
	if documents, err := managed.ListDocuments(context.Background(), DocumentFilter{}); err != nil || len(documents) != 0 {
		t.Fatalf("documents after rejected metadata = %#v, err = %v", documents, err)
	}
}

func TestManagedSourcesIngestFileValidatesOptionsBeforeRead(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	called := false
	managed.readFile = func(context.Context, string) ([]byte, error) {
		called = true
		return []byte("content"), nil
	}
	_, err := managed.IngestFile(context.Background(), "document.md", DocumentOptions{
		Title: strings.Repeat("x", MaxManagedMetadataBytes+1),
	})
	if err == nil {
		t.Fatal("IngestFile() accepted oversized title")
	}
	if called {
		t.Fatal("IngestFile() read file before validating options")
	}
}

func TestManagedSourcesIngestFilePropagatesCancellationToRead(t *testing.T) {
	managed, _, _ := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	started := make(chan struct{})
	managed.readFile = func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := managed.IngestFile(ctx, "document.md", DocumentOptions{})
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("IngestFile() error = %v, want context.Canceled", err)
	}
}

func TestReadManagedRegularFileRejectsCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "document.md")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readManagedRegularFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("readManagedRegularFile() error = %v, want context.Canceled", err)
	}
}

func TestNormalizeManagedTagsRejectsRawBoundsBeforeNormalization(t *testing.T) {
	tooMany := make([]string, MaxManagedTags+1)
	for i := range tooMany {
		tooMany[i] = " "
	}
	for name, tags := range map[string][]string{
		"raw count": tooMany,
		"raw bytes": {strings.Repeat(" ", MaxManagedTagBytes+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeManagedTags(tags); err == nil {
				t.Fatal("normalizeManagedTags() accepted raw bounds violation")
			}
		})
	}
}

func TestManagedSourcesDeleteIsAtomic(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "keep me", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		CREATE TRIGGER fail_managed_delete
		BEFORE DELETE ON managed_documents
		BEGIN
			SELECT RAISE(ABORT, 'forced managed delete failure');
		END;
	`); err != nil {
		t.Fatalf("create delete trigger: %v", err)
	}
	if err := managed.DeleteDocument(ctx, document.ID); err == nil {
		t.Fatal("DeleteDocument() succeeded despite trigger")
	}
	if len(requireManagedChunks(t, store, document.source)) == 0 {
		t.Fatal("failed delete removed chunks")
	}
	requireManagedStatus(t, store, document.ID, DocumentStateIndexed, DocumentFreshnessFresh)

	if _, err := store.db.Exec(`DROP TRIGGER fail_managed_delete`); err != nil {
		t.Fatalf("drop delete trigger: %v", err)
	}
	if err := managed.DeleteDocument(ctx, document.ID); err != nil {
		t.Fatalf("DeleteDocument() error: %v", err)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks after delete = %#v", chunks)
	}
	if err := managed.DeleteDocument(ctx, document.ID); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("second DeleteDocument() error = %v, want ErrDocumentNotFound", err)
	}
}

func TestManagedSourcesListMarksChangedAndMissingFilesStale(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	dir := t.TempDir()
	changedPath := filepath.Join(dir, "changed.md")
	missingPath := filepath.Join(dir, "missing.md")
	if err := os.WriteFile(changedPath, []byte("old changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(missingPath, []byte("old missing"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := managed.IngestFile(ctx, changedPath, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	missing, err := managed.IngestFile(ctx, missingPath, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changedPath, []byte("new changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}

	documents, err := managed.ListDocuments(ctx, DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments() error: %v", err)
	}
	byID := make(map[string]Document, len(documents))
	for _, document := range documents {
		byID[document.ID] = document
	}
	for _, document := range []Document{changed, missing} {
		got := byID[document.ID]
		if got.State != DocumentStateIndexed || got.Freshness != DocumentFreshnessStale {
			t.Fatalf("document %q status = %s/%s, want indexed/stale", got.ID, got.State, got.Freshness)
		}
		chunks := requireManagedChunks(t, store, got.source)
		if len(chunks) == 0 || chunks[0].Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
			t.Fatalf("document %q chunks = %#v", got.ID, chunks)
		}
	}
}

func TestManagedSourcesChunkMetadataPropagatesCollectionAndTags(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	content := "restart safely"
	document, err := managed.IngestText(ctx, "Runbook.MD", content, DocumentOptions{
		Title:      "Operations Runbook",
		MIMEType:   "text/markdown",
		Collection: "ops",
		Tags:       []string{" beta ", "alpha", "beta", ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(document.Tags, ",") != "alpha,beta" {
		t.Fatalf("normalized tags = %#v", document.Tags)
	}
	tagsJSON, _ := json.Marshal([]string{"alpha", "beta"})
	want := map[string]string{
		"managed_document_id":  document.ID,
		"managed_title":        "Operations Runbook",
		"managed_kind":         string(DocumentKindText),
		"managed_origin":       "",
		"managed_mime_type":    "text/markdown",
		"managed_content_hash": contentHash(content),
		"managed_collection":   "ops",
		"managed_tags":         string(tagsJSON),
		"managed_state":        string(DocumentStateIndexed),
		"managed_freshness":    string(DocumentFreshnessFresh),
	}
	chunks := requireManagedChunks(t, store, document.source)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %#v", chunks)
	}
	for key, value := range want {
		if chunks[0].Chunk.Metadata[key] != value {
			t.Errorf("metadata[%q] = %q, want %q; metadata=%#v", key, chunks[0].Chunk.Metadata[key], value, chunks[0].Chunk.Metadata)
		}
	}
}

func TestManagedSourcesInitialEmbedFailureIsFailedNotIndexed(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1", err: errors.New("embed unavailable")})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err == nil || document.ID == "" {
		t.Fatalf("IngestText() document=%#v error=%v, want assigned document and error", document, err)
	}
	if document.State != DocumentStateFailed || document.Freshness != DocumentFreshnessUnknown || document.LastError == "" {
		t.Fatalf("failed document = %#v", document)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessUnknown)
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks after initial failure = %#v", chunks)
	}
}

func TestManagedSourcesCanceledInitialIngestPersistsFailedState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{
		vectorSpaceID: "test/v1",
		cancel:        cancel,
	})

	document, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("IngestText() error = %v, want context.Canceled", err)
	}
	if document.State != DocumentStateFailed || document.Freshness != DocumentFreshnessUnknown {
		t.Fatalf("document = %#v, want failed/unknown", document)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessUnknown)
}

func TestManagedSourcesCanceledReindexPersistsFailedState(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/v1"}
	managed, _, store := newManagedTestService(t, embedder)
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	embedder.cancel = cancel
	failed, err := managed.ReindexDocument(ctx, document.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReindexDocument() error = %v, want context.Canceled", err)
	}
	if failed.State != DocumentStateFailed || failed.Freshness != DocumentFreshnessStale {
		t.Fatalf("document = %#v, want failed/stale", failed)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
}

func TestManagedSourcesQueuedCallHonorsCanceledContext(t *testing.T) {
	embedStarted := make(chan struct{})
	releaseEmbed := make(chan struct{})
	managed, _, _ := newManagedTestService(t, EmbedderFunc(
		func(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
			close(embedStarted)
			select {
			case <-releaseEmbed:
			case <-ctx.Done():
				return EmbedResult{}, ctx.Err()
			}
			embeddings := make([][]float64, len(inputs))
			for i := range embeddings {
				embeddings[i] = []float64{1, float64(i + 1)}
			}
			return EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/v1"}, nil
		},
	))
	defer func() {
		select {
		case <-releaseEmbed:
		default:
			close(releaseEmbed)
		}
	}()

	ingestDone := make(chan error, 1)
	go func() {
		_, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
		ingestDone <- err
	}()
	select {
	case <-embedStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not reach blocking embedder")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	listDone := make(chan error, 1)
	go func() {
		_, err := managed.ListDocuments(ctx, DocumentFilter{})
		listDone <- err
	}()
	select {
	case err := <-listDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ListDocuments() error = %v, want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("canceled ListDocuments() remained queued behind ingest")
	}

	close(releaseEmbed)
	if err := <-ingestDone; err != nil {
		t.Fatalf("IngestText() error: %v", err)
	}
}

func TestManagedSourcesRegistryFinalizeFailureRollsBackChunks(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	if _, err := store.db.Exec(`
		CREATE TRIGGER fail_managed_finalize
		BEFORE UPDATE OF state ON managed_documents
		WHEN NEW.state = 'indexed'
		BEGIN
			SELECT RAISE(ABORT, 'forced managed finalize failure');
		END;
	`); err != nil {
		t.Fatalf("create finalize trigger: %v", err)
	}
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err == nil || !strings.Contains(err.Error(), "forced managed finalize failure") {
		t.Fatalf("IngestText() error = %v", err)
	}
	if document.State != DocumentStateFailed || document.Freshness != DocumentFreshnessUnknown {
		t.Fatalf("document = %#v", document)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessUnknown)
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks committed despite finalize failure: %#v", chunks)
	}
}

func TestManagedSourcesFailedReindexPreservesOldChunksAndMarksStale(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/v1"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("old content"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(ctx, path, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	before := requireManagedChunks(t, store, document.source)
	if err := os.WriteFile(path, []byte("new content"), 0o600); err != nil {
		t.Fatal(err)
	}
	embedder.err = errors.New("embed unavailable")

	failed, err := managed.ReindexDocument(ctx, document.ID)
	if err == nil {
		t.Fatal("ReindexDocument() succeeded despite embed failure")
	}
	if failed.ID != document.ID || failed.State != DocumentStateFailed || failed.Freshness != DocumentFreshnessStale {
		t.Fatalf("failed document = %#v", failed)
	}
	if failed.ContentHash != document.ContentHash || failed.SourceSignature != document.SourceSignature || failed.VectorSpaceID != document.VectorSpaceID {
		t.Fatalf("failed provenance = %#v, want last committed provenance %#v", failed, document)
	}
	after := requireManagedChunks(t, store, document.source)
	if len(after) != len(before) || len(after) == 0 || after[0].Chunk.Content != before[0].Chunk.Content {
		t.Fatalf("chunks before=%#v after=%#v", before, after)
	}
	if after[0].Chunk.Metadata["managed_state"] != string(DocumentStateFailed) || after[0].Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
		t.Fatalf("chunk metadata after failure = %#v", after[0].Chunk.Metadata)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
}

func TestManagedSourcesAllowsSoleSourceVectorSpaceMigration(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	embedder.vectorSpaceID = "test/new"
	embedder.dimension = 3

	migrated, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ID != document.ID || migrated.VectorSpaceID != "test/new" {
		t.Fatalf("migrated = %#v", migrated)
	}
	chunks := requireManagedChunks(t, store, document.source)
	if len(chunks) != 1 || chunks[0].VectorSpaceID != "test/new" || len(chunks[0].Embedding) != 3 || chunks[0].Chunk.Content != "old content" {
		t.Fatalf("chunks after migration = %#v", chunks)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateIndexed, DocumentFreshnessFresh)
	requireManagedVectorSpaceID(t, store, document.ID, "test/new")
}

func TestManagedSourcesRejectsVectorMigrationWithIncompatibleRemainingCorpus(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "remaining.md",
		[]Chunk{makeChunk("remaining.md", "remaining content", 1, 1, "")},
		[][]float64{{1, 1}}, "remaining-hash", "test/old"); err != nil {
		t.Fatal(err)
	}
	beforeDocument := requireManagedChunks(t, store, document.source)
	beforeRemaining := requireManagedChunks(t, store, "remaining.md")
	embedder.vectorSpaceID = "test/new"
	embedder.dimension = 3

	failed, err := managed.ReindexDocument(ctx, document.ID)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("ReindexDocument() error = %v, want ErrVectorSpaceDrift", err)
	}
	if failed.VectorSpaceID != "test/old" {
		t.Fatalf("failed document = %#v, want retained vector space", failed)
	}
	afterDocument := requireManagedChunks(t, store, document.source)
	afterRemaining := requireManagedChunks(t, store, "remaining.md")
	for name, beforeAfter := range map[string]struct{ before, after []ChunkWithEmbedding }{
		"document":  {beforeDocument, afterDocument},
		"remaining": {beforeRemaining, afterRemaining},
	} {
		if len(beforeAfter.before) != len(beforeAfter.after) || len(beforeAfter.after) != 1 ||
			beforeAfter.before[0].Chunk.Content != beforeAfter.after[0].Chunk.Content ||
			beforeAfter.before[0].VectorSpaceID != beforeAfter.after[0].VectorSpaceID ||
			len(beforeAfter.before[0].Embedding) != len(beforeAfter.after[0].Embedding) {
			t.Fatalf("%s chunks before=%#v after=%#v", name, beforeAfter.before, beforeAfter.after)
		}
	}
	requireManagedVectorSpaceID(t, store, document.ID, "test/old")
}

func TestManagedSourcesRejectsMigrationWithInteriorRemainingDimensionDrift(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id        string
		dimension int
	}{
		{"remaining-first", 3},
		{"remaining-interior", 2},
		{"remaining-last", 3},
	} {
		embedding, err := encodeEmbedding(make([]float64, row.dimension))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO chunks
			(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			VALUES (?, '', ?, 1, 1, '', '{}', ?, 1, '', '', 'test/new')`, row.id, row.id+".md", embedding); err != nil {
			t.Fatal(err)
		}
	}
	before := requireManagedChunks(t, store, document.source)
	embedder.vectorSpaceID = "test/new"
	embedder.dimension = 3

	failed, err := managed.ReindexDocument(ctx, document.ID)
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("ReindexDocument() error = %v, want remaining-corpus dimension mismatch", err)
	}
	if failed.VectorSpaceID != "test/old" {
		t.Fatalf("failed document = %#v, want retained vector space", failed)
	}
	after := requireManagedChunks(t, store, document.source)
	if len(before) != len(after) || len(after) != 1 || before[0].VectorSpaceID != after[0].VectorSpaceID ||
		len(before[0].Embedding) != len(after[0].Embedding) || before[0].Chunk.Content != after[0].Chunk.Content {
		t.Fatalf("managed chunks before=%#v after=%#v", before, after)
	}
	requireManagedVectorSpaceID(t, store, document.ID, "test/old")
}

func TestManagedSourcesReopenMigratesPastStaleEmbeddingCache(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "managed-migration.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	oldEmbedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	indexer, err := NewIndexerWithEmbedder(oldEmbedder, store, WithEmbeddingModel("old"))
	if err != nil {
		t.Fatal(err)
	}
	indexer.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		return []Chunk{makeChunk(source, content, 1, 1, "")}, nil
	})
	managed, err := NewManagedSources(indexer, store)
	if err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	remainingEmbedding, err := encodeEmbedding([]float64{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO chunks
		(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
		VALUES ('remaining', 'remaining content', 'remaining.md', 1, 1, '', '{}', ?, 1, '', '', 'test/new')`, remainingEmbedding); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.writeEmbeddingErr == nil {
		t.Fatal("writeEmbeddingErr = nil, want cached mixed-dimension error")
	}
	newEmbedder := &managedTestEmbedder{vectorSpaceID: "test/new", dimension: 3}
	indexer, err = NewIndexerWithEmbedder(newEmbedder, store, WithEmbeddingModel("new"))
	if err != nil {
		t.Fatal(err)
	}
	indexer.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		return []Chunk{makeChunk(source, content, 1, 1, "")}, nil
	})
	managed, err = NewManagedSources(indexer, store)
	if err != nil {
		t.Fatal(err)
	}

	migrated, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ID != document.ID || migrated.VectorSpaceID != "test/new" {
		t.Fatalf("migrated = %#v", migrated)
	}
	chunks := requireManagedChunks(t, store, migrated.source)
	if len(chunks) != 1 || len(chunks[0].Embedding) != 3 || chunks[0].VectorSpaceID != "test/new" {
		t.Fatalf("migrated chunks = %#v", chunks)
	}
}

func TestManagedSourcesNewDocumentRejectsCorpusVectorSpaceDrift(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/new"})
	ctx := context.Background()
	legacy := []Chunk{makeChunk("legacy.md", "legacy content", 1, 1, "")}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(
		ctx, "legacy.md", legacy, [][]float64{{1, 1}}, "legacy-hash", "test/old",
	); err != nil {
		t.Fatalf("seed legacy source: %v", err)
	}

	document, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("IngestText() error = %v, want ErrVectorSpaceDrift", err)
	}
	if document.State != DocumentStateFailed || document.Freshness != DocumentFreshnessUnknown {
		t.Fatalf("document = %#v, want failed/unknown", document)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessUnknown)
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("managed chunks committed despite corpus drift: %#v", chunks)
	}
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.KnownIDs) != 1 || probe.KnownIDs[0] != "test/old" || probe.HasUnknown {
		t.Fatalf("probe = %#v, want only test/old", probe)
	}
}

func TestManagedSourcesNewDocumentRejectsKnownLegacyVectorSpaceMixture(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/new"})
	ctx := context.Background()
	if err := store.Store(
		ctx,
		[]Chunk{makeChunk("legacy.md", "legacy content", 1, 1, "")},
		[][]float64{{1, 1}},
	); err != nil {
		t.Fatalf("seed legacy source: %v", err)
	}

	document, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
		t.Fatalf("IngestText() error = %v, want ErrCorpusMixedVectorSpaces", err)
	}
	if document.State != DocumentStateFailed || document.Freshness != DocumentFreshnessUnknown {
		t.Fatalf("document = %#v, want failed/unknown", document)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessUnknown)
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("managed chunks committed into legacy corpus: %#v", chunks)
	}
}

func TestManagedSourcesAllowsVectorMigrationAfterLowLevelDelete(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBySource(ctx, document.source); err != nil {
		t.Fatalf("DeleteBySource() error: %v", err)
	}
	embedder.vectorSpaceID = "test/new"
	embedder.dimension = 3

	migrated, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ID != document.ID || migrated.VectorSpaceID != "test/new" {
		t.Fatalf("migrated = %#v", migrated)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 1 || len(chunks[0].Embedding) != 3 {
		t.Fatalf("chunks after migration = %#v", chunks)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateIndexed, DocumentFreshnessFresh)
	requireManagedVectorSpaceID(t, store, document.ID, "test/new")
}

func TestManagedSourcesAllowsVectorMigrationAfterZeroChunkTransition(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, idx, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	idx.chunker = chunkerFunc(func(string, string) ([]Chunk, error) { return nil, nil })
	zeroChunk, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatalf("zero-chunk ReindexDocument() error: %v", err)
	}
	if zeroChunk.VectorSpaceID != "test/old" {
		t.Fatalf("zero-chunk vector space = %q, want retained test/old", zeroChunk.VectorSpaceID)
	}
	requireManagedVectorSpaceID(t, store, document.ID, "test/old")

	idx.chunker = chunkerFunc(func(source, content string) ([]Chunk, error) {
		return []Chunk{makeChunk(source, content, 1, 1, "")}, nil
	})
	embedder.vectorSpaceID = "test/new"
	embedder.dimension = 3
	migrated, err := managed.ReindexDocument(ctx, document.ID)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ID != document.ID || migrated.VectorSpaceID != "test/new" {
		t.Fatalf("migrated = %#v", migrated)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 1 || len(chunks[0].Embedding) != 3 {
		t.Fatalf("chunks after migration = %#v", chunks)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateIndexed, DocumentFreshnessFresh)
	requireManagedVectorSpaceID(t, store, document.ID, "test/new")
}

func TestManagedSourcesLowLevelDeleteCannotLeaveIndexedRegistryState(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBySource(context.Background(), document.source); err != nil {
		t.Fatalf("DeleteBySource() error: %v", err)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks after low-level delete = %#v", chunks)
	}
	got := requireManagedDocument(t, managed, document.ID)
	if got.State != DocumentStateFailed || got.Freshness != DocumentFreshnessStale {
		t.Fatalf("listed document = %#v", got)
	}
}

func TestManagedSourcesDetectsSameCountLowLevelReplacement(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	replacement := []Chunk{makeChunk(document.source, "foreign content", 1, 1, "")}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(
		ctx, document.source, replacement, [][]float64{{1, 1}}, "foreign-signature", "test/v1",
	); err != nil {
		t.Fatalf("low-level replacement: %v", err)
	}

	got := requireManagedDocument(t, managed, document.ID)
	if got.State != DocumentStateFailed || got.Freshness != DocumentFreshnessStale {
		t.Fatalf("document = %#v, want failed/stale after foreign replacement", got)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
}

func TestIndexDirectoryPruneSkipsManagedSources(t *testing.T) {
	managed, idx, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook.md", "restart safely", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	idx.workspaceRoot = filepath.Clean(root)
	if errs := idx.pruneDeletedSources(context.Background(), nil); len(errs) != 0 {
		t.Fatalf("prune errors = %#v", errs)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) == 0 {
		t.Fatal("directory prune removed managed chunks")
	}
	requireManagedStatus(t, store, document.ID, DocumentStateIndexed, DocumentFreshnessFresh)
}

func TestIndexDirectoryPruneDoesNotReserveManagedSourcePrefix(t *testing.T) {
	_, idx, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	source := managedSourcePrefix + "notes.md"
	// This malformed legacy source is not a generated managed-document source,
	// so prune must still delete it normally.
	chunk := makeChunk(source, "legacy content", 1, 1, "")
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, source, []Chunk{chunk}, [][]float64{{1, 1}}, "sig", "test/v1"); err != nil {
		t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID() error: %v", err)
	}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	idx.workspaceRoot = filepath.Clean(root)

	if errs := idx.pruneDeletedSources(ctx, nil); len(errs) != 0 {
		t.Fatalf("prune errors = %#v", errs)
	}
	if chunks := requireManagedChunks(t, store, source); len(chunks) != 0 {
		t.Fatalf("legacy prefixed source survived prune: %#v", chunks)
	}
}

func TestManagedSourcesValidationAndNilIndexer(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	ctx := context.Background()
	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "blank text name", call: func() error { _, err := managed.IngestText(ctx, "  ", "content", DocumentOptions{}); return err }},
		{name: "empty named text", call: func() error { _, err := managed.IngestText(ctx, "name", "", DocumentOptions{}); return err }},
		{name: "invalid utf8 text", call: func() error {
			_, err := managed.IngestText(ctx, "name", string([]byte{0xff}), DocumentOptions{})
			return err
		}},
		{name: "blank file path", call: func() error { _, err := managed.IngestFile(ctx, "  ", DocumentOptions{}); return err }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	badFile := filepath.Join(t.TempDir(), "bad.txt")
	if err := os.WriteFile(badFile, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.IngestFile(ctx, badFile, DocumentOptions{}); err == nil {
		t.Fatal("IngestFile() accepted non-UTF-8 file")
	}

	withoutIndexer, err := NewManagedSources(nil, store)
	if err != nil {
		t.Fatalf("NewManagedSources(nil, store) error: %v", err)
	}
	if _, err := withoutIndexer.ListDocuments(ctx, DocumentFilter{}); err != nil {
		t.Fatalf("ListDocuments() without indexer error: %v", err)
	}
	if _, err := withoutIndexer.IngestText(ctx, "name", "content", DocumentOptions{}); err == nil {
		t.Fatal("IngestText() without indexer succeeded")
	}
	if _, err := NewManagedSources(nil, nil); err == nil {
		t.Fatal("NewManagedSources(nil, nil) succeeded")
	}
}
