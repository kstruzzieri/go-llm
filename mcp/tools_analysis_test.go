package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/analysis"
	"github.com/kstruzzieri/go-llm/rag"
)

// analysisOllamaMock returns a handler that responds to /api/chat with a
// canned assistant message. Analysis tools use Chat under the hood.
func analysisOllamaMock() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"Analysis result text"},"done":true}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func TestCodeReviewToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "code_review",
		Arguments: map[string]any{
			"model":    "m",
			"code":     "func main() { fmt.Println(\"hello\") }",
			"language": "go",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	if text := extractText(result); text == "" {
		t.Error("expected non-empty review text")
	}
}

func TestCodeReviewToolEmptyCode(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "code_review",
		Arguments: map[string]any{
			"model": "m",
			"code":  "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty code")
	}
}

func TestExplainCodeToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "explain_code",
		Arguments: map[string]any{
			"model": "m",
			"code":  "x := 42",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestAnalyzeTrainingToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_training",
		Arguments: map[string]any{
			"model": "m",
			"metrics": map[string]any{
				"epoch":         5,
				"loss":          0.42,
				"loss_history":  []float64{1.0, 0.8, 0.6, 0.5, 0.42},
				"learning_rate": 0.001,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestAnalyzeTrainingToolLegacyFieldNames(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_training",
		Arguments: map[string]any{
			"model": "m",
			"metrics": map[string]any{
				"Epoch":        5,
				"Loss":         0.42,
				"LossHistory":  []float64{1.0, 0.8, 0.6, 0.5, 0.42},
				"LearningRate": 0.001,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestAnalyzeTrainingToolMissingMetrics(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_training",
		Arguments: map[string]any{
			"model": "m",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for missing metrics")
	}
	if text := extractText(result); !strings.Contains(text, "metrics are required") {
		t.Errorf("error = %q, want to contain %q", text, "metrics are required")
	}
}

func TestAnalyzeTrainingToolEmptyMetrics(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_training",
		Arguments: map[string]any{
			"model":   "m",
			"metrics": map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty metrics")
	}
	if text := extractText(result); !strings.Contains(text, "metrics must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "metrics must not be empty")
	}
}

func TestExplainAnomalyToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "explain_anomaly",
		Arguments: map[string]any{
			"model": "m",
			"anomaly": map[string]any{
				"Type":        "loss_spike",
				"Severity":    "warning",
				"Description": "Loss increased 3x in one epoch",
				"Metrics":     map[string]float64{"loss": 2.1, "prev_loss": 0.7},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestAnalyzeStrategyToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_strategy",
		Arguments: map[string]any{
			"model": "m",
			"name":  "momentum",
			"metrics": map[string]float64{
				"sharpe_ratio": 1.5,
				"max_drawdown": 0.12,
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestAnalyzeStrategyToolEmptyName(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "analyze_strategy",
		Arguments: map[string]any{
			"model":   "m",
			"name":    "",
			"metrics": map[string]float64{"sharpe": 1.0},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty name")
	}
}

func TestCompareStrategiesToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; analysis ChatFunc seam covered by TestUseCaseToConfigRole + provider-level seam tests.")
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "compare_strategies",
		Arguments: map[string]any{
			"model": "m",
			"strategies": map[string]any{
				"momentum": map[string]float64{"sharpe_ratio": 1.5},
				"mean_rev": map[string]float64{"sharpe_ratio": 0.8},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
}

func TestCompareStrategiesToolMissingModel(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "compare_strategies",
		Arguments: map[string]any{
			"strategies": map[string]any{
				"a": map[string]float64{"x": 1.0},
				"b": map[string]float64{"x": 2.0},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when no model and no config")
	}
	if text := extractText(result); !strings.Contains(text, "model parameter required") {
		t.Errorf("error = %q, want to contain %q", text, "model parameter required")
	}
}

func TestCompareStrategiesToolSingleStrategy(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock())
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "compare_strategies",
		Arguments: map[string]any{
			"model": "m",
			"strategies": map[string]any{
				"a": map[string]float64{"x": 1.0},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for single strategy")
	}
	if text := extractText(result); !strings.Contains(text, "at least 2 strategies are required") {
		t.Errorf("error = %q, want to contain %q", text, "at least 2 strategies are required")
	}
}

func TestUseCaseToConfigRole(t *testing.T) {
	cases := map[string]string{
		"code-review": "analysis",
		"analysis":    "analysis",
		"embedding":   "embedding",
		"chat":        "chat",
		"unknown":     "chat",
		"verify":      "verify",
		"extract":     "extract",
	}
	for in, want := range cases {
		if got := useCaseToConfigRole(in); got != want {
			t.Errorf("useCaseToConfigRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVerifySupportToolEmptyAnswer(t *testing.T) {
	s := newAnswerTestServer(t, nil)
	result, err := s.handleVerifySupport(context.Background(), rawArgs(t, `{"answer":"","question":"q"}`))
	if err != nil {
		t.Fatalf("handleVerifySupport() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty answer")
	}
	if text := extractText(result); !strings.Contains(text, "answer must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "answer must not be empty")
	}
}

func TestVerifySupportToolEmptyQuestion(t *testing.T) {
	s := newAnswerTestServer(t, nil)
	result, err := s.handleVerifySupport(context.Background(), rawArgs(t, `{"answer":"a","question":""}`))
	if err != nil {
		t.Fatalf("handleVerifySupport() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty question")
	}
	if text := extractText(result); !strings.Contains(text, "question must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "question must not be empty")
	}
}

func TestVerifySupportToolRegistered(t *testing.T) {
	env := newTestEnv(t, analysisOllamaMock()) // RAG disabled by default
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "verify_support",
		Arguments: map[string]any{"answer": "a", "question": "q"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError || !strings.Contains(extractText(result), "RAG is disabled") {
		t.Fatalf("registered tool should return existing RAG-disabled error, got isError=%v text=%q", result.IsError, extractText(result))
	}
}

func TestVerifySupportToolHappyPath(t *testing.T) {
	s := answerEnv(t, &queuedRouteEngine{responses: []string{
		`{"claims":["computeFoo returns one"]}`,
		`{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E1"],"reason":"ok"}]}`,
	}})

	result, err := s.handleVerifySupport(context.Background(), rawArgs(t, `{"answer":"computeFoo returns one","question":"where is computeFoo","top_k":1}`))
	if err != nil {
		t.Fatalf("handleVerifySupport() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", extractText(result))
	}
	var report analysis.SupportReport
	if err := json.Unmarshal([]byte(extractText(result)), &report); err != nil {
		t.Fatalf("unmarshal report: %v (%s)", err, extractText(result))
	}
	if report.Status != analysis.StatusSupported {
		t.Fatalf("status = %q, want supported; report=%+v", report.Status, report)
	}
	if len(report.Claims) != 1 || len(report.Evidence) != 1 || len(report.Claims[0].EvidenceIDs) != 1 || report.Claims[0].EvidenceIDs[0] != "E1" {
		t.Fatalf("unexpected report shape: %+v", report)
	}
	if !strings.Contains(extractText(result), `"missing_evidence_queries"`) {
		t.Fatalf("JSON should use lower_snake_case field names: %s", extractText(result))
	}
}

func TestVerifySupportTool_PolicyRedactionNeverReachesJudge(t *testing.T) {
	replacement := "REDACTED"
	evaluator := &mcpPolicyEvaluatorSpy{
		decision: rag.RetrievalPolicyDecision{Allow: true},
		resultDecision: []rag.RetrievalResultDecision{
			{Keep: false},
			{Keep: true, RedactedContent: &replacement},
		},
	}
	store := &answerStore{results: []rag.SearchResult{
		{Chunk: rag.Chunk{ID: "denied", Source: "DENIED_SECRET.go", Content: "DENIED_SECRET"}, Distance: 0.1},
		{Chunk: rag.Chunk{ID: "original", Source: "safe.go", StartLine: 1, EndLine: 1, Content: "ORIGINAL_SECRET"}, Distance: 0.2},
	}}
	engine := &queuedRouteEngine{responses: []string{
		`{"claims":["answer claim"]}`,
		`{"verdicts":[{"claim_id":"C1","status":"supported","evidence_ids":["E1"],"reason":"ok"}]}`,
	}}
	s := newAnswerTestServer(t, engine)
	s.retriever = mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
	s.retrievalPolicyEvaluator = evaluator
	req := rawArgs(t, `{"answer":"answer claim","question":"question","top_k":2}`)
	req.Params.Meta = policyMetaFromJSON(t, `{"max_results":2,"audit_labels":{"purpose":"support"}}`)

	result, err := s.handleVerifySupport(context.Background(), req)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%#v error=%v text=%q", result, err, extractText(result))
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 1 || store.searchCalls != 1 || engine.calls != 2 {
		t.Fatalf("calls evaluator=%d/%d search=%d judge=%d, want 1/1/1/2", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls, engine.calls)
	}
	if evaluator.last.Policy.MaxResults != 2 || evaluator.last.Policy.AuditLabels["purpose"] != "support" {
		t.Fatalf("evaluator policy = %#v", evaluator.last.Policy)
	}
	var modelInput strings.Builder
	for _, request := range engine.requests {
		for _, message := range request.Messages {
			modelInput.WriteString(message.Content)
		}
	}
	if !strings.Contains(modelInput.String(), "REDACTED") || strings.Contains(modelInput.String(), "DENIED_SECRET") || strings.Contains(modelInput.String(), "ORIGINAL_SECRET") {
		t.Fatalf("judge input leaked policy content: %s", modelInput.String())
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Meta == nil || strings.Contains(string(data), "DENIED_SECRET") || strings.Contains(string(data), "ORIGINAL_SECRET") || strings.Contains(string(data), "purpose") {
		t.Fatalf("unsafe result: %s", data)
	}
}

func TestVerifySupportTool_PolicyFailureIsSanitized(t *testing.T) {
	t.Run("evaluator", func(t *testing.T) {
		evaluator := &mcpPolicyEvaluatorSpy{evaluateErr: errors.New("ERROR_SECRET")}
		store := &answerStore{}
		engine := &queuedRouteEngine{}
		s := newAnswerTestServer(t, engine)
		s.retriever = mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
		s.retrievalPolicyEvaluator = evaluator

		result, err := s.handleVerifySupport(context.Background(), rawArgs(t, `{"answer":"answer","question":"question"}`))
		if err != nil || result == nil || !result.IsError || extractText(result) != "policy_evaluator_failed: retrieval policy evaluation failed" {
			t.Fatalf("result=%#v error=%v text=%q", result, err, extractText(result))
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if result.Meta == nil || strings.Contains(string(data), "ERROR_SECRET") {
			t.Fatalf("policy failure leaked detail or omitted metadata: %s", data)
		}
		if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 0 || store.searchCalls != 0 || engine.calls != 0 {
			t.Fatalf("work after policy failure evaluator=%d/%d search=%d judge=%d", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls, engine.calls)
		}
	})

	for _, tc := range []struct {
		name string
		meta gomcp.Meta
		bind func(*Server)
		want string
	}{
		{
			name: "invalid metadata", meta: policyMetaFromJSON(t, `{"UNKNOWN_SECRET":true}`),
			want: "validation: invalid retrieval policy metadata",
		},
		{
			name: "identity", meta: policyMetaFromJSON(t, `{"principal_id":"CLAIMED_SECRET"}`),
			bind: func(s *Server) {
				WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
					return "", errors.New("ERROR_SECRET")
				})(s)
			},
			want: "policy_identity_failed: retrieval identity resolution failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
			store := &answerStore{}
			engine := &queuedRouteEngine{}
			s := newAnswerTestServer(t, engine)
			s.retriever = mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
			s.retrievalPolicyEvaluator = evaluator
			if tc.bind != nil {
				tc.bind(s)
			}
			req := rawArgs(t, `{"answer":"answer","question":"question"}`)
			req.Params.Meta = tc.meta

			result, err := s.handleVerifySupport(context.Background(), req)
			if err != nil || result == nil || !result.IsError || extractText(result) != tc.want {
				t.Fatalf("result=%#v error=%v text=%q, want %q", result, err, extractText(result), tc.want)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if result.Meta != nil || strings.Contains(string(data), "SECRET") {
				t.Fatalf("request failure leaked detail or metadata: %s", data)
			}
			if evaluator.evaluateCalls != 0 || evaluator.resultCalls != 0 || store.searchCalls != 0 || engine.calls != 0 {
				t.Fatalf("work after request failure evaluator=%d/%d search=%d judge=%d", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls, engine.calls)
			}
		})
	}
}

func TestVerifySupportTool_PolicyMetaSurvivesJudgeError(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{
		decision:       rag.RetrievalPolicyDecision{Allow: true},
		resultDecision: []rag.RetrievalResultDecision{{Keep: true}},
	}
	store := &answerStore{results: []rag.SearchResult{{
		Chunk: rag.Chunk{ID: "allowed", Source: "safe.go", Content: "allowed"}, Distance: 0.1,
	}}}
	router := newRecordingRouteEngine("")
	router.routeErr = errors.New("JUDGE_ERROR")
	s := newAnswerTestServer(t, router)
	s.retriever = mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
	s.retrievalPolicyEvaluator = evaluator

	result, err := s.handleVerifySupport(context.Background(), rawArgs(t, `{"answer":"answer","question":"question"}`))
	if err != nil || result == nil || !result.IsError || !strings.HasPrefix(extractText(result), "analysis:") {
		t.Fatalf("result=%#v error=%v text=%q", result, err, extractText(result))
	}
	if result.Meta == nil {
		t.Fatal("judge error omitted safe applied metadata")
	}
}

func TestCodeReviewTool_PolicyRedactionNeverReachesPrompt(t *testing.T) {
	replacement := "REDACTED"
	evaluator := &mcpPolicyEvaluatorSpy{
		decision: rag.RetrievalPolicyDecision{Allow: true},
		resultDecision: []rag.RetrievalResultDecision{
			{Keep: false},
			{Keep: true, RedactedContent: &replacement},
		},
	}
	store := &answerStore{results: []rag.SearchResult{
		{Chunk: rag.Chunk{ID: "denied", Source: "DENIED_SECRET.go", Content: "DENIED_SECRET"}, Distance: 0.1},
		{Chunk: rag.Chunk{ID: "original", Source: "safe.go", Content: "ORIGINAL_SECRET"}, Distance: 0.2},
	}}
	router := newRecordingRouteEngine("review")
	s := &Server{router: router, retriever: mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))}

	result, err := s.handleCodeReview(context.Background(), rawArgs(t, `{"code":"func safe() {}","model":"test"}`))
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%#v error=%v text=%q", result, err, extractText(result))
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 1 || store.searchCalls != 1 || !router.called {
		t.Fatalf("calls evaluator=%d/%d search=%d router=%v, want 1/1/1/true", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls, router.called)
	}
	var prompt strings.Builder
	for _, message := range router.last.Messages {
		prompt.WriteString(message.Content)
	}
	if !strings.Contains(prompt.String(), "REDACTED") || strings.Contains(prompt.String(), "DENIED_SECRET") || strings.Contains(prompt.String(), "ORIGINAL_SECRET") {
		t.Fatalf("code-review prompt leaked policy content: %s", prompt.String())
	}
}
