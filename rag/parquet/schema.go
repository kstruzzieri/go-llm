package parquet

// DType specifies the floating-point precision for exported embeddings.
type DType int

const (
	// Float32 is the default ML-native precision.
	Float32 DType = iota
	// Float64 provides full precision, opt-in.
	Float64
)

// String returns the human-readable name of the DType.
func (d DType) String() string {
	switch d {
	case Float32:
		return "float32"
	case Float64:
		return "float64"
	default:
		return "unknown"
	}
}

// embeddingRowF32 is the Parquet row schema for float32 embeddings.
type embeddingRowF32 struct {
	ID            string    `parquet:"id"`
	Content       string    `parquet:"content"`
	Source        string    `parquet:"source"`
	StartLine     int32     `parquet:"start_line"`
	EndLine       int32     `parquet:"end_line"`
	Language      string    `parquet:"language"`
	Embedding     []float32 `parquet:"embedding,list"`
	EmbeddingNorm float32   `parquet:"embedding_norm"`
	QualityFlag   string    `parquet:"quality_flag"`
}

// embeddingRowF64 is the Parquet row schema for float64 embeddings.
type embeddingRowF64 struct {
	ID            string    `parquet:"id"`
	Content       string    `parquet:"content"`
	Source        string    `parquet:"source"`
	StartLine     int32     `parquet:"start_line"`
	EndLine       int32     `parquet:"end_line"`
	Language      string    `parquet:"language"`
	Embedding     []float64 `parquet:"embedding,list"`
	EmbeddingNorm float64   `parquet:"embedding_norm"`
	QualityFlag   string    `parquet:"quality_flag"`
}
