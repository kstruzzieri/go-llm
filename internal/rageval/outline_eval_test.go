package rageval

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestBuildOutlineFixtureContract(t *testing.T) {
	const dimension = 768

	fixture, err := buildOutlineFixture(dimension)
	if err != nil {
		t.Fatalf("buildOutlineFixture: %v", err)
	}
	if len(fixture.chunks) != 1401 {
		t.Fatalf("chunks = %d, want 1401", len(fixture.chunks))
	}
	if len(fixture.embeddings) != len(fixture.chunks) {
		t.Fatalf("embeddings = %d, want %d", len(fixture.embeddings), len(fixture.chunks))
	}

	ids := make(map[string]rag.Chunk, len(fixture.chunks))
	sources := make(map[string]struct{})
	for _, chunk := range fixture.chunks {
		if chunk.ID == "" || chunk.Source == "" {
			t.Fatalf("chunk missing id or source: %#v", chunk)
		}
		if outlineIdentity(chunk) == "" {
			t.Fatalf("chunk %q has empty identity", chunk.ID)
		}
		if got := len(fixture.embeddings[chunk.ID]); got != dimension {
			t.Fatalf("chunk %q embedding dimension = %d, want %d", chunk.ID, got, dimension)
		}
		ids[chunk.ID] = chunk
		sources[chunk.Source] = struct{}{}
	}
	if len(sources) != 138 {
		t.Fatalf("sources = %d, want 138", len(sources))
	}
	if len(fixture.queries) != 20 {
		t.Fatalf("queries = %d, want 20", len(fixture.queries))
	}

	queries := make(map[string]struct{}, len(fixture.queries))
	categories := make(map[string]int)
	for _, query := range fixture.queries {
		if query.Query == "" {
			t.Fatal("query text is empty")
		}
		if query.CurrentFile == "" {
			t.Fatalf("query %q has no current-file context", query.Query)
		}
		if _, exists := queries[query.Query]; exists {
			t.Fatalf("duplicate query text %q", query.Query)
		}
		queries[query.Query] = struct{}{}
		categories[query.Category]++
		if len(query.Embedding) != dimension {
			t.Fatalf("query %q embedding dimension = %d, want %d", query.Query, len(query.Embedding), dimension)
		}
		if len(query.ExpectedIDs) != 2 || len(query.ExpectedSources) != 2 {
			t.Fatalf("query %q expected supports = (%d ids, %d sources), want 2 each", query.Query, len(query.ExpectedIDs), len(query.ExpectedSources))
		}
		for i, id := range query.ExpectedIDs {
			chunk, ok := ids[id]
			if !ok {
				t.Fatalf("query %q references unknown chunk %q", query.Query, id)
			}
			if chunk.Source != query.ExpectedSources[i] {
				t.Fatalf("query %q source for %q = %q, want %q", query.Query, id, query.ExpectedSources[i], chunk.Source)
			}
		}
		if query.CurrentFile != query.ExpectedSources[0] {
			t.Fatalf("query %q current file = %q, want %q", query.Query, query.CurrentFile, query.ExpectedSources[0])
		}
	}
	for _, category := range []string{"direct_symbol", "path_and_pairing", "outline_summary", "content_only", "distributed_support"} {
		if got := categories[category]; got != 4 {
			t.Fatalf("category %q queries = %d, want 4", category, got)
		}
	}
}

func TestOutlineIdentityUsesStableKeyThenID(t *testing.T) {
	stableA := outlineIdentity(rag.Chunk{ID: "first", StableKey: "shared"})
	stableB := outlineIdentity(rag.Chunk{ID: "second", StableKey: "shared"})
	if stableA == "" || stableA != stableB {
		t.Fatalf("equal stable keys produced identities %q and %q", stableA, stableB)
	}
	idA := outlineIdentity(rag.Chunk{ID: "first"})
	idB := outlineIdentity(rag.Chunk{ID: "second"})
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("empty-key IDs produced identities %q and %q", idA, idB)
	}
	if outlineIdentity(rag.Chunk{ID: "shared"}) == stableA {
		t.Fatal("ID identity collides with stable-key identity")
	}
}

func TestSeedOutlineStoreCreatesFileBackedCorpus(t *testing.T) {
	fixture, err := buildOutlineFixture(768)
	if err != nil {
		t.Fatalf("buildOutlineFixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "outline.db")
	if err := seedOutlineStore(context.Background(), path, fixture); err != nil {
		t.Fatalf("seedOutlineStore: %v", err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = store.Close() }()
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalChunks != 1401 || stats.TotalSources != 138 || stats.EmbeddingDim != 768 {
		t.Fatalf("stats = %#v, want 1401 chunks, 138 sources, dim 768", stats)
	}
}
