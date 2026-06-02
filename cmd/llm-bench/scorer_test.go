package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

type fakeJudgeClient struct {
	req    ollama.ChatRequest
	reqs   []ollama.ChatRequest
	resp   *ollama.ChatResponse
	resps  []*ollama.ChatResponse
	err    error
	called int
}

func (f *fakeJudgeClient) Chat(_ context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	f.called++
	f.req = req
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.resps) > 0 {
		idx := f.called - 1
		if idx >= len(f.resps) {
			idx = len(f.resps) - 1
		}
		return f.resps[idx], nil
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

func (f fakeJudgeModelChecker) ShowModel(_ context.Context, name string) (*ollama.ModelInfo, error) {
	return &ollama.ModelInfo{Name: name}, nil
}

type fakeJudgeChecker struct {
	available []string
	showFn    func(name string) (*ollama.ModelInfo, error)
}

func (f *fakeJudgeChecker) AvailableModels(_ context.Context) ([]string, error) {
	return f.available, nil
}

func (f *fakeJudgeChecker) ShowModel(_ context.Context, name string) (*ollama.ModelInfo, error) {
	if f.showFn == nil {
		return &ollama.ModelInfo{Name: name, Digest: ""}, nil
	}
	return f.showFn(name)
}

func TestNewScorerAppliesJudgeTimeoutToHTTPClient(t *testing.T) {
	const judgeTimeout = 25 * time.Millisecond
	const serverDelay = 250 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(serverDelay):
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []map[string]any{{"name": "gemma4:31b"}},
		})
	}))
	defer srv.Close()

	start := time.Now()
	_, err := newScorer(context.Background(), "llm-judge", scorerOptions{
		ollamaURL:    srv.URL,
		judgeModel:   "gemma4:31b",
		judgeTimeout: judgeTimeout,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("newScorer() error = nil, want timeout from judge HTTP client")
	}
	if elapsed >= serverDelay/2 {
		t.Fatalf("newScorer() returned after %s, want HTTP client timeout near %s", elapsed, judgeTimeout)
	}
}

type stubChatClient struct{}

func (stubChatClient) Chat(context.Context, ollama.ChatRequest) (*ollama.ChatResponse, error) {
	return nil, nil
}

func TestLLMJudgeBuildJudgeCallPopulatesToolArgsValidInBaseScore(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name": "search_code",
		"inputSchema": {"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}
	}`)}
	trace := Trace{
		ID:     "tj",
		System: "sys",
		Tools:  tools,
		Turns:  []Turn{{Role: "user", Content: "?"}},
		Golden: Golden{ToolCalls: []string{"search_code"}, FinalAnswerCriteria: "non-empty"},
	}
	actual := Result{
		Model: "ollama/x",
		Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{
				{Name: "search_code", Arguments: json.RawMessage(`{"query":"foo"}`)},
			}},
			{Role: "assistant", Content: "found"},
		},
	}
	scorer := &LLMJudgeScorer{Client: stubChatClient{}, JudgeModel: "ollama/gemma4:31b"}
	_, base, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	if !base.ToolArgsValidComputed {
		t.Fatalf("ToolArgsValidComputed=false; want true")
	}
	if base.ToolArgsValid != 1.0 {
		t.Fatalf("ToolArgsValid=%v; want 1.0", base.ToolArgsValid)
	}
	if strings.Contains(base.Notes, "schema validation pending") {
		t.Fatalf("Notes still references validation pending: %q", base.Notes)
	}
}

func TestLLMJudgeBuildJudgeCallIgnoresUnusedInvalidToolSchema(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{
			"name": "search_code",
			"inputSchema": {"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}
		}`),
		json.RawMessage(`{"name":"unused_bad","inputSchema":{"$ref":"http://attacker.example.com/schema.json"}}`),
	}
	trace := Trace{
		ID:     "tj-unused-bad",
		System: "sys",
		Tools:  tools,
		Turns:  []Turn{{Role: "user", Content: "?"}},
		Golden: Golden{ToolCalls: []string{"search_code"}, FinalAnswerCriteria: "non-empty"},
	}
	actual := Result{
		Model: "ollama/x",
		Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{
				{Name: "search_code", Arguments: json.RawMessage(`{"query":"foo"}`)},
			}},
			{Role: "assistant", Content: "found"},
		},
	}
	scorer := &LLMJudgeScorer{Client: stubChatClient{}, JudgeModel: "ollama/gemma4:31b"}
	_, base, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	if !base.ToolArgsValidComputed || base.ToolArgsValid != 1.0 {
		t.Fatalf("ToolArgsValid=%v computed=%v; want 1.0/true", base.ToolArgsValid, base.ToolArgsValidComputed)
	}
}

