package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func promptText(msg *gomcp.PromptMessage) string {
	if tc, ok := msg.Content.(*gomcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestCodeReviewPromptBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "code-review",
		Arguments: map[string]string{
			"code":     "func main() {}",
			"language": "go",
			"focus":    "security",
		},
	})
	if err != nil {
		t.Fatalf("GetPrompt() error = %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(result.Messages))
	}

	system := promptText(result.Messages[0])
	if !strings.Contains(system, "code reviewer") {
		t.Errorf("system message = %q, want to contain %q", system, "code reviewer")
	}
	if !strings.Contains(system, "go") {
		t.Errorf("system message should mention language 'go'")
	}
	if !strings.Contains(system, "security") {
		t.Errorf("system message should mention focus 'security'")
	}

	user := promptText(result.Messages[1])
	if !strings.Contains(user, "func main()") {
		t.Errorf("user message should contain the code")
	}
}

func TestCodeReviewPromptMissingCode(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	_, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name:      "code-review",
		Arguments: map[string]string{},
	})
	if err == nil {
		t.Fatal("expected error for missing code argument")
	}
}

func TestExplainPromptBrief(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "explain",
		Arguments: map[string]string{
			"code":  "x := 42",
			"depth": "brief",
		},
	})
	if err != nil {
		t.Fatalf("GetPrompt() error = %v", err)
	}
	system := promptText(result.Messages[0])
	if !strings.Contains(system, "concise") {
		t.Errorf("brief mode system = %q, want to contain %q", system, "concise")
	}
}

func TestExplainPromptDefaultDetailed(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "explain",
		Arguments: map[string]string{
			"code": "x := 42",
		},
	})
	if err != nil {
		t.Fatalf("GetPrompt() error = %v", err)
	}
	system := promptText(result.Messages[0])
	if !strings.Contains(system, "thorough") {
		t.Errorf("default mode system = %q, want to contain %q", system, "thorough")
	}
}

func TestRAGQueryPromptDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock) // RAG disabled
	defer env.cleanup()

	_, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "rag-query",
		Arguments: map[string]string{
			"question": "What does main do?",
		},
	})
	if err == nil {
		t.Fatal("expected error when RAG disabled")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "disabled")
	}
}

func TestRAGQueryPrompt_ForwardsPolicyMetaAndRedacts(t *testing.T) {
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
	s := &Server{
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
		retrievalPolicyEvaluator: evaluator,
	}
	WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
		return "resolved-principal", nil
	})(s)
	req := &gomcp.GetPromptRequest{Params: &gomcp.GetPromptParams{
		Name: "rag-query", Arguments: map[string]string{"question": "question", "top_k": "2"},
		Meta: policyMetaFromJSON(t, `{
			"principal_id":"CLAIMED_SECRET","session_id":"session",
			"max_results":2,"audit_labels":{"purpose":"support"}
		}`),
	}}

	result, err := s.handleRAGQueryPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRAGQueryPrompt() error = %v", err)
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 1 || store.searchCalls != 1 {
		t.Fatalf("retrieval calls evaluator=%d/%d search=%d, want 1/1/1", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls)
	}
	if evaluator.last.Policy.PrincipalID != "resolved-principal" || evaluator.last.Policy.SessionID != "session" ||
		evaluator.last.Policy.MaxResults != 2 || evaluator.last.Policy.AuditLabels["purpose"] != "support" {
		t.Fatalf("evaluator policy = %#v", evaluator.last.Policy)
	}
	rendered := result.Description
	for _, message := range result.Messages {
		rendered += promptText(message)
	}
	if !strings.Contains(rendered, "REDACTED") || strings.Contains(rendered, "DENIED_SECRET") || strings.Contains(rendered, "ORIGINAL_SECRET") {
		t.Fatalf("prompt leaked policy content: %s", rendered)
	}
	metaJSON, err := json.Marshal(result.Meta)
	if err != nil {
		t.Fatal(err)
	}
	if result.Meta == nil || strings.Contains(string(metaJSON), "SECRET") || strings.Contains(string(metaJSON), "principal") || strings.Contains(string(metaJSON), "purpose") {
		t.Fatalf("unsafe result metadata: %s", metaJSON)
	}
}

