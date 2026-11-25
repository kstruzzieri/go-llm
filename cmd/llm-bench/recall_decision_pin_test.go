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
	"testing"
)

// recallDecisionSourceSHA256 pins the recall scoring and decision code that
// recallScorerRevision and recallDecisionRuleText name: every declaration in
// recall_score.go and recall_gate.go, and the functions that assemble, score,
// pair, count and decide from the replies a report reads (recallDecisionPath).
// A registration records both names and the report refuses others, so a
// change to this code must bump the revision (and the rule text if behavior
// changed) and re-register; this pin is what notices the change. Outside it,
// by design: the capture-verification code shared with the injection report
// (manifest and artifact loading, trace hashing, model selectors), the
// capture destination checks (recall_destination.go), the
// registration loader (its tests and the file digest bind it), and rendering.
const recallDecisionSourceSHA256 = "34c8968f0061a7a27e607e2f8407833ccaf4b647152adc2beb325070a7a8f88f"

// recallDecisionPath lists, per file, the declarations on the scoring and
// decision path; nil means every declaration in the file. Methods are named
// without their receiver.
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
	{"recall_registration.go", []string{"recallCellKey", "recallTrialOrder", "recallSplitFamilyCount"}},
	{"paired.go", []string{"modelKey"}},
	{"scorer.go", []string{"canonicalCandidateModelKey"}},
	{"injection_outcome.go", []string{"decodeRawArgs"}},
}

// recallDecisionSource returns the pinned code as its token stream
// (go/scanner, comments and the semicolons a line break inserts dropped), so
// comment and whitespace edits leave it unchanged and any code edit changes
// it (so does a reformat that adds or drops a trailing comma, or reordered
// declarations: those fail loudly and need only a pin update).
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
	if got := hex.EncodeToString(sum[:]); got != recallDecisionSourceSHA256 {
		t.Errorf("the recall scoring or decision code changed (source sha256 %s, pinned %s): before the confirmatory freeze, bump recallScorerRevision (now %q), update recallDecisionRuleText if the behavior changed, regenerate the registrations with %s and update recallDecisionSourceSHA256; after the freeze, revert the change instead",
			got, recallDecisionSourceSHA256, recallScorerRevision, recallFixtureRegenCommand)
	}
}
