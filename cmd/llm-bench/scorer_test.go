package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

type fakeJudgeClient struct {
	req    ollama.ChatRequest
	resp   *ollama.ChatResponse
	err    error
	called int
}

func (f *fakeJudgeClient) Chat(_ context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	f.called++
	f.req = req
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

type fakeJudgeModelChecker struct {
	models []string
	err    error
}

func (f fakeJudgeModelChecker) AvailableModels(context.Context) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.models, nil
}

func TestExactMatchScorerRequiresGoldenSubstring(t *testing.T) {
	s := &ExactMatchScorer{}
	trace := Trace{ID: "t1", Golden: Golden{FinalAnswerCriteria: "describe the bug"}}
	actual := Result{Transcript: []Turn{{Role: "assistant", Content: "some answer"}}}

	_, err := s.Score(context.Background(), trace, actual)
	if !errors.Is(err, errMissingGolden) {
		t.Fatalf("err = %v, want errMissingGolden", err)
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

func TestToolSequenceScoreDeduplicatesActualToolNames(t *testing.T) {
	score := toolSequenceScore([]string{"read_file"}, []string{"read_file", "read_file"})
	if score != 1.0 {
		t.Fatalf("toolSequenceScore() = %f, want 1.0", score)
	}
}

func TestLLMJudgeScorerScoresFromJSON(t *testing.T) {
	client := &fakeJudgeClient{
		resp: &ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Role:    "assistant",
				Content: `{"answer_quality":0.75,"justification":"Identifies the main issue but misses the fallback detail."}`,
			},
		},
	}
	s, err := newLLMJudgeScorer(client, "gemma4:31b", 0)
	if err != nil {
		t.Fatalf("newLLMJudgeScorer() error: %v", err)
	}

	trace := Trace{
		ID:     "judge-hit",
		System: "review code",
		Turns:  []Turn{{Role: "user", Content: "Find the bug"}},
		Golden: Golden{
			ToolCalls:           []string{"read_file"},
			FinalAnswerCriteria: "Identifies the circular fallback bug",
		},
	}
	actual := Result{
		Model: "qwen3-coder-next:latest",
		Transcript: []Turn{{
			Role:    "assistant",
			Content: "There is a circular fallback between router candidates.",
			ToolCalls: []ToolCall{{
				Name: "read_file",
			}},
		}},
	}

	score, err := s.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score() error: %v", err)
	}
	if score.AnswerQuality != 0.75 {
		t.Fatalf("AnswerQuality = %f, want 0.75", score.AnswerQuality)
	}
	if score.ToolSequenceMatch != 1.0 {
		t.Fatalf("ToolSequenceMatch = %f, want 1.0", score.ToolSequenceMatch)
	}
	if !strings.Contains(score.Notes, "llm-judge=gemma4:31b") {
		t.Fatalf("Notes = %q, want judge model note", score.Notes)
	}
	if client.called != 1 {
		t.Fatalf("judge client called %d times, want 1", client.called)
	}
	if client.req.Model != "gemma4:31b" {
		t.Fatalf("judge model = %q, want gemma4:31b", client.req.Model)
	}
	if client.req.Format != "json" {
		t.Fatalf("judge format = %q, want json", client.req.Format)
	}
	if client.req.Options == nil {
		t.Fatal("judge options = nil, want deterministic-ish judge options")
	}
	if client.req.Options.Temperature != judgeTemperature {
		t.Fatalf("judge temperature = %f, want %f", client.req.Options.Temperature, judgeTemperature)
	}
	if client.req.Options.NumPredict != judgeTokenBudget {
		t.Fatalf("judge num_predict = %d, want %d", client.req.Options.NumPredict, judgeTokenBudget)
	}
	if client.req.KeepAlive != benchKeepAlive {
		t.Fatalf("judge keep_alive = %q, want %q", client.req.KeepAlive, benchKeepAlive)
	}
}

