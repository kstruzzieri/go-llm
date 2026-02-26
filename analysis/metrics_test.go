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

func TestNewMetricsAnalyzer(t *testing.T) {
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
			ma, err := NewMetricsAnalyzer(tt.client, tt.model)
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
			if ma == nil {
				t.Fatal("expected non-nil MetricsAnalyzer")
			}
		})
	}
}

func TestAnalyzeTraining(t *testing.T) {
	const analysisResult = "Training is progressing well. Loss is decreasing steadily."
	srv := newMockChatServer(t, analysisResult)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	metrics := TrainingMetrics{
		Epoch:         10,
		Loss:          0.05,
		LossHistory:   []float64{0.5, 0.3, 0.1, 0.05},
		RewardMean:    0.85,
		RewardHistory: []float64{0.1, 0.5, 0.7, 0.85},
		KLDivergence:  0.02,
		LearningRate:  0.001,
		CustomMetrics: map[string]float64{"accuracy": 0.95},
	}

	result, err := ma.AnalyzeTraining(context.Background(), metrics)
	if err != nil {
		t.Fatalf("AnalyzeTraining() error: %v", err)
	}
	if result != analysisResult {
		t.Errorf("expected %q, got %q", analysisResult, result)
	}
}

func TestAnalyzeTrainingPromptContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}

		userMsg := req.Messages[1].Content
		if !strings.Contains(userMsg, "epoch 5") {
			t.Error("expected prompt to mention epoch")
		}
		if !strings.Contains(userMsg, "Current Loss") {
			t.Error("expected prompt to contain loss")
		}
		if !strings.Contains(userMsg, "Learning Rate") {
			t.Error("expected prompt to contain learning rate")
		}

		resp := ollama.ChatResponse{
			Model:   req.Model,
			Message: ollama.ChatMessage{Role: "assistant", Content: "analysis"},
			Done:    true,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	_, err = ma.AnalyzeTraining(context.Background(), TrainingMetrics{
		Epoch:        5,
		Loss:         0.1,
		LearningRate: 0.001,
	})
	if err != nil {
		t.Fatalf("AnalyzeTraining() error: %v", err)
	}
}

func TestAnalyzeTrainingValidation(t *testing.T) {
	client := ollama.NewClient()
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	tests := []struct {
		name      string
		metrics   TrainingMetrics
		errSubstr string
	}{
		{
			name:      "negative epoch",
			metrics:   TrainingMetrics{Epoch: -1, LearningRate: 0.001},
			errSubstr: "epoch must be non-negative",
		},
		{
			name:      "negative learning rate",
			metrics:   TrainingMetrics{Epoch: 0, LearningRate: -0.001},
			errSubstr: "learning rate must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ma.AnalyzeTraining(context.Background(), tt.metrics)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
			if !strings.Contains(err.Error(), "analysis: analyze training:") {
				t.Errorf("error %q should have analysis: prefix", err.Error())
			}
		})
	}
}

func TestExplainAnomaly(t *testing.T) {
	const explanation = "KL drift indicates policy divergence. Reduce learning rate."
	srv := newMockChatServer(t, explanation)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	anomaly := AnomalyInfo{
		Type:        "kl_drift",
		Severity:    "warning",
		Description: "KL divergence exceeded threshold",
		Metrics:     map[string]float64{"kl_divergence": 0.15},
	}

	result, err := ma.ExplainAnomaly(context.Background(), anomaly)
	if err != nil {
		t.Fatalf("ExplainAnomaly() error: %v", err)
	}
	if result != explanation {
		t.Errorf("expected %q, got %q", explanation, result)
	}
}

func TestExplainAnomalyValidation(t *testing.T) {
	client := ollama.NewClient()
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	tests := []struct {
		name      string
		anomaly   AnomalyInfo
		errSubstr string
	}{
		{
			name:      "empty type",
			anomaly:   AnomalyInfo{Type: "", Severity: "warning"},
			errSubstr: "type is required",
		},
		{
			name:      "empty severity",
			anomaly:   AnomalyInfo{Type: "loss_spike", Severity: ""},
			errSubstr: "severity is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ma.ExplainAnomaly(context.Background(), tt.anomaly)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
			if !strings.Contains(err.Error(), "analysis: explain anomaly:") {
				t.Errorf("error %q should have analysis: prefix", err.Error())
			}
		})
	}
}

func TestAnalyzeTrainingServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	ma, err := NewMetricsAnalyzer(client, "test-model")
	if err != nil {
		t.Fatalf("NewMetricsAnalyzer() error: %v", err)
	}

	_, err = ma.AnalyzeTraining(context.Background(), TrainingMetrics{Epoch: 1, LearningRate: 0.001})
	if err == nil {
		t.Fatal("expected error for server error")
	}
	if !strings.Contains(err.Error(), "analysis: analyze training:") {
		t.Errorf("error %q should have analysis: prefix", err.Error())
	}
}
