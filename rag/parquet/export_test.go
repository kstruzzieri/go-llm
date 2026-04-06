package parquet

import (
	"context"
	"fmt"
	"iter"
	"math"
	"os"
	"path/filepath"
	"bytes"
	"strings"
	"testing"

	pq "github.com/parquet-go/parquet-go"

	"github.com/kstruzzieri/go-llm/rag"
)

// mockExportable is a test double for rag.Exportable.
type mockExportable struct {
	chunks []rag.ExportedChunk
	filter *rag.ExportFilter // captured for assertions
	err    error             // returned from ExportChunks itself
}

func (m *mockExportable) ExportChunks(_ context.Context, filter *rag.ExportFilter) (iter.Seq2[rag.ExportedChunk, error], error) {
	m.filter = filter
	if m.err != nil {
		return nil, m.err
	}
	return func(yield func(rag.ExportedChunk, error) bool) {
		for _, ec := range m.chunks {
			if !yield(ec, nil) {
				return
			}
		}
	}, nil
}

// helper to create a clean chunk with a uniform embedding.
func makeChunk(id, content, source, lang string, startLine, endLine int, emb []float64) rag.ExportedChunk {
	return rag.ExportedChunk{
		Chunk: rag.Chunk{
			ID:        id,
			Content:   content,
			Source:    source,
			StartLine: startLine,
			EndLine:   endLine,
			Language:  lang,
		},
		Embedding: emb,
	}
}

func uniformEmb(dim int, val float64) []float64 {
	e := make([]float64, dim)
	for i := range e {
		e[i] = val
	}
	return e
}

func TestRoundTripFloat32(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	emb := []float64{0.1, 0.2, 0.3}
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "hello world", "/src/main.go", "go", 1, 10, emb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out, WithDType(Float32))
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.RowCount != 1 {
		t.Errorf("RowCount = %d, want 1", info.RowCount)
	}
	if info.EmbeddingDim != 3 {
		t.Errorf("EmbeddingDim = %d, want 3", info.EmbeddingDim)
	}
	if info.DType != Float32 {
		t.Errorf("DType = %v, want Float32", info.DType)
	}
	if info.FileSizeBytes == 0 {
		t.Error("FileSizeBytes = 0")
	}

	// Read back with parquet-go.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	reader := pq.NewGenericReader[embeddingRowF32](f)
	defer func() { _ = reader.Close() }()

	rows := make([]embeddingRowF32, 1)
	n, err := reader.Read(rows)
	if n == 0 && err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("read %d rows, want 1", n)
	}

	row := rows[0]
	if row.ID != "c1" {
		t.Errorf("ID = %q, want %q", row.ID, "c1")
	}
	if row.Content != "hello world" {
		t.Errorf("Content = %q, want %q", row.Content, "hello world")
	}
	if row.Source != "/src/main.go" {
		t.Errorf("Source = %q, want %q", row.Source, "/src/main.go")
	}
	if row.StartLine != 1 {
		t.Errorf("StartLine = %d, want 1", row.StartLine)
	}
	if row.EndLine != 10 {
		t.Errorf("EndLine = %d, want 10", row.EndLine)
	}
	if row.Language != "go" {
		t.Errorf("Language = %q, want %q", row.Language, "go")
	}
	if len(row.Embedding) != 3 {
		t.Fatalf("Embedding len = %d, want 3", len(row.Embedding))
	}
	if row.QualityFlag != "" {
		t.Errorf("QualityFlag = %q, want empty", row.QualityFlag)
	}

	// Verify norm is approximately correct.
	expectedNorm := float32(math.Sqrt(0.01 + 0.04 + 0.09))
	if math.Abs(float64(row.EmbeddingNorm-expectedNorm)) > 0.001 {
		t.Errorf("EmbeddingNorm = %f, want ~%f", row.EmbeddingNorm, expectedNorm)
	}
}

