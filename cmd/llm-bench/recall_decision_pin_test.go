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
	{"recall_registration.go", []string{"recallCellKey", "recallCaptureOrder", "recallTrialOrder", "recallSplitFamilyCount"}},
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
			if f.names != nil {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || !slices.Contains(f.names, fd.Name.Name) {
					continue
				}
				found[fd.Name.Name] = true
			}
			code := src[fset.Position(d.Pos()).Offset:fset.Position(d.End()).Offset]
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
		for _, n := range f.names {
			if !found[n] {
				t.Fatalf("%s: pinned function %s not found; update recallDecisionPath", f.file, n)
			}
		}
	}
	return out.Bytes()
}

func TestRecallScoringAndDecisionCodePinned(t *testing.T) {
	sum := sha256.Sum256(recallDecisionSource(t))
	if got := hex.EncodeToString(sum[:]); !strings.HasSuffix(recallScorerRevision, "+sha256:"+got) {
		t.Errorf("the registered scorer_revision %q must include the current source digest %s: before the confirmatory freeze, update recallDecisionSourceSHA256, update recallDecisionRuleText if the rule changed, and regenerate with %s; after the freeze, revert the change instead",
			recallScorerRevision, got, recallFixtureRegenCommand)
	}
}