func TestRAGQueryPrompt_PolicyFailureIsSanitized(t *testing.T) {
	t.Run("evaluator", func(t *testing.T) {
		evaluator := &mcpPolicyEvaluatorSpy{evaluateErr: errors.New("ERROR_SECRET")}
		store := &answerStore{}
		s := &Server{
			retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
			retrievalPolicyEvaluator: evaluator,
		}
		result, err := s.handleRAGQueryPrompt(context.Background(), &gomcp.GetPromptRequest{Params: &gomcp.GetPromptParams{
			Name: "rag-query", Arguments: map[string]string{"question": "PROMPT_SECRET"},
		}})
		if result != nil || err == nil || err.Error() != "policy_evaluator_failed: retrieval policy evaluation failed" || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("result=%#v error=%v", result, err)
		}
		if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 0 || store.searchCalls != 0 {
			t.Fatalf("work after evaluator failure evaluator=%d/%d search=%d", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls)
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
			s := &Server{
				retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
				retrievalPolicyEvaluator: evaluator,
			}
			if tc.bind != nil {
				tc.bind(s)
			}
			req := &gomcp.GetPromptRequest{Params: &gomcp.GetPromptParams{
				Name: "rag-query", Arguments: map[string]string{"question": "PROMPT_SECRET"}, Meta: tc.meta,
			}}
			result, err := s.handleRAGQueryPrompt(context.Background(), req)
			if result != nil || err == nil || err.Error() != tc.want || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("result=%#v error=%v, want %q", result, err, tc.want)
			}
			if evaluator.evaluateCalls != 0 || evaluator.resultCalls != 0 || store.searchCalls != 0 {
				t.Fatalf("work after request failure evaluator=%d/%d search=%d", evaluator.evaluateCalls, evaluator.resultCalls, store.searchCalls)
			}
		})
	}
}

func TestRAGQueryPrompt_LegacyBytesAndNilMeta(t *testing.T) {
	store := &answerStore{results: []rag.SearchResult{{
		Chunk: rag.Chunk{ID: "legacy", Source: "legacy.go", StartLine: 7, EndLine: 8, Content: "alpha\nbeta"},
		Score: 0.01, Distance: 0.25,
	}}}
	s := &Server{retriever: mcpTestRetriever(t, store)}
	req := &gomcp.GetPromptRequest{Params: &gomcp.GetPromptParams{
		Name: "rag-query", Arguments: map[string]string{"question": "Where?", "top_k": "1"},
	}}
	want := &gomcp.GetPromptResult{
		Description: "Question with RAG context",
		Messages: []*gomcp.PromptMessage{
			{Role: "assistant", Content: &gomcp.TextContent{Text: "Use the following context from the codebase to answer the question. If the context is not relevant, say so.\n\nRelevant code context:\n\n--- legacy.go (lines 7-8, similarity: 0.75) ---\n7| alpha\n8| beta\n\n"}},
			{Role: "user", Content: &gomcp.TextContent{Text: "Where?"}},
		},
	}

	got, err := s.handleRAGQueryPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRAGQueryPrompt() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy prompt changed:\n got: %#v\nwant: %#v", got, want)
	}
	if got.Meta != nil {
		t.Fatalf("legacy metadata = %#v, want nil", got.Meta)
	}
}

func TestRefactorPromptBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "refactor",
		Arguments: map[string]string{
			"code":     "func f() { for i := 0; i < 100; i++ { fmt.Println(i) } }",
			"goal":     "extract the loop into a helper function",
			"language": "go",
		},
	})
	if err != nil {
		t.Fatalf("GetPrompt() error = %v", err)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("message count = %d, want 2", len(result.Messages))
	}

	user := promptText(result.Messages[1])
	if !strings.Contains(user, "extract the loop") {
		t.Errorf("user message should contain the goal")
	}
}

func TestRefactorPromptMissingGoal(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	_, err := env.session.GetPrompt(context.Background(), &gomcp.GetPromptParams{
		Name: "refactor",
		Arguments: map[string]string{
			"code": "x := 42",
		},
	})
	if err == nil {
		t.Fatal("expected error for missing goal argument")
	}
}