func TestLLMJudgeScorerRejectsSelfJudging(t *testing.T) {
	client := &fakeJudgeClient{
		resp: &ollama.ChatResponse{Message: ollama.ChatMessage{Content: `{"answer_quality":1,"justification":"ok"}`}},
	}
	s, err := newLLMJudgeScorer(client, "gemma4:31b", 0)
	if err != nil {
		t.Fatalf("newLLMJudgeScorer() error: %v", err)
	}

	trace := Trace{
		ID:     "self-judge",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "answer correctly"},
	}
	actual := Result{
		Model:      "ollama/gemma4:31b",
		Transcript: []Turn{{Role: "assistant", Content: "answer"}},
	}

	_, err = s.Score(context.Background(), trace, actual)
	if !errors.Is(err, errJudgeSelfPreference) {
		t.Fatalf("err = %v, want errJudgeSelfPreference", err)
	}
	if client.called != 0 {
		t.Fatalf("judge client called despite self-judging guard")
	}
}

func TestLLMJudgeScorerRejectsNamespacedSelfJudging(t *testing.T) {
	client := &fakeJudgeClient{
		resp: &ollama.ChatResponse{Message: ollama.ChatMessage{Content: `{"answer_quality":1,"justification":"ok"}`}},
	}
	s, err := newLLMJudgeScorer(client, "hf.co/org/model:tag", 0)
	if err != nil {
		t.Fatalf("newLLMJudgeScorer() error: %v", err)
	}

	trace := Trace{
		ID:     "namespaced-self-judge",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "answer correctly"},
	}
	actual := Result{
		Model:      "ollama/hf.co/org/model:tag",
		Transcript: []Turn{{Role: "assistant", Content: "answer"}},
	}

	_, err = s.Score(context.Background(), trace, actual)
	if !errors.Is(err, errJudgeSelfPreference) {
		t.Fatalf("err = %v, want errJudgeSelfPreference", err)
	}
	if client.called != 0 {
		t.Fatalf("judge client called despite namespaced self-judging guard")
	}
}

func TestLLMJudgeScorerRequiresRubric(t *testing.T) {
	s, err := newLLMJudgeScorer(&fakeJudgeClient{}, "gemma4:31b", 0)
	if err != nil {
		t.Fatalf("newLLMJudgeScorer() error: %v", err)
	}

	trace := Trace{
		ID:     "missing-rubric",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
	}
	actual := Result{
		Model:      "qwen3:8b",
		Transcript: []Turn{{Role: "assistant", Content: "answer"}},
	}

	_, err = s.Score(context.Background(), trace, actual)
	if !errors.Is(err, errMissingJudgeCriteria) {
		t.Fatalf("err = %v, want errMissingJudgeCriteria", err)
	}
}

func TestLLMJudgeScorerErrorsOnEmptyFinalAnswer(t *testing.T) {
	client := &fakeJudgeClient{}
	s, err := newLLMJudgeScorer(client, "gemma4:31b", 0)
	if err != nil {
		t.Fatalf("newLLMJudgeScorer() error: %v", err)
	}

	trace := Trace{
		ID:     "empty-answer",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "q"}},
		Golden: Golden{FinalAnswerCriteria: "answer correctly"},
	}
	actual := Result{
		Model:      "qwen3:8b",
		Transcript: []Turn{{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}}},
	}

	_, err = s.Score(context.Background(), trace, actual)
	if !errors.Is(err, errNoAssistantFinalAnswer) {
		t.Fatalf("err = %v, want errNoAssistantFinalAnswer", err)
	}
	if client.called != 0 {
		t.Fatalf("judge client called for empty answer")
	}
}

func TestValidateJudgeModelRequiresAvailableModel(t *testing.T) {
	err := validateJudgeModel(context.Background(), fakeJudgeModelChecker{
		models: []string{"qwen3:8b"},
	}, "gemma4:31b")
	if err == nil {
		t.Fatal("validateJudgeModel() error = nil, want unavailable model error")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %q, want not available", err.Error())
	}
}

