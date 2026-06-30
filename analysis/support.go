package analysis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
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

// topLevelJSONObjects returns each balanced, top-level {...} substring in s,
// honoring string literals and escapes so braces inside JSON strings do not
// break nesting. Order is preserved.
func topLevelJSONObjects(s string) []string {
	var out []string
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

// lastJSONObjectWith returns the raw bytes of the last top-level JSON object in
// reply that parses and contains key, or nil. Choosing the last match skips
// example/prefatory JSON the model may emit before its real answer.
func lastJSONObjectWith(reply, key string) []byte {
	objs := topLevelJSONObjects(reply)
	for i := len(objs) - 1; i >= 0; i-- {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(objs[i]), &probe); err != nil {
			continue
		}
		if _, ok := probe[key]; ok {
			return []byte(objs[i])
		}
	}
	return nil
}

const extractSystem = "You decompose an answer into atomic, independently-checkable factual claims. " +
	"Use only the answer text; do not add facts."

const extractInstruction = "Return ONLY JSON of the form {\"claims\": [\"claim 1\", \"claim 2\"]}. " +
	"Each claim must be one self-contained factual statement. No commentary, no markdown."

// extractReply is the expected JSON shape from the extract stage.
type extractReply struct {
	Claims []string `json:"claims"`
}

// extractClaims runs the extract stage and returns helper-labeled claims
// (C1..). Malformed or empty model output falls back to treating the whole
// answer as a single claim. Claims default to StatusUnsupported until verified.
func (j *SupportJudge) extractClaims(ctx context.Context, answer string) ([]ClaimSupport, error) {
	zero := 0.0
	resp, err := j.chat(ctx, config.UseCaseExtract, provider.ChatRequest{
		Model:   j.model,
		Options: provider.ModelOptions{Temperature: &zero},
		Messages: []provider.ChatMessage{
			{Role: "system", Content: extractSystem},
			{Role: "user", Content: "Answer:\n" + answer + "\n\n" + extractInstruction},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("analysis: judge support: extract: %w", err)
	}

	texts := parseClaimsLenient(resp.Content)
	if len(texts) == 0 {
		texts = []string{answer}
	}
	claims := make([]ClaimSupport, len(texts))
	for i, c := range texts {
		claims[i] = ClaimSupport{
			ID:     fmt.Sprintf("C%d", i+1),
			Claim:  c,
			Status: StatusUnsupported,
		}
	}
	return claims, nil
}

// parseClaimsLenient extracts non-empty claim strings from a model reply, or
// nil when nothing usable is present.
func parseClaimsLenient(reply string) []string {
	obj := lastJSONObjectWith(reply, "claims")
	if obj == nil {
		return nil
	}
	var er extractReply
	if err := json.Unmarshal(obj, &er); err != nil {
		return nil
	}
	var out []string
	for _, c := range er.Claims {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// buildEvidenceBlocks formats evidence as labeled blocks (E1..) for the verify
// prompt and returns the matching EvidenceRefs. Evidence is assumed ranked
// best-first; once the running size would exceed maxChars, the lower-ranked
// tail is dropped. The first block is always kept even if it alone exceeds the
// budget. maxChars <= 0 uses defaultMaxEvidenceChars.
func buildEvidenceBlocks(evidence []rag.SearchResult, maxChars int) (string, []EvidenceRef) {
	if maxChars <= 0 {
		maxChars = defaultMaxEvidenceChars
	}
	var b strings.Builder
	refs := make([]EvidenceRef, 0, len(evidence))
	for i, r := range evidence {
		id := fmt.Sprintf("E%d", i+1)
		block := fmt.Sprintf("%s: %s (lines %d-%d)\n%s\n\n", id, r.Chunk.Source, r.Chunk.StartLine, r.Chunk.EndLine, r.Chunk.Content)
		if i > 0 && b.Len()+len(block) > maxChars {
			break
		}
		b.WriteString(block)
		refs = append(refs, EvidenceRef{
			ID:        id,
			ChunkID:   r.Chunk.ID,
			Source:    r.Chunk.Source,
			StartLine: r.Chunk.StartLine,
			EndLine:   r.Chunk.EndLine,
		})
	}
	return b.String(), refs
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
