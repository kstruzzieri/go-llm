package completion

import (
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestAssembleFIMPrompt(t *testing.T) {
	tests := []struct {
		name   string
		fim    *provider.FIMConfig
		prefix string
		suffix string
		want   string
	}{
		{
			name:   "Qwen family",
			fim:    &provider.FIMConfig{Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>"},
			prefix: "func main() {\n\t",
			suffix: "\n}",
			want:   "<|fim_prefix|>func main() {\n\t<|fim_suffix|>\n}<|fim_middle|>",
		},
		{
			name:   "CodeLlama family with leading spaces",
			fim:    &provider.FIMConfig{Prefix: "<PRE>", Suffix: " <SUF>", Middle: " <MID>"},
			prefix: "int x = ",
			suffix: ";\n",
			want:   "<PRE>int x =  <SUF>;\n <MID>",
		},
		{
			name:   "StarCoder family",
			fim:    &provider.FIMConfig{Prefix: "<fim_prefix>", Suffix: "<fim_suffix>", Middle: "<fim_middle>"},
			prefix: "def foo():",
			suffix: "\n    return 1",
			want:   "<fim_prefix>def foo():<fim_suffix>\n    return 1<fim_middle>",
		},
		{
			name:   "Mistral family",
			fim:    &provider.FIMConfig{Prefix: "[PREFIX]", Suffix: "[SUFFIX]", Middle: "[MIDDLE]"},
			prefix: "fn main() {",
			suffix: "}",
			want:   "[PREFIX]fn main() {[SUFFIX]}[MIDDLE]",
		},
		{
			name:   "DeepSeek-Coder-V2",
			fim:    &provider.FIMConfig{Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>"},
			prefix: "BEFORE",
			suffix: "AFTER",
			want:   "<|fim_prefix|>BEFORE<|fim_suffix|>AFTER<|fim_middle|>",
		},
		{
			name:   "empty prefix",
			fim:    &provider.FIMConfig{Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>"},
			prefix: "",
			suffix: "func main() {}",
			want:   "<|fim_prefix|><|fim_suffix|>func main() {}<|fim_middle|>",
		},
		{
			name:   "empty suffix",
			fim:    &provider.FIMConfig{Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>"},
			prefix: "package main\n",
			suffix: "",
			want:   "<|fim_prefix|>package main\n<|fim_suffix|><|fim_middle|>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := assembleFIMPrompt(tt.fim, tt.prefix, tt.suffix)
			if got != tt.want {
				t.Errorf("assembleFIMPrompt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripStopTokens(t *testing.T) {
	tests := []struct {
		name       string
		completion string
		stops      []string
		want       string
	}{
		{
			name:       "no stop tokens",
			completion: "fmt.Println()",
			stops:      nil,
			want:       "fmt.Println()",
		},
		{
			name:       "strip endoftext at tail",
			completion: "fmt.Println()\n<|endoftext|>",
			stops:      []string{"<|endoftext|>"},
			want:       "fmt.Println()\n",
		},
		{
			name:       "strip EOT at tail",
			completion: "return 42<EOT>",
			stops:      []string{"<EOT>", "</s>"},
			want:       "return 42",
		},
		{
			name:       "strip /MIDDLE at tail",
			completion: "x + y[/MIDDLE]",
			stops:      []string{"[/MIDDLE]", "</s>"},
			want:       "x + y",
		},
		{
			name:       "do not strip from middle of text",
			completion: "x<|endoftext|>y",
			stops:      []string{"<|endoftext|>"},
			want:       "x<|endoftext|>y",
		},
		{
			name:       "strip longest match when multiple overlap at tail",
			completion: "result</s>",
			stops:      []string{"</s>", "<EOT>"},
			want:       "result",
		},
		{
			name:       "strip language stop pattern at tail",
			completion: "return nil\n}\nfunc ",
			stops:      []string{"<|endoftext|>", "\nfunc "},
			want:       "return nil\n}",
		},
		{
			name:       "empty completion",
			completion: "",
			stops:      []string{"<|endoftext|>"},
			want:       "",
		},
		{
			name:       "completion is exactly a stop token",
			completion: "<|endoftext|>",
			stops:      []string{"<|endoftext|>"},
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripStopTokens(tt.completion, tt.stops)
			if got != tt.want {
				t.Errorf("stripStopTokens() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMergeStopTokens(t *testing.T) {
	tests := []struct {
		name       string
		modelStops []string
		langStops  []string
		wantLen    int
		wantFirst  string
	}{
		{
			name:       "model only",
			modelStops: []string{"<|endoftext|>", "<|fim_pad|>"},
			langStops:  nil,
			wantLen:    2,
			wantFirst:  "<|endoftext|>",
		},
		{
			name:       "model and language merged",
			modelStops: []string{"<|endoftext|>"},
			langStops:  []string{"\nfunc ", "\ntype "},
			wantLen:    3,
			wantFirst:  "<|endoftext|>",
		},
		{
			name:       "duplicates removed",
			modelStops: []string{"<|endoftext|>"},
			langStops:  []string{"<|endoftext|>", "\nfunc "},
			wantLen:    2,
			wantFirst:  "<|endoftext|>",
		},
		{
			name:       "cap at 10 preserving all model stops",
			modelStops: []string{"a", "b", "c", "d", "e", "f", "g", "h"},
			langStops:  []string{"i", "j", "k", "l"},
			wantLen:    10,
			wantFirst:  "a",
		},
		{
			name:       "both empty",
			modelStops: nil,
			langStops:  nil,
			wantLen:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeStopTokens(tt.modelStops, tt.langStops)
			if len(got) != tt.wantLen {
				t.Fatalf("mergeStopTokens() len = %d, want %d; got %v", len(got), tt.wantLen, got)
			}
			if tt.wantLen > 0 && got[0] != tt.wantFirst {
				t.Errorf("first stop = %q, want %q", got[0], tt.wantFirst)
			}
			for _, ms := range tt.modelStops {
				found := false
				for _, g := range got {
					if g == ms {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("model stop %q was dropped", ms)
				}
			}
		})
	}
}
