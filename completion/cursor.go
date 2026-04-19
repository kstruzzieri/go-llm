package completion

import (
	"path/filepath"
	"strings"
)

// CursorContext classifies where the cursor sits in the source file.
type CursorContext int

const (
	ContextUnknown CursorContext = iota
	ContextImportBlock
	ContextBetweenDeclarations
	ContextFunctionBody
	ContextAfterOpenBrace
	ContextCommentBlock
)

// String returns the name of the cursor context.
func (c CursorContext) String() string {
	switch c {
	case ContextImportBlock:
		return "import_block"
	case ContextBetweenDeclarations:
		return "between_declarations"
	case ContextFunctionBody:
		return "function_body"
	case ContextAfterOpenBrace:
		return "after_open_brace"
	case ContextCommentBlock:
		return "comment_block"
	default:
		return "unknown"
	}
}

// CompletionShape classifies what kind of completion is likely needed.
type CompletionShape int

const (
	ShapeUnknown CompletionShape = iota
	ShapeToken
	ShapeExpression
	ShapeLine
	ShapeBlock
	ShapeDeclaration
)

// String returns the name of the completion shape.
func (s CompletionShape) String() string {
	switch s {
	case ShapeToken:
		return "token"
	case ShapeExpression:
		return "expression"
	case ShapeLine:
		return "line"
	case ShapeBlock:
		return "block"
	case ShapeDeclaration:
		return "declaration"
	default:
		return "unknown"
	}
}

// CursorAnalysis captures the lightweight heuristic result.
type CursorAnalysis struct {
	Context    CursorContext
	Shape      CompletionShape
	Confidence float64
	Reason     string
}

// LanguageHints provides per-language keywords for cursor detection
// and anchored structural stop patterns for FIM termination.
type LanguageHints struct {
	ImportKeywords      []string
	DeclarationKeywords []string
	CommentPrefixes     []string
	StopPatterns        []string
}

var extToLanguage = map[string]string{
	".go":   "go",
	".py":   "python",
	".ts":   "typescript",
	".tsx":  "typescript",
	".js":   "javascript",
	".jsx":  "javascript",
	".java": "java",
	".rs":   "rust",
	".rb":   "ruby",
	".c":    "c",
	".cpp":  "cpp",
	".cc":   "cpp",
	".sql":  "sql",
	".yaml": "yaml",
	".yml":  "yaml",
	".json": "json",
}

