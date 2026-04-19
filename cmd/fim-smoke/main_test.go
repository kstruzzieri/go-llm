package main

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestSummarizeTemplate(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty",
			input: "",
			want:  "<empty>",
		},
		{
			name:  "collapses whitespace",
			input: "  {{ .Prompt }}\n\n{{ .Suffix }}  ",
			want:  "{{ .Prompt }} {{ .Suffix }}",
		},
		{
			name:  "truncates long template",
			input: strings.Repeat("abcdef", 20),
			want:  strings.Repeat("abcdef", 12) + "abcde...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeTemplate(tt.input); got != tt.want {
				t.Fatalf("summarizeTemplate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnsupportedFIMError(t *testing.T) {
	profile := &provider.ModelProfile{
		Template: "{{ .Prompt }}",
		Caps:     provider.CapGenerate | provider.CapStream,
	}

	err := unsupportedFIMError("http://localhost:11434/", "qwen3-coder-next:latest", profile)
	if err == nil {
		t.Fatal("unsupportedFIMError() returned nil")
	}

	msg := err.Error()
	checks := []string{
		`model "qwen3-coder-next:latest" does not support native FIM (template does not reference .Suffix)`,
		`detected template: {{ .Prompt }}`,
		`detected capabilities: generate,stream`,
		`ollama show qwen3-coder-next:latest --modelfile`,
		`curl -s http://localhost:11434/api/show -d '{"model":"qwen3-coder-next:latest"}'`,
	}
	for _, want := range checks {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q\nfull message:\n%s", want, msg)
		}
	}
}