func TestExactMatchScorerPopulatesToolArgsValid(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name": "read_file",
		"inputSchema": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	}`)}
	trace := Trace{
		ID:     "t",
		System: "sys",
		Tools:  tools,
		Turns: []Turn{{Role: "user", Content: "?"}, {Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{}`)},
		}}},
		Golden: Golden{ToolCalls: []string{"read_file"}, FinalAnswerSubstring: "ok"},
	}
	actual := Result{
		Model: "ollama/x",
		Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{
				{Name: "read_file", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "assistant", Content: "ok"},
		},
	}

	score, err := (&ExactMatchScorer{}).Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !score.ToolArgsValidComputed {
		t.Fatalf("ToolArgsValidComputed=false; want true")
	}
	if score.ToolArgsValid != 0.0 {
		t.Fatalf("ToolArgsValid=%v; want 0.0 (missing required arg)", score.ToolArgsValid)
	}
	if strings.Contains(score.Notes, "schema validation pending") {
		t.Fatalf("Notes still claims validation pending: %q", score.Notes)
	}
}

func TestExactMatchScorerIgnoresUnusedInvalidToolSchema(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{
			"name": "read_file",
			"inputSchema": {"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
		}`),
		json.RawMessage(`{"name":"unused_bad","inputSchema":{"$ref":"http://attacker.example.com/schema.json"}}`),
	}
	trace := Trace{
		ID:     "unused-bad",
		System: "sys",
		Tools:  tools,
		Turns: []Turn{{Role: "user", Content: "?"}, {Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)},
		}}},
		Golden: Golden{ToolCalls: []string{"read_file"}, FinalAnswerSubstring: "ok"},
	}
	actual := Result{
		Model: "ollama/x",
		Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{
				{Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)},
			}},
			{Role: "assistant", Content: "ok"},
		},
	}

	score, err := (&ExactMatchScorer{}).Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !score.ToolArgsValidComputed || score.ToolArgsValid != 1.0 {
		t.Fatalf("ToolArgsValid=%v computed=%v; want 1.0/true", score.ToolArgsValid, score.ToolArgsValidComputed)
	}
}

func TestExactMatchScorerSkipsSchemaCompilationWhenNoActualToolCalls(t *testing.T) {
	trace := Trace{
		ID:     "unused-bad-no-calls",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"unused_bad","inputSchema":{"$ref":"http://attacker.example.com/schema.json"}}`),
		},
		Turns:  []Turn{{Role: "user", Content: "?"}},
		Golden: Golden{FinalAnswerSubstring: "ok"},
	}
	actual := Result{
		Model:      "ollama/x",
		Transcript: []Turn{{Role: "assistant", Content: "ok"}},
	}

	score, err := (&ExactMatchScorer{}).Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if !score.ToolArgsValidComputed || score.ToolArgsValid != 1.0 {
		t.Fatalf("ToolArgsValid=%v computed=%v; want 1.0/true", score.ToolArgsValid, score.ToolArgsValidComputed)
	}
}

func TestScoreToolArgsValidComputedFieldDefaultsZero(t *testing.T) {
	s := Score{}
	if s.ToolArgsValidComputed {
		t.Fatalf("zero value should be false")
	}
}

func TestScoreToolArgsValidComputedFieldAssignable(t *testing.T) {
	s := Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}
	if !s.ToolArgsValidComputed || s.ToolArgsValid != 1.0 {
		t.Fatalf("assignment lost data: %+v", s)
	}
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

func TestCaptureScorerAcceptsCriteriaOnlyTrace(t *testing.T) {
	s := &CaptureScorer{}
	trace := Trace{
		ID:     "criteria-only",
		Golden: Golden{FinalAnswerCriteria: "use the rubric, not a substring"},
	}
	actual := Result{Transcript: []Turn{{Role: "assistant", Content: "answer"}}}
	if _, err := s.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("CaptureScorer.Score() error = %v; want nil", err)
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

// TestLLMJudgeScorer_HappyPath_PreRefactorBaseline pins the end-to-end happy
// path behavior of LLMJudgeScorer.Score before the upcoming refactor splits
// it into helpers. Keep this test green through the refactor as a regression
// anchor.
func TestLLMJudgeScorer_HappyPath_PreRefactorBaseline(t *testing.T) {
	trace := Trace{
		ID:     "baseline-trace",
		System: "you are an assistant",
		Turns: []Turn{
			{Role: "user", Content: "what is 2+2?"},
		},
		Golden: Golden{
			ToolCalls:           nil,
			FinalAnswerCriteria: "exactly 4",
		},
	}
	actual := Result{
		Model:   "ollama/qwen3-coder-next:latest",
		TraceID: "baseline-trace",
		Transcript: []Turn{
			{Role: "assistant", Content: "the answer is 4"},
		},
	}
	judge := &fakeJudgeClient{
		resp: &ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Content: `{"answer_quality":1.0,"justification":"correct"}`,
			},
		},
	}
	scorer := &LLMJudgeScorer{
		Client:     judge,
		JudgeModel: "ollama/gemma4:31b",
	}
	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want 1.0", score.AnswerQuality)
	}
	if !strings.Contains(score.Notes, "correct") {
		t.Fatalf("Notes missing justification: %q", score.Notes)
	}
	if judge.req.Model != "gemma4:31b" {
		t.Fatalf("judge request model = %q; want gemma4:31b", judge.req.Model)
	}
}

