package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestSQLiteStorePersistsPackedEmbeddingsAndReportsStorage(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	chunks := []Chunk{
		{ID: "a", Content: "alpha", Source: "a.go", StartLine: 1, EndLine: 1},
		{ID: "b", Content: "beta", Source: "b.go", StartLine: 1, EndLine: 1},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatalf("Store: %v", err)
	}

	rows, err := store.DB().QueryContext(ctx, `SELECT typeof(embedding), length(embedding), embedding FROM chunks ORDER BY id`)
	if err != nil {
		t.Fatalf("query embeddings: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var storageType string
		var size int
		var encoded []byte
		if err := rows.Scan(&storageType, &size, &encoded); err != nil {
			t.Fatalf("scan embedding: %v", err)
		}
		if storageType != "blob" {
			t.Fatalf("SQLite type = %q, want blob", storageType)
		}
		if want := packedEmbeddingHeaderSize + 3*4; size != want {
			t.Fatalf("embedding bytes = %d, want %d", size, want)
		}
		if _, format, err := decodeEmbedding(encoded); err != nil || format != embeddingFormatPackedFloat32 {
			t.Fatalf("decode format = %q, err = %v", format, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate embeddings: %v", err)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.EmbeddingFormat != EmbeddingFormatPackedFloat32 || stats.EmbeddingDim != 3 {
		t.Fatalf("format/dimension = %q/%d, want %q/3", stats.EmbeddingFormat, stats.EmbeddingDim, EmbeddingFormatPackedFloat32)
	}
	if want := int64(2 * (packedEmbeddingHeaderSize + 3*4)); stats.EmbeddingBytes != want {
		t.Fatalf("EmbeddingBytes = %d, want %d", stats.EmbeddingBytes, want)
	}
}

func TestSQLiteStoreReadsHomogeneousLegacyJSONAcrossLibraryPaths(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	seedLegacyEmbeddingRows(t, path, false)
	ctx := context.Background()

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	if got, err := store.Search(ctx, []float64{1, 0}, 2); err != nil || len(got) != 2 || got[0].Chunk.ID != "legacy-a" {
		t.Fatalf("Search = %#v, err = %v", got, err)
	}
	if got, err := store.SearchMulti(ctx, []float64{1, 0}, "alpha", 2, QueryContext{}); err != nil || len(got) != 2 {
		t.Fatalf("SearchMulti len = %d, err = %v", len(got), err)
	}
	if got, err := store.GetBySource(ctx, "legacy.go"); err != nil || len(got) != 2 || got[0].Embedding[0] != 1 {
		t.Fatalf("GetBySource = %#v, err = %v", got, err)
	}
	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks: %v", err)
	}
	exported := 0
	for _, err := range seq {
		if err != nil {
			t.Fatalf("ExportChunks iteration: %v", err)
		}
		exported++
	}
	if exported != 2 {
		t.Fatalf("exported rows = %d, want 2", exported)
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.EmbeddingFormat != EmbeddingFormatLegacyJSON || stats.EmbeddingDim != 2 {
		t.Fatalf("legacy format/dimension = %q/%d", stats.EmbeddingFormat, stats.EmbeddingDim)
	}

	before := countChunks(t, store)
	err = store.Store(ctx, []Chunk{{ID: "new", Content: "new", Source: "new.go"}}, [][]float64{{1, 0}})
	if err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatalf("Store error = %v, want legacy rebuild requirement", err)
	}
	if after := countChunks(t, store); after != before {
		t.Fatalf("legacy store mutated: chunks before=%d after=%d", before, after)
	}
}

func TestSQLiteSnapshotReadsHomogeneousLegacyJSON(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	seedLegacyEmbeddingRows(t, path, false)
	store, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = store.Close() }()

	got, err := store.Search(context.Background(), []float64{1, 0}, 2)
	if err != nil || len(got) != 2 || got[0].Chunk.ID != "legacy-a" {
		t.Fatalf("snapshot Search = %#v, err = %v", got, err)
	}
}

func TestSQLiteStoreRejectsMixedEmbeddingFormats(t *testing.T) {
	path := t.TempDir() + "/mixed.db"
	seedLegacyEmbeddingRows(t, path, true)
	ctx := context.Background()

	store, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = store.Close() }()
	_, err = store.Search(ctx, []float64{1, 0}, 2)
	if err == nil || !strings.Contains(err.Error(), "mixed embedding formats") {
		t.Fatalf("Search error = %v, want mixed-format error", err)
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.EmbeddingFormat != EmbeddingFormatMixed {
		t.Fatalf("EmbeddingFormat = %q, want %q", stats.EmbeddingFormat, EmbeddingFormatMixed)
	}
}

func TestSQLiteStoreClassifiesMixedBLOBFormatsAcrossAllRows(t *testing.T) {
	path := t.TempDir() + "/mixed-blob.db"
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Store(context.Background(), []Chunk{{ID: "packed", Content: "packed", Source: "packed.go"}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO chunks
		(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
		VALUES ('legacy-blob', 'legacy', 'legacy.go', 1, 1, '', '{}', ?, 1, '', '', '')`, []byte(`[0,1]`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.EmbeddingFormat != EmbeddingFormatMixed || stats.EmbeddingDim != 0 {
		t.Fatalf("format/dimension = %q/%d, want mixed/0", stats.EmbeddingFormat, stats.EmbeddingDim)
	}
	if err := store.Store(context.Background(), []Chunk{{ID: "later", Source: "later.go"}}, [][]float64{{1, 0}}); err == nil || !strings.Contains(err.Error(), "mixed") {
		t.Fatalf("Store error = %v, want mixed-format refusal", err)
	}
}

func TestSQLiteStoreRejectsLaterCorruptPackedRowOnOpen(t *testing.T) {
	path := t.TempDir() + "/corrupt-later.db"
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Store(context.Background(), []Chunk{{ID: "first", Source: "a.go"}, {ID: "second", Source: "b.go"}}, [][]float64{{1, 0}, {0, 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE chunks SET embedding = ? WHERE id = 'second'`, []byte("GLLV")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := NewSQLiteStore(path); err == nil || !strings.Contains(err.Error(), "second") {
		t.Fatalf("NewSQLiteStore error = %v, want later-row corruption", err)
	}
}

func TestSQLiteStoreClassifiesMixedPackedDimensionsAcrossAllRows(t *testing.T) {
	path := t.TempDir() + "/mixed-dimension.db"
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Store(context.Background(), []Chunk{{ID: "two", Source: "two.go"}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	three, err := encodeEmbedding([]float64{1, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO chunks
		(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
		VALUES ('three', '', 'three.go', 1, 1, '', '{}', ?, 1, '', '', '')`, three); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.EmbeddingFormat != EmbeddingFormatMixed || stats.EmbeddingDim != 0 {
		t.Fatalf("format/dimension = %q/%d, want mixed/0", stats.EmbeddingFormat, stats.EmbeddingDim)
	}
}

func TestSQLiteStoreRejectsDimensionDriftAcrossWritesWithoutMutation(t *testing.T) {
	t.Run("Store", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()
		if err := store.Store(ctx, []Chunk{{ID: "two", Source: "two.go"}}, [][]float64{{1, 0}}); err != nil {
			t.Fatal(err)
		}
		if err := store.Store(ctx, []Chunk{{ID: "three", Source: "three.go"}}, [][]float64{{1, 0, 0}}); err == nil || !strings.Contains(err.Error(), "dimension") {
			t.Fatalf("Store error = %v, want dimension mismatch", err)
		}
		if got := countChunks(t, store); got != 1 {
			t.Fatalf("chunks = %d, want original row only", got)
		}
	})

	t.Run("ReplaceSource", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()
		original := Chunk{ID: "original", Content: "original", Source: "a.go"}
		if err := store.Store(ctx, []Chunk{original}, [][]float64{{1, 0}}); err != nil {
			t.Fatal(err)
		}
		err := store.ReplaceSource(ctx, "a.go", []Chunk{{ID: "replacement", Source: "a.go"}}, [][]float64{{1, 0, 0}})
		if err == nil || !strings.Contains(err.Error(), "dimension") {
			t.Fatalf("ReplaceSource error = %v, want dimension mismatch", err)
		}
		got, err := store.GetBySource(ctx, "a.go")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Chunk.ID != original.ID {
			t.Fatalf("source after rejected replace = %+v", got)
		}
	})
}

func TestSQLiteStoreRejectsDimensionDriftAcrossHandles(t *testing.T) {
	path := t.TempDir() + "/multi-handle.db"
	first, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	ctx := context.Background()
	if err := first.Store(ctx, []Chunk{{ID: "two", Source: "two.go"}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := second.Store(ctx, []Chunk{{ID: "three", Source: "three.go"}}, [][]float64{{1, 0, 0}}); err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("second handle Store error = %v, want dimension mismatch", err)
	}
	if got := countChunks(t, first); got != 1 {
		t.Fatalf("chunks = %d, want original row only", got)
	}
}

func TestSQLiteStoreAllowsNewDimensionAfterDeletingLastRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.Store(ctx, []Chunk{{ID: "two", Source: "old.go"}}, [][]float64{{1, 0}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteBySource(ctx, "old.go"); err != nil {
		t.Fatal(err)
	}
	if err := store.Store(ctx, []Chunk{{ID: "three", Source: "new.go"}}, [][]float64{{1, 0, 0}}); err != nil {
		t.Fatalf("Store after emptying corpus: %v", err)
	}
	got, err := store.GetBySource(ctx, "new.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Embedding) != 3 {
		t.Fatalf("new corpus = %+v", got)
	}
}

func TestPackedFloat32RetrievalQualityAgainstLegacyJSON(t *testing.T) {
	const (
		dimension = 4096
		count     = 64
		k         = 10
	)
	ctx := context.Background()
	packed := newTestStore(t)
	legacy := newTestStore(t)
	chunks := make([]Chunk, count)
	embeddings := make([][]float64, count)
	for i := range chunks {
		chunks[i] = Chunk{
			ID: fmt.Sprintf("chunk-%03d", i), Content: "alpha retrieval token",
			Source: fmt.Sprintf("source-%03d.go", i), StartLine: 1, EndLine: 1,
		}
		embeddings[i] = benchmarkVector(uint64(i+1), dimension)
	}
	if err := packed.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("packed Store: %v", err)
	}
	tx, err := legacy.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, chunk := range chunks {
		encoded, err := json.Marshal(embeddings[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks
			(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			VALUES (?, ?, ?, ?, ?, '', '{}', ?, 1, '', '', '')`,
			chunk.ID, chunk.Content, chunk.Source, chunk.StartLine, chunk.EndLine, string(encoded)); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	query := benchmarkVector(0, dimension)
	packedDense, err := packed.Search(ctx, query, k)
	if err != nil {
		t.Fatal(err)
	}
	legacyDense, err := legacy.Search(ctx, query, k)
	if err != nil {
		t.Fatal(err)
	}
	maxDenseDelta := assertPackedQuality(t, packedDense, legacyDense)

	packedHybrid, err := packed.SearchMulti(ctx, query, "alpha retrieval token", k, QueryContext{})
	if err != nil {
		t.Fatal(err)
	}
	legacyHybrid, err := legacy.SearchMulti(ctx, query, "alpha retrieval token", k, QueryContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(packedHybrid) != len(legacyHybrid) {
		t.Fatalf("hybrid result lengths = %d/%d", len(packedHybrid), len(legacyHybrid))
	}
	var maxHybridDelta float64
	for i := range packedHybrid {
		if packedHybrid[i].Chunk.ID != legacyHybrid[i].Chunk.ID {
			t.Fatalf("hybrid rank %d ID = %q, want legacy %q", i, packedHybrid[i].Chunk.ID, legacyHybrid[i].Chunk.ID)
		}
		delta := math.Abs(packedHybrid[i].Score - legacyHybrid[i].Score)
		maxHybridDelta = max(maxHybridDelta, delta)
		if delta > 1e-6 {
			t.Fatalf("hybrid rank %d semantic delta = %g, want <= 1e-6", i, delta)
		}
		if packedHybrid[i].RankScore != legacyHybrid[i].RankScore {
			t.Fatalf("hybrid rank %d fused score = %g, want legacy %g", i, packedHybrid[i].RankScore, legacyHybrid[i].RankScore)
		}
	}
	t.Logf("maximum semantic score delta: dense=%g hybrid=%g", maxDenseDelta, maxHybridDelta)
}

func assertPackedQuality(t *testing.T, packed, legacy []SearchResult) float64 {
	t.Helper()
	if len(packed) != len(legacy) {
		t.Fatalf("result lengths = %d/%d", len(packed), len(legacy))
	}
	var maximumDelta float64
	for i := range packed {
		if packed[i].Chunk.ID != legacy[i].Chunk.ID {
			t.Fatalf("dense rank %d ID = %q, want legacy %q", i, packed[i].Chunk.ID, legacy[i].Chunk.ID)
		}
		delta := math.Abs(packed[i].Score - legacy[i].Score)
		maximumDelta = max(maximumDelta, delta)
		if delta > 1e-6 {
			t.Fatalf("dense rank %d semantic delta = %g, want <= 1e-6", i, delta)
		}
		if packed[i].Distance != 1-packed[i].Score {
			t.Fatalf("dense rank %d distance invariant = %g, score = %g", i, packed[i].Distance, packed[i].Score)
		}
	}
	return maximumDelta
}

func seedLegacyEmbeddingRows(t *testing.T, path string, mixed bool) {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore seed: %v", err)
	}
	if mixed {
		if err := store.Store(context.Background(), []Chunk{{ID: "packed", Content: "packed", Source: "packed.go"}}, [][]float64{{0, 1}}); err != nil {
			t.Fatalf("Store packed seed: %v", err)
		}
	}
	_, err = store.DB().Exec(`INSERT INTO chunks
		(id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
		VALUES
		('legacy-a', 'alpha', 'legacy.go', 1, 1, 'go', '{}', '[1,0]', 1, '', '', ''),
		('legacy-b', 'beta', 'legacy.go', 2, 2, 'go', '{}', '[0,1]', 1, '', '', '')`)
	if err != nil {
		t.Fatalf("insert legacy rows: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}
}

func countChunks(t *testing.T, store *SQLiteStore) int {
	t.Helper()
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&count); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return count
}
