package main

import (
	"context"
	"fmt"
	"strings"
)

// Score captures the evaluation dimensions for a single (model, trace) run.
// See docs/llm/benchmark-plan.md for the scoring rationale.
type Score struct {
	ToolSequenceMatch float64 // [0,1] — how close actual tool calls were to the golden sequence
	ToolArgsValid     float64 // [0,1] — fraction of tool calls with valid arguments
	AnswerQuality     float64 // [0,1] — final-answer quality per the active Scorer
	LatencyMs         int64
	TotalTokens       int
	Notes             string
}

// Scorer is the pluggable strategy for evaluating a replay result.
//
// The AnswerQuality dimension is load-bearing: how it's computed
// determines whether the harness is trustworthy enough to override public
// benchmark narratives. The three shipped strategies trade cost, quality
// of judgment, and reproducibility differently — see benchmark-plan.md.
type Scorer interface {
	Score(ctx context.Context, trace Trace, actual Result) (Score, error)
}

// newScorer returns the Scorer matching the given name.
func newScorer(name string) (Scorer, error) {
	switch name {
	case "exact-match":
		return &ExactMatchScorer{}, nil
	case "llm-judge":
		return nil, fmt.Errorf("llm-judge scorer not yet implemented (see docs/llm/benchmark-plan.md Phase 3)")
	case "manual":
		return nil, fmt.Errorf("manual scorer not yet implemented")
	default:
		return nil, fmt.Errorf("unknown scorer %q", name)
	}
}

// ExactMatchScorer is a dependency-free baseline. Answer quality is scored
// as 1.0 iff the golden substring appears in the assistant's response,
// 0.0 otherwise. Use it to bootstrap the harness; graduate to `llm-judge`
// before drawing conclusions.
type ExactMatchScorer struct{}

// Score implements Scorer. ToolArgsValid is left unset (zero) because the
// tool loop has not yet been wired in Phase 1, so per-call argument
// validation against trace.Tools schemas is not computable. The Notes
// field records this so aggregate consumers can distinguish "not scored"
// from "scored zero".
func (s *ExactMatchScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	needle := strings.TrimSpace(trace.Golden.FinalAnswerSubstring)
	if needle == "" {
		return Score{}, fmt.Errorf("trace %q: %w", trace.ID, errMissingGolden)
	}

	score := Score{
		ToolSequenceMatch: toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		Notes:             "ToolArgsValid not computed (tool loop pending; see benchmark-plan.md Phase 2)",
	}

	finalText := lastAssistantContent(actual.Transcript)
	if strings.Contains(finalText, needle) {
		score.AnswerQuality = 1.0
	} else {
		score.AnswerQuality = 0.0
	}

	return score, nil
}

// toolSequenceScore computes a simple Jaccard overlap between the expected
// and actual tool-call sequences, ignoring order for now. A Levenshtein-
// based sequence comparison is a follow-up once the tool loop records real
// ordered calls.
func toolSequenceScore(expected, actual []string) float64 {
	if len(expected) == 0 && len(actual) == 0 {
		return 1.0
	}
	if len(expected) == 0 || len(actual) == 0 {
		return 0.0
	}
	expSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expSet[e] = struct{}{}
	}
	union := make(map[string]struct{}, len(expected)+len(actual))
	for k := range expSet {
		union[k] = struct{}{}
	}
	intersect := 0
	for _, a := range actual {
		union[a] = struct{}{}
		if _, ok := expSet[a]; ok {
			intersect++
		}
	}
	if len(union) == 0 {
		return 0.0
	}
	return float64(intersect) / float64(len(union))
}

func extractToolNames(turns []Turn) []string {
	var names []string
	for _, t := range turns {
		for _, tc := range t.ToolCalls {
			names = append(names, tc.Name)
		}
	}
	return names
}

// lastAssistantContent returns the content of the final assistant turn, or
// "" if there is no assistant turn. It does NOT walk backward past an
// empty-content assistant turn to find a prior non-empty one — doing so
// would return stale content if the last action was a tool call with no
// final answer. Callers can distinguish "no final answer" from "answer
// didn't match" via an empty return.
func lastAssistantContent(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Content
		}
	}
	return ""
}
