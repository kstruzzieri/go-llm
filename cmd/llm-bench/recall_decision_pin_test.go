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
	{"recall.go", []string{"recallWorkspaceFile", "currentValue", "validateRecallTrace", "validateRecallTurns"}},
	{"recall_build.go", []string{"parseRecallTraceID", "recallCells"}},
	{"recall_registration.go", []string{"recallCellKey", "recallBaseOrder", "recallCaptureOrder", "recallTrialOrder", "recallSplitFamilyCount"}},
	{"calibration.go", []string{"pairIDHash"}},
	{"paired.go", []string{"modelKey"}},
	{"scorer.go", []string{"canonicalCandidateModelKey"}},
	{"injection_outcome.go", []string{"decodeRawArgs"}},
}

// recallDecisionSource returns the pinned code as its token stream
// (go/scanner, comments and the semicolons a line break inserts dropped), so
// comment and whitespace edits leave it unchanged and any code edit changes
// it (so does adding or dropping a trailing comma, or reordering declarations).
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
			var parts [][]byte
			if f.names == nil {
				parts = [][]byte{src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset]}
			} else {
				parts = pinnedDeclParts(fset, src, d, f.names, found)
			}
			for _, code := range parts {
				var s scanner.Scanner
				s.Init(token.NewFileSet().AddFile(f.file, -1, len(code)), code, nil, 0)
				fmt.Fprintf(&out, "%s\n", f.file)
				for {
					_, tok, lit := s.Scan()
					if tok == token.EOF {
						break
					}
					if tok == token.SEMICOLON && lit == "\n" {
						continue
					}
					fmt.Fprintf(&out, "%s %s\n", tok, lit)
				}
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

// pinnedDeclParts returns the source slices of d matching names and records
// each matched identifier in found.
func pinnedGenDeclParts(fset *token.FileSet, src []byte, gd *ast.GenDecl, names []string, found map[string]bool) [][]byte {
	var parts [][]byte
	for _, spec := range gd.Specs {
		var ids []*ast.Ident
		switch s := spec.(type) {
		case *ast.ValueSpec:
			ids = s.Names
		case *ast.TypeSpec:
			ids = []*ast.Ident{s.Name}
		}
		matched := false
		for _, id := range ids {
			if slices.Contains(names, id.Name) {
				found[id.Name] = true
				matched = true
			}
		}
		if !matched {
			continue
		}
		if len(gd.Specs) == 1 {
			parts = append(parts, src[fset.Position(gd.Pos()).Offset:fset.Position(gd.End()).Offset])
			break
		}
		part := append([]byte(gd.Tok.String()+" "), src[fset.Position(spec.Pos()).Offset:fset.Position(spec.End()).Offset]...)
		parts = append(parts, part)
	}
	return parts
}

func pinnedDeclParts(fset *token.FileSet, src []byte, d ast.Decl, names []string, found map[string]bool) [][]byte {
	switch decl := d.(type) {
	case *ast.FuncDecl:
		if slices.Contains(names, decl.Name.Name) {
			found[decl.Name.Name] = true
			return [][]byte{src[fset.Position(decl.Pos()).Offset:fset.Position(decl.End()).Offset]}
		}
	case *ast.GenDecl:
		return pinnedGenDeclParts(fset, src, decl, names, found)
	}
	return nil
}

func TestRecallScoringAndDecisionCodePinned(t *testing.T) {
	src := recallDecisionSource(t)
	if !bytes.Contains(src, []byte("IDENT recallBaseOrder\n")) {
		t.Fatal("recallDecisionSource must include var recallBaseOrder")
	}
	sum := sha256.Sum256(src)
	if got := hex.EncodeToString(sum[:]); !strings.HasSuffix(recallScorerRevision, "+sha256:"+got) {
		t.Errorf("the registered scorer_revision %q must include the current source digest %s: before the confirmatory freeze, update recallDecisionSourceSHA256, update recallDecisionRuleText if the rule changed, and regenerate with %s; after the freeze, revert the change instead",
			recallScorerRevision, got, recallFixtureRegenCommand)
	}
}