func TestBuildJudgeCall_PopulatesBaseScoreAndRequest(t *testing.T) {
	trace := Trace{
		ID:     "t1",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "hi"}},
		Golden: Golden{
			ToolCalls:           []string{"read_file"},
			FinalAnswerCriteria: "fc",
		},
	}
	actual := Result{
		Model:   "ollama/cand",
		TraceID: "t1",
		Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
			{Role: "assistant", Content: "done"},
		},
	}
	scorer := &LLMJudgeScorer{Client: &fakeJudgeClient{}, JudgeModel: "ollama/judge"}
	req, base, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	if req.Model != "judge" {
		t.Fatalf("req.Model = %q; want judge", req.Model)
	}
	if req.Format != "json" {
		t.Fatalf("req.Format = %q; want json", req.Format)
	}
	if req.Think == nil || *req.Think {
		t.Fatalf("req.Think = %v; want explicit false", req.Think)
	}
	if base.ToolSequenceMatch != 1.0 {
		t.Fatalf("baseScore.ToolSequenceMatch = %v; want 1.0", base.ToolSequenceMatch)
	}
}

func TestBuildJudgeCall_UsesCategoricalJudgeContract(t *testing.T) {
	trace := Trace{
		ID:     "categorical-prompt",
		System: "sys",
		Turns:  []Turn{{Role: "user", Content: "question"}},
		Golden: Golden{FinalAnswerCriteria: "answer the question correctly"},
	}
	actual := Result{
		Model:      "ollama/candidate",
		TraceID:    "categorical-prompt",
		Transcript: []Turn{{Role: "assistant", Content: "answer"}},
	}
	scorer := &LLMJudgeScorer{Client: &fakeJudgeClient{}, JudgeModel: "ollama/judge"}

	req, _, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	system := req.Messages[0].Content
	for _, want := range []string{
		"answer_quality must be exactly one of `0.0`, `0.5`, or `1.0`",
		"0.0 means the answer is wrong, fabricated, absent, or materially misleading.",
		"0.5 means the answer is partially correct but missing important requirements, or contains a contained technical flaw.",
		"1.0 means the answer fully satisfies the rubric with no material technical error.",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("judge system prompt missing %q:\n%s", want, system)
		}
	}
	if strings.Contains(system, "must be a number from 0 to 1") {
		t.Fatalf("judge system prompt still allows continuous scores:\n%s", system)
	}
}

func TestBuildJudgeCall_StripsBenchProviderFromJudgeRequestModel(t *testing.T) {
	trace := Trace{ID: "t", System: "s", Turns: []Turn{{Role: "user", Content: "x"}}, Golden: Golden{FinalAnswerCriteria: "fc"}}
	actual := Result{Model: "ollama/cand", TraceID: "t", Transcript: []Turn{{Role: "assistant", Content: "y"}}}
	scorer := &LLMJudgeScorer{Client: &fakeJudgeClient{}, JudgeModel: "ollama/hf.co/org/model:tag"}

	req, _, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	if req.Model != "hf.co/org/model:tag" {
		t.Fatalf("req.Model = %q; want hf.co/org/model:tag", req.Model)
	}
}

func TestBuildJudgeCall_SelfPreferenceGuard(t *testing.T) {
	trace := Trace{ID: "t", System: "s", Turns: []Turn{{Role: "user", Content: "x"}}, Golden: Golden{FinalAnswerCriteria: "fc"}}
	actual := Result{Model: "ollama/same", TraceID: "t", Transcript: []Turn{{Role: "assistant", Content: "y"}}}
	scorer := &LLMJudgeScorer{Client: &fakeJudgeClient{}, JudgeModel: "ollama/same"}
	if _, _, err := scorer.buildJudgeCall(trace, actual); !errors.Is(err, errJudgeSelfPreference) {
		t.Fatalf("got %v; want errJudgeSelfPreference", err)
	}
}

