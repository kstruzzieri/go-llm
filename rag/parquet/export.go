package parquet

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pq "github.com/parquet-go/parquet-go"

	"github.com/kstruzzieri/go-llm/rag"
)

// ExportOption configures the behavior of ExportDataset.
type ExportOption func(*exportConfig)

type exportConfig struct {
	sourcePattern string
	language      string
	dtype         DType
	model         string
	rowGroupSize  int
	normMin       float64
	normMax       float64
}

func defaultConfig() exportConfig {
	return exportConfig{
		dtype:        Float32,
		rowGroupSize: 10000,
		normMin:      0.1,
		normMax:      10.0,
	}
}

// WithSourcePattern filters exported chunks by source path glob pattern.
func WithSourcePattern(pattern string) ExportOption {
	return func(c *exportConfig) { c.sourcePattern = pattern }
}

// WithLanguage filters exported chunks by exact language match.
func WithLanguage(lang string) ExportOption {
	return func(c *exportConfig) { c.language = lang }
}

// WithDType sets the embedding precision in the output Parquet file.
func WithDType(dt DType) ExportOption {
	return func(c *exportConfig) { c.dtype = dt }
}

// WithModel sets the embedding model name included in file metadata.
func WithModel(name string) ExportOption {
	return func(c *exportConfig) { c.model = name }
}

// WithRowGroupSize sets the maximum number of rows per Parquet row group.
func WithRowGroupSize(n int) ExportOption {
	return func(c *exportConfig) { c.rowGroupSize = n }
}

// WithNormRange sets the L2 norm range for outlier detection.
func WithNormRange(min, max float64) ExportOption {
	return func(c *exportConfig) {
		c.normMin = min
		c.normMax = max
	}
}

func (c *exportConfig) validate() error {
	if c.rowGroupSize <= 0 {
		return fmt.Errorf("parquet: row group size must be > 0, got %d", c.rowGroupSize)
	}
	if c.normMin < 0 {
		return fmt.Errorf("parquet: norm range min must be >= 0, got %f", c.normMin)
	}
	if c.normMin > c.normMax {
		return fmt.Errorf("parquet: norm range min (%f) must be <= max (%f)", c.normMin, c.normMax)
	}
	if c.dtype != Float32 && c.dtype != Float64 {
		return fmt.Errorf("parquet: unknown dtype %d", c.dtype)
	}
	return nil
}

// ExportVectorStore is a convenience wrapper that asserts Exportable and exports.
func ExportVectorStore(ctx context.Context, store rag.VectorStore, outputPath string, opts ...ExportOption) (*DatasetInfo, error) {
	exp, ok := store.(rag.Exportable)
	if !ok {
		return nil, fmt.Errorf("parquet: store does not implement rag.Exportable")
	}
	return ExportDataset(ctx, exp, outputPath, opts...)
}

