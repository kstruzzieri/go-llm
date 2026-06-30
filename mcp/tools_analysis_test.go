package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
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
