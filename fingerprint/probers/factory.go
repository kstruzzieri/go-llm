package probers

import (
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// ProberFactoryInput contains the already-built backend handles needed to
// choose the correct fingerprint.ModelProber. Only the fields for APIFormat
// are required.
type ProberFactoryInput struct {
	APIFormat            string
	OllamaClient         *ollama.Client
	OpenAICompatProvider *openaicompat.Provider
	Capabilities         []string
}

// NewProberForAPIFormat selects the ModelProber implementation for a provider
// api_format value. It is library wiring only; callers still decide where to
// create the Profiler via fingerprint.NewProfiler(store, prober).
func NewProberForAPIFormat(in ProberFactoryInput) (fingerprint.ModelProber, error) {
	switch strings.ToLower(strings.TrimSpace(in.APIFormat)) {
	case "", "ollama":
		if in.OllamaClient == nil {
			return nil, fmt.Errorf("fingerprint: ollama prober requires OllamaClient")
		}
		return fingerprint.NewProber(in.OllamaClient), nil
	case "openai-compat":
		if in.OpenAICompatProvider == nil {
			return nil, fmt.Errorf("fingerprint: openai-compat prober requires OpenAICompatProvider")
		}
		return NewOpenAICompatProber(
			in.OpenAICompatProvider,
			WithOpenAICompatCapabilities(in.Capabilities),
		), nil
	default:
		return nil, fmt.Errorf("fingerprint: unsupported api_format %q", in.APIFormat)
	}
}
