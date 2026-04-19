package completion

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

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
		{
			name:       "strip chained trailing stop tokens",
			completion: "return 42<|fim_pad|><|endoftext|>",
			stops:      []string{"<|endoftext|>", "<|fim_pad|>"},
			want:       "return 42",
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

func TestComputeBudgetMaxTokens(t *testing.T) {
	fim := &provider.FIMConfig{
		Prefix:          "<|fim_prefix|>",
		Suffix:          "<|fim_suffix|>",
		Middle:          "<|fim_middle|>",
		StopTokens:      []string{"<|endoftext|>"},
		PrefixBudgetPct: 75,
	}

	tests := []struct {
		name    string
		ctx     CursorContext
		shape   CompletionShape
		conf    float64
		lang    string
		tier    provider.Tier
		wantMin int
		wantMax int
	}{
		{
			name:    "import block + token shape = small budget",
			ctx:     ContextImportBlock,
			shape:   ShapeToken,
			conf:    0.75,
			lang:    "go",
			tier:    provider.TierGood,
			wantMin: 16,
			wantMax: 32,
		},
		{
			name:    "between declarations + declaration shape + Java + best tier",
			ctx:     ContextBetweenDeclarations,
			shape:   ShapeDeclaration,
			conf:    0.75,
			lang:    "java",
			tier:    provider.TierBest,
			wantMin: 384,
			wantMax: 512,
		},
		{
			name:    "function body + block shape + Go + good tier",
			ctx:     ContextFunctionBody,
			shape:   ShapeBlock,
			conf:    0.5,
			lang:    "go",
			tier:    provider.TierGood,
			wantMin: 100,
			wantMax: 200,
		},
		{
			name:    "YAML gets halved",
			ctx:     ContextFunctionBody,
			shape:   ShapeBlock,
			conf:    0.5,
			lang:    "yaml",
			tier:    provider.TierGood,
			wantMin: 50,
			wantMax: 80,
		},
		{
			name:    "basic tier halves output",
			ctx:     ContextFunctionBody,
			shape:   ShapeBlock,
			conf:    0.5,
			lang:    "go",
			tier:    provider.TierBasic,
			wantMin: 50,
			wantMax: 80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := CursorAnalysis{
				Context:    tt.ctx,
				Shape:      tt.shape,
				Confidence: tt.conf,
				Reason:     "test",
			}
			budget := ComputeBudget(analysis, tt.lang, tt.tier, fim, 32768, "prefix", "suffix")
			if budget.MaxTokens < tt.wantMin || budget.MaxTokens > tt.wantMax {
				t.Errorf("MaxTokens = %d, want [%d, %d]", budget.MaxTokens, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestComputeBudgetTemperature(t *testing.T) {
	fim := &provider.FIMConfig{
		Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>",
	}

	tests := []struct {
		name     string
		ctx      CursorContext
		conf     float64
		wantTemp float64
	}{
		{"import block", ContextImportBlock, 0.75, 0.0},
		{"comment block", ContextCommentBlock, 0.75, 0.15},
		{"function body", ContextFunctionBody, 0.75, 0.2},
		{"after open brace", ContextAfterOpenBrace, 0.75, 0.2},
		{"between declarations", ContextBetweenDeclarations, 0.75, 0.3},
		{"unknown", ContextUnknown, 0.75, 0.2},
		{"low confidence clamps to 0.2", ContextBetweenDeclarations, 0.5, 0.2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := CursorAnalysis{
				Context: tt.ctx, Shape: ShapeBlock, Confidence: tt.conf, Reason: "test",
			}
			budget := ComputeBudget(analysis, "go", provider.TierGood, fim, 32768, "prefix", "suffix")
			if budget.Temperature != tt.wantTemp {
				t.Errorf("Temperature = %v, want %v", budget.Temperature, tt.wantTemp)
			}
		})
	}
}

func TestComputeBudgetPrefixRatio(t *testing.T) {
	fim := &provider.FIMConfig{
		Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>",
		PrefixBudgetPct: 75,
	}

	tests := []struct {
		name       string
		prefix     string
		suffix     string
		conf       float64
		wantMinPct int
		wantMaxPct int
	}{
		{
			name:       "default ratio with mid-file cursor",
			prefix:     strings.Repeat("x", 500),
			suffix:     strings.Repeat("y", 500),
			conf:       0.75,
			wantMinPct: 70,
			wantMaxPct: 80,
		},
		{
			name:       "near file top — shift toward suffix",
			prefix:     strings.Repeat("x", 50),
			suffix:     strings.Repeat("y", 950),
			conf:       0.75,
			wantMinPct: 20,
			wantMaxPct: 30,
		},
		{
			name:       "near file end — almost all prefix",
			prefix:     strings.Repeat("x", 950),
			suffix:     strings.Repeat("y", 50),
			conf:       0.75,
			wantMinPct: 85,
			wantMaxPct: 95,
		},
		{
			name:       "low confidence — no override",
			prefix:     strings.Repeat("x", 50),
			suffix:     strings.Repeat("y", 950),
			conf:       0.5,
			wantMinPct: 70,
			wantMaxPct: 80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := CursorAnalysis{
				Context: ContextFunctionBody, Shape: ShapeBlock,
				Confidence: tt.conf, Reason: "test",
			}
			budget := ComputeBudget(analysis, "go", provider.TierGood, fim, 32768, tt.prefix, tt.suffix)
			if budget.PrefixBudgetPct < tt.wantMinPct || budget.PrefixBudgetPct > tt.wantMaxPct {
				t.Errorf("PrefixBudgetPct = %d, want [%d, %d]", budget.PrefixBudgetPct, tt.wantMinPct, tt.wantMaxPct)
			}
		})
	}
}

func TestComputeBudgetStopTokenMerge(t *testing.T) {
	fim := &provider.FIMConfig{
		Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>",
		StopTokens: []string{"<|endoftext|>", "<|fim_pad|>"},
	}
	analysis := CursorAnalysis{
		Context: ContextFunctionBody, Shape: ShapeBlock,
		Confidence: 0.75, Reason: "test",
	}

	budget := ComputeBudget(analysis, "go", provider.TierGood, fim, 32768, "prefix", "suffix")

	if len(budget.StopTokens) < 2 {
		t.Fatalf("expected at least 2 stop tokens (model), got %d", len(budget.StopTokens))
	}
	if budget.StopTokens[0] != "<|endoftext|>" {
		t.Errorf("first stop = %q, want <|endoftext|>", budget.StopTokens[0])
	}
	if budget.StopTokens[1] != "<|fim_pad|>" {
		t.Errorf("second stop = %q, want <|fim_pad|>", budget.StopTokens[1])
	}
	hasLangStop := false
	for _, s := range budget.StopTokens {
		if s == "\nfunc " {
			hasLangStop = true
			break
		}
	}
	if !hasLangStop {
		t.Error("expected Go language stop pattern \\nfunc in merged stops")
	}
}
