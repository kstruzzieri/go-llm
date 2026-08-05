package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseModelTargets(t *testing.T) {
	got, err := parseModelTargets("ollama/qwen3-coder-next:latest, gemma4:31b, openai-compat/Qwen/Qwen3-Coder")
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
		{
			Display:  "openai-compat/Qwen/Qwen3-Coder",
			Provider: "openai-compat",
			Model:    "Qwen/Qwen3-Coder",
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

func TestParseModelTargetsCanonicalUniqueness(t *testing.T) {
	for _, csv := range []string{
		"m,ollama/m",
		"M,ollama/m",
		"openai-compat/M,OPENAI-COMPAT/m",
	} {
		if _, err := parseModelTargets(csv); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("parseModelTargets(%q) err = %v; want canonical duplicate rejection", csv, err)
		}
	}
	got, err := parseModelTargets("ollama/m,openai-compat/m")
	if err != nil {
		t.Fatalf("distinct providers rejected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("targets = %+v; want two provider-distinct targets", got)
	}
}
