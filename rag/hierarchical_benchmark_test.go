package rag

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"
)

const (
	hierarchicalBenchmarkK              = 5
	hierarchicalBenchmarkCandidateLimit = 50
)

type hierarchicalBenchmarkQuery struct {
	label        string
	concentrated bool
	index        int
	targets      map[string]int
}

type hierarchicalBenchmarkFixture struct {
	chunks     []Chunk
	querySets  map[string][]hierarchicalBenchmarkQuery
	queryOrder []hierarchicalBenchmarkQuery
}

func buildHierarchicalBenchmarkFixture(size int) hierarchicalBenchmarkFixture {
	fixture := hierarchicalBenchmarkFixture{
		chunks: make([]Chunk, size),
		querySets: map[string][]hierarchicalBenchmarkQuery{
			"concentrated": nil,
			"distributed":  nil,
		},
	}
	for i := range fixture.chunks {
		fixture.chunks[i] = Chunk{
			ID:        fmt.Sprintf("chunk-%05d", i),
			Content:   "ordinary benchmark corpus text",
			Source:    fmt.Sprintf("/work/distractors/pkg%03d/file%05d.go", i%32, i),
			StartLine: i + 1,
			EndLine:   i + 1,
			Language:  "go",
			Metadata:  map[string]string{},
		}
	}

	nextTarget := 0
	for _, set := range []string{"concentrated", "distributed"} {
		for queryNumber := 0; queryNumber < 3; queryNumber++ {
			query := hierarchicalBenchmarkQuery{
				label:        fmt.Sprintf("%s-%d", set, queryNumber),
				concentrated: set == "concentrated",
				index:        len(fixture.queryOrder),
				targets:      make(map[string]int, hierarchicalBenchmarkK),
			}
			for rank := 0; rank < hierarchicalBenchmarkK; rank++ {
				chunk := &fixture.chunks[nextTarget]
				chunk.Content = "relevant " + query.label
				if query.concentrated {
					chunk.Source = fmt.Sprintf("/work/targets/%s/file%d.go", query.label, rank)
				} else {
					chunk.Source = fmt.Sprintf("/work/targets/%s/group%d/file.go", query.label, rank)
				}
				chunk.Metadata = map[string]string{"benchmark_target": query.label}
				query.targets[chunk.ID] = rank
				nextTarget++
			}
			fixture.querySets[set] = append(fixture.querySets[set], query)
			fixture.queryOrder = append(fixture.queryOrder, query)
		}
	}
	return fixture
}

func TestHierarchicalBenchmarkFixtures(t *testing.T) {
	for _, size := range []int{100, 1_000, 10_000} {
		fixture := buildHierarchicalBenchmarkFixture(size)
		if len(fixture.chunks) != size {
			t.Fatalf("size %d: chunks=%d", size, len(fixture.chunks))
		}

		chunks := make(map[string]Chunk, len(fixture.chunks))
		for _, chunk := range fixture.chunks {
			chunks[chunk.ID] = chunk
		}
		for set, queries := range fixture.querySets {
			if len(queries) == 0 {
				t.Fatalf("size %d: %s query set is empty", size, set)
			}
			for _, query := range queries {
				if len(query.targets) != hierarchicalBenchmarkK {
					t.Fatalf("size %d: %s targets=%d", size, query.label, len(query.targets))
				}
				for target := range query.targets {
					chunk, ok := chunks[target]
					if !ok || chunk.Metadata["benchmark_target"] != query.label {
						t.Fatalf("size %d: %s target %s has wrong label", size, query.label, target)
					}
				}
			}
		}
	}
}

type hierarchicalBenchmarkEmbedder struct {
	queries map[string]int
}

func (e hierarchicalBenchmarkEmbedder) Embed(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
	if len(inputs) == 0 {
		return EmbedResult{}, nil
	}
	embeddings := make([][]float64, len(inputs))
	for i, input := range inputs {
		if err := ctx.Err(); err != nil {
			return EmbedResult{}, err
		}
		query, ok := e.queries[input]
		if !ok {
			return EmbedResult{}, fmt.Errorf("unknown benchmark query %q", input)
		}
		embeddings[i] = []float64{float64(query)}
	}
	return EmbedResult{Embeddings: embeddings}, nil
}