func TestRoundTripFloat64(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	emb := []float64{1.0, 2.0, 3.0}
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/src/lib.py", "python", 5, 15, emb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out, WithDType(Float64))
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.DType != Float64 {
		t.Errorf("DType = %v, want Float64", info.DType)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	reader := pq.NewGenericReader[embeddingRowF64](f)
	defer func() { _ = reader.Close() }()

	rows := make([]embeddingRowF64, 1)
	n, err := reader.Read(rows)
	if n == 0 && err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != 1 {
		t.Fatalf("read %d rows, want 1", n)
	}

	row := rows[0]
	expectedNorm := math.Sqrt(1 + 4 + 9)
	if math.Abs(row.EmbeddingNorm-expectedNorm) > 1e-10 {
		t.Errorf("EmbeddingNorm = %f, want %f", row.EmbeddingNorm, expectedNorm)
	}
	// Full precision check for float64.
	for i, v := range emb {
		if row.Embedding[i] != v {
			t.Errorf("Embedding[%d] = %f, want %f", i, row.Embedding[i], v)
		}
	}
}

func TestFilterPassthrough(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	_, err := ExportDataset(context.Background(), mock, out,
		WithSourcePattern("*rag/*.go"),
		WithLanguage("go"),
	)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if mock.filter == nil {
		t.Fatal("filter was nil")
	}
	if mock.filter.SourcePattern != "*rag/*.go" {
		t.Errorf("SourcePattern = %q, want %q", mock.filter.SourcePattern, "*rag/*.go")
	}
	if mock.filter.Language != "go" {
		t.Errorf("Language = %q, want %q", mock.filter.Language, "go")
	}
}

func TestQualityZeroVector(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, uniformEmb(3, 0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.ZeroVectors != 1 {
		t.Errorf("ZeroVectors = %d, want 1", info.Quality.ZeroVectors)
	}
	if info.Quality.FlaggedRows != 1 {
		t.Errorf("FlaggedRows = %d, want 1", info.Quality.FlaggedRows)
	}

	// Verify the quality_flag in the file.
	rows := readF32Rows(t, out)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].QualityFlag != "zero_vector" {
		t.Errorf("QualityFlag = %q, want %q", rows[0].QualityFlag, "zero_vector")
	}
	// Embedding preserved as-is (all zeros).
	for i, v := range rows[0].Embedding {
		if v != 0 {
			t.Errorf("Embedding[%d] = %f, want 0", i, v)
		}
	}
}

func TestQualityNaNEmbedding(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	nanEmb := []float64{1.0, math.NaN(), 3.0}
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, nanEmb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.NaNReplaced != 1 {
		t.Errorf("NaNReplaced = %d, want 1", info.Quality.NaNReplaced)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "nan_replaced" {
		t.Errorf("QualityFlag = %q, want %q", rows[0].QualityFlag, "nan_replaced")
	}
	// Embedding replaced with zeros.
	for i, v := range rows[0].Embedding {
		if v != 0 {
			t.Errorf("Embedding[%d] = %f, want 0", i, v)
		}
	}
	if rows[0].EmbeddingNorm != 0 {
		t.Errorf("EmbeddingNorm = %f, want 0", rows[0].EmbeddingNorm)
	}
}

func TestQualityEmptyContent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "  \t\n  ", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.EmptyContent != 1 {
		t.Errorf("EmptyContent = %d, want 1", info.Quality.EmptyContent)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "empty_content" {
		t.Errorf("QualityFlag = %q, want %q", rows[0].QualityFlag, "empty_content")
	}
}

func TestQualityCleanData(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.CleanRows != 1 {
		t.Errorf("CleanRows = %d, want 1", info.Quality.CleanRows)
	}
	if info.Quality.FlaggedRows != 0 {
		t.Errorf("FlaggedRows = %d, want 0", info.Quality.FlaggedRows)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "" {
		t.Errorf("QualityFlag = %q, want empty", rows[0].QualityFlag)
	}
}

func TestQualityOutlierNorm(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// Create an embedding with a very large norm (well above default max of 10.0).
	bigEmb := uniformEmb(3, 100.0) // norm = sqrt(30000) ≈ 173
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, bigEmb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.OutlierNorms != 1 {
		t.Errorf("OutlierNorms = %d, want 1", info.Quality.OutlierNorms)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "outlier_norm" {
		t.Errorf("QualityFlag = %q, want %q", rows[0].QualityFlag, "outlier_norm")
	}
}

func TestQualityMultiFlagPriority(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// NaN embedding + empty content → nan_replaced wins, both counters increment.
	nanEmb := []float64{math.NaN(), 0.0, 0.0}
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "  ", "/a.go", "go", 1, 1, nanEmb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.NaNReplaced != 1 {
		t.Errorf("NaNReplaced = %d, want 1", info.Quality.NaNReplaced)
	}
	if info.Quality.EmptyContent != 1 {
		t.Errorf("EmptyContent = %d, want 1", info.Quality.EmptyContent)
	}
	if info.Quality.FlaggedRows != 1 {
		t.Errorf("FlaggedRows = %d, want 1", info.Quality.FlaggedRows)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "nan_replaced" {
		t.Errorf("QualityFlag = %q, want %q (NaN should win priority)", rows[0].QualityFlag, "nan_replaced")
	}
}

func TestQualityNaNNotCountedAsZeroVector(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	nanEmb := []float64{math.NaN(), math.NaN(), math.NaN()}
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, nanEmb),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.NaNReplaced != 1 {
		t.Errorf("NaNReplaced = %d, want 1", info.Quality.NaNReplaced)
	}
	if info.Quality.ZeroVectors != 0 {
		t.Errorf("ZeroVectors = %d, want 0 (NaN should not count as zero)", info.Quality.ZeroVectors)
	}
}

func TestQualityFloat32Float64Consistency(t *testing.T) {
	// Same data should produce identical quality flags regardless of DType.
	embs := [][]float64{
		uniformEmb(3, 1.0),          // clean
		uniformEmb(3, 0),            // zero_vector
		{math.NaN(), 0.1, 0.2},     // nan_replaced
		uniformEmb(3, 100.0),        // outlier_norm
	}

	for _, dt := range []DType{Float32, Float64} {
		dir := t.TempDir()
		out := filepath.Join(dir, "test.parquet")

		var chunks []rag.ExportedChunk
		for i, e := range embs {
			chunks = append(chunks, makeChunk(
				fmt.Sprintf("c%d", i), "content", "/a.go", "go", i, i+1, e,
			))
		}
		mock := &mockExportable{chunks: chunks}

		info, err := ExportDataset(context.Background(), mock, out, WithDType(dt))
		if err != nil {
			t.Fatalf("DType=%v: ExportDataset: %v", dt, err)
		}

		if info.Quality.CleanRows != 1 {
			t.Errorf("DType=%v: CleanRows = %d, want 1", dt, info.Quality.CleanRows)
		}
		if info.Quality.ZeroVectors != 1 {
			t.Errorf("DType=%v: ZeroVectors = %d, want 1", dt, info.Quality.ZeroVectors)
		}
		if info.Quality.NaNReplaced != 1 {
			t.Errorf("DType=%v: NaNReplaced = %d, want 1", dt, info.Quality.NaNReplaced)
		}
		if info.Quality.OutlierNorms != 1 {
			t.Errorf("DType=%v: OutlierNorms = %d, want 1", dt, info.Quality.OutlierNorms)
		}
	}
}

func TestQualityAllRowsFlagged(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, uniformEmb(3, 0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.CleanRows != 0 {
		t.Errorf("CleanRows = %d, want 0", info.Quality.CleanRows)
	}
	// NormStats should be zeroed.
	ns := info.Quality.NormStats
	if ns.Mean != 0 || ns.StdDev != 0 || ns.Min != 0 || ns.Max != 0 {
		t.Errorf("NormStats = %+v, want all zeros when CleanRows == 0", ns)
	}

	// Verify metadata doesn't contain norm stats.
	meta := readFileMetadata(t, out)
	if _, ok := meta["go_llm.quality.norm_mean"]; ok {
		t.Error("norm_mean should be omitted when CleanRows == 0")
	}
	if _, ok := meta["go_llm.quality.norm_stddev"]; ok {
		t.Error("norm_stddev should be omitted when CleanRows == 0")
	}
}

func TestRowGroupBoundaries(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// 25 rows with row group size 10 → should produce 3 row groups.
	var chunks []rag.ExportedChunk
	for i := range 25 {
		chunks = append(chunks, makeChunk(
			fmt.Sprintf("c%d", i), "content", "/a.go", "go", i, i+1, uniformEmb(4, 1.0),
		))
	}
	mock := &mockExportable{chunks: chunks}

	info, err := ExportDataset(context.Background(), mock, out, WithRowGroupSize(10))
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.RowCount != 25 {
		t.Errorf("RowCount = %d, want 25", info.RowCount)
	}

	// Verify multiple row groups by reading the file metadata.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	pf, err := pq.OpenFile(f, fi.Size())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	numRowGroups := len(pf.RowGroups())
	if numRowGroups < 3 {
		t.Errorf("got %d row groups, want >= 3 for 25 rows with rowGroupSize=10", numRowGroups)
	}
}

func TestOptionValidation(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	tests := []struct {
		name string
		opts []ExportOption
		want string
	}{
		{"negative row group size", []ExportOption{WithRowGroupSize(-1)}, "row group size must be > 0"},
		{"zero row group size", []ExportOption{WithRowGroupSize(0)}, "row group size must be > 0"},
		{"norm min > max", []ExportOption{WithNormRange(10, 1)}, "norm range min"},
		{"negative norm min", []ExportOption{WithNormRange(-1, 10)}, "norm range min must be >= 0"},
		{"unknown dtype", []ExportOption{WithDType(DType(99))}, "unknown dtype"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExportDataset(context.Background(), mock, out, tt.opts...)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestEmptyStore(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{chunks: nil}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.RowCount != 0 {
		t.Errorf("RowCount = %d, want 0", info.RowCount)
	}
	if info.EmbeddingDim != 0 {
		t.Errorf("EmbeddingDim = %d, want 0", info.EmbeddingDim)
	}

	// File should exist.
	if _, err := os.Stat(out); err != nil {
		t.Errorf("output file missing: %v", err)
	}

	// Metadata should omit embedding_dim.
	meta := readFileMetadata(t, out)
	if _, ok := meta["go_llm.embedding_dim"]; ok {
		t.Error("embedding_dim should be omitted for empty export")
	}
}

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// Mock that returns an error during iteration.
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}
	// First, create a successful file.
	_, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("initial export: %v", err)
	}
	origData, _ := os.ReadFile(out)

	// Now export with a dimension mismatch to trigger error mid-write.
	badMock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
			makeChunk("c2", "y", "/b.go", "go", 1, 1, uniformEmb(5, 1.0)), // dim mismatch
		},
	}
	_, err = ExportDataset(context.Background(), badMock, out)
	if err == nil {
		t.Fatal("expected error from dim mismatch")
	}

	// Original file should be byte-for-byte intact.
	afterData, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("original file missing after failed export: %v", readErr)
	}
	if !bytes.Equal(origData, afterData) {
		t.Error("original file was modified by failed export")
	}

	// No orphaned temp files should remain.
	temps, _ := filepath.Glob(filepath.Join(dir, ".parquet-export-*.tmp"))
	if len(temps) != 0 {
		t.Errorf("orphaned temp files after failed export: %v", temps)
	}
}

func TestOverwriteExistingFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock1 := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "first", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}
	// Create initial file.
	_, err := ExportDataset(context.Background(), mock1, out)
	if err != nil {
		t.Fatalf("initial export: %v", err)
	}
	origInfo, _ := os.Stat(out)

	// Overwrite with different data.
	mock2 := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c2", "second content that is longer", "/b.go", "go", 1, 1, uniformEmb(3, 0.5)),
			makeChunk("c3", "third", "/c.go", "go", 1, 1, uniformEmb(3, 0.5)),
		},
	}
	info, err := ExportDataset(context.Background(), mock2, out)
	if err != nil {
		t.Fatalf("overwrite export: %v", err)
	}
	if info.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", info.RowCount)
	}

	// File size should differ (different data).
	newInfo, _ := os.Stat(out)
	if newInfo.Size() == origInfo.Size() {
		t.Error("file size unchanged after overwrite with different data")
	}

	// No orphaned temp files.
	temps, _ := filepath.Glob(filepath.Join(dir, ".parquet-export-*.tmp"))
	if len(temps) != 0 {
		t.Errorf("orphaned temp files after overwrite: %v", temps)
	}
}

