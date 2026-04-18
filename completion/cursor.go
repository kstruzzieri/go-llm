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
