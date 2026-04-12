package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

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