type hierarchicalBenchmarkStore struct {
	fixture *hierarchicalBenchmarkFixture
}

func (*hierarchicalBenchmarkStore) Store(context.Context, []Chunk, [][]float64) error { return nil }
func (*hierarchicalBenchmarkStore) DeleteBySource(context.Context, string) error      { return nil }
func (s *hierarchicalBenchmarkStore) Stats(context.Context) (StoreStats, error) {
	return StoreStats{TotalChunks: len(s.fixture.chunks), EmbeddingDim: 1}, nil
}
func (*hierarchicalBenchmarkStore) Close() error { return nil }

type hierarchicalBenchmarkRanked struct {
	chunk      Chunk
	score      float64
	rankScore  float64
	isRelevant bool
}

func (s *hierarchicalBenchmarkStore) ranked(ctx context.Context, queryIndex int, hybrid bool) ([]hierarchicalBenchmarkRanked, error) {
	if queryIndex < 0 || queryIndex >= len(s.fixture.queryOrder) {
		return nil, fmt.Errorf("unknown benchmark query index %d", queryIndex)
	}
	query := s.fixture.queryOrder[queryIndex]
	ranked := make([]hierarchicalBenchmarkRanked, len(s.fixture.chunks))
	for i, chunk := range s.fixture.chunks {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		targetRank, relevant := query.targets[chunk.ID]
		score := 0.2 + float64((i+query.index)%100)/10_000
		if relevant {
			if query.concentrated && targetRank > 0 {
				score = 0.78 - float64(targetRank)/1_000
			} else {
				score = 0.999 - float64(targetRank)/200
			}
		} else if hardRank := (i*37 + query.index*101) % len(s.fixture.chunks); hardRank < 30 {
			score = 0.95 - float64(hardRank)/250
		}
		rankScore := score
		if hybrid && relevant {
			rankScore += 0.01
		}
		ranked[i] = hierarchicalBenchmarkRanked{chunk: chunk, score: score, rankScore: rankScore, isRelevant: relevant}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].rankScore != ranked[j].rankScore {
			return ranked[i].rankScore > ranked[j].rankScore
		}
		return ranked[i].chunk.ID < ranked[j].chunk.ID
	})
	return ranked, nil
}

func benchmarkQueryIndex(embedding []float64) (int, error) {
	if len(embedding) != 1 || embedding[0] != float64(int(embedding[0])) {
		return 0, fmt.Errorf("invalid benchmark embedding %v", embedding)
	}
	return int(embedding[0]), nil
}

func (s *hierarchicalBenchmarkStore) Search(ctx context.Context, embedding []float64, k int) ([]SearchResult, error) {
	query, err := benchmarkQueryIndex(embedding)
	if err != nil {
		return nil, err
	}
	ranked, err := s.ranked(ctx, query, false)
	if err != nil {
		return nil, err
	}
	if k > 0 && k < len(ranked) {
		ranked = ranked[:k]
	}
	results := make([]SearchResult, len(ranked))
	for i, result := range ranked {
		results[i] = SearchResult{Chunk: result.chunk, Score: result.score, Distance: 1 - result.score}
	}
	return results, nil
}

func (s *hierarchicalBenchmarkStore) SearchMulti(ctx context.Context, embedding []float64, _ string, k int, _ QueryContext) ([]ScoredResult, error) {
	query, err := benchmarkQueryIndex(embedding)
	if err != nil {
		return nil, err
	}
	ranked, err := s.ranked(ctx, query, true)
	if err != nil {
		return nil, err
	}
	if k > 0 && k < len(ranked) {
		ranked = ranked[:k]
	}
	results := make([]ScoredResult, len(ranked))
	for i, result := range ranked {
		results[i] = ScoredResult{
			SearchResult: SearchResult{Chunk: result.chunk, Score: result.score, Distance: 1 - result.score},
			RankScore:    result.rankScore,
			Signals:      map[string]float64{"semantic": result.score, "keyword": boolScore(result.isRelevant)},
		}
	}
	return results, nil
}

