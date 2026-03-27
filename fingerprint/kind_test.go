package fingerprint

import "testing"

func TestInferKindFromCapabilities(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []string
		want         ModelKind
	}{
		{"completion capability", []string{"completion", "tools"}, ModelKindChat},
		{"embedding capability", []string{"embedding"}, ModelKindEmbedding},
		{"dual capability returns embedding kind", []string{"completion", "embedding"}, ModelKindEmbedding},
		{"empty capabilities", []string{}, ModelKindUnknown},
		{"nil capabilities", nil, ModelKindUnknown},
		{"unrecognized capabilities", []string{"vision"}, ModelKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferKindFromCapabilities(tt.capabilities)
			if got != tt.want {
				t.Errorf("InferKindFromCapabilities(%v) = %q, want %q", tt.capabilities, got, tt.want)
			}
		})
	}
}

func TestInferKind(t *testing.T) {
	tests := []struct {
		name     string
		family   string
		template string
		want     ModelKind
	}{
		{"bert family", "bert", "", ModelKindEmbedding},
		{"nomic-bert family", "nomic-bert", "", ModelKindEmbedding},
		{"qwen2 with template", "qwen2", "{{ .System }}", ModelKindChat},
		{"llama with template", "llama", "{{ .Prompt }}", ModelKindChat},
		{"embedding family with template still embedding", "bert", "{{ .Prompt }}", ModelKindEmbedding},
		{"unknown family no template", "custom", "", ModelKindUnknown},
		{"empty everything", "", "", ModelKindUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferKind(tt.family, tt.template)
			if got != tt.want {
				t.Errorf("InferKind(%q, %q) = %q, want %q", tt.family, tt.template, got, tt.want)
			}
		})
	}
}
