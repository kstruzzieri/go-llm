package analysis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestNewStrategyAnalyzer(t *testing.T) {
	client := ollama.NewClient()

	tests := []struct {
		name      string
		client    *ollama.Client
		model     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "valid",
			client:  client,
			model:   "test-model",
			wantErr: false,
		},
		{
			name:      "nil client",
			client:    nil,
			model:     "test-model",
			wantErr:   true,
			errSubstr: "client is required",
		},
		{
			name:      "empty model",
			client:    client,
			model:     "",
			wantErr:   true,
			errSubstr: "model is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sa, err := NewStrategyAnalyzer(tt.client, tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sa == nil {
				t.Fatal("expected non-nil StrategyAnalyzer")
			}
		})
	}
}

func TestAnalyzeStrategy(t *testing.T) {
	const analysisResult = "Strong risk-adjusted returns with moderate drawdown."
	srv := newMockChatServer(t, analysisResult)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	metrics := map[string]float64{
		"sharpe_ratio":  1.85,
		"max_drawdown":  0.12,
		"annual_return": 0.24,
		"win_rate":      0.62,
	}

	result, err := sa.AnalyzeStrategy(context.Background(), "momentum_v2", metrics)
	if err != nil {
		t.Fatalf("AnalyzeStrategy() error: %v", err)
	}
	if result != analysisResult {
		t.Errorf("expected %q, got %q", analysisResult, result)
	}
}

func TestAnalyzeStrategyValidation(t *testing.T) {
	client := ollama.NewClient()
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	tests := []struct {
		name      string
		strategy  string
		metrics   map[string]float64
		errSubstr string
	}{
		{
			name:      "empty name",
			strategy:  "",
			metrics:   map[string]float64{"sharpe": 1.0},
			errSubstr: "name is required",
		},
		{
			name:      "nil metrics",
			strategy:  "test",
			metrics:   nil,
			errSubstr: "metrics are required",
		},
		{
			name:      "empty metrics",
			strategy:  "test",
			metrics:   map[string]float64{},
			errSubstr: "metrics are required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sa.AnalyzeStrategy(context.Background(), tt.strategy, tt.metrics)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
			if !strings.Contains(err.Error(), "analysis: analyze strategy:") {
				t.Errorf("error %q should have analysis: prefix", err.Error())
			}
		})
	}
}

func TestAnalyzeStrategyPromptContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}

		userMsg := req.Messages[1].Content
		if !strings.Contains(userMsg, "mean_revert") {
			t.Error("expected prompt to contain strategy name")
		}
		if !strings.Contains(userMsg, "sharpe_ratio") {
			t.Error("expected prompt to contain metric name")
		}

		resp := ollama.ChatResponse{
			Model:   req.Model,
			Message: ollama.ChatMessage{Role: "assistant", Content: "analysis"},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	_, err = sa.AnalyzeStrategy(context.Background(), "mean_revert", map[string]float64{
		"sharpe_ratio": 1.5,
	})
	if err != nil {
		t.Fatalf("AnalyzeStrategy() error: %v", err)
	}
}

func TestCompareStrategies(t *testing.T) {
	const comparison = "Strategy A outperforms on risk-adjusted basis."
	srv := newMockChatServer(t, comparison)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	strategies := map[string]map[string]float64{
		"momentum": {"sharpe_ratio": 1.85, "max_drawdown": 0.12},
		"mean_rev": {"sharpe_ratio": 1.20, "max_drawdown": 0.08},
	}

	result, err := sa.CompareStrategies(context.Background(), strategies)
	if err != nil {
		t.Fatalf("CompareStrategies() error: %v", err)
	}
	if result != comparison {
		t.Errorf("expected %q, got %q", comparison, result)
	}
}

func TestCompareStrategiesValidation(t *testing.T) {
	client := ollama.NewClient()
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	tests := []struct {
		name       string
		strategies map[string]map[string]float64
		errSubstr  string
	}{
		{
			name:       "nil strategies",
			strategies: nil,
			errSubstr:  "at least 2 strategies are required",
		},
		{
			name: "single strategy",
			strategies: map[string]map[string]float64{
				"only_one": {"sharpe": 1.0},
			},
			errSubstr: "at least 2 strategies are required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sa.CompareStrategies(context.Background(), tt.strategies)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
			if !strings.Contains(err.Error(), "analysis: compare strategies:") {
				t.Errorf("error %q should have analysis: prefix", err.Error())
			}
		})
	}
}

func TestCompareStrategiesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	sa, err := NewStrategyAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewStrategyAnalyzer() error: %v", err)
	}

	strategies := map[string]map[string]float64{
		"a": {"sharpe": 1.0},
		"b": {"sharpe": 2.0},
	}

	_, err = sa.CompareStrategies(context.Background(), strategies)
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "analysis: compare strategies:") {
		t.Errorf("error %q should have analysis: prefix", err.Error())
	}
}
