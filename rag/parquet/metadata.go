package parquet

import (
	"fmt"
	"time"
)

// Version is the go-llm version stamped into exported Parquet files.
const Version = "0.1.0"

// DatasetInfo describes an exported Parquet dataset.
type DatasetInfo struct {
	RowCount      int
	EmbeddingDim  int
	DType         DType
	FileSizeBytes int64
	ExportedAt    time.Time
	Filters       FilterSummary
	Quality       DatasetQuality
}

// FilterSummary records which filters were applied during export.
type FilterSummary struct {
	SourcePattern string
	Language      string
}

// DatasetQuality holds aggregate quality statistics for the exported dataset.
type DatasetQuality struct {
	TotalRows    int // all rows written (clean + flagged)
	CleanRows    int // rows with empty quality_flag
	FlaggedRows  int // rows with non-empty quality_flag
	ZeroVectors  int
	NaNReplaced  int
	EmptyContent int
	OutlierNorms int
	NormStats    NormStats // computed from clean rows only
}

// NormStats holds L2 norm statistics computed from clean rows.
type NormStats struct {
	Min    float64
	Max    float64
	Mean   float64
	StdDev float64
}

// buildMetadata converts export results into a string map for Parquet file-level metadata.
func buildMetadata(info *DatasetInfo) map[string]string {
	m := map[string]string{
		"go_llm.version":          Version,
		"go_llm.export_timestamp": info.ExportedAt.UTC().Format(time.RFC3339),
		"go_llm.dtype":            info.DType.String(),
		"go_llm.row_count":        fmt.Sprintf("%d", info.RowCount),
	}

	if info.EmbeddingDim > 0 {
		m["go_llm.embedding_dim"] = fmt.Sprintf("%d", info.EmbeddingDim)
	}

	if info.Filters.SourcePattern != "" {
		m["go_llm.filter.source_pattern"] = info.Filters.SourcePattern
		m["go_llm.filter.source_semantics"] = "sqlite_glob"
	}
	if info.Filters.Language != "" {
		m["go_llm.filter.language"] = info.Filters.Language
	}

	q := info.Quality
	m["go_llm.quality.clean_rows"] = fmt.Sprintf("%d", q.CleanRows)
	m["go_llm.quality.flagged_rows"] = fmt.Sprintf("%d", q.FlaggedRows)
	m["go_llm.quality.zero_vectors"] = fmt.Sprintf("%d", q.ZeroVectors)
	m["go_llm.quality.nan_replaced"] = fmt.Sprintf("%d", q.NaNReplaced)
	m["go_llm.quality.empty_content"] = fmt.Sprintf("%d", q.EmptyContent)
	m["go_llm.quality.outlier_norms"] = fmt.Sprintf("%d", q.OutlierNorms)

	if q.CleanRows > 0 {
		m["go_llm.quality.norm_mean"] = fmt.Sprintf("%.3f", q.NormStats.Mean)
		m["go_llm.quality.norm_stddev"] = fmt.Sprintf("%.3f", q.NormStats.StdDev)
	}

	return m
}

// buildMetadataWithModel adds the optional embedding model key.
func buildMetadataWithModel(info *DatasetInfo, model string) map[string]string {
	m := buildMetadata(info)
	if model != "" {
		m["go_llm.embedding_model"] = model
	}
	return m
}
