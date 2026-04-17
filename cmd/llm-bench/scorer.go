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

// Score implements Scorer.
func (s *ExactMatchScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	score := Score{
		ToolSequenceMatch: toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		ToolArgsValid:     1.0, // TODO: validate against trace.Tools JSON schemas once the tool loop is wired
	}

	finalText := lastAssistantContent(actual.Transcript)
	needle := strings.TrimSpace(trace.Golden.FinalAnswerSubstring)
	if needle == "" {
		score.AnswerQuality = 0.5
		score.Notes = "no FinalAnswerSubstring in golden; defaulting AnswerQuality=0.5"
	} else if strings.Contains(finalText, needle) {
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

func lastAssistantContent(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && turns[i].Content != "" {
			return turns[i].Content
		}
	}
	return ""
}