func boolScore(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

type hierarchicalBenchmarkMeasurement struct {
	hits, targets, selectedGroups, inspectedCandidates int
}

func runHierarchicalBenchmarkQueries(ctx context.Context, retriever *Retriever, queries []hierarchicalBenchmarkQuery, hierarchical bool) (hierarchicalBenchmarkMeasurement, error) {
	var measurement hierarchicalBenchmarkMeasurement
	for _, query := range queries {
		request := RetrievalRequest{
			Query:        query.label,
			K:            hierarchicalBenchmarkK,
			QueryContext: QueryContext{WorkspaceRoot: "/work"},
		}
		var results []ScoredResult
		if hierarchical {
			response, err := retriever.RetrieveHierarchical(ctx, HierarchicalRetrievalRequest{
				Request:        request,
				CandidateLimit: hierarchicalBenchmarkCandidateLimit,
				MaxDepth:       2,
				MaxGroups:      1,
				MaxTokens:      1 << 20,
				Timeout:        10 * time.Second,
			})
			if err != nil {
				return measurement, err
			}
			results = response.Results
			measurement.inspectedCandidates += response.Trace.Budget.InspectedCandidates
			for _, group := range response.Trace.Groups {
				if group.Selected {
					measurement.selectedGroups++
				}
			}
		} else {
			response, err := retriever.RetrieveRequest(ctx, request)
			if err != nil {
				return measurement, err
			}
			results = response.Results
		}
		measurement.targets += len(query.targets)
		for _, result := range results {
			if _, ok := query.targets[result.Chunk.ID]; ok {
				measurement.hits++
			}
		}
	}
	return measurement, nil
}

func BenchmarkHierarchicalRetrieval(b *testing.B) {
	for _, size := range []int{100, 1_000, 10_000} {
		fixture := buildHierarchicalBenchmarkFixture(size)
		store := &hierarchicalBenchmarkStore{fixture: &fixture}
		queries := make(map[string]int, len(fixture.queryOrder))
		for _, query := range fixture.queryOrder {
			queries[query.label] = query.index
		}
		embedder := hierarchicalBenchmarkEmbedder{queries: queries}

		for _, set := range []string{"concentrated", "distributed"} {
			for _, mode := range []string{"dense", "hybrid"} {
				options := []RetrieverOption(nil)
				if mode == "dense" {
					options = append(options, WithVectorOnly())
				}
				retriever, err := NewRetrieverWithEmbedder(embedder, store, options...)
				if err != nil {
					b.Fatal(err)
				}
				for _, hierarchical := range []bool{false, true} {
					name := "flat"
					if hierarchical {
						name = "hierarchical"
					}
					b.Run(fmt.Sprintf("size=%d/set=%s/mode=%s/%s", size, set, mode, name), func(b *testing.B) {
						b.ReportAllocs()
						if _, err := runHierarchicalBenchmarkQueries(context.Background(), retriever, fixture.querySets[set], hierarchical); err != nil {
							b.Fatal(err)
						}
						b.ResetTimer()
						var total hierarchicalBenchmarkMeasurement
						for i := 0; i < b.N; i++ {
							measurement, err := runHierarchicalBenchmarkQueries(context.Background(), retriever, fixture.querySets[set], hierarchical)
							if err != nil {
								b.Fatal(err)
							}
							total.hits += measurement.hits
							total.targets += measurement.targets
							total.selectedGroups += measurement.selectedGroups
							total.inspectedCandidates += measurement.inspectedCandidates
						}
						b.StopTimer()
						b.ReportMetric(float64(total.hits)/float64(b.N), "target_hits/op")
						b.ReportMetric(100*float64(total.hits)/float64(total.targets), "target_coverage_pct")
						if hierarchical {
							b.ReportMetric(float64(total.selectedGroups)/float64(b.N), "selected_groups/op")
							b.ReportMetric(float64(total.inspectedCandidates)/float64(b.N), "inspected_candidates/op")
						}
					})
				}
			}
		}
	}
}