func TestDatasetInfoAccuracy(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "hello", "/a.go", "go", 1, 5, uniformEmb(4, 1.0)),
			makeChunk("c2", "world", "/b.go", "go", 1, 3, uniformEmb(4, 0.5)),
			makeChunk("c3", "test", "/c.go", "go", 1, 2, uniformEmb(4, 0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out,
		WithSourcePattern("*.go"),
		WithLanguage("go"),
		WithModel("nomic-embed-text"),
	)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", info.RowCount)
	}
	if info.EmbeddingDim != 4 {
		t.Errorf("EmbeddingDim = %d, want 4", info.EmbeddingDim)
	}
	if info.Quality.CleanRows != 2 {
		t.Errorf("CleanRows = %d, want 2", info.Quality.CleanRows)
	}
	if info.Quality.FlaggedRows != 1 {
		t.Errorf("FlaggedRows = %d, want 1", info.Quality.FlaggedRows)
	}
	if info.Quality.ZeroVectors != 1 {
		t.Errorf("ZeroVectors = %d, want 1", info.Quality.ZeroVectors)
	}
	if info.Filters.SourcePattern != "*.go" {
		t.Errorf("SourcePattern = %q, want %q", info.Filters.SourcePattern, "*.go")
	}
	if info.Filters.Language != "go" {
		t.Errorf("Language = %q, want %q", info.Filters.Language, "go")
	}
}

