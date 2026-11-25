package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type recallSource struct {
	setting string
	tokens  []string
}

// validateSource binds the answer to the named setting in all three versions.
// In S2 whitespace may change only where the source language ignores it.
func (f recallFamily) validateSource() error {
	var before recallSource
	for _, version := range []struct{ name, source, want string }{
		{"file_before", f.FileBefore, f.Value.Lesson},
		{"file_s1", f.FileS1, f.Value.Changed},
		{"file_s2", f.FileS2, f.Value.Lesson},
	} {
		var shape recallSource
		var err error
		switch strings.ToLower(filepath.Ext(f.NamedFile)) {
		case ".go":
			shape, err = recallGoSource(version.source, f.Value, version.want)
		case ".yaml", ".yml":
			shape, err = recallYAMLSource(version.source, f.Value, version.want)
		default:
			err = errors.New("unsupported named file format")
		}
		if err != nil {
			return fmt.Errorf("%s: value.identifier %q: %w", version.name, f.Value.Identifier, err)
		}
		switch version.name {
		case "file_before":
			before = shape
		case "file_s1":
			if before.setting != shape.setting {
				return errors.New("file_s1 moves the named setting")
			}
		case "file_s2":
			if !slices.Equal(before.tokens, shape.tokens) {
				return errors.New("file_s2 formatting changes the meaning of file_before")
			}
		}
	}
	return nil
}

func recallGoSource(source string, value RecallValue, want string) (recallSource, error) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "", source, parser.SkipObjectResolution)
	if err != nil {
		return recallSource{}, err
	}
	matches := 0
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
			continue
		}
		for _, spec := range gen.Specs {
			v := spec.(*ast.ValueSpec)
			for i, name := range v.Names {
				if name.Name != value.Identifier {
					continue
				}
				matches++
				if len(v.Names) != len(v.Values) || !recallGoValue(v.Values[i], value.Type, want) {
					return recallSource{}, fmt.Errorf("initializer must be %q", want)
				}
			}
		}
	}
	if matches != 1 {
		return recallSource{}, fmt.Errorf("want one package declaration, found %d", matches)
	}

	// The scanner preserves string literals, comments and inserted semicolons,
	// all of which can change meaning when whitespace moves.
	var lex scanner.Scanner
	lex.Init(fs.File(file.Pos()), []byte(source), nil, scanner.ScanComments)
	shape := recallSource{setting: file.Name.Name + "." + value.Identifier}
	for {
		_, tok, literal := lex.Scan()
		if tok == token.EOF {
			return shape, nil
		}
		shape.tokens = append(shape.tokens, tok.String(), literal)
	}
}

func recallGoValue(expr ast.Expr, kind, want string) bool {
	switch v := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return kind == "identifier" && v.Name == want
	case *ast.BasicLit:
		if v.Kind == token.STRING && kind == "identifier" {
			s, err := strconv.Unquote(v.Value)
			return err == nil && s == want
		}
		return kind == "number" && (v.Kind == token.INT || v.Kind == token.FLOAT) && v.Value == want
	}
	return false
}

var recallYAMLKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
var recallYAMLScalar = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// The YAML fixtures use block mappings with plain keys and scalar values.
// Reject syntax outside that subset so its nesting cannot be misinterpreted.
func recallYAMLSource(source string, value RecallValue, want string) (recallSource, error) {
	type level struct {
		indent int
		parent string
	}
	levels := []level{{indent: 0}}
	var shape recallSource
	seen := map[string]bool{}
	var lastPath string
	var lastWasMap bool
	matches := 0
	for line := range strings.SplitSeq(source, "\n") {
		body := strings.TrimLeft(line, " ")
		indent := len(line) - len(body)
		body = strings.TrimSuffix(body, "\r")
		if body == "" || strings.HasPrefix(body, "#") {
			continue
		}
		dedented := indent < levels[len(levels)-1].indent
		for len(levels) > 1 && indent < levels[len(levels)-1].indent {
			levels = levels[:len(levels)-1]
		}
		if indent != levels[len(levels)-1].indent {
			if dedented || !lastWasMap {
				return recallSource{}, errors.New("invalid YAML mapping indentation")
			}
			levels = append(levels, level{indent, lastPath})
		}
		key, rhs, ok := strings.Cut(body, ":")
		if !ok || !recallYAMLKey.MatchString(key) || (rhs != "" && rhs[0] != ' ') {
			return recallSource{}, errors.New("expected a plain YAML mapping entry")
		}
		rhs, _, _ = strings.Cut(rhs, " #")
		rhs = strings.Trim(rhs, " ")
		if rhs != "" && !recallYAMLScalar.MatchString(rhs) {
			return recallSource{}, errors.New("expected a plain YAML scalar")
		}
		lastPath = levels[len(levels)-1].parent + "/" + key
		lastWasMap = rhs == ""
		if seen[lastPath] {
			return recallSource{}, fmt.Errorf("duplicate YAML key %q", lastPath)
		}
		seen[lastPath] = true
		shape.tokens = append(shape.tokens, lastPath, rhs)
		if key == value.Identifier {
			shape.setting = lastPath
			matches++
			if rhs != want || rhs == "" {
				return recallSource{}, fmt.Errorf("value must be %q", want)
			}
		}
	}
	if matches != 1 {
		return recallSource{}, fmt.Errorf("want one YAML setting, found %d", matches)
	}
	return shape, nil
}
