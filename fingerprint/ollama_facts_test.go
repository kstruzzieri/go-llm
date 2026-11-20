package fingerprint

import (
	"reflect"
	"testing"
)

func TestOllamaCapabilityFacts_CanonicalAndAliasVocabulary(t *testing.T) {
	y, n := CapProbeYes, CapProbeNo
	cases := []struct {
		name     string
		caps     []string
		template string
		want     map[string]CapProbeState
	}{
		{"completion", []string{"completion"}, "", map[string]CapProbeState{"chat": y, "generate": y, "stream": y, "tool_call": n}},
		{"tools", []string{"tools"}, "", map[string]CapProbeState{"tool_call": y}},
		{"tool_call canonical", []string{"tool_call"}, "", map[string]CapProbeState{"tool_call": y}},
		{"embedding", []string{"embedding"}, "", map[string]CapProbeState{"embed": y, "tool_call": n}},
		{"embed canonical", []string{"embed"}, "", map[string]CapProbeState{"embed": y, "tool_call": n}},
		{"thinking", []string{"thinking"}, "", map[string]CapProbeState{"thinking": y, "tool_call": n}},
		{"insert", []string{"insert"}, "", map[string]CapProbeState{"generate": y, "stream": y, "insert": y, "tool_call": n}},
		{"canonical singles", []string{"chat", "generate", "stream"}, "", map[string]CapProbeState{"chat": y, "generate": y, "stream": y, "tool_call": n}},
		{"case insensitive", []string{"Completion", "TOOLS"}, "", map[string]CapProbeState{"chat": y, "generate": y, "stream": y, "tool_call": y}},
		{"unknown ignored", []string{"vision", "tools"}, "", map[string]CapProbeState{"tool_call": y}},
		{"empty array", []string{}, "", map[string]CapProbeState{"tool_call": n}},
		{"nil array no template", nil, "", map[string]CapProbeState{}},
		{"nil array template yes", nil, "{{ .Prompt }}{{ .Suffix }}", map[string]CapProbeState{"insert": y}},
		{"nil array template no", nil, "{{ .Prompt }}", map[string]CapProbeState{"insert": n}},
		{"template overrides insert token", []string{"insert", "tools"}, "{{ .Prompt }}", map[string]CapProbeState{"generate": y, "stream": y, "insert": n, "tool_call": y}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := OllamaCapabilityFacts(c.caps, c.template); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("OllamaCapabilityFacts(%v, %q) = %v, want %v", c.caps, c.template, got, c.want)
			}
		})
	}
}

func TestOllamaCapabilityFacts_AllSevenCanonicalNamesReachable(t *testing.T) {
	got := OllamaCapabilityFacts([]string{"completion", "insert", "tools", "embedding", "thinking"}, "")
	for _, name := range []string{"chat", "generate", "insert", "embed", "stream", "tool_call", "thinking"} {
		if got[name] != CapProbeYes {
			t.Errorf("capability %q = %q, want yes (%v)", name, got[name], got)
		}
	}
	if len(got) != 7 {
		t.Errorf("got %d facts, want 7: %v", len(got), got)
	}
}
