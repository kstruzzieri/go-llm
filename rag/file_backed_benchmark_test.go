package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fileBenchChunks  = 1401
	fileBenchSources = 138
	fileBenchVSID    = "benchmark/file-backed-v1"
)

type benchmarkWeights struct{}

func (benchmarkWeights) WeightsBatch(_ context.Context, keys []string) (map[string]float64, error) {
	weights := make(map[string]float64, len(keys))
	for i, key := range keys {
		weights[key] = float64(i%11) / 10
	}
	return weights, nil
}

func benchmarkVector(seed uint64, dim int) []float64 {
	vector := make([]float64, dim)
	state := seed + 0x9e3779b97f4a7c15
	for i := range vector {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		vector[i] = 2*float64(state>>11)/(1<<53) - 1
	}
	return vector
}

func benchmarkCorpus(n, dim int) ([]Chunk, [][]float64) {
	chunks := make([]Chunk, n)
	embeddings := make([][]float64, n)
	for i := range chunks {
		source := fmt.Sprintf("pkg%03d/file%03d.go", i%fileBenchSources, i%fileBenchSources)
		chunks[i] = Chunk{
			ID:        fmt.Sprintf("chunk-%04d", i),
			Content:   fmt.Sprintf("func benchmarkSymbol%04d() { /* alpha retrieval token package %03d */ }", i, i%fileBenchSources),
			Source:    source,
			StartLine: 1 + (i/fileBenchSources)*12,
			EndLine:   10 + (i/fileBenchSources)*12,
			Language:  "go",
			StableKey: fmt.Sprintf("stable-%04d", i),
			Metadata:  map[string]string{"kind": "function"},
		}
		embeddings[i] = benchmarkVector(uint64(i+1), dim)
	}
	return chunks, embeddings
}

func createFileBenchDB(tb testing.TB, n, dim int) (string, int64) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), fmt.Sprintf("rag-%d-%d.db", n, dim))
	store, err := NewSQLiteStore(path)
	if err != nil {
		tb.Fatalf("create benchmark store: %v", err)
	}
	chunks, embeddings := benchmarkCorpus(n, dim)
	tx, err := store.beginWriteTx(context.Background())
	if err != nil {
		tb.Fatalf("begin benchmark seed: %v", err)
	}
	if err := store.insertChunksTx(context.Background(), tx, chunks, embeddings, "benchmark-v1", fileBenchVSID); err != nil {
		_ = tx.Rollback()
		tb.Fatalf("seed benchmark store: %v", err)
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit benchmark seed: %v", err)
	}
	if _, err := store.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		tb.Fatalf("checkpoint benchmark store: %v", err)
	}
	if err := store.Close(); err != nil {
		tb.Fatalf("close benchmark writer: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		tb.Fatalf("stat benchmark store: %v", err)
	}
	return path, info.Size()
}

func openFileBenchStore(tb testing.TB, path string, behavioral bool) *SQLiteStore {
	tb.Helper()
	store, err := OpenSQLiteStoreReadOnly(path)
	if err != nil {
		tb.Fatalf("open benchmark store: %v", err)
	}
	if behavioral {
		store.SetBehavioralWeighter(benchmarkWeights{})
	}
	return store
}

func fileBenchDimensions(tb testing.TB) []int {
	tb.Helper()
	raw := os.Getenv("GO_LLM_RAG_FILE_BENCH_DIMS")
	if raw == "" {
		raw = "768,4096"
	}
	var dims []int
	for _, field := range strings.Split(raw, ",") {
		dim, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || dim <= 0 {
			tb.Fatalf("invalid GO_LLM_RAG_FILE_BENCH_DIMS value %q", field)
		}
		dims = append(dims, dim)
	}
	return dims
}

func reportFileBenchSize(b *testing.B, size int64) {
	b.ReportMetric(float64(size), "db-bytes")
}

func reportStages(b *testing.B, stages map[string]time.Duration) {
	for name, elapsed := range stages {
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "ns/"+name)
	}
}