func TestMaterializeJudgement_AppendsJustificationToNotes(t *testing.T) {
	base := Score{ToolSequenceMatch: 0.5, Notes: "pre-existing"}
	got, _, err := materializeJudgement(base, "ollama/gemma4:31b", `{"answer_quality":0.5,"justification":"missed caveat"}`)
	if err != nil {
		t.Fatalf("materializeJudgement: %v", err)
	}
	if got.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %v; want 0.5", got.AnswerQuality)
	}
	if got.ToolSequenceMatch != 0.5 {
		t.Fatalf("ToolSequenceMatch lost from base: %v", got.ToolSequenceMatch)
	}
	if !strings.Contains(got.Notes, "missed caveat") {
		t.Fatalf("Notes missing justification: %q", got.Notes)
	}
}

func TestLLMJudgeScorerScoresFromJSON(t *testing.T) {
	client := &fakeJudgeClient{
		resp: &ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Role:    "assistant",
				Content: `{"answer_quality":0.5,"justification":"Identifies the main issue but misses the fallback detail."}`,
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
	if score.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %f, want 0.5", score.AnswerQuality)
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

func TestResolveJudgeDigest_ReturnsDigestWhenAvailable(t *testing.T) {
	var gotName string
	checker := &fakeJudgeChecker{
		available: []string{"ollama/gemma4:31b"},
		showFn: func(name string) (*ollama.ModelInfo, error) {
			gotName = name
			return &ollama.ModelInfo{Name: name, Digest: "sha256:deadbeef"}, nil
		},
	}
	digest, err := resolveJudgeDigest(context.Background(), checker, "ollama/gemma4:31b")
	if err != nil {
		t.Fatalf("resolveJudgeDigest: %v", err)
	}
	if digest != "sha256:deadbeef" {
		t.Fatalf("digest = %q; want sha256:deadbeef", digest)
	}
	if gotName != "gemma4:31b" {
		t.Fatalf("ShowModel name = %q; want gemma4:31b", gotName)
	}
}

func TestResolveJudgeDigest_StripsOnlyBenchProvider(t *testing.T) {
	var gotName string
	checker := &fakeJudgeChecker{
		showFn: func(name string) (*ollama.ModelInfo, error) {
			gotName = name
			return &ollama.ModelInfo{Name: name, Digest: "sha256:namespaced"}, nil
		},
	}
	digest, err := resolveJudgeDigest(context.Background(), checker, "ollama/hf.co/org/model:tag")
	if err != nil {
		t.Fatalf("resolveJudgeDigest: %v", err)
	}
	if digest != "sha256:namespaced" {
		t.Fatalf("digest = %q; want sha256:namespaced", digest)
	}
	if gotName != "hf.co/org/model:tag" {
		t.Fatalf("ShowModel name = %q; want hf.co/org/model:tag", gotName)
	}
}

func TestResolveJudgeDigest_EmptyOnError(t *testing.T) {
	checker := &fakeJudgeChecker{
		showFn: func(name string) (*ollama.ModelInfo, error) {
			return nil, errors.New("api/show unavailable")
		},
	}
	digest, err := resolveJudgeDigest(context.Background(), checker, "ollama/gemma4:31b")
	if err != nil {
		t.Fatalf("resolveJudgeDigest: %v", err) // expected to swallow and return empty
	}
	if digest != "" {
		t.Fatalf("digest = %q; want empty on error", digest)
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

func TestParseJudgeResponseIgnoresTrailingTextForCategoricalScore(t *testing.T) {
	got, err := parseJudgeResponse("{\"answer_quality\":0.5,\"justification\":\"partial\"}\nextra text")
	if err != nil {
		t.Fatalf("parseJudgeResponse() error: %v", err)
	}
	if got.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %f, want 0.5", got.AnswerQuality)
	}
}

func TestParseJudgeResponseRejectsOffGridScore(t *testing.T) {
	_, err := parseJudgeResponse(`{"answer_quality":0.25,"justification":"between labels"}`)
	if !errors.Is(err, errOffGridJudgeScore) {
		t.Fatalf("err = %v, want errOffGridJudgeScore", err)
	}
}

func TestParseJudgeResponseRejectsScoreOnlyAlias(t *testing.T) {
	_, err := parseJudgeResponse(`{"score":1.0,"justification":"legacy alias"}`)
	if !errors.Is(err, errMalformedJudgeResponse) {
		t.Fatalf("err = %v, want errMalformedJudgeResponse", err)
	}
	if err != nil && strings.Contains(err.Error(), "off-grid") {
		t.Fatalf("err = %v, want schema error instead of off-grid repair", err)
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

// TestLLMJudgeScorer_CacheHit_ReusesContentButRecomputesToolSequenceMatch
// pins the load-bearing invariant of the cache integration: a cache hit
// reuses the judge's response content (so AnswerQuality and justification
// are stable), but ToolSequenceMatch is recomputed fresh from the current
// actual transcript so tool-loop changes can never be masked by a stale
// cache entry.
func TestLLMJudgeScorer_CacheHit_ReusesContentButRecomputesToolSequenceMatch(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{
		resp: &ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Content: `{"answer_quality":0.5,"justification":"meh"}`,
			},
		},
	}
	scorer := &LLMJudgeScorer{
		Client:           judge,
		JudgeModel:       "ollama/gemma4:31b",
		JudgeModelDigest: "sha256:abc",
		Cache:            c,
		Clock:            func() time.Time { return time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC) },
	}
	trace := Trace{
		ID:     "tx",
		System: "s",
		Turns:  []Turn{{Role: "user", Content: "hi"}},
		Golden: Golden{ToolCalls: []string{"read_file"}, FinalAnswerCriteria: "c"},
	}
	actualA := Result{Model: "ollama/cand", Transcript: []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
		{Role: "assistant", Content: "done"},
	}}
	s1, err := scorer.Score(context.Background(), trace, actualA)
	if err != nil {
		t.Fatalf("first Score: %v", err)
	}
	if s1.ToolSequenceMatch != 1.0 {
		t.Fatalf("first ToolSequenceMatch=%v; want 1.0", s1.ToolSequenceMatch)
	}
	if judge.called != 1 {
		t.Fatalf("judge called %d times; want 1", judge.called)
	}

	s2, err := scorer.Score(context.Background(), trace, actualA)
	if err != nil {
		t.Fatalf("second Score: %v", err)
	}
	if judge.called != 1 {
		t.Fatalf("judge called %d times after cache-hit; want still 1", judge.called)
	}
	if s2.AnswerQuality != s1.AnswerQuality {
		t.Fatalf("cache returned different AnswerQuality: %v vs %v", s2.AnswerQuality, s1.AnswerQuality)
	}
	if s2.ToolSequenceMatch != 1.0 {
		t.Fatalf("cache-hit ToolSequenceMatch=%v; want 1.0 (recomputed fresh)", s2.ToolSequenceMatch)
	}
}

// TestParseJudgeResponse_SkipsPrecedingNonVerdictObject reproduces the second
// live calibration failure ("missing answer_quality"): when judging code,
// Opus emitted a JSON-ish brace pair (an echoed struct literal / `{}`) BEFORE
// the verdict. The first balanced object must NOT win — the parser must select
// the object that actually carries answer_quality.
func TestParseJudgeResponse_SkipsPrecedingNonVerdictObject(t *testing.T) {
	content := "The candidate wrote `type T struct{}` and returned `{}`. Verdict:\n" +
		`{"answer_quality":0.0,"justification":"materially wrong"}`
	got, err := parseJudgeResponse(content)
	if err != nil {
		t.Fatalf("parseJudgeResponse: %v", err)
	}
	if got.AnswerQuality != 0.0 {
		t.Fatalf("answer_quality = %v; want 0.0", got.AnswerQuality)
	}
	if got.Justification != "materially wrong" {
		t.Fatalf("justification = %q; want \"materially wrong\"", got.Justification)
	}
}

// TestParseJudgeResponse_ToleratesLineComments reproduces the live calibration
// failure: a frontier judge (Opus) emitted a // line comment OUTSIDE the JSON
// (json.Unmarshal: "invalid character '/' looking for beginning of object key
// string"). Comments outside strings must be stripped, but a // that lives
// INSIDE the justification string (common when judging code) must survive.
func TestParseJudgeResponse_ToleratesLineComments(t *testing.T) {
	content := "{\n  \"answer_quality\": 1.0, // perfect, matches golden\n  \"justification\": \"candidate produced `// Open opens the store` verbatim\"\n}"
	got, err := parseJudgeResponse(content)
	if err != nil {
		t.Fatalf("parseJudgeResponse: %v", err)
	}
	if got.AnswerQuality != 1.0 {
		t.Fatalf("answer_quality = %v; want 1.0", got.AnswerQuality)
	}
	if !strings.Contains(got.Justification, "// Open opens the store") {
		t.Fatalf("justification lost the in-string // content: %q", got.Justification)
	}
}

// TestParseJudgeResponse_ToleratesBlockCommentsWithBraces pins that block
// comments are stripped AND that a brace inside a comment does not throw off
// the balanced-object scan (the comment-aware extractor must not count it).
func TestParseJudgeResponse_ToleratesBlockCommentsWithBraces(t *testing.T) {
	content := "```json\n{\n  /* note: a } here is not a real brace */\n  \"answer_quality\": 0.5,\n  \"justification\": \"partial\"\n}\n```"
	got, err := parseJudgeResponse(content)
	if err != nil {
		t.Fatalf("parseJudgeResponse: %v", err)
	}
	if got.AnswerQuality != 0.5 {
		t.Fatalf("answer_quality = %v; want 0.5", got.AnswerQuality)
	}
	if got.Justification != "partial" {
		t.Fatalf("justification = %q; want \"partial\"", got.Justification)
	}
}

// TestLLMJudgeScorer_CacheKeyIncludesProvider pins that two scorers differing
// only in JudgeProvider produce different cache keys, so a frontier judge's
// verdict can never reuse a same-named local Ollama judge's cached score.
func TestLLMJudgeScorer_CacheKeyIncludesProvider(t *testing.T) {
	req := ollama.ChatRequest{Messages: []ollama.ChatMessage{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
	}}
	ollamaJudge := &LLMJudgeScorer{JudgeModel: "m", JudgeProvider: "ollama"}
	compatJudge := &LLMJudgeScorer{JudgeModel: "m", JudgeProvider: "openai-compat"}
	if ollamaJudge.cacheKeyForJudgeRequest(req) == compatJudge.cacheKeyForJudgeRequest(req) {
		t.Fatalf("cache key did not vary with JudgeProvider")
	}
}

// TestLLMJudgeScorer_CachePutRecordsProvider pins that Score persists the
// scorer's JudgeProvider into the cache row for per-verdict provenance.
func TestLLMJudgeScorer_CachePutRecordsProvider(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":1.0,"justification":"good"}`,
	}}}
	scorer := &LLMJudgeScorer{
		Client:        judge,
		JudgeModel:    "claude-x",
		JudgeProvider: "openai-compat",
		Cache:         c,
	}
	trace := Trace{ID: "t-prov", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/cand", Transcript: []Turn{{Role: "assistant", Content: "ans"}}}
	if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("Score: %v", err)
	}
	var stored string
	if err := c.db.QueryRow(`SELECT judge_provider FROM judge_cache WHERE trace_id = ?`, "t-prov").Scan(&stored); err != nil {
		t.Fatalf("query judge_provider: %v", err)
	}
	if stored != "openai-compat" {
		t.Fatalf("stored judge_provider=%q; want openai-compat", stored)
	}
}

