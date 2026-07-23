package provider

import "testing"

func TestParseModelName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  ParsedModel
	}{
		// --- Basic models with param size ---
		{
			name:  "simple model with size tag",
			input: "qwen3:8b",
			want: ParsedModel{
				Raw:       "qwen3:8b",
				Family:    "qwen3",
				Variant:   "",
				ParamSize: "8b",
				Quant:     "",
				Tag:       "8b",
			},
		},
		{
			name:  "gemma2 with size",
			input: "gemma2:9b",
			want: ParsedModel{
				Raw:       "gemma2:9b",
				Family:    "gemma2",
				Variant:   "",
				ParamSize: "9b",
				Quant:     "",
				Tag:       "9b",
			},
		},
		{
			name:  "starcoder2 with size",
			input: "starcoder2:15b",
			want: ParsedModel{
				Raw:       "starcoder2:15b",
				Family:    "starcoder2",
				Variant:   "",
				ParamSize: "15b",
				Quant:     "",
				Tag:       "15b",
			},
		},
		{
			name:  "phi4 with size",
			input: "phi4:14b",
			want: ParsedModel{
				Raw:       "phi4:14b",
				Family:    "phi4",
				Variant:   "",
				ParamSize: "14b",
				Quant:     "",
				Tag:       "14b",
			},
		},

		// --- Fractional param sizes ---
		{
			name:  "phi3 with fractional size",
			input: "phi3:3.8b",
			want: ParsedModel{
				Raw:       "phi3:3.8b",
				Family:    "phi3",
				Variant:   "",
				ParamSize: "3.8b",
				Quant:     "",
				Tag:       "3.8b",
			},
		},

		// --- Quantization levels ---
		{
			name:  "model with quant in tag",
			input: "qwen3:8b-q4_K_M",
			want: ParsedModel{
				Raw:       "qwen3:8b-q4_K_M",
				Family:    "qwen3",
				Variant:   "",
				ParamSize: "8b",
				Quant:     "q4_k_m",
				Tag:       "8b-q4_K_M",
			},
		},
		{
			name:  "gemma3 with size and quant",
			input: "gemma3:27b-q8_0",
			want: ParsedModel{
				Raw:       "gemma3:27b-q8_0",
				Family:    "gemma3",
				Variant:   "",
				ParamSize: "27b",
				Quant:     "q8_0",
				Tag:       "27b-q8_0",
			},
		},
		{
			name:  "llama3 with size and quant",
			input: "llama3:8b-q8_0",
			want: ParsedModel{
				Raw:       "llama3:8b-q8_0",
				Family:    "llama3",
				Variant:   "",
				ParamSize: "8b",
				Quant:     "q8_0",
				Tag:       "8b-q8_0",
			},
		},
		{
			name:  "fp16 quant level",
			input: "qwen3:8b-fp16",
			want: ParsedModel{
				Raw:       "qwen3:8b-fp16",
				Family:    "qwen3",
				Variant:   "",
				ParamSize: "8b",
				Quant:     "fp16",
				Tag:       "8b-fp16",
			},
		},

		// --- MoE active params ---
		{
			name:  "MoE model with active params and quant",
			input: "qwen3.5:35b-a3b-q4_K_M",
			want: ParsedModel{
				Raw:       "qwen3.5:35b-a3b-q4_K_M",
				Family:    "qwen3.5",
				Variant:   "a3b",
				ParamSize: "35b",
				Quant:     "q4_k_m",
				Tag:       "35b-a3b-q4_K_M",
			},
		},
		{
			name:  "MoE model with active params no quant",
			input: "qwen3.5:35b-a3b",
			want: ParsedModel{
				Raw:       "qwen3.5:35b-a3b",
				Family:    "qwen3.5",
				Variant:   "a3b",
				ParamSize: "35b",
				Quant:     "",
				Tag:       "35b-a3b",
			},
		},

		// --- Mixtral-style MoE param sizes ---
		{
			name:  "mixtral MoE format",
			input: "mixtral:8x7b-q4_K_M",
			want: ParsedModel{
				Raw:       "mixtral:8x7b-q4_K_M",
				Family:    "mixtral",
				Variant:   "",
				ParamSize: "8x7b",
				Quant:     "q4_k_m",
				Tag:       "8x7b-q4_K_M",
			},
		},

		// --- Family-level variants ---
		{
			name:  "coder variant in family",
			input: "qwen2.5-coder:7b",
			want: ParsedModel{
				Raw:       "qwen2.5-coder:7b",
				Family:    "qwen2.5",
				Variant:   "coder",
				ParamSize: "7b",
				Quant:     "",
				Tag:       "7b",
			},
		},

		// --- Tag-level variants ---
		{
			name:  "instruct variant in tag",
			input: "mistral:7b-instruct-q4_0",
			want: ParsedModel{
				Raw:       "mistral:7b-instruct-q4_0",
				Family:    "mistral",
				Variant:   "instruct",
				ParamSize: "7b",
				Quant:     "q4_0",
				Tag:       "7b-instruct-q4_0",
			},
		},
		{
			name:  "instruct variant with version suffix",
			input: "mistral:7b-instruct-v0.3",
			want: ParsedModel{
				Raw:       "mistral:7b-instruct-v0.3",
				Family:    "mistral",
				Variant:   "instruct-v0.3",
				ParamSize: "7b",
				Quant:     "",
				Tag:       "7b-instruct-v0.3",
			},
		},
		{
			name:  "codellama instruct with quant",
			input: "codellama:13b-instruct-q4_K_M",
			want: ParsedModel{
				Raw:       "codellama:13b-instruct-q4_K_M",
				Family:    "codellama",
				Variant:   "instruct",
				ParamSize: "13b",
				Quant:     "q4_k_m",
				Tag:       "13b-instruct-q4_K_M",
			},
		},

		// --- Compound families ---
		{
			name:  "deepseek-r1 compound family",
			input: "deepseek-r1:70b",
			want: ParsedModel{
				Raw:       "deepseek-r1:70b",
				Family:    "deepseek-r1",
				Variant:   "",
				ParamSize: "70b",
				Quant:     "",
				Tag:       "70b",
			},
		},
		{
			name:  "deepseek-r1 smaller size",
			input: "deepseek-r1:7b",
			want: ParsedModel{
				Raw:       "deepseek-r1:7b",
				Family:    "deepseek-r1",
				Variant:   "",
				ParamSize: "7b",
				Quant:     "",
				Tag:       "7b",
			},
		},
		{
			name:  "deepseek-coder-v2 compound family",
			input: "deepseek-coder-v2:16b",
			want: ParsedModel{
				Raw:       "deepseek-coder-v2:16b",
				Family:    "deepseek-coder-v2",
				Variant:   "",
				ParamSize: "16b",
				Quant:     "",
				Tag:       "16b",
			},
		},

		// --- Embedding models ---
		{
			name:  "nomic-embed-text with latest tag",
			input: "nomic-embed-text:latest",
			want: ParsedModel{
				Raw:       "nomic-embed-text:latest",
				Family:    "nomic-embed-text",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},
		{
			name:  "nomic-embed-text no tag",
			input: "nomic-embed-text",
			want: ParsedModel{
				Raw:       "nomic-embed-text",
				Family:    "nomic-embed-text",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},

		// --- No tag / latest tag ---
		{
			name:  "no tag defaults to latest",
			input: "llama3.2",
			want: ParsedModel{
				Raw:       "llama3.2",
				Family:    "llama3.2",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},
		{
			name:  "explicit latest tag",
			input: "codellama:latest",
			want: ParsedModel{
				Raw:       "codellama:latest",
				Family:    "codellama",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},
		{
			name:  "qwen3-coder-next with latest tag",
			input: "qwen3-coder-next:latest",
			want: ParsedModel{
				Raw:       "qwen3-coder-next:latest",
				Family:    "qwen3-coder-next",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},

		// --- Edge cases ---
		{
			name:  "empty string",
			input: "",
			want: ParsedModel{
				Raw:       "",
				Family:    "",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "",
			},
		},
		{
			name:  "family only no size",
			input: "mistral",
			want: ParsedModel{
				Raw:       "mistral",
				Family:    "mistral",
				Variant:   "",
				ParamSize: "",
				Quant:     "",
				Tag:       "latest",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseModelName(tt.input)
			if got.Raw != tt.want.Raw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.want.Raw)
			}
			if got.Family != tt.want.Family {
				t.Errorf("Family = %q, want %q", got.Family, tt.want.Family)
			}
			if got.Variant != tt.want.Variant {
				t.Errorf("Variant = %q, want %q", got.Variant, tt.want.Variant)
			}
			if got.ParamSize != tt.want.ParamSize {
				t.Errorf("ParamSize = %q, want %q", got.ParamSize, tt.want.ParamSize)
			}
			if got.Quant != tt.want.Quant {
				t.Errorf("Quant = %q, want %q", got.Quant, tt.want.Quant)
			}
			if got.Tag != tt.want.Tag {
				t.Errorf("Tag = %q, want %q", got.Tag, tt.want.Tag)
			}
		})
	}
}

func TestParsedModelNormalizedFamily(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"qwen3:8b", "qwen3"},
		{"qwen2.5-coder:7b", "qwen2.5"},
		{"Llama3.2:8b", "llama3.2"},
		{"DeepSeek-R1:7b", "deepseek-r1"},
		{"STARCODER2:15b", "starcoder2"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := ParseModelName(tt.input).NormalizedFamily()
			if got != tt.want {
				t.Errorf("NormalizedFamily() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseModelNamePreservesRaw(t *testing.T) {
	inputs := []string{
		"qwen3:8b",
		"qwen3.5:35b-a3b-q4_K_M",
		"nomic-embed-text",
		"",
		"codellama:latest",
	}
	for _, input := range inputs {
		got := ParseModelName(input)
		if got.Raw != input {
			t.Errorf("ParseModelName(%q).Raw = %q, want %q", input, got.Raw, input)
		}
	}
}

func TestSplitModelTag(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantBase string
		wantTag  string
	}{
		{"with colon", "qwen3:8b", "qwen3", "8b"},
		{"no colon", "qwen3", "qwen3", "latest"},
		{"multiple colons", "registry:5000/model:tag", "registry:5000/model", "tag"},
		{"colon at end", "model:", "model", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, tag := splitModelTag(tt.input)
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
			if tag != tt.wantTag {
				t.Errorf("tag = %q, want %q", tag, tt.wantTag)
			}
		})
	}
}

func TestExtractFamilyVariant(t *testing.T) {
	tests := []struct {
		name        string
		family      string
		wantBase    string
		wantVariant string
	}{
		{"plain family", "qwen3", "qwen3", ""},
		{"coder suffix", "qwen2.5-coder", "qwen2.5", "coder"},
		{"instruct suffix", "llama3-instruct", "llama3", "instruct"},
		{"compound family", "deepseek-r1", "deepseek-r1", ""},
		{"compound coder-v2", "deepseek-coder-v2", "deepseek-coder-v2", ""},
		{"nomic-embed-text", "nomic-embed-text", "nomic-embed-text", ""},
		{"chat suffix", "llama3-chat", "llama3", "chat"},
		{"vision suffix", "llava-vision", "llava", "vision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, variant := extractFamilyVariant(tt.family)
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
			if variant != tt.wantVariant {
				t.Errorf("variant = %q, want %q", variant, tt.wantVariant)
			}
		})
	}
}