func TestFileMetadata(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "hello", "/a.go", "go", 1, 5, uniformEmb(4, 1.0)),
		},
	}

	_, err := ExportDataset(context.Background(), mock, out,
		WithModel("nomic-embed-text"),
		WithSourcePattern("*.go"),
		WithLanguage("go"),
	)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	meta := readFileMetadata(t, out)

	checks := map[string]string{
		"go_llm.version":                 Version,
		"go_llm.dtype":                   "float32",
		"go_llm.row_count":               "1",
		"go_llm.embedding_dim":           "4",
		"go_llm.embedding_model":         "nomic-embed-text",
		"go_llm.filter.source_pattern":   "*.go",
		"go_llm.filter.source_semantics": "sqlite_glob",
		"go_llm.filter.language":         "go",
		"go_llm.quality.clean_rows":      "1",
		"go_llm.quality.flagged_rows":    "0",
		"go_llm.quality.norm_mean":       "2.000", // uniformEmb(4, 1.0) → norm = sqrt(4) = 2.0
		"go_llm.quality.norm_stddev":     "0.000", // single row → stddev 0
	}

	for k, want := range checks {
		got, ok := meta[k]
		if !ok {
			t.Errorf("metadata key %q missing", k)
			continue
		}
		if got != want {
			t.Errorf("metadata[%q] = %q, want %q", k, got, want)
		}
	}

	// Timestamp should be present and parseable.
	if _, ok := meta["go_llm.export_timestamp"]; !ok {
		t.Error("export_timestamp missing")
	}
}

