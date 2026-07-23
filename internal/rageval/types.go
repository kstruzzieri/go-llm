package rageval

import "time"

const (
	// SchemaVersion identifies the JSON shape of Report. Bump on incompatible
	// changes so future readers can detect drift between code and committed
	// baseline.json. v1 → v2 migration is unspecified until #95 ratifies the
	// agentic RAG public package surface.
	SchemaVersion = "rag-eval-baseline/v1"

	// vectorSpaceID isolates the eval fixture from any real corpus a developer
	// may have indexed locally.
	vectorSpaceID = "fixture/rag-eval-v1"

	// OwnerKeith identifies the human accountable for threshold values.
	OwnerKeith = "Keith"

	// StatusPendingOwnerBefore95 marks reports generated before owner ratification.
	StatusPendingOwnerBefore95 = "pending_owner_values_before_95"

	// StatusThresholdsRatified marks reports whose non-null thresholds are
	// owner-approved regression floors (not aspirational targets).
	StatusThresholdsRatified = "thresholds_ratified"

	// charsPerTokenEstimate is the heuristic rag.Retriever.BuildContext uses
	// to convert a maxTokens budget to a character budget. Mirror it here so
	// contextTokens() produces a comparable estimate.
	charsPerTokenEstimate = 4
)

// replayTimestamp freezes QueryContext.Timestamp passed into hybrid retrieval
// so the temporal scorer produces deterministic output across runs. The chosen
// value (2023-11-14 22:13:20 UTC) is arbitrary but must never change — updating
// it would invalidate every committed baseline's hybrid numbers.
var replayTimestamp = time.Unix(1_700_000_000, 0)

// floatPtr returns a pointer to v. Used to populate nullable ThresholdSummary
// fields without per-call temporary variables.
func floatPtr(v float64) *float64 { return &v }

// Fixture is the committed golden retrieval corpus and query set.
type Fixture struct {
	Corpus  []FixtureChunk `json:"corpus"`
	Queries []QueryFixture `json:"queries"`
}

// FixtureChunk is one indexed chunk in the synthetic evaluation corpus.
type FixtureChunk struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	StartLine int               `json:"start_line"`
	EndLine   int               `json:"end_line"`
	Language  string            `json:"language"`
	StableKey string            `json:"stable_key"`
	Content   string            `json:"content"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Embedding []float64         `json:"embedding"`
}

// QueryFixture is one golden retrieval question.
type QueryFixture struct {
	ID          string    `json:"id"`
	Category    string    `json:"category"`
	Query       string    `json:"query"`
	ExpectedIDs []string  `json:"expected_ids"`
	CurrentFile string    `json:"current_file,omitempty"`
	Embedding   []float64 `json:"embedding"`
}

// RunOptions controls baseline generation.
type RunOptions struct {
	WarmRuns       int
	MeasureLatency bool
}

// Report is the stable JSON output committed as the baseline artifact.
type Report struct {
	SchemaVersion string           `json:"schema_version"`
	Corpus        CorpusSummary    `json:"corpus"`
	Thresholds    ThresholdSummary `json:"thresholds"`
	Modes         []ModeReport     `json:"modes"`
}

type CorpusSummary struct {
	Chunks     int            `json:"chunks"`
	Queries    int            `json:"queries"`
	Categories map[string]int `json:"categories"`
	TopK       []int          `json:"top_k"`
	VectorID   string         `json:"vector_space_id"`
	Notes      []string       `json:"notes"`
}

type ThresholdSummary struct {
	Owner                             string   `json:"owner"`
	Status                            string   `json:"status"`
	MinimumStaticRecallAt5            *float64 `json:"minimum_static_recall_at_5"`
	MinimumHybridRecallAt5Improvement *float64 `json:"minimum_hybrid_recall_at_5_improvement"`
	MaximumDuplicateRate              *float64 `json:"maximum_duplicate_rate"`
	MinimumContextPrecisionProxy      *float64 `json:"minimum_context_precision_proxy"`
	MaximumOptInHybridP95LatencyMS    *float64 `json:"maximum_opt_in_hybrid_p95_latency_ms"`
	MaximumFutureDefaultP95LatencyMS  *float64 `json:"maximum_future_default_p95_latency_ms"`
	MaximumAverageContextTokenGrowth  *float64 `json:"maximum_average_context_token_growth"`
}

type ModeReport struct {
	Name    string        `json:"name"`
	Summary ModeSummary   `json:"summary"`
	Queries []QueryReport `json:"queries"`
}

type ModeSummary struct {
	RecallAt5                float64        `json:"recall_at_5"`
	RecallAt10               float64        `json:"recall_at_10"`
	MRRAt5                   float64        `json:"mrr_at_5"`
	MRRAt10                  float64        `json:"mrr_at_10"`
	DuplicateRateAt10        float64        `json:"duplicate_rate_at_10"`
	ContextPrecisionAt5      float64        `json:"context_precision_at_5"`
	ContextPrecisionAt10     float64        `json:"context_precision_at_10"`
	AverageContextTokensAt5  float64        `json:"average_context_tokens_at_5"`
	AverageContextTokensAt10 float64        `json:"average_context_tokens_at_10"`
	ColdLatencyMS            LatencySummary `json:"cold_latency_ms"`
	WarmLatencyMS            LatencySummary `json:"warm_latency_ms"`
}

type QueryReport struct {
	ID            string         `json:"id"`
	Category      string         `json:"category"`
	ExpectedIDs   []string       `json:"expected_ids"`
	ResultIDs     []string       `json:"result_ids"`
	Metrics       []KMetrics     `json:"metrics"`
	ColdLatencyMS float64        `json:"cold_latency_ms"`
	WarmLatencyMS LatencySummary `json:"warm_latency_ms"`
}

type KMetrics struct {
	K                     int     `json:"k"`
	Recall                float64 `json:"recall"`
	MRR                   float64 `json:"mrr"`
	DuplicateRate         float64 `json:"duplicate_rate"`
	ContextPrecisionProxy float64 `json:"context_precision_proxy"`
	ContextTokens         int     `json:"context_tokens"`
}

type LatencySummary struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50"`
	P95   float64 `json:"p95"`
}

func defaultRunOptions(opts RunOptions) RunOptions {
	if opts.WarmRuns < 0 {
		opts.WarmRuns = 0
	}
	return opts
}

func elapsedMS(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / float64(time.Millisecond)
}
