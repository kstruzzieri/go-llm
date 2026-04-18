package main

import (
	"context"
	"strings"
	"testing"
)

func TestExactMatchScorerRequiresGoldenSubstring(t *testing.T) {
	s := &ExactMatchScorer{}
	trace := Trace{ID: "t1", Golden: Golden{FinalAnswerCriteria: "describe the bug"}}
	actual := Result{Transcript: []Turn{{Role: "assistant", Content: "some answer"}}}

	_, err := s.Score(context.Background(), trace, actual)
	if err == nil {
		t.Fatal("expected error when golden.final_answer_substring is missing, got nil")
	}
	if !strings.Contains(err.Error(), "final_answer_substring") {
		t.Errorf("error = %v, want mention of final_answer_substring", err)
	}
}

func TestExactMatchScorerSubstringHit(t *testing.T) {
	s := &ExactMatchScorer{}
	trace := Trace{
		ID: "t2",
		Golden: Golden{
			ToolCalls:            []string{"read_file"},
			FinalAnswerSubstring: "circular fallback",
		},
	}
	actual := Result{Transcript: []Turn{
		{Role: "assistant", Content: "the bug is a circular fallback between routers",
			ToolCalls: []ToolCall{{Name: "read_file"}}},
	}}

	score, err := s.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Errorf("AnswerQuality = %f, want 1.0", score.AnswerQuality)
	}
	if score.ToolSequenceMatch != 1.0 {
		t.Errorf("ToolSequenceMatch = %f, want 1.0", score.ToolSequenceMatch)
	}
}

func TestExactMatchScorerSubstringMiss(t *testing.T) {
	s := &ExactMatchScorer{}
	trace := Trace{
		ID:     "t3",
		Golden: Golden{FinalAnswerSubstring: "circular fallback"},
	}
	actual := Result{Transcript: []Turn{{Role: "assistant", Content: "looks fine"}}}

	score, err := s.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if score.AnswerQuality != 0.0 {
		t.Errorf("AnswerQuality = %f, want 0.0", score.AnswerQuality)
	}
}
