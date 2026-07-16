package rag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type managedTestEmbedder struct {
	vectorSpaceID string
	err           error
}

func (e *managedTestEmbedder) Embed(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
	if e.err != nil {
		return EmbedResult{}, e.err
	}
	embeddings := make([][]float64, len(inputs))
	for i := range inputs {
		embeddings[i] = []float64{1, float64(i + 1)}
	}
	return EmbedResult{Embeddings: embeddings, VectorSpaceID: e.vectorSpaceID}, nil
}

func newManagedTestService(t *testing.T, embedder *managedTestEmbedder) (*ManagedSources, *Indexer, *SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
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
	return managed, idx, store
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
	var chunkCount int
	if err := store.db.QueryRow(`SELECT chunk_count FROM managed_documents WHERE id = ?`, document.ID).Scan(&chunkCount); err != nil {
		t.Fatalf("query committed chunk count: %v", err)
	}
	if chunkCount != 0 {
		t.Fatalf("committed chunk_count = %d, want 0", chunkCount)
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "chunk_count") {
		t.Fatalf("public document JSON leaked chunk_count: %s", encoded)
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

func TestManagedSourcesVectorSpaceDriftFailsClosed(t *testing.T) {
	embedder := &managedTestEmbedder{vectorSpaceID: "test/old"}
	managed, _, store := newManagedTestService(t, embedder)
	ctx := context.Background()
	document, err := managed.IngestText(ctx, "runbook.md", "old content", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	embedder.vectorSpaceID = "test/new"

	failed, err := managed.ReindexDocument(ctx, document.ID)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("ReindexDocument() error = %v, want ErrVectorSpaceDrift", err)
	}
	if failed.State != DocumentStateFailed || failed.Freshness != DocumentFreshnessStale {
		t.Fatalf("failed document = %#v", failed)
	}
	chunks := requireManagedChunks(t, store, document.source)
	if len(chunks) != 1 || chunks[0].VectorSpaceID != "test/old" || chunks[0].Chunk.Content != "old content" {
		t.Fatalf("chunks after drift = %#v", chunks)
	}
	if chunks[0].Chunk.Metadata["managed_state"] != string(DocumentStateFailed) || chunks[0].Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
		t.Fatalf("chunk metadata after drift = %#v", chunks[0].Chunk.Metadata)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
}

func TestManagedSourcesVectorSpaceDriftAfterLowLevelDeleteFailsClosed(t *testing.T) {
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

	failed, err := managed.ReindexDocument(ctx, document.ID)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("ReindexDocument() error = %v, want ErrVectorSpaceDrift", err)
	}
	if failed.State != DocumentStateFailed || failed.Freshness != DocumentFreshnessStale {
		t.Fatalf("failed document = %#v", failed)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks after rejected drift = %#v", chunks)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
	requireManagedVectorSpaceID(t, store, document.ID, "test/old")
}

func TestManagedSourcesVectorSpaceDriftAfterZeroChunkTransitionFailsClosed(t *testing.T) {
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
	failed, err := managed.ReindexDocument(ctx, document.ID)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("ReindexDocument() error = %v, want ErrVectorSpaceDrift", err)
	}
	if failed.State != DocumentStateFailed || failed.Freshness != DocumentFreshnessStale {
		t.Fatalf("failed document = %#v", failed)
	}
	if chunks := requireManagedChunks(t, store, document.source); len(chunks) != 0 {
		t.Fatalf("chunks after rejected drift = %#v", chunks)
	}
	requireManagedStatus(t, store, document.ID, DocumentStateFailed, DocumentFreshnessStale)
	requireManagedVectorSpaceID(t, store, document.ID, "test/old")
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
