package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseModelTargets(t *testing.T) {
	got, err := parseModelTargets("ollama/qwen3-coder-next:latest, gemma4:31b")
	if err != nil {
		t.Fatalf("parseModelTargets() error: %v", err)
	}

	want := []ModelTarget{
		{
			Display:  "ollama/qwen3-coder-next:latest",
			Provider: "ollama",
			Model:    "qwen3-coder-next:latest",
		},
		{
			Display:  "gemma4:31b",
			Provider: "ollama",
			Model:    "gemma4:31b",
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseModelTargets() = %#v, want %#v", got, want)
	}
}

func TestParseModelTargetRejectsInvalidSelectors(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"/gemma4:31b",
		"ollama/",
		"anthropic/claude-sonnet",
	}

	for _, input := range tests {
		t.Run(strings.ReplaceAll(input, " ", "_"), func(t *testing.T) {
			if _, err := parseModelTarget(input); err == nil {
				t.Fatalf("parseModelTarget(%q) returned nil error", input)
			}
		})
	}
}
