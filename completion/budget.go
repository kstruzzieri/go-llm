package completion

import (
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

const maxStopTokens = 10

// assembleFIMPrompt constructs the FIM prompt from control tokens and source text.
func assembleFIMPrompt(fim *provider.FIMConfig, prefix, suffix string) string {
	totalLen := len(fim.Prefix) + len(prefix) + len(fim.Suffix) + len(suffix) + len(fim.Middle)
	var b strings.Builder
	b.Grow(totalLen)
	b.WriteString(fim.Prefix)
	b.WriteString(prefix)
	b.WriteString(fim.Suffix)
	b.WriteString(suffix)
	b.WriteString(fim.Middle)
	return b.String()
}

// stripStopTokens removes any effective stop token that appears at the tail
// of the completion text. Only the tail is checked — interior matches are
// left alone so that the model may legitimately emit these bytes.
func stripStopTokens(completion string, stopTokens []string) string {
	if completion == "" || len(stopTokens) == 0 {
		return completion
	}
	for _, tok := range stopTokens {
		if tok == "" {
			continue
		}
		if strings.HasSuffix(completion, tok) {
			return completion[:len(completion)-len(tok)]
		}
	}
	return completion
}

// mergeStopTokens combines model-native and language-specific stop tokens
// into a single deduplicated list, capped at maxStopTokens. Model stops
// have highest priority and are never dropped.
func mergeStopTokens(modelStops, langStops []string) []string {
	if len(modelStops) == 0 && len(langStops) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(modelStops)+len(langStops))
	result := make([]string, 0, len(modelStops)+len(langStops))

	for _, tok := range modelStops {
		if !seen[tok] {
			seen[tok] = true
			result = append(result, tok)
		}
	}

	for _, tok := range langStops {
		if !seen[tok] && len(result) < maxStopTokens {
			seen[tok] = true
			result = append(result, tok)
		}
	}

	return result
}

// ComputedBudget holds all adaptive values for a single FIM request.
type ComputedBudget struct {
	MaxTokens       int
	NumCtx          int
	PrefixBudgetPct int
	Temperature     float64
	StopTokens      []string
}

// BudgetTrace captures the adaptive budget decisions for debugging.
type BudgetTrace struct {
	DetectedContext    CursorContext
	CompletionShape    CompletionShape
	DetectorConfidence float64
	DecisionReason     string
	Language           string
	MaxTokens          int
	NumCtx             int
	PrefixBudgetPct    int
	Temperature        float64
	ShapeMult          float64
	StopTokenCount     int
}

var contextBaseTokens = map[CursorContext]int{
	ContextImportBlock:         40,
	ContextBetweenDeclarations: 384,
	ContextFunctionBody:        128,
	ContextAfterOpenBrace:      224,
	ContextCommentBlock:        64,
	ContextUnknown:             128,
}

var languageMultiplier = map[string]float64{
	"go":         1.0,
	"python":     1.1,
	"java":       1.5,
	"typescript": 1.1,
	"javascript": 1.1,
	"rust":       1.2,
	"yaml":       0.5,
	"json":       0.5,
	"sql":        0.8,
	"ruby":       1.0,
	"c":          1.0,
	"cpp":        1.2,
}

var tierMultiplier = map[provider.Tier]float64{
	provider.TierBasic: 0.5,
	provider.TierGood:  1.0,
	provider.TierGreat: 1.25,
	provider.TierBest:  1.5,
}

var shapeMultiplier = map[CompletionShape]float64{
	ShapeToken:       0.3,
	ShapeExpression:  0.5,
	ShapeLine:        0.75,
	ShapeBlock:       1.0,
	ShapeDeclaration: 1.5,
	ShapeUnknown:     1.0,
}

var contextTemperature = map[CursorContext]float64{
	ContextImportBlock:         0.0,
	ContextCommentBlock:        0.15,
	ContextFunctionBody:        0.2,
	ContextAfterOpenBrace:      0.2,
	ContextBetweenDeclarations: 0.3,
	ContextUnknown:             0.2,
}

// ComputeBudget calculates an adaptive budget from cursor analysis, language,
// model tier, FIM config, context window, and raw prefix/suffix.
func ComputeBudget(analysis CursorAnalysis, lang string, tier provider.Tier,
	fim *provider.FIMConfig, ctxWindow int, prefix, suffix string) ComputedBudget {

	base := contextBaseTokens[analysis.Context]
	if base == 0 {
		base = 128
	}

	langMult := languageMultiplier[lang]
	if langMult == 0 {
		langMult = 1.0
	}

	tMult := tierMultiplier[tier]
	if tMult == 0 {
		tMult = 1.0
	}

	sMult := shapeMultiplier[analysis.Shape]
	if sMult == 0 {
		sMult = 1.0
	}

	maxTokens := int(float64(base) * langMult * tMult * sMult)
	if maxTokens < 16 {
		maxTokens = 16
	}
	if maxTokens > 512 {
		maxTokens = 512
	}

	temp := contextTemperature[analysis.Context]
	if analysis.Confidence < 0.75 && temp > 0.2 {
		temp = 0.2
	}

	familyPct := fim.PrefixBudgetPct
	if familyPct == 0 {
		familyPct = 75
	}

	budgetPct := familyPct
	if analysis.Confidence >= 0.75 {
		totalLen := len(prefix) + len(suffix)
		if totalLen > 0 {
			cursorRatio := float64(len(prefix)) / float64(totalLen)
			if cursorRatio < 0.15 {
				budgetPct = familyPct - 50
				if budgetPct < 20 {
					budgetPct = 20
				}
			} else if cursorRatio > 0.85 {
				budgetPct = familyPct + 15
				if budgetPct > 95 {
					budgetPct = 95
				}
			}
		}
	}

	hints := languageHintsFor(lang)
	stops := mergeStopTokens(fim.StopTokens, hints.StopPatterns)

	return ComputedBudget{
		MaxTokens:       maxTokens,
		NumCtx:          ctxWindow,
		PrefixBudgetPct: budgetPct,
		Temperature:     temp,
		StopTokens:      stops,
	}
}