func TestEmbeddingDimMismatch(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
			makeChunk("c2", "y", "/b.go", "go", 1, 1, uniformEmb(5, 1.0)),
		},
	}

	_, err := ExportDataset(context.Background(), mock, out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "dimension mismatch") {
		t.Errorf("error %q should mention dimension mismatch", err.Error())
	}

	// No output file should exist.
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("output file should not exist after error")
	}
}

func TestContextCancellation(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	ctx, cancel := context.WithCancel(context.Background())

	// Create a mock that cancels context during iteration.
	cancellingMock := &cancelExportable{
		cancelAfter: 2,
		cancel:      cancel,
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
			makeChunk("c2", "y", "/b.go", "go", 2, 2, uniformEmb(3, 1.0)),
			makeChunk("c3", "z", "/c.go", "go", 3, 3, uniformEmb(3, 1.0)),
		},
	}

	_, err := ExportDataset(ctx, cancellingMock, out)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// cancelExportable cancels the context after yielding N chunks.
type cancelExportable struct {
	cancelAfter int
	cancel      context.CancelFunc
	chunks      []rag.ExportedChunk
}

func (m *cancelExportable) ExportChunks(_ context.Context, _ *rag.ExportFilter) (iter.Seq2[rag.ExportedChunk, error], error) {
	yielded := 0
	return func(yield func(rag.ExportedChunk, error) bool) {
		for _, ec := range m.chunks {
			yielded++
			if yielded > m.cancelAfter {
				m.cancel()
			}
			if !yield(ec, nil) {
				return
			}
		}
	}, nil
}

