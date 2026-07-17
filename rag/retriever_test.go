package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestRetrieveManagedRegistryRejectsForgedOrphanAndMismatchedMetadata(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(ctx, "trusted.md", "trusted", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	trusted := requireManagedChunks(t, store, document.source)[0].Chunk
	trusted.ID = "trusted"
	forged := trusted
	forged.ID = "forged"
	forged.Metadata = make(map[string]string, len(trusted.Metadata))
	for key, value := range trusted.Metadata {
		forged.Metadata[key] = value
	}
	forged.Metadata["managed_kind"] = string(DocumentKindFile)
	forged.Metadata["managed_origin"] = filepath.Join(t.TempDir(), "must-not-read")
	forged.Metadata["managed_content_hash"] = contentHash("forged")
	forged.Metadata["managed_freshness"] = string(DocumentFreshnessFresh)
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, document.source, []Chunk{trusted, forged}, [][]float64{{0.9, 0.1}, {1, 0}}, document.ContentHash, document.VectorSpaceID); err != nil {
		t.Fatal(err)
	}
	orphanSource := managedSourcePrefix + strings.Repeat("a", 32) + ".md"
	orphan := forged
	orphan.ID, orphan.Source = "orphan", orphanSource
	orphan.Metadata["managed_kind"] = string(DocumentKindText)
	orphan.Metadata["managed_origin"] = ""
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, orphanSource, []Chunk{orphan}, [][]float64{{0.99, 0.01}}, "orphan", document.VectorSpaceID); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := r.RetrieveScoped(ctx, "q", 5, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil || len(scoped) != 1 || scoped[0].Chunk.ID != "trusted" {
		t.Fatalf("scoped results=%#v error=%v, want only trusted", scoped, err)
	}
	unscoped, err := r.Retrieve(ctx, "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range unscoped {
		if result.Chunk.ID == "forged" && result.Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessFresh) {
			t.Fatalf("forged freshness=%q, want unchanged (arbitrary path must not be read)", result.Chunk.Metadata["managed_freshness"])
		}
	}
}

func TestRetrieveFreshnessPropagatesCancellationAfterRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "managed.md")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.IngestFile(ctx, path, DocumentOptions{}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	r.readManagedFile = func(context.Context, string) ([]byte, error) {
		cancel()
		return []byte("content"), nil
	}
	_, err = r.Retrieve(ctx, "q", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Retrieve error=%v, want context.Canceled", err)
	}
}