// TestLLMJudgeScorer_CachePutPreservesSemicolonsInJustification pins the
// fix for the brittle joined-Notes round-trip: a judge justification
// containing "; " (very common in real model output) must be persisted
// verbatim into the cache's justification column instead of being
// silently truncated at the first "; " segment boundary.
func TestLLMJudgeScorer_CachePutPreservesSemicolonsInJustification(t *testing.T) {
	c, _ := newTestCache(t)
	// Justification with embedded "; " — the kind that would have been
	// truncated by the old judgeJustificationFromNotes round-trip.
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":0.5,"justification":"covered A; missed B; also dropped C"}`,
	}}}
	scorer := &LLMJudgeScorer{
		Client:           judge,
		JudgeModel:       "ollama/gemma4:31b",
		JudgeModelDigest: "sha256:abc",
		Cache:            c,
	}
	trace := Trace{ID: "t-semi", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/cand", Transcript: []Turn{{Role: "assistant", Content: "ans"}}}
	if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("Score: %v", err)
	}
	// Read the row back via the test cache's underlying db.
	var stored string
	if err := c.db.QueryRow(`SELECT justification FROM judge_cache WHERE trace_id = ?`, "t-semi").Scan(&stored); err != nil {
		t.Fatalf("query justification: %v", err)
	}
	want := "covered A; missed B; also dropped C"
	if stored != want {
		t.Fatalf("Justification truncated: got %q, want %q", stored, want)
	}
}

