package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/rag"
)

// normalizeWS collapses internal whitespace runs to single spaces and trims
// both ends. strings.Fields splits on any Unicode whitespace and drops empties.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quoteInChunk reports whether quote appears in the chunk's raw content after
// conservative whitespace normalization. Case-sensitive (code is). Matches
// against rag.Chunk.Content, never BuildContext output: BuildContext prefixes
// each line with "<n>| " (PR #227 line anchors), which would corrupt matching.
func quoteInChunk(chunk rag.Chunk, quote string) bool {
	q := normalizeWS(quote)
	if q == "" {
		return false
	}
	return strings.Contains(normalizeWS(chunk.Content), q)
}

// extractJSONObjects returns every top-level {...} object found in s, in order.
// It tracks string state and backslash escapes so braces inside JSON strings do
// not affect nesting depth. Text between objects is ignored. Byte iteration is
// sufficient because the structural characters are all ASCII.
func extractJSONObjects(s string) []json.RawMessage {
	var out []json.RawMessage
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
					out = append(out, json.RawMessage(s[start:i+1]))
					start = -1
				}
			}
		}
	}
	return out
}

// modelAnswer is the ONLY shape the model is asked to produce. status,
// answer_found, sources, and verified are server-owned and never requested.
type modelAnswer struct {
	Answer   string          `json:"answer"`
	Evidence []modelEvidence `json:"evidence"`
}

type modelEvidence struct {
	ID    string `json:"id"`    // short prompt-assigned label E1..En, not the SHA chunk ID
	Quote string `json:"quote"` // span the model claims to have copied verbatim
}

// parseModelAnswer returns the LAST extracted JSON object that decodes into a
// modelAnswer carrying at least an "answer" or "evidence" key. Local models
// sometimes emit example/scaffolding JSON before the real answer, so the final
// valid object is the answer attempt.
func parseModelAnswer(text string) (modelAnswer, bool) {
	var best modelAnswer
	found := false
	for _, raw := range extractJSONObjects(text) {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			continue
		}
		_, hasAnswer := keys["answer"]
		_, hasEvidence := keys["evidence"]
		if !hasAnswer && !hasEvidence {
			continue
		}
		var ma modelAnswer
		if err := json.Unmarshal(raw, &ma); err != nil {
			continue
		}
		best, found = ma, true
	}
	return best, found
}

const (
	statusAnswerFound           = "answer_found"
	statusUnverifiedAnswer      = "unverified_answer"
	statusNotInRetrievedContext = "not_in_retrieved_context"
	statusMalformedOutput       = "malformed_output"
	statusRetrievalMiss         = "retrieval_miss"  // #230
	statusCorpusAbsent          = "corpus_absent"   // #230
	statusNoUsefulTerms         = "no_useful_terms" // #230
)

// evidenceBlock pairs a prompt-assigned label (E1..En) with the retrieved chunk
// it represents, so verification can match a cited quote against the exact chunk.
type evidenceBlock struct {
	ID    string
	Chunk rag.Chunk
}

type answerSource struct {
	Source    string `json:"source"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type answerQuote struct {
	ID        string `json:"id"`
	Source    string `json:"source"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Text      string `json:"text"`
}

type answerDiagnostics struct {
	RetrievedChunkIDs       []string `json:"retrieved_chunk_ids,omitempty"`
	EvidenceIDs             []string `json:"evidence_ids,omitempty"`
	VerifiedQuoteIDs        []string `json:"verified_quote_ids,omitempty"`
	CandidateAnswer         string   `json:"candidate_answer,omitempty"`
	KeywordTerms            []string `json:"keyword_terms,omitempty"`              // #230
	CorpusMatchCount        *int     `json:"corpus_match_count,omitempty"`         // #230
	CorpusOutsideMatchCount *int     `json:"corpus_outside_match_count,omitempty"` // #230
}

type ragAnswerResult struct {
	Status             string             `json:"status"`
	AnswerFound        bool               `json:"answer_found"`
	Answer             string             `json:"answer"`
	Sources            []answerSource     `json:"sources"`
	Quotes             []answerQuote      `json:"quotes"`
	Verified           bool               `json:"verified"`
	VerificationErrors []string           `json:"verification_errors"`
	Diagnostics        *answerDiagnostics `json:"diagnostics,omitempty"`
}

// deriveAnswer verifies the model's cited evidence against the built blocks and
// produces the audited result for the #229 status set. A returned status of
// statusNotInRetrievedContext marks the model-declined branch that #230 refines.
func deriveAnswer(ma modelAnswer, blocks []evidenceBlock) ragAnswerResult {
	byID := make(map[string]rag.Chunk, len(blocks))
	for _, bl := range blocks {
		byID[bl.ID] = bl.Chunk
	}

	res := ragAnswerResult{
		Sources:            []answerSource{},
		Quotes:             []answerQuote{},
		VerificationErrors: []string{},
	}

	if strings.TrimSpace(ma.Answer) == "" {
		// Model declined; no-answer branch (#230 may refine).
		res.Status = statusNotInRetrievedContext
		return res
	}

	seenSource := make(map[string]bool)
	for _, ev := range ma.Evidence {
		chunk, ok := byID[ev.ID]
		if !ok {
			res.VerificationErrors = append(res.VerificationErrors, fmt.Sprintf("unknown id %q", ev.ID))
			continue
		}
		if !quoteInChunk(chunk, ev.Quote) {
			res.VerificationErrors = append(res.VerificationErrors, fmt.Sprintf("quote not found in %s", ev.ID))
			continue
		}
		res.Quotes = append(res.Quotes, answerQuote{
			ID: ev.ID, Source: chunk.Source, StartLine: chunk.StartLine, EndLine: chunk.EndLine, Text: ev.Quote,
		})
		key := fmt.Sprintf("%s:%d-%d", chunk.Source, chunk.StartLine, chunk.EndLine)
		if !seenSource[key] {
			seenSource[key] = true
			res.Sources = append(res.Sources, answerSource{Source: chunk.Source, StartLine: chunk.StartLine, EndLine: chunk.EndLine})
		}
	}

	if len(res.Quotes) > 0 {
		res.Status = statusAnswerFound
		res.AnswerFound = true
		res.Verified = true
		res.Answer = strings.TrimSpace(ma.Answer) // public answer only when verified
	} else {
		res.Status = statusUnverifiedAnswer // answer given but unsupported (terminal)
	}
	return res
}
