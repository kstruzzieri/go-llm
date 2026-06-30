package analysis

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/rag"
)

// SupportStatus classifies how well an answer (or a single claim) is supported
// by retrieved evidence.
type SupportStatus string

const (
	// StatusSupported means every checked claim is backed by evidence.
	StatusSupported SupportStatus = "supported"
	// StatusPartial means some claims are supported and some are not.
	StatusPartial SupportStatus = "partial"
	// StatusUnsupported means no claim is backed (or evidence contradicts one).
	StatusUnsupported SupportStatus = "unsupported"
)

// EvidenceRef identifies one retrieved chunk by the stable label (E1..) used in
// claim verdicts, so a SupportReport is self-describing once it leaves the call
// site.
type EvidenceRef struct {
	ID        string `json:"id"`         // E1.. label referenced by ClaimSupport.EvidenceIDs
	ChunkID   string `json:"chunk_id"`   // rag.Chunk.ID
	Source    string `json:"source"`     // file path
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ClaimSupport is the verdict for one atomic claim extracted from the answer.
type ClaimSupport struct {
	ID           string        `json:"id"`           // C1.. label assigned by the helper
	Claim        string        `json:"claim"`        // claim text owned by the helper
	Status       SupportStatus `json:"status"`
	EvidenceIDs  []string      `json:"evidence_ids"` // E1.. labels into SupportReport.Evidence
	Contradicted bool          `json:"contradicted"` // evidence actively refutes the claim
	Reason       string        `json:"reason"`       // why unsupported / what is missing
}

// SupportReport is the structured result of judging an answer against evidence.
type SupportReport struct {
	Status                 SupportStatus  `json:"status"`
	Claims                 []ClaimSupport `json:"claims"`
	Evidence               []EvidenceRef  `json:"evidence"`
	MissingEvidence        []string       `json:"missing_evidence"`
	MissingEvidenceQueries []string       `json:"missing_evidence_queries"`
}

// ErrSupportVerifyMalformed is returned when the verify stage produces output
// that cannot be parsed into per-claim verdicts. A verifier failure is never
// silently reported as "unsupported".
var ErrSupportVerifyMalformed = errors.New("analysis: verify stage produced malformed output")

// defaultMaxEvidenceChars bounds the evidence text included in the verify
// prompt unless overridden by WithMaxEvidenceChars.
const defaultMaxEvidenceChars = 6000

// supportConfig holds per-call options for Judge.
type supportConfig struct {
	maxEvidenceChars int
}

// SupportOption configures a Judge call.
type SupportOption func(*supportConfig)

// WithMaxEvidenceChars bounds the total characters of evidence text included in
// the verify prompt (default 6000). A value <= 0 leaves the default in place.
func WithMaxEvidenceChars(n int) SupportOption {
	return func(cfg *supportConfig) {
		if n > 0 {
			cfg.maxEvidenceChars = n
		}
	}
}

// SupportJudge evaluates whether an answer is grounded in retrieved evidence
// using a two-stage extract -> verify pipeline. It is safe for concurrent use.
type SupportJudge struct {
	chat  ChatFunc
	model string
}

// NewSupportJudgeWithChat builds a SupportJudge over a router-aware ChatFunc.
// model is an optional explicit pin: "" routes each stage by its use-case
// (config.UseCaseExtract / config.UseCaseVerify) via RoleForUseCase; a non-empty
// value pins both stages to that model. Returns an error for a nil chat.
func NewSupportJudgeWithChat(chat ChatFunc, model string) (*SupportJudge, error) {
	if chat == nil {
		return nil, fmt.Errorf("analysis: new support judge: chat is required")
	}
	return &SupportJudge{chat: chat, model: model}, nil
}

// Judge runs the two-stage pipeline and returns a structured SupportReport.
// Stages are added in later tasks; this stub validates input only.
func (j *SupportJudge) Judge(ctx context.Context, answer string, evidence []rag.SearchResult, opts ...SupportOption) (*SupportReport, error) {
	if answer == "" {
		return nil, fmt.Errorf("analysis: judge support: answer is required")
	}
	cfg := &supportConfig{maxEvidenceChars: defaultMaxEvidenceChars}
	for _, opt := range opts {
		opt(cfg)
	}
	_ = ctx
	_ = evidence
	return &SupportReport{Status: StatusUnsupported}, nil
}