func TestExportChunksError(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		err: fmt.Errorf("db connection lost"),
	}

	_, err := ExportDataset(context.Background(), mock, out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "db connection lost") {
		t.Errorf("error %q should contain cause", err.Error())
	}
}

func TestNoFilterPassesNilFilter(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "x", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	_, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if mock.filter != nil {
		t.Errorf("filter should be nil when no filter options set, got %+v", mock.filter)
	}
}

func TestNormStatsAccuracy(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// Two clean rows with known norms.
	// [1.0, 0, 0] → norm = 1.0
	// [0, 2.0, 0] → norm = 2.0
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "a", "/a.go", "go", 1, 1, []float64{1.0, 0, 0}),
			makeChunk("c2", "b", "/b.go", "go", 1, 1, []float64{0, 2.0, 0}),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	ns := info.Quality.NormStats
	if ns.Min != 1.0 {
		t.Errorf("NormStats.Min = %f, want 1.0", ns.Min)
	}
	if ns.Max != 2.0 {
		t.Errorf("NormStats.Max = %f, want 2.0", ns.Max)
	}
	expectedMean := 1.5
	if math.Abs(ns.Mean-expectedMean) > 1e-10 {
		t.Errorf("NormStats.Mean = %f, want %f", ns.Mean, expectedMean)
	}
	// Population stddev of [1.0, 2.0] = sqrt(0.25) = 0.5
	expectedStdDev := 0.5
	if math.Abs(ns.StdDev-expectedStdDev) > 1e-10 {
		t.Errorf("NormStats.StdDev = %f, want %f", ns.StdDev, expectedStdDev)
	}
}

// --- helpers ---

func readF32Rows(t *testing.T, path string) []embeddingRowF32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	reader := pq.NewGenericReader[embeddingRowF32](f)
	defer func() { _ = reader.Close() }()

	rows := make([]embeddingRowF32, 100)
	n, readErr := reader.Read(rows)
	if n == 0 && readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	return rows[:n]
}

func readFileMetadata(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	pf, err := pq.OpenFile(f, fi.Size())
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}

	meta := make(map[string]string)
	for _, kv := range pf.Metadata().KeyValueMetadata {
		meta[kv.Key] = kv.Value
	}
	return meta
}

// --- Additional quality flag priority tests ---

func TestQualityPriorityZeroVectorOverEmptyContent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// Zero-vector embedding + empty content → zero_vector wins.
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "  ", "/a.go", "go", 1, 1, uniformEmb(3, 0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.ZeroVectors != 1 {
		t.Errorf("ZeroVectors = %d, want 1", info.Quality.ZeroVectors)
	}
	if info.Quality.EmptyContent != 1 {
		t.Errorf("EmptyContent = %d, want 1", info.Quality.EmptyContent)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "zero_vector" {
		t.Errorf("QualityFlag = %q, want %q (zero_vector > empty_content)", rows[0].QualityFlag, "zero_vector")
	}
}

func TestQualityPriorityOutlierNormOverEmptyContent(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// Outlier norm + empty content → outlier_norm wins.
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "  ", "/a.go", "go", 1, 1, uniformEmb(3, 100.0)),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	if info.Quality.OutlierNorms != 1 {
		t.Errorf("OutlierNorms = %d, want 1", info.Quality.OutlierNorms)
	}
	if info.Quality.EmptyContent != 1 {
		t.Errorf("EmptyContent = %d, want 1", info.Quality.EmptyContent)
	}

	rows := readF32Rows(t, out)
	if rows[0].QualityFlag != "outlier_norm" {
		t.Errorf("QualityFlag = %q, want %q (outlier_norm > empty_content)", rows[0].QualityFlag, "outlier_norm")
	}
}