func TestRetrieveLegacyStoreWithoutManagedTableSkipsRegistry(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.db.Exec(`DROP TABLE managed_documents`); err != nil {
		t.Fatal(err)
	}
	source := managedSourcePrefix + strings.Repeat("a", 32) + ".md"
	if err := store.Store(context.Background(), []Chunk{{ID: "plain", Source: source, Metadata: map[string]string{}}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Retrieve(context.Background(), "q", 1)
	if err != nil || len(got) != 1 || got[0].Chunk.ID != "plain" {
		t.Fatalf("legacy retrieval=%#v error=%v, want ordinary result", got, err)
	}
}

func TestRetrieveManagedRegistryErrorsForMalformedPresentTable(t *testing.T) {
	store := newTestStore(t)
	if _, err := store.db.Exec(`DROP TABLE managed_documents; CREATE TABLE managed_documents (source TEXT)`); err != nil {
		t.Fatal(err)
	}
	source := managedSourcePrefix + strings.Repeat("b", 32) + ".md"
	if err := store.Store(context.Background(), []Chunk{{ID: "bad", Source: source, Metadata: map[string]string{}}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Retrieve(context.Background(), "q", 1); err == nil {
		t.Fatal("Retrieve succeeded with malformed managed_documents table")
	}
}

func TestRetrieveScopedAcceptsUnixBackslashExtension(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is not a valid Windows filename character")
	}
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "notes.\\tag")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(ctx, path, DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(document.source, ".\\tag") {
		t.Fatalf("managed source=%q, want backslash extension", document.source)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RetrieveScoped(ctx, "q", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil || len(got) != 1 || got[0].Chunk.Source != document.source {
		t.Fatalf("scoped results=%#v error=%v, want backslash-extension document", got, err)
	}
}

func TestValidGeneratedManagedSourceIsPlatformIndependent(t *testing.T) {
	id := strings.Repeat("a", 32)
	for _, test := range []struct {
		name, source string
		want         bool
	}{
		{"empty suffix", managedSourcePrefix + id, true},
		{"normal extension", managedSourcePrefix + id + ".md", true},
		{"multi-dot suffix", managedSourcePrefix + id + ".foo.bar", true},
		{"unix backslash extension", managedSourcePrefix + id + ".\\tag", true},
		{"mismatched id", managedSourcePrefix + strings.Repeat("b", 32) + ".md", false},
		{"non-dot suffix", managedSourcePrefix + id + "suffix", false},
		{"uppercase suffix", managedSourcePrefix + id + ".MD", false},
		{"slash suffix", managedSourcePrefix + id + ".dir/file", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validGeneratedManagedSource(test.source, id); got != test.want {
				t.Fatalf("validGeneratedManagedSource(%q) = %v, want %v", test.source, got, test.want)
			}
		})
	}
}

func TestRetrieveScopedRejectsRegistryRelativeFileOriginWithoutReading(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(path, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(ctx, path, DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE managed_documents SET origin = 'relative.txt' WHERE id = ?`, document.ID); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	called := false
	r.readManagedFile = func(context.Context, string) ([]byte, error) { called = true; return nil, errors.New("must not read") }
	got, err := r.RetrieveScoped(ctx, "q", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil || len(got) != 0 || called {
		t.Fatalf("scoped results=%#v error=%v read=%v, want exclusion without read", got, err, called)
	}
}

func replaceManagedScopeChunk(t *testing.T, store *SQLiteStore, document Document, id string, embedding []float64) Chunk {
	t.Helper()
	chunk := requireManagedChunks(t, store, document.source)[0].Chunk
	chunk.ID = id
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), document.source, []Chunk{chunk}, [][]float64{embedding}, document.ContentHash, document.VectorSpaceID); err != nil {
		t.Fatal(err)
	}
	return chunk
}

func TestRetrieveScopedFiltersBeforeTopKAndRequiresAllTags(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	outside, err := managed.IngestText(ctx, "outside.md", "outside", DocumentOptions{Collection: "other", Tags: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	oneTag, err := managed.IngestText(ctx, "one-tag.md", "one-tag", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	match, err := managed.IngestText(ctx, "match.md", "match", DocumentOptions{Collection: "ops", Tags: []string{"alpha", "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	replaceManagedScopeChunk(t, store, outside, "outside", []float64{1, 0})
	replaceManagedScopeChunk(t, store, oneTag, "one-tag", []float64{0.99, 0.01})
	replaceManagedScopeChunk(t, store, match, "match", []float64{0.98, 0.02})
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RetrieveScoped(ctx, "q", 1, RetrievalScope{Collection: " ops ", Tags: []string{" beta ", "alpha", "beta"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Chunk.ID != "match" {
		t.Fatalf("scoped results = %#v, want only match after filtering", got)
	}
}

func TestRetrieveScopedTagsDoNotRequireACollection(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	match, err := managed.IngestText(ctx, "match.md", "match", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	miss, err := managed.IngestText(ctx, "miss.md", "miss", DocumentOptions{Collection: "other", Tags: []string{"beta"}})
	if err != nil {
		t.Fatal(err)
	}
	replaceManagedScopeChunk(t, store, match, "match", []float64{1, 0})
	replaceManagedScopeChunk(t, store, miss, "miss", []float64{0.9, 0.1})
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RetrieveScoped(ctx, "q", 1, RetrievalScope{Tags: []string{"alpha"}})
	if err != nil || len(got) != 1 || got[0].Chunk.ID != "match" {
		t.Fatalf("tag-scoped results=%#v error=%v, want match", got, err)
	}
}

func TestRetrieveScopedRejectsInvalidScopeAndCustomStoreBeforeWork(t *testing.T) {
	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	plain := &retrieverPlainStore{}
	custom, err := NewRetrieverWithEmbedder(emb, plain, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := custom.RetrieveScoped(context.Background(), "q", 1, RetrievalScope{Collection: "ops"}); err == nil || !strings.Contains(err.Error(), "SQLiteStore") {
		t.Fatalf("custom scoped retrieval error = %v, want SQLiteStore error", err)
	}
	if emb.calls != 0 || plain.searchCalls != 0 {
		t.Fatalf("custom store work: embeds=%d searches=%d, want 0/0", emb.calls, plain.searchCalls)
	}
	if _, err := custom.RetrieveScoredScoped(context.Background(), "q", 1, RetrievalScope{Collection: "ops"}, QueryContext{}); err == nil || !strings.Contains(err.Error(), "SQLiteStore") {
		t.Fatalf("custom scored retrieval error = %v, want SQLiteStore error", err)
	}
	if emb.calls != 0 || plain.searchCalls != 0 {
		t.Fatalf("custom scored store work: embeds=%d searches=%d, want 0/0", emb.calls, plain.searchCalls)
	}

	store := newTestStore(t)
	r, err := NewRetrieverWithEmbedder(emb, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.RetrieveScoped(context.Background(), "q", 1, RetrievalScope{Tags: []string{strings.Repeat("x", MaxManagedTagBytes+1)}}); err == nil {
		t.Fatal("invalid scope succeeded")
	}
	if emb.calls != 0 {
		t.Fatalf("embed calls = %d, want 0 after invalid scope", emb.calls)
	}
}

func TestRetrieveFreshnessWithoutListDoesNotWrite(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "managed.md")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	document, err := managed.IngestFile(context.Background(), path, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	var beforeState, beforeFreshness string
	if err := store.db.QueryRow(`SELECT state, freshness FROM managed_documents WHERE id = ?`, document.ID).Scan(&beforeState, &beforeFreshness); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}}, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}
	results, err := r.Retrieve(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
		t.Fatalf("results = %#v, want stale managed file", results)
	}
	if got := requireManagedChunks(t, store, document.source)[0].Chunk.Metadata["managed_freshness"]; got != string(DocumentFreshnessFresh) {
		t.Fatalf("retrieval mutated stored metadata freshness = %q, want fresh", got)
	}
	var afterState, afterFreshness string
	if err := store.db.QueryRow(`SELECT state, freshness FROM managed_documents WHERE id = ?`, document.ID).Scan(&afterState, &afterFreshness); err != nil {
		t.Fatal(err)
	}
	if beforeState != afterState || beforeFreshness != afterFreshness {
		t.Fatalf("search wrote registry: before=%s/%s after=%s/%s", beforeState, beforeFreshness, afterState, afterFreshness)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	results, err = r.Retrieve(context.Background(), "q", 5)
	if err != nil || len(results) != 1 || results[0].Chunk.Metadata["managed_freshness"] != string(DocumentFreshnessStale) {
		t.Fatalf("deleted file results=%#v error=%v, want stale", results, err)
	}
}

func TestRetrieveScopedScoredPreservesHybridContextAndRanking(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	outside, err := managed.IngestText(ctx, "outside.md", "alpha", DocumentOptions{Collection: "other", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	match, err := managed.IngestText(ctx, "match.md", "alpha", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	replaceManagedScopeChunk(t, store, outside, "outside", []float64{1, 0})
	matchChunk := replaceManagedScopeChunk(t, store, match, "match", []float64{0.9, 0.1})
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}}, store)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.RetrieveScoredScoped(ctx, "alpha", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}}, QueryContext{CurrentFile: matchChunk.Source})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Chunk.ID != "match" || got[0].Signals["structural"] == 0 {
		t.Fatalf("scoped scored results = %#v, want hybrid match with context", got)
	}
}

func TestRetrieverRetrieve(t *testing.T) {
	// Mock embed server returns a fixed vector
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.EmbedResponse{Embeddings: [][]float64{{1.0, 0.0, 0.0}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	// Pre-populate store
	chunks := []Chunk{
		{ID: "c1", Content: "relevant content", Source: "a.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c2", Content: "irrelevant content", Source: "b.go", StartLine: 1, EndLine: 3, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{0.9, 0.1, 0.0}, // similar to query
		{0.0, 0.0, 1.0}, // orthogonal to query
	}
	_ = store.Store(context.Background(), chunks, embeddings)

	retriever := NewRetriever(client, store)
	results, err := retriever.Retrieve(context.Background(), "test query", 2)
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// First result should be the more similar one
	if results[0].Chunk.ID != "c1" {
		t.Errorf("top result ID = %q, want %q", results[0].Chunk.ID, "c1")
	}
}

func TestRetrieverBuildContext(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{
			Chunk: Chunk{
				Source: "main.go", StartLine: 1, EndLine: 10,
				Content: "func main() { fmt.Println(\"hello\") }",
			},
			Score: 0.95,
		},
		{
			Chunk: Chunk{
				Source: "util.go", StartLine: 5, EndLine: 15,
				Content: "func helper() string { return \"help\" }",
			},
			Score: 0.80,
		},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "main.go") {
		t.Error("context should contain source file name")
	}
	if !strings.Contains(ctx, "0.95") {
		t.Error("context should contain similarity score")
	}
	if !strings.Contains(ctx, "func main()") {
		t.Error("context should contain code content")
	}
}

func TestRetrieverBuildContextEmpty(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)
	ctx := retriever.BuildContext(nil, 1000)
	if ctx != "" {
		t.Errorf("expected empty context, got %q", ctx)
	}
}

func TestRetrieverBuildContextTruncation(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{
			Chunk: Chunk{Source: "a.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("x", 500)},
			Score: 0.9,
		},
		{
			Chunk: Chunk{Source: "b.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("y", 500)},
			Score: 0.8,
		},
	}

	// Very small maxTokens should truncate
	ctx := retriever.BuildContext(results, 50) // 50 tokens ~= 200 chars
	if strings.Contains(ctx, "b.go") {
		t.Error("second result should be truncated due to token limit")
	}
}

func TestRetrieverBuildContextLineNumbersSingleLine(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "a.go", StartLine: 42, EndLine: 42, Content: "x := compute()"}, Score: 0.9},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "42| x := compute()") {
		t.Errorf("context missing line-anchored content; got:\n%s", ctx)
	}
	// Attribution (source, line range, score) is still present.
	if !strings.Contains(ctx, "a.go") || !strings.Contains(ctx, "lines 42-42") || !strings.Contains(ctx, "0.90") {
		t.Errorf("context missing attribution; got:\n%s", ctx)
	}
}

func TestRetrieverBuildContextLineNumbersMultiLine(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "m.go", StartLine: 10, EndLine: 12, Content: "func f() {\n\treturn 1\n}"}, Score: 0.8},
	}

	ctx := retriever.BuildContext(results, 1000)
	for _, want := range []string{"10| func f() {", "11| \treturn 1", "12| }"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("context missing %q; got:\n%s", want, ctx)
		}
	}
}

func TestRetrieverBuildContextLineNumbersTrailingNewline(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	// Content ending in a newline must not produce a phantom numbered line.
	results := []SearchResult{
		{Chunk: Chunk{Source: "t.go", StartLine: 5, EndLine: 6, Content: "a\nb\n"}, Score: 0.7},
	}

	ctx := retriever.BuildContext(results, 1000)
	if !strings.Contains(ctx, "5| a") || !strings.Contains(ctx, "6| b") {
		t.Errorf("context missing numbered lines; got:\n%s", ctx)
	}
	if strings.Contains(ctx, "7| ") {
		t.Errorf("context numbered a phantom trailing line; got:\n%s", ctx)
	}
}

func TestRetrieverBuildContextLineNumbersTruncation(t *testing.T) {
	client := ollama.NewClient()
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	retriever := NewRetriever(client, store)

	results := []SearchResult{
		{Chunk: Chunk{Source: "first.go", StartLine: 1, EndLine: 1, Content: "small"}, Score: 0.9},
		{Chunk: Chunk{Source: "second.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("y", 500)}, Score: 0.8},
	}

	// Budget fits the first numbered chunk but not the large second one.
	ctx := retriever.BuildContext(results, 40) // ~160 chars
	if !strings.Contains(ctx, "1| small") {
		t.Errorf("first chunk should be line-numbered and present; got:\n%s", ctx)
	}
	if strings.Contains(ctx, "second.go") {
		t.Error("second result should be truncated due to token limit")
	}
}

func TestRetrieverWithCustomModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.EmbedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "custom-embed" {
			t.Errorf("expected model %q, got %q", "custom-embed", req.Model)
		}
		resp := ollama.EmbedResponse{Embeddings: [][]float64{{1.0, 0.0}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	_ = store.Store(context.Background(),
		[]Chunk{{ID: "c1", Content: "test", Source: "a.go", Metadata: map[string]string{}}},
		[][]float64{{1.0, 0.0}},
	)

	retriever := NewRetriever(client, store, WithRetrieverModel("custom-embed"))
	results, err := retriever.Retrieve(context.Background(), "query", 1)
	if err != nil {
		t.Fatalf("Retrieve() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}