func BenchmarkFileBackedRAG(b *testing.B) {
	if os.Getenv("GO_LLM_RAG_FILE_BENCH") != "1" {
		b.Skip("set GO_LLM_RAG_FILE_BENCH=1 to generate the representative file-backed corpus")
	}
	for _, dim := range fileBenchDimensions(b) {
		dim := dim
		b.Run(fmt.Sprintf("chunks=%d/dim=%d", fileBenchChunks, dim), func(b *testing.B) {
			path, dbBytes := createFileBenchDB(b, fileBenchChunks, dim)
			query := benchmarkVector(0, dim)
			queryText := "find alpha retrieval token"

			b.Run("open_read_only", func(b *testing.B) {
				b.ReportAllocs()
				reportFileBenchSize(b, dbBytes)
				for i := 0; i < b.N; i++ {
					store := openFileBenchStore(b, path, false)
					if err := store.Close(); err != nil {
						b.Fatal(err)
					}
				}
			})

			for _, hybrid := range []bool{false, true} {
				name := "dense"
				if hybrid {
					name = "hybrid_all_signals"
				}
				b.Run(name+"_cold_open_query", func(b *testing.B) {
					b.ReportAllocs()
					reportFileBenchSize(b, dbBytes)
					for i := 0; i < b.N; i++ {
						store := openFileBenchStore(b, path, hybrid)
						var err error
						if hybrid {
							_, err = store.SearchMulti(context.Background(), query, queryText, 5, QueryContext{CurrentFile: "pkg000/file000.go"})
						} else {
							_, err = store.Search(context.Background(), query, 5)
						}
						if err != nil {
							b.Fatal(err)
						}
						if err := store.Close(); err != nil {
							b.Fatal(err)
						}
					}
				})

				b.Run(name+"_warm", func(b *testing.B) {
					store := openFileBenchStore(b, path, hybrid)
					defer func() { _ = store.Close() }()
					if hybrid {
						_, err := store.SearchMulti(context.Background(), query, queryText, 5, QueryContext{CurrentFile: "pkg000/file000.go"})
						if err != nil {
							b.Fatal(err)
						}
					} else if _, err := store.Search(context.Background(), query, 5); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						var err error
						if hybrid {
							_, err = store.SearchMulti(context.Background(), query, queryText, 5, QueryContext{CurrentFile: "pkg000/file000.go"})
						} else {
							_, err = store.Search(context.Background(), query, 5)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					reportFileBenchSize(b, dbBytes)
				})

				b.Run(name+"_stage_attribution", func(b *testing.B) {
					store := openFileBenchStore(b, path, hybrid)
					defer func() { _ = store.Close() }()
					stages := make(map[string]time.Duration)
					store.recordStage = func(stage string, elapsed time.Duration) { stages[stage] += elapsed }
					// Untimed warmup, mirroring the warm sub-benchmarks; its
					// stage timings are discarded below.
					var warmErr error
					if hybrid {
						_, warmErr = store.SearchMulti(context.Background(), query, queryText, 5, QueryContext{CurrentFile: "pkg000/file000.go"})
					} else {
						_, warmErr = store.Search(context.Background(), query, 5)
					}
					if warmErr != nil {
						b.Fatal(warmErr)
					}
					clear(stages)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						var err error
						if hybrid {
							_, err = store.SearchMulti(context.Background(), query, queryText, 5, QueryContext{CurrentFile: "pkg000/file000.go"})
						} else {
							_, err = store.Search(context.Background(), query, 5)
						}
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					reportFileBenchSize(b, dbBytes)
					reportStages(b, stages)
				})
			}

			b.Run("retriever_hybrid_warm", func(b *testing.B) {
				store := openFileBenchStore(b, path, true)
				defer func() { _ = store.Close() }()
				embedder := EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
					return EmbedResult{Embeddings: [][]float64{query}, VectorSpaceID: fileBenchVSID}, nil
				})
				retriever, err := NewRetrieverWithEmbedder(embedder, store, WithRetrieverModel("benchmark"))
				if err != nil {
					b.Fatal(err)
				}
				if _, err := retriever.Retrieve(context.Background(), queryText, 5); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := retriever.Retrieve(context.Background(), queryText, 5); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				reportFileBenchSize(b, dbBytes)
			})

			b.Run("retriever_hybrid_stage_attribution", func(b *testing.B) {
				store := openFileBenchStore(b, path, true)
				defer func() { _ = store.Close() }()
				stages := make(map[string]time.Duration)
				embedder := EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
					start := time.Now()
					result := EmbedResult{Embeddings: [][]float64{query}, VectorSpaceID: fileBenchVSID}
					stages["query_embedding"] += time.Since(start)
					return result, nil
				})
				retriever, err := NewRetrieverWithEmbedder(embedder, store, WithRetrieverModel("benchmark"))
				if err != nil {
					b.Fatal(err)
				}
				store.recordStage = func(stage string, elapsed time.Duration) { stages[stage] += elapsed }
				// Untimed warmup, mirroring retriever_hybrid_warm; its stage
				// timings (including query_embedding) are discarded below.
				if _, err := retriever.Retrieve(context.Background(), queryText, 5); err != nil {
					b.Fatal(err)
				}
				clear(stages)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := retriever.Retrieve(context.Background(), queryText, 5); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				reportFileBenchSize(b, dbBytes)
				reportStages(b, stages)
			})

			b.Run("context_construction", func(b *testing.B) {
				b.StopTimer()
				store := openFileBenchStore(b, path, false)
				embedder := EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
					return EmbedResult{Embeddings: [][]float64{query}, VectorSpaceID: fileBenchVSID}, nil
				})
				retriever, err := NewRetrieverWithEmbedder(embedder, store, WithRetrieverModel("benchmark"))
				if err != nil {
					b.Fatal(err)
				}
				results, err := store.Search(context.Background(), query, 5)
				_ = store.Close()
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.StartTimer()
				for i := 0; i < b.N; i++ {
					if got := retriever.BuildContext(results, 4096); got == "" {
						b.Fatal("empty context")
					}
				}
				b.StopTimer()
				reportFileBenchSize(b, dbBytes)
			})
		})
	}
}