func TestValidateJudgeModelAcceptsProviderQualifiedSelector(t *testing.T) {
	err := validateJudgeModel(context.Background(), fakeJudgeModelChecker{
		models: []string{"gemma4:31b"},
	}, "ollama/gemma4:31b")
	if err != nil {
		t.Fatalf("validateJudgeModel() error: %v", err)
	}
}

func TestValidateJudgeModelPreservesNamespacedModelIDs(t *testing.T) {
	err := validateJudgeModel(context.Background(), fakeJudgeModelChecker{
		models: []string{"hf.co/org/model:tag"},
	}, "ollama/hf.co/org/model:tag")
	if err != nil {
		t.Fatalf("validateJudgeModel() error: %v", err)
	}

	err = validateJudgeModel(context.Background(), fakeJudgeModelChecker{
		models: []string{"hf.co/other/model:tag"},
	}, "ollama/hf.co/org/model:tag")
	if err == nil {
		t.Fatal("validateJudgeModel() error = nil, want distinct namespaced model rejection")
	}
}

func TestBuildJudgePromptStripsRawAndTruncatesLargeTurns(t *testing.T) {
	large := strings.Repeat("x", maxJudgeTurnContentBytes+100)
	trace := Trace{
		ID:     "large-trace",
		System: "sys",
		Turns: []Turn{{
			Role:    "tool",
			Content: large,
			Raw:     json.RawMessage(`{"large":"raw payload should not be included"}`),
		}},
		Golden: Golden{FinalAnswerCriteria: "answer correctly"},
	}
	actual := Result{
		Model:      "qwen3:8b",
		Transcript: []Turn{{Role: "assistant", Content: "final"}},
	}

	prompt, err := buildJudgePrompt(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgePrompt() error: %v", err)
	}
	if strings.Contains(prompt, "raw payload should not be included") {
		t.Fatalf("judge prompt includes raw payload: %s", prompt)
	}
	if !strings.Contains(prompt, `"content_truncated": true`) {
		t.Fatalf("judge prompt does not mark truncated content: %s", prompt)
	}
}

func TestParseJudgeResponseExtractsJSON(t *testing.T) {
	got, err := parseJudgeResponse("```json\n{\"answer_quality\":0.5,\"justification\":\"partly correct\"}\n```")
	if err != nil {
		t.Fatalf("parseJudgeResponse() error: %v", err)
	}
	if got.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %f, want 0.5", got.AnswerQuality)
	}
	if got.Justification != "partly correct" {
		t.Fatalf("Justification = %q, want partly correct", got.Justification)
	}
}

func TestParseJudgeResponseIgnoresTrailingText(t *testing.T) {
	got, err := parseJudgeResponse("{\"answer_quality\":0.25,\"justification\":\"weak\"}\nextra text")
	if err != nil {
		t.Fatalf("parseJudgeResponse() error: %v", err)
	}
	if got.AnswerQuality != 0.25 {
		t.Fatalf("AnswerQuality = %f, want 0.25", got.AnswerQuality)
	}
}

func TestParseJudgeResponseRejectsOutOfRangeScore(t *testing.T) {
	_, err := parseJudgeResponse(`{"answer_quality":1.2,"justification":"too high"}`)
	if !errors.Is(err, errMalformedJudgeResponse) {
		t.Fatalf("err = %v, want errMalformedJudgeResponse", err)
	}
}

func TestParseJudgeResponseRejectsNegativeScore(t *testing.T) {
	_, err := parseJudgeResponse(`{"answer_quality":-0.1,"justification":"too low"}`)
	if !errors.Is(err, errMalformedJudgeResponse) {
		t.Fatalf("err = %v, want errMalformedJudgeResponse", err)
	}
}