// ExportDataset exports chunks from an Exportable source to a Parquet file.
// The output file is written atomically — no partial file is visible on failure.
func ExportDataset(
	ctx context.Context,
	src rag.Exportable,
	outputPath string,
	opts ...ExportOption,
) (*DatasetInfo, error) {
	// Step 1: Parse and validate options.
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Step 2: Build ExportFilter from options.
	var filter *rag.ExportFilter
	if cfg.sourcePattern != "" || cfg.language != "" {
		filter = &rag.ExportFilter{
			SourcePattern: cfg.sourcePattern,
			Language:      cfg.language,
		}
	}

	// Step 3: Create temp file in the same directory as outputPath.
	dir := filepath.Dir(outputPath)
	tmpFile, err := os.CreateTemp(dir, ".parquet-export-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("parquet: create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	// Cleanup on any error path. tmpClosed prevents double-close.
	tmpClosed := false
	cleanup := func() {
		if !tmpClosed {
			_ = tmpFile.Close()
			tmpClosed = true
		}
		_ = os.Remove(tmpPath)
	}
	success := false
	defer func() {
		if !success {
			cleanup()
		}
	}()

	// Step 4-9: Delegate to typed writer based on DType.
	var info *DatasetInfo
	switch cfg.dtype {
	case Float32:
		info, err = exportTyped[embeddingRowF32](ctx, src, tmpFile, filter, &cfg, buildRowF32)
	case Float64:
		info, err = exportTyped[embeddingRowF64](ctx, src, tmpFile, filter, &cfg, buildRowF64)
	}
	if err != nil {
		return nil, err
	}

	// Step 10: Atomic publish. Try rename first (atomic overwrite on Unix).
	// If rename fails (e.g., Windows cannot overwrite an existing file),
	// fall back to remove + rename. This preserves the existing file until
	// the rename is confirmed to succeed on Unix, and only removes on
	// platforms where overwrite-rename is not supported.
	if err := tmpFile.Close(); err != nil {
		tmpClosed = true
		return nil, fmt.Errorf("parquet: close temp file: %w", err)
	}
	tmpClosed = true
	if err := os.Rename(tmpPath, outputPath); err != nil {
		// Rename failed — likely Windows with existing destination.
		// Guard: refuse to remove directories to prevent accidental data loss
		// if outputPath is a directory instead of a file.
		if fi, statErr := os.Stat(outputPath); statErr == nil && fi.IsDir() {
			return nil, fmt.Errorf("parquet: output path %q is a directory, not a file", outputPath)
		}
		// Remove destination and retry; if remove fails, report the
		// original rename error to avoid masking the root cause.
		if removeErr := os.Remove(outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return nil, fmt.Errorf("parquet: rename to output: %w", err)
		}
		if err := os.Rename(tmpPath, outputPath); err != nil {
			return nil, fmt.Errorf("parquet: rename to output: %w", err)
		}
	}

	// Step 11: Stat the output file for size.
	// The export is complete at this point — a stat failure only means we
	// can't report the file size, not that the export failed.
	fi, err := os.Stat(outputPath)
	if err != nil {
		return nil, fmt.Errorf("parquet: stat output: %w", err)
	}
	info.FileSizeBytes = fi.Size()
	success = true

	return info, nil
}

// rowBuilder converts processed export data into a typed Parquet row.
type rowBuilder[T any] func(ec rag.ExportedChunk, embedding any, norm float64, flag string) T

// exportTyped runs the core export pipeline for a specific row type.
func exportTyped[T any](
	ctx context.Context,
	src rag.Exportable,
	w io.Writer,
	filter *rag.ExportFilter,
	cfg *exportConfig,
	build rowBuilder[T],
) (*DatasetInfo, error) {
	// Early context check before allocating resources.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Step 4: Create Parquet writer.
	writer := pq.NewGenericWriter[T](w,
		pq.MaxRowsPerRowGroup(int64(cfg.rowGroupSize)),
	)

	// closeWriter is a helper for error paths. Writer close errors on error
	// paths are secondary to the primary error, but we close to release resources.
	closeWriter := func() {
		_ = writer.Close()
	}

	// Step 5: Get iterator from source.
	seq, err := src.ExportChunks(ctx, filter)
	if err != nil {
		closeWriter()
		return nil, err
	}

	// Step 6: Iterate, detect quality, write rows.
	exportedAt := time.Now()
	var (
		embDim  int
		quality DatasetQuality
		welford welfordState
		buf     = make([]T, 0, cfg.rowGroupSize)
	)

	for ec, iterErr := range seq {
		if iterErr != nil {
			closeWriter()
			return nil, fmt.Errorf("parquet: iterate chunks: %w", iterErr)
		}

		// Check context.
		if err := ctx.Err(); err != nil {
			closeWriter()
			return nil, err
		}

		// Validate embedding dimension consistency.
		dim := len(ec.Embedding)
		if embDim == 0 {
			if dim == 0 {
				closeWriter()
				return nil, fmt.Errorf("parquet: chunk %q has empty embedding (0 dimensions)", ec.Chunk.ID)
			}
			embDim = dim
		} else if dim != embDim {
			closeWriter()
			return nil, fmt.Errorf("parquet: embedding dimension mismatch: expected %d, got %d", embDim, dim)
		}

		// Validate line numbers fit in Parquet int32 schema.
		if ec.Chunk.StartLine < 0 || ec.Chunk.StartLine > math.MaxInt32 {
			closeWriter()
			return nil, fmt.Errorf("parquet: start_line %d exceeds Parquet int32 range for chunk %q", ec.Chunk.StartLine, ec.Chunk.ID)
		}
		if ec.Chunk.EndLine < 0 || ec.Chunk.EndLine > math.MaxInt32 {
			closeWriter()
			return nil, fmt.Errorf("parquet: end_line %d exceeds Parquet int32 range for chunk %q", ec.Chunk.EndLine, ec.Chunk.ID)
		}

		// Phase 1: DETECT on original float64 embedding.
		hasNaN := containsNaNOrInf(ec.Embedding)
		norm := l2Norm(ec.Embedding) // NaN if hasNaN
		isZero := !hasNaN && norm == 0
		isOutlier := !hasNaN && !isZero && (norm < cfg.normMin || norm > cfg.normMax)
		isEmpty := strings.TrimSpace(ec.Chunk.Content) == ""

		// Increment counters for ALL detected conditions.
		if hasNaN {
			quality.NaNReplaced++
		}
		if isZero {
			quality.ZeroVectors++
		}
		if isOutlier {
			quality.OutlierNorms++
		}
		if isEmpty {
			quality.EmptyContent++
		}

		// Select highest-severity flag.
		flag := selectQualityFlag(hasNaN, isZero, isOutlier, isEmpty)

		// Phase 2: MUTATE if needed.
		if hasNaN {
			ec.Embedding = make([]float64, dim)
			norm = 0
		}

		// Update running NormStats for clean rows only.
		if flag == "" {
			welford.update(norm)
		}

		quality.TotalRows++
		if flag != "" {
			quality.FlaggedRows++
		} else {
			quality.CleanRows++
		}

		// Phase 3: Convert to target dtype and build row.
		var convertedEmb any
		switch cfg.dtype {
		case Float32:
			convertedEmb = toFloat32Slice(ec.Embedding)
		case Float64:
			convertedEmb = ec.Embedding
		default:
			closeWriter()
			return nil, fmt.Errorf("parquet: unsupported dtype %d in export loop", cfg.dtype)
		}

		row := build(ec, convertedEmb, norm, flag)
		buf = append(buf, row)

		// Flush buffer when full.
		if len(buf) >= cfg.rowGroupSize {
			if _, err := writer.Write(buf); err != nil {
				closeWriter()
				return nil, fmt.Errorf("parquet: write rows: %w", err)
			}
			buf = buf[:0]
		}
	}

	// Step 7: Flush remaining rows.
	if len(buf) > 0 {
		if _, err := writer.Write(buf); err != nil {
			closeWriter()
			return nil, fmt.Errorf("parquet: write remaining rows: %w", err)
		}
	}

	// Finalize norm stats.
	quality.NormStats = welford.finalize()

	// Step 8: Set file-level metadata with deterministic key ordering.
	info := &DatasetInfo{
		RowCount:     quality.TotalRows,
		EmbeddingDim: embDim,
		DType:        cfg.dtype,
		ExportedAt:   exportedAt,
		Filters: FilterSummary{
			SourcePattern: cfg.sourcePattern,
			Language:      cfg.language,
		},
		Quality: quality,
	}

	meta := buildMetadataWithModel(info, cfg.model)
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writer.SetKeyValueMetadata(k, meta[k])
	}

	// Step 9: Close writer (flushes footer).
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("parquet: close writer: %w", err)
	}

	return info, nil
}