// resolveLanguage returns the explicit language if set, otherwise infers
// from the file extension. Returns "" if unknown.
func resolveLanguage(explicit, filePath string) string {
	if explicit != "" {
		return strings.ToLower(explicit)
	}
	if filePath == "" {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	return extToLanguage[ext]
}

var knownLanguageHints = map[string]LanguageHints{
	"go": {
		ImportKeywords:      []string{"import "},
		DeclarationKeywords: []string{"func ", "type ", "var ", "const "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nfunc ", "\ntype ", "\nvar ", "\nconst "},
	},
	"python": {
		ImportKeywords:      []string{"import ", "from "},
		DeclarationKeywords: []string{"def ", "class ", "async def "},
		CommentPrefixes:     []string{"#"},
		StopPatterns:        []string{"\ndef ", "\nclass ", "\nasync def "},
	},
	"java": {
		ImportKeywords:      []string{"import "},
		DeclarationKeywords: []string{"public ", "private ", "protected ", "class ", "interface "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\npublic ", "\nprivate ", "\nprotected ", "\nclass "},
	},
	"typescript": {
		ImportKeywords:      []string{"import ", "require("},
		DeclarationKeywords: []string{"function ", "class ", "const ", "export ", "interface "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nfunction ", "\nclass ", "\nexport ", "\ninterface "},
	},
	"javascript": {
		ImportKeywords:      []string{"import ", "require("},
		DeclarationKeywords: []string{"function ", "class ", "const ", "export "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nfunction ", "\nclass ", "\nexport "},
	},
	"rust": {
		ImportKeywords:      []string{"use "},
		DeclarationKeywords: []string{"fn ", "struct ", "enum ", "impl ", "trait ", "pub fn "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nfn ", "\npub fn ", "\nstruct ", "\nenum ", "\nimpl ", "\ntrait "},
	},
	"ruby": {
		ImportKeywords:      []string{"require ", "require_relative "},
		DeclarationKeywords: []string{"def ", "class ", "module "},
		CommentPrefixes:     []string{"#"},
		StopPatterns:        []string{"\ndef ", "\nclass ", "\nmodule "},
	},
	"c": {
		ImportKeywords:      []string{"#include "},
		DeclarationKeywords: []string{"void ", "int ", "char ", "struct ", "typedef "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nvoid ", "\nint ", "\nchar ", "\nstruct "},
	},
	"cpp": {
		ImportKeywords:      []string{"#include "},
		DeclarationKeywords: []string{"void ", "int ", "class ", "struct ", "namespace ", "template "},
		CommentPrefixes:     []string{"//", "/*"},
		StopPatterns:        []string{"\nvoid ", "\nint ", "\nclass ", "\nstruct ", "\nnamespace "},
	},
	"sql": {
		ImportKeywords:      nil,
		DeclarationKeywords: []string{"SELECT ", "CREATE ", "INSERT ", "UPDATE ", "DELETE "},
		CommentPrefixes:     []string{"--", "/*"},
		StopPatterns:        nil,
	},
	"yaml": {
		ImportKeywords:      nil,
		DeclarationKeywords: nil,
		CommentPrefixes:     []string{"#"},
		StopPatterns:        nil,
	},
	"json": {
		ImportKeywords:      nil,
		DeclarationKeywords: nil,
		CommentPrefixes:     nil,
		StopPatterns:        nil,
	},
}

var genericHints = LanguageHints{
	DeclarationKeywords: []string{"func ", "function ", "class ", "def "},
	CommentPrefixes:     []string{"//", "#"},
	StopPatterns:        nil,
}

// languageHintsFor returns the language hints for the given language,
// falling back to generic hints for unknown languages.
func languageHintsFor(lang string) LanguageHints {
	if h, ok := knownLanguageHints[lang]; ok {
		return h
	}
	return genericHints
}

// AnalyzeCursor examines the last ~200 chars of prefix and first ~200 chars
// of suffix to classify cursor position and intended completion shape.
// Detection uses string scanning only — no regex, no AST.
func AnalyzeCursor(prefix, suffix, language string) CursorAnalysis {
	hints := languageHintsFor(language)

	prefixWindow := prefix
	if len(prefixWindow) > 200 {
		prefixWindow = prefixWindow[len(prefixWindow)-200:]
	}
	suffixWindow := suffix
	if len(suffixWindow) > 200 {
		suffixWindow = suffixWindow[:200]
	}

	ctx, ctxConf, ctxReason := detectContext(prefixWindow, suffixWindow, hints)
	shape, shapeConf, shapeReason := detectShape(prefixWindow, suffixWindow, ctx, hints)

	// Overall confidence takes the stronger of the two signals: a strong
	// lexical shape match (e.g., "return ", "= ") corroborates the context,
	// so a weak context reading should not cap a strong shape signal.
	confidence := ctxConf
	if shapeConf > confidence {
		confidence = shapeConf
	}

	return CursorAnalysis{
		Context:    ctx,
		Shape:      shape,
		Confidence: confidence,
		Reason:     ctxReason + "; " + shapeReason,
	}
}

func detectContext(prefix, suffix string, hints LanguageHints) (CursorContext, float64, string) {
	if isInImportBlock(prefix, hints) {
		return ContextImportBlock, 0.75, "import keyword in prefix"
	}
	if isBetweenDeclarations(prefix, suffix, hints) {
		return ContextBetweenDeclarations, 0.75, "between top-level declarations"
	}
	if endsWithOpenBrace(prefix) {
		return ContextAfterOpenBrace, 1.0, "prefix ends with open brace"
	}
	if isInComment(prefix, hints) {
		return ContextCommentBlock, 0.75, "comment prefix detected"
	}
	if prefix == "" && suffix == "" {
		return ContextBetweenDeclarations, 0.25, "empty file"
	}
	if prefix == "" {
		return ContextBetweenDeclarations, 0.25, "cursor at file start"
	}
	if suffix == "" {
		return ContextBetweenDeclarations, 0.5, "cursor at file end"
	}
	return ContextFunctionBody, 0.5, "inside code block"
}

func detectShape(prefix, suffix string, ctx CursorContext, hints LanguageHints) (CompletionShape, float64, string) {
	switch ctx {
	case ContextImportBlock:
		return ShapeToken, 0.75, "import completion"
	case ContextBetweenDeclarations:
		return ShapeDeclaration, 0.75, "declaration boundary"
	case ContextCommentBlock:
		return ShapeLine, 0.75, "comment continuation"
	case ContextAfterOpenBrace:
		return ShapeBlock, 1.0, "block after open brace"
	}

	trimmed := strings.TrimRight(prefix, " \t")

	if strings.HasSuffix(trimmed, ",") || strings.HasSuffix(trimmed, ".") {
		return ShapeToken, 0.5, "after comma/dot"
	}
	if strings.HasSuffix(trimmed, "= ") || strings.HasSuffix(prefix, "= ") {
		return ShapeExpression, 0.5, "after assignment"
	}
	if strings.HasSuffix(trimmed, "return ") || strings.HasSuffix(prefix, "return ") {
		return ShapeExpression, 0.75, "after return"
	}
	if strings.HasSuffix(trimmed, "(") || strings.HasSuffix(trimmed, "[") {
		return ShapeExpression, 0.5, "after open paren/bracket"
	}
	if strings.HasSuffix(trimmed, ": ") || strings.HasSuffix(prefix, ": ") {
		return ShapeExpression, 0.5, "after colon"
	}

	return ShapeBlock, 0.25, "default shape"
}

func isInImportBlock(prefix string, hints LanguageHints) bool {
	if len(hints.ImportKeywords) == 0 || prefix == "" {
		return false
	}

	lastLine := prefix
	if idx := strings.LastIndex(lastLine, "\n"); idx >= 0 {
		lastLine = lastLine[idx+1:]
	}
	trimmedLine := strings.TrimLeft(lastLine, " \t")

	for _, kw := range hints.ImportKeywords {
		if strings.HasPrefix(trimmedLine, kw) {
			return true
		}
	}

	groupStart := strings.LastIndex(prefix, "import (")
	if groupStart == -1 {
		return false
	}

	groupBody := prefix[groupStart+len("import ("):]
	return !strings.Contains(groupBody, ")")
}

func isBetweenDeclarations(prefix, suffix string, hints LanguageHints) bool {
	if len(hints.DeclarationKeywords) == 0 {
		return false
	}
	trimmedPrefix := strings.TrimRight(prefix, " \t\n")

	prefixEndsDecl := strings.HasSuffix(trimmedPrefix, "}") ||
		strings.HasSuffix(trimmedPrefix, ")")

	if !prefixEndsDecl && trimmedPrefix != "" {
		return false
	}

	trimmedSuffix := strings.TrimLeft(suffix, " \t\n")
	for _, kw := range hints.DeclarationKeywords {
		if strings.HasPrefix(trimmedSuffix, kw) {
			return true
		}
	}

	return trimmedSuffix == "" && prefixEndsDecl
}

func endsWithOpenBrace(prefix string) bool {
	trimmed := strings.TrimRight(prefix, " \t")
	return strings.HasSuffix(trimmed, "{\n") ||
		strings.HasSuffix(trimmed, "{\n\t") ||
		strings.HasSuffix(trimmed, "{\n    ") ||
		strings.HasSuffix(trimmed, "{")
}

func isInComment(prefix string, hints LanguageHints) bool {
	if len(hints.CommentPrefixes) == 0 {
		return false
	}
	lines := strings.Split(prefix, "\n")
	if len(lines) == 0 {
		return false
	}
	lastLine := strings.TrimLeft(lines[len(lines)-1], " \t")
	for _, cp := range hints.CommentPrefixes {
		if strings.HasPrefix(lastLine, cp) {
			return true
		}
	}
	return false
}