func TestNormStatsSingleCleanRow(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// One clean row with norm = 1.0.
	mock := &mockExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "content", "/a.go", "go", 1, 1, []float64{1.0, 0, 0}),
		},
	}

	info, err := ExportDataset(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportDataset: %v", err)
	}

	ns := info.Quality.NormStats
	if ns.Min != 1.0 {
		t.Errorf("NormStats.Min = %f, want 1.0", ns.Min)
	}
	if ns.Max != 1.0 {
		t.Errorf("NormStats.Max = %f, want 1.0", ns.Max)
	}
	if ns.Mean != 1.0 {
		t.Errorf("NormStats.Mean = %f, want 1.0", ns.Mean)
	}
	if ns.StdDev != 0 {
		t.Errorf("NormStats.StdDev = %f, want 0 (single sample)", ns.StdDev)
	}
}

func TestExportVectorStoreHappyPath(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	// mockExportable satisfies rag.Exportable but not rag.VectorStore.
	// Use a combined mock for ExportVectorStore.
	mock := &mockVectorStoreExportable{
		chunks: []rag.ExportedChunk{
			makeChunk("c1", "hello", "/a.go", "go", 1, 1, uniformEmb(3, 1.0)),
		},
	}

	info, err := ExportVectorStore(context.Background(), mock, out)
	if err != nil {
		t.Fatalf("ExportVectorStore: %v", err)
	}
	if info.RowCount != 1 {
		t.Errorf("RowCount = %d, want 1", info.RowCount)
	}
}

func TestExportVectorStoreNonExportable(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	mock := &mockVectorStoreOnly{}

	_, err := ExportVectorStore(context.Background(), mock, out)
	if err == nil {
		t.Fatal("expected error for non-Exportable store")
	}
	if !strings.Contains(err.Error(), "does not implement") {
		t.Errorf("error %q should mention 'does not implement'", err.Error())
	}
}

// mockVectorStoreExportable satisfies both VectorStore and Exportable.
type mockVectorStoreExportable struct {
	chunks []rag.ExportedChunk
}

func (m *mockVectorStoreExportable) Store(_ context.Context, _ []rag.Chunk, _ [][]float64) error {
	return nil
}
func (m *mockVectorStoreExportable) Search(_ context.Context, _ []float64, _ int) ([]rag.SearchResult, error) {
	return nil, nil
}
func (m *mockVectorStoreExportable) DeleteBySource(_ context.Context, _ string) error { return nil }
func (m *mockVectorStoreExportable) Stats(_ context.Context) (rag.StoreStats, error) {
	return rag.StoreStats{}, nil
}
func (m *mockVectorStoreExportable) Close() error { return nil }
func (m *mockVectorStoreExportable) ExportChunks(_ context.Context, _ *rag.ExportFilter) (iter.Seq2[rag.ExportedChunk, error], error) {
	return func(yield func(rag.ExportedChunk, error) bool) {
		for _, ec := range m.chunks {
			if !yield(ec, nil) {
				return
			}
		}
	}, nil
}

// mockVectorStoreOnly satisfies VectorStore but NOT Exportable.
type mockVectorStoreOnly struct{}

func (m *mockVectorStoreOnly) Store(_ context.Context, _ []rag.Chunk, _ [][]float64) error {
	return nil
}
func (m *mockVectorStoreOnly) Search(_ context.Context, _ []float64, _ int) ([]rag.SearchResult, error) {
	return nil, nil
}
func (m *mockVectorStoreOnly) DeleteBySource(_ context.Context, _ string) error { return nil }
func (m *mockVectorStoreOnly) Stats(_ context.Context) (rag.StoreStats, error) {
	return rag.StoreStats{}, nil
}
func (m *mockVectorStoreOnly) Close() error { return nil }

func TestLineNumberInt32Overflow(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "test.parquet")

	tests := []struct {
		name      string
		startLine int
		endLine   int
		wantErr   string
	}{
		{"start_line overflow", math.MaxInt32 + 1, 1, "start_line"},
		{"end_line overflow", 1, math.MaxInt32 + 1, "end_line"},
		{"negative start_line", -1, 1, "start_line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := &mockExportable{
				chunks: []rag.ExportedChunk{
					makeChunk("c1", "x", "/a.go", "go", tt.startLine, tt.endLine, uniformEmb(3, 1.0)),
				},
			}
			_, err := ExportDataset(context.Background(), mock, out)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}