func TestBenchmarkVectorDeterministic(t *testing.T) {
	a := benchmarkVector(42, 4096)
	b := benchmarkVector(42, 4096)
	c := benchmarkVector(43, 4096)
	if len(a) != 4096 || len(b) != 4096 || len(c) != 4096 {
		t.Fatal("benchmark vectors must preserve the requested realistic dimension")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed differs at dimension %d", i)
		}
	}
	if a[0] == c[0] {
		t.Fatal("different seeds produced the same first component")
	}
}

func TestSQLiteStoreStageAttribution(t *testing.T) {
	store := newTestStore(t)
	seedHybridCorpus(t, store)
	store.SetBehavioralWeighter(benchmarkWeights{})
	seen := make(map[string]bool)
	store.recordStage = func(stage string, _ time.Duration) { seen[stage] = true }

	ctx := context.Background()
	if _, err := store.Search(ctx, []float64{1, 0, 0}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SearchMulti(ctx, []float64{1, 0, 0}, "alpha", 1, QueryContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProbeVectorSpaces(ctx); err != nil {
		t.Fatal(err)
	}

	for _, stage := range []string{
		"dense_scan_decode_hydrate_semantic",
		"ranking_top_k",
		"corpus_load_decode_candidate_hydration",
		"vector_dimension_validation",
		"semantic_scoring",
		"keyword_scoring",
		"temporal_scoring",
		"structural_scoring",
		"behavioral_scoring",
		"fusion_ranking_top_k",
		"vector_space_probe_validation",
	} {
		if !seen[stage] {
			t.Errorf("stage %q was not recorded", stage)
		}
	}
}