// buildRowF32 constructs a float32 Parquet row.
func buildRowF32(ec rag.ExportedChunk, embedding any, norm float64, flag string) embeddingRowF32 {
	emb, _ := embedding.([]float32) // safe: only called with []float32 from exportTyped switch
	return embeddingRowF32{
		ID:            ec.Chunk.ID,
		Content:       ec.Chunk.Content,
		Source:        ec.Chunk.Source,
		StartLine:     int32(ec.Chunk.StartLine),
		EndLine:       int32(ec.Chunk.EndLine),
		Language:      ec.Chunk.Language,
		Embedding:     emb,
		EmbeddingNorm: float32(norm),
		QualityFlag:   flag,
	}
}

// buildRowF64 constructs a float64 Parquet row.
func buildRowF64(ec rag.ExportedChunk, embedding any, norm float64, flag string) embeddingRowF64 {
	emb, _ := embedding.([]float64) // safe: only called with []float64 from exportTyped switch
	return embeddingRowF64{
		ID:            ec.Chunk.ID,
		Content:       ec.Chunk.Content,
		Source:        ec.Chunk.Source,
		StartLine:     int32(ec.Chunk.StartLine),
		EndLine:       int32(ec.Chunk.EndLine),
		Language:      ec.Chunk.Language,
		Embedding:     emb,
		EmbeddingNorm: norm,
		QualityFlag:   flag,
	}
}

// selectQualityFlag picks the highest-severity flag for a row.
// Priority: nan_replaced > zero_vector > outlier_norm > empty_content.
func selectQualityFlag(hasNaN, isZero, isOutlier, isEmpty bool) string {
	switch {
	case hasNaN:
		return "nan_replaced"
	case isZero:
		return "zero_vector"
	case isOutlier:
		return "outlier_norm"
	case isEmpty:
		return "empty_content"
	default:
		return ""
	}
}

// containsNaNOrInf returns true if any element is NaN or Inf.
func containsNaNOrInf(v []float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return true
		}
	}
	return false
}

// l2Norm computes the L2 (Euclidean) norm of a vector.
func l2Norm(v []float64) float64 {
	var sum float64
	for _, x := range v {
		sum += x * x
	}
	return math.Sqrt(sum)
}

// toFloat32Slice converts []float64 to []float32.
// Values exceeding float32 range become +/-Inf per IEEE 754.
// This is acceptable because the NaN/Inf check runs on the original
// float64 values, and float32 overflow from valid float64 values is
// only possible with extreme embedding magnitudes (>3.4e38) which
// would already be flagged as outlier_norm.
func toFloat32Slice(v []float64) []float32 {
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(x)
	}
	return out
}

// welfordState implements Welford's online algorithm for running mean/variance.
type welfordState struct {
	n    int
	mean float64
	m2   float64
	min  float64
	max  float64
}

func (w *welfordState) update(x float64) {
	w.n++
	if w.n == 1 {
		w.min = x
		w.max = x
	} else {
		if x < w.min {
			w.min = x
		}
		if x > w.max {
			w.max = x
		}
	}
	delta := x - w.mean
	w.mean += delta / float64(w.n)
	delta2 := x - w.mean
	w.m2 += delta * delta2
}

// finalize computes population standard deviation (not sample).
// Population stddev is used because we have the complete exported dataset,
// not a sample from a larger population.
func (w *welfordState) finalize() NormStats {
	if w.n == 0 {
		return NormStats{}
	}
	var stddev float64
	if w.n > 1 {
		stddev = math.Sqrt(w.m2 / float64(w.n))
	}
	return NormStats{
		Min:    w.min,
		Max:    w.max,
		Mean:   w.mean,
		StdDev: stddev,
	}
}
