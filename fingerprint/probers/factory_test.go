package probers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func TestNewProberForAPIFormat_SelectsOpenAICompatWithCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	prov := openaicompat.NewProvider(openaicompat.NewClient(srv.URL))
	prober, err := NewProberForAPIFormat(ProberFactoryInput{
		APIFormat:            "openai-compat",
		OpenAICompatProvider: prov,
		Capabilities:         []string{"completion", "embedding"},
	})
	if err != nil {
		t.Fatalf("NewProberForAPIFormat() error = %v", err)
	}

	oc, ok := prober.(*OpenAICompatProber)
	if !ok {
		t.Fatalf("prober type = %T, want *OpenAICompatProber", prober)
	}

	det, err := oc.DetectKind(context.Background(), "local-fim")
	if err != nil {
		t.Fatalf("DetectKind() error = %v", err)
	}
	if det.Source != "capabilities" {
		t.Fatalf("Source = %q, want capabilities", det.Source)
	}
	if !containsCapability(det.Capabilities, "completion") || !containsCapability(det.Capabilities, "embedding") {
		t.Fatalf("Capabilities = %v, want completion and embedding", det.Capabilities)
	}
}

func TestNewProberForAPIFormat_SelectsOllama(t *testing.T) {
	prober, err := NewProberForAPIFormat(ProberFactoryInput{
		APIFormat:    "ollama",
		OllamaClient: ollama.NewClient(ollama.WithBaseURL("http://127.0.0.1:1")),
	})
	if err != nil {
		t.Fatalf("NewProberForAPIFormat() error = %v", err)
	}
	if _, ok := prober.(*fingerprint.OllamaProber); !ok {
		t.Fatalf("prober type = %T, want *fingerprint.OllamaProber", prober)
	}
}

func TestNewProberForAPIFormat_RejectsUnsupportedFormat(t *testing.T) {
	_, err := NewProberForAPIFormat(ProberFactoryInput{APIFormat: "anthropic"})
	if err == nil {
		t.Fatal("NewProberForAPIFormat() error = nil, want unsupported api_format error")
	}
	if !strings.Contains(err.Error(), `unsupported api_format "anthropic"`) {
		t.Fatalf("error = %q, want unsupported api_format", err)
	}
}

func TestNewProberForAPIFormat_OllamaRequiresClient(t *testing.T) {
	// Empty api_format defaults to the ollama branch, which requires a client.
	_, err := NewProberForAPIFormat(ProberFactoryInput{APIFormat: ""})
	if err == nil {
		t.Fatal("NewProberForAPIFormat() error = nil, want missing-OllamaClient error")
	}
	if !strings.Contains(err.Error(), "requires OllamaClient") {
		t.Fatalf("error = %q, want requires OllamaClient", err)
	}
}

func TestNewProberForAPIFormat_OpenAICompatRequiresProvider(t *testing.T) {
	_, err := NewProberForAPIFormat(ProberFactoryInput{APIFormat: "openai-compat"})
	if err == nil {
		t.Fatal("NewProberForAPIFormat() error = nil, want missing-OpenAICompatProvider error")
	}
	if !strings.Contains(err.Error(), "requires OpenAICompatProvider") {
		t.Fatalf("error = %q, want requires OpenAICompatProvider", err)
	}
}