// TestLLMJudgeScorer_BypassCache_AlwaysCallsJudgeAndDoesNotPersist pins the
// BypassCache=true contract: every Score call must invoke the judge (Get is
// skipped, so cached entries can never satisfy the call), and no row may
// ever be written to the cache (Put is also skipped). This guards against
// regressions where BypassCache short-circuits only one side of the cache.
func TestLLMJudgeScorer_BypassCache_AlwaysCallsJudgeAndDoesNotPersist(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":0.5,"justification":"x"}`,
	}}}
	scorer := &LLMJudgeScorer{
		Client:      judge,
		JudgeModel:  "ollama/gemma4:31b",
		Cache:       c,
		BypassCache: true,
	}
	trace := Trace{ID: "t", System: "s", Turns: []Turn{{Role: "user", Content: "h"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/cand", Transcript: []Turn{{Role: "assistant", Content: "a"}}}

	for i := 0; i < 3; i++ {
		if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
			t.Fatalf("Score #%d: %v", i, err)
		}
	}
	if judge.called != 3 {
		t.Fatalf("BypassCache=true should call judge each time; called=%d", judge.called)
	}
	// Verify nothing was written to the cache by querying any row count.
	var count int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM judge_cache`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("BypassCache=true persisted %d rows; want 0", count)
	}
}

func TestLLMJudgeScorer_OffGridRetriesOnceWithRepairPrompt(t *testing.T) {
	judge := &fakeJudgeClient{resps: []*ollama.ChatResponse{
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.7,"justification":"continuous"}`}},
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.5,"justification":"categorical repair"}`}},
	}}
	scorer := &LLMJudgeScorer{Client: judge, JudgeModel: "ollama/judge"}
	trace := Trace{ID: "repair", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/candidate", Transcript: []Turn{{Role: "assistant", Content: "answer"}}}

	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality=%v; want repaired 0.5", score.AnswerQuality)
	}
	if judge.called != 2 {
		t.Fatalf("judge called %d times; want initial + one repair", judge.called)
	}
	if len(judge.reqs) != 2 || !strings.Contains(judgeUserPromptOf(judge.reqs[1]), "Repair instruction") {
		t.Fatalf("second request missing repair instruction: %+v", judge.reqs)
	}
}

func TestLLMJudgeScorer_OffGridRetryFailureSurfacesError(t *testing.T) {
	judge := &fakeJudgeClient{resps: []*ollama.ChatResponse{
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.7,"justification":"continuous"}`}},
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.9,"justification":"still continuous"}`}},
	}}
	scorer := &LLMJudgeScorer{Client: judge, JudgeModel: "ollama/judge"}
	trace := Trace{ID: "repair-fail", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/candidate", Transcript: []Turn{{Role: "assistant", Content: "answer"}}}

	_, err := scorer.Score(context.Background(), trace, actual)
	if !errors.Is(err, errOffGridJudgeScore) {
		t.Fatalf("err=%v; want errOffGridJudgeScore", err)
	}
	if judge.called != 2 {
		t.Fatalf("judge called %d times; want exactly 2", judge.called)
	}
}

func TestLLMJudgeScorer_OffGridRepairCachesOnlyRepairRequest(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resps: []*ollama.ChatResponse{
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.7,"justification":"continuous"}`}},
		{Message: ollama.ChatMessage{Content: `{"answer_quality":0.5,"justification":"repair valid"}`}},
	}}
	scorer := &LLMJudgeScorer{
		Client:           judge,
		JudgeModel:       "ollama/judge",
		JudgeModelDigest: "sha256:shim",
		Cache:            c,
		Clock:            func() time.Time { return time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC) },
	}
	trace := Trace{ID: "repair-cache", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/candidate", Transcript: []Turn{{Role: "assistant", Content: "answer"}}}

	if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("Score: %v", err)
	}
	var requestJSON string
	var storedQuality float64
	var rowCount int
	rows, err := c.db.Query(`SELECT request_json, answer_quality FROM judge_cache WHERE trace_id = ?`, "repair-cache")
	if err != nil {
		t.Fatalf("query repair cache row: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		rowCount++
		if err := rows.Scan(&requestJSON, &storedQuality); err != nil {
			t.Fatalf("scan repair cache row: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate repair cache rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("cache rows=%d; want 1 valid repaired row", rowCount)
	}
	if storedQuality != 0.5 {
		t.Fatalf("stored answer_quality=%v; want 0.5", storedQuality)
	}
	if !strings.Contains(requestJSON, "Repair instruction") {
		t.Fatalf("cached request_json does not contain repair instruction:\n%s", requestJSON)
	}
	if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("second Score: %v", err)
	}
	if judge.called != 2 {
		t.Fatalf("judge called %d times after repaired cache hit; want still 2", judge.called)
	}
}

func TestLLMJudgeScorer_NormalCacheMissDoesNotCountRepairProbeMiss(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":1.0,"justification":"valid first response"}`,
	}}}
	scorer := &LLMJudgeScorer{
		Client:     judge,
		JudgeModel: "ollama/judge",
		Cache:      c,
	}
	trace := Trace{ID: "normal-cache-miss", System: "s", Turns: []Turn{{Role: "user", Content: "u"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/candidate", Transcript: []Turn{{Role: "assistant", Content: "answer"}}}

	if _, err := scorer.Score(context.Background(), trace, actual); err != nil {
		t.Fatalf("Score: %v", err)
	}
	hits, misses := c.Stats()
	if hits != 0 || misses != 1 {
		t.Fatalf("cache stats after normal miss = hits:%d misses:%d; want hits:0 misses:1", hits, misses)
	}
}

func TestLLMJudgeScorer_MalformedCacheHitFallsBackToJudge(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":1.0,"justification":"fresh"}`,
	}}}
	scorer := &LLMJudgeScorer{
		Client:     judge,
		JudgeModel: "ollama/gemma4:31b",
		Cache:      c,
	}
	trace := Trace{ID: "bad-cache", System: "s", Turns: []Turn{{Role: "user", Content: "h"}}, Golden: Golden{FinalAnswerCriteria: "c"}}
	actual := Result{Model: "ollama/cand", TraceID: "bad-cache", Transcript: []Turn{{Role: "assistant", Content: "a"}}}
	req, _, err := scorer.buildJudgeCall(trace, actual)
	if err != nil {
		t.Fatalf("buildJudgeCall: %v", err)
	}
	cacheKey := canonicalCacheKey(judgeCacheRequest{
		Version:      judgeCacheKeyVersion,
		JudgeModel:   normalizeModelSelector(scorer.JudgeModel),
		SystemPrompt: judgeSystemPrompt,
		UserPrompt:   judgeUserPromptOf(req),
		Format:       req.Format,
		Think:        req.Think,
		Temperature:  judgeTemperature,
		NumPredict:   judgeTokenBudget,
	})
	now := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	if err := c.Put(context.Background(), judgeCacheEntry{
		CacheKey:        cacheKey,
		JudgeModel:      scorer.JudgeModel,
		TraceID:         trace.ID,
		CandidateModel:  actual.Model,
		PromptHash:      "bad",
		RequestJSON:     "{}",
		ResponseContent: "not-json",
		AnswerQuality:   0,
		Justification:   "bad",
		CreatedAt:       now,
		LastUsedAt:      now,
	}); err != nil {
		t.Fatalf("seed bad cache row: %v", err)
	}
	score, err := scorer.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if judge.called != 1 {
		t.Fatalf("judge called %d times; want 1 fallback call", judge.called)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want fresh judge score 1.0", score.AnswerQuality)
	}
}

