package main

import (
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/completion"
)

func TestCheckNoExpectShortCircuits(t *testing.T) {
	f := &Fixture{Path: "x.txt", Expect: nil}
	resp := &completion.FIMResponse{Completion: "ignored"}
	res := Check(f, resp, nil)
	if !res.OK() {
		t.Errorf("no-expect fixture should pass, got %v", res.Failures)
	}
}

func TestCheckContextMismatch(t *testing.T) {
	f := &Fixture{Expect: &Expect{Context: "function_body"}}
	resp := &completion.FIMResponse{CursorContext: completion.ContextAfterOpenBrace}
	res := Check(f, resp, nil)
	if res.OK() {
		t.Fatal("expected failure for context mismatch")
	}
	if !strings.Contains(res.Failures[0], "context=") {
		t.Errorf("unexpected failure text: %q", res.Failures[0])
	}
}

func TestCheckShapeAcceptsAny(t *testing.T) {
	f := &Fixture{Expect: &Expect{Shape: []string{"block", "declaration"}}}
	resp := &completion.FIMResponse{CompletionShape: completion.ShapeDeclaration}
	res := Check(f, resp, nil)
	if !res.OK() {
		t.Errorf("declaration should match %v, got %v", f.Expect.Shape, res.Failures)
	}
}

func TestCheckShapeRejectedWhenNotInSet(t *testing.T) {
	f := &Fixture{Expect: &Expect{Shape: []string{"declaration"}}}
	resp := &completion.FIMResponse{CompletionShape: completion.ShapeToken}
	res := Check(f, resp, nil)
	if res.OK() {
		t.Fatal("expected failure for shape not in allowed set")
	}
}

func TestCheckPrefixPctBounds(t *testing.T) {
	f := &Fixture{Expect: &Expect{MinPrefixPct: 70, MaxPrefixPct: 80}}

	resp := &completion.FIMResponse{BudgetTrace: &completion.BudgetTrace{PrefixBudgetPct: 75}}
	if res := Check(f, resp, nil); !res.OK() {
		t.Errorf("75 should be in [70,80], got %v", res.Failures)
	}

	resp.BudgetTrace.PrefixBudgetPct = 65
	if res := Check(f, resp, nil); res.OK() {
		t.Error("65 should fail min 70")
	}

	resp.BudgetTrace.PrefixBudgetPct = 85
	if res := Check(f, resp, nil); res.OK() {
		t.Error("85 should fail max 80")
	}
}

func TestCheckMinTokens(t *testing.T) {
	f := &Fixture{Expect: &Expect{MinTokens: 5}}

	resp := &completion.FIMResponse{Tokens: 10}
	if res := Check(f, resp, nil); !res.OK() {
		t.Errorf("10 should pass min 5, got %v", res.Failures)
	}

	resp.Tokens = 3
	if res := Check(f, resp, nil); res.OK() {
		t.Error("3 should fail min 5")
	}
}

func TestCheckStopLeakDetected(t *testing.T) {
	f := &Fixture{Expect: &Expect{NoStopLeak: true}}
	stops := []string{"<|endoftext|>"}

	clean := &completion.FIMResponse{Completion: "fmt.Println(x)"}
	if res := Check(f, clean, stops); !res.OK() {
		t.Errorf("clean output should pass, got %v", res.Failures)
	}

	leaked := &completion.FIMResponse{Completion: "fmt.Println(x)<|endoftext|>"}
	res := Check(f, leaked, stops)
	if res.OK() {
		t.Fatal("leaked output should fail")
	}
	if !strings.Contains(res.Failures[0], "<|endoftext|>") {
		t.Errorf("failure should name the leaked token: %q", res.Failures[0])
	}
}

func TestFindStopLeakEmptyInputs(t *testing.T) {
	if got := findStopLeak("", []string{"x"}); got != "" {
		t.Errorf("empty output should short-circuit, got %q", got)
	}
	if got := findStopLeak("body", nil); got != "" {
		t.Errorf("empty stops should short-circuit, got %q", got)
	}
	if got := findStopLeak("body", []string{""}); got != "" {
		t.Errorf("empty stop token should be skipped, got %q", got)
	}
}
