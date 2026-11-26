package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// recallDecisionPath lists, per file, the declarations on the scoring and
// decision path, including the trial order used by capture and report.
// Nil means every declaration; methods are named without their receiver.
// Shared capture verification, destination checks, registration validation
// and report rendering have their own tests outside this pin.
var recallDecisionPath = []struct {
	file  string
	names []string
}{
	{"recall_score.go", nil},
	{"recall_gate.go", nil},
	{"recall_report.go", []string{"recallReply", "scoreRecallA", "scoreRecallB", "pairRecallA", "pairRecallB",
		"recallACell", "recallBCell", "evaluateRecallHeadroom", "recallDeterminism",
		"recallRegisteredSet", "has", "recallIndexOf", "get", "recallTraceID", "unattempted",
		"runRecallReport", "recallLabelTokens", "add", "recallHeadroomReport", "recallHeadroomPrecondition",
		"loadRecallCaptures", "recallCheckArtifact", "verifyRecallTrialOrder"}},
	{"recall.go", []string{"recallNumberPattern", "recallWorkspaceFile", "currentValue", "validateRecallTrace", "validateRecallTurns"}},
	{"recall_build.go", []string{"parseRecallTraceID", "recallCells"}},
	{"recall_registration.go", []string{"recallCellKey", "recallBaseOrder", "recallCaptureCounterbalanceScheme",
		"recallCaptureOrder", "recallTrialOrder", "recallSplitFamilyCount"}},
	{"calibration.go", []string{"pairIDHash"}},
	{"paired.go", []string{"modelKey"}},
	{"scorer.go", []string{"canonicalCandidateModelKey", "normalizeModelSelector", "openAICompatTransport"}},
	{"target.go", []string{"defaultBenchProvider", "supportedCandidateProvider"}},
	{"injection.go", []string{"validateToolEndingTurns"}},
	{"injection_outcome.go", []string{"decodeRawArgs"}},
}

// recallDecisionSource returns the pinned declarations as a token stream.
// Comments and formatting that leave the tokens and statement boundaries
// intact do not change the digest.
func recallDecisionSource(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, f := range recallDecisionPath {
		src, err := os.ReadFile(f.file)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, d := range file.Decls {
			if f.names == nil || matchRecallDeclaration(d, f.names, found) {
				fmt.Fprintf(&out, "%s\n", f.file)
				writeRecallDecisionTokens(&out, src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset])
			}
		}
		for _, n := range f.names {
			if !found[n] {
				t.Fatalf("%s: pinned declaration %s not found; update recallDecisionPath", f.file, n)
			}
		}
	}
	return out.Bytes()
}

// matchRecallDeclaration selects a whole declaration. A grouped const can
// depend on the preceding specs through iota or an implicit initializer.
func matchRecallDeclaration(d ast.Decl, names []string, found map[string]bool) bool {
	matched := false
	match := func(id *ast.Ident) {
		if slices.Contains(names, id.Name) {
			found[id.Name] = true
			matched = true
		}
	}
	switch decl := d.(type) {
	case *ast.FuncDecl:
		match(decl.Name)
	case *ast.GenDecl:
		for _, spec := range decl.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				for _, id := range s.Names {
					match(id)
				}
			case *ast.TypeSpec:
				match(s.Name)
			}
		}
	}
	return matched
}

func writeRecallDecisionTokens(out *bytes.Buffer, code []byte) {
	var s scanner.Scanner
	s.Init(token.NewFileSet().AddFile("", -1, len(code)), code, nil, 0)
	// A newline can insert a significant semicolon, e.g. between a switch
	// initializer and its opening brace. Only omit optional terminators.
	pendingSemicolon := false
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			return
		}
		if pendingSemicolon && tok != token.RPAREN && tok != token.RBRACE {
			fmt.Fprintln(out, "SEMICOLON")
		}
		pendingSemicolon = tok == token.SEMICOLON
		if !pendingSemicolon {
			fmt.Fprintf(out, "%s %s\n", tok, lit)
		}
	}
}

func TestRecallDecisionTokensKeepStatementBoundaries(t *testing.T) {
	tokens := func(code string) string {
		var out bytes.Buffer
		writeRecallDecisionTokens(&out, []byte(code))
		return out.String()
	}
	tag := tokens("func f() { switch value() { case true: g() } }")
	formatted := tokens("func f() {\n// comment\n switch value() {\n case true: g()\n }\n}")
	if tag != formatted {
		t.Fatal("formatting changed the token stream")
	}
	implicit := tokens("func f() { switch value()\n { case true: g() } }")
	explicit := tokens("func f() { switch value(); { case true: g() } }")
	if implicit != explicit || tag == implicit {
		t.Fatal("the switch initializer and switch tag must have distinct token streams")
	}
}

func TestRecallScoringAndDecisionCodePinned(t *testing.T) {
	sum := sha256.Sum256(recallDecisionSource(t))
	if got := hex.EncodeToString(sum[:]); !strings.HasSuffix(recallScorerRevision, "+sha256:"+got) {
		t.Errorf("the registered scorer_revision %q must include the current source digest %s: before the confirmatory freeze, update recallDecisionSourceSHA256, update recallDecisionRuleText if the rule changed, and regenerate with %s; after the freeze, revert the change instead",
			recallScorerRevision, got, recallFixtureRegenCommand)
	}
}