// TestLLMJudgeScorer_CacheHit_FreshToolSequenceMatch is the load-bearing
// invariant of the judge-cache architecture: on a cache hit the judge MUST
// NOT be re-invoked, but the returned Score's ToolSequenceMatch MUST come
// from THIS call's baseScore (computed fresh from current trace+actual),
// never from a cached judgement field. Without this contract, a future
// refactor that accidentally caches the full Score could silently mask
// tool-loop regressions, since the cached ToolSequenceMatch would be
// returned even when the candidate's actual tool calls have changed.
func TestLLMJudgeScorer_CacheHit_FreshToolSequenceMatch(t *testing.T) {
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{
		resp: &ollama.ChatResponse{
			Message: ollama.ChatMessage{
				Content: `{"answer_quality":0.5,"justification":"j"}`,
			},
		},
	}
	scorer := &LLMJudgeScorer{
		Client:     judge,
		JudgeModel: "ollama/gemma4:31b",
		Cache:      c,
	}

	// Trace whose golden expects a single tool call.
	trace := Trace{
		ID:     "t",
		System: "s",
		Turns:  []Turn{{Role: "user", Content: "hi"}},
		Golden: Golden{ToolCalls: []string{"read_file"}, FinalAnswerCriteria: "c"},
	}
	// First call: actual matches golden → ToolSequenceMatch=1.0 baked into
	// baseScore and the judgement is persisted to the cache.
	actualMatch := Result{Model: "ollama/cand", Transcript: []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
		{Role: "assistant", Content: "answer"},
	}}
	if _, err := scorer.Score(context.Background(), trace, actualMatch); err != nil {
		t.Fatalf("seed Score: %v", err)
	}
	if judge.called != 1 {
		t.Fatalf("judge called %d; want 1", judge.called)
	}

	// Second call: same trace + same actual → CACHE HIT. judge NOT re-called.
	// The ToolSequenceMatch in the returned Score must come from THIS call's
	// recomputation (against current trace+actual), not from the cached
	// judgement.
	s2, err := scorer.Score(context.Background(), trace, actualMatch)
	if err != nil {
		t.Fatalf("hit Score: %v", err)
	}
	if judge.called != 1 {
		t.Fatalf("judge re-called on cache hit; called=%d", judge.called)
	}
	// ToolSequenceMatch is computed fresh from current trace+actual:
	if s2.ToolSequenceMatch != 1.0 {
		t.Fatalf("cache-hit ToolSequenceMatch=%v; want 1.0 (recomputed fresh)", s2.ToolSequenceMatch)
	}
}
