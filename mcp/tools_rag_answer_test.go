package mcp

import (
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestQuoteInChunk(t *testing.T) {
	chunk := rag.Chunk{Content: "func BuildContext(results []SearchResult)  {\n\treturn x\n}"}
	tests := []struct {
		name  string
		quote string
		want  bool
	}{
		{"exact", "func BuildContext(results []SearchResult)", true},
		{"whitespace reflow", "func BuildContext(results []SearchResult) {", true}, // double space collapsed
		{"newline reflow", "[]SearchResult) { return x }", true},
		{"case mismatch", "func buildcontext", false},
		{"absent", "func NeverHere()", false},
		{"empty quote", "", false},
		{"line-anchor prefix not present", "42| func BuildContext", false}, // verify vs raw Content, not BuildContext output
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteInChunk(chunk, tc.quote); got != tc.want {
				t.Errorf("quoteInChunk(%q) = %v, want %v", tc.quote, got, tc.want)
			}
		})
	}
}

func TestExtractJSONObjects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", `{"a":1}`, []string{`{"a":1}`}},
		{"prose around", "sure:\n{\"a\":1}\nthanks", []string{`{"a":1}`}},
		{"two objects", `{"a":1} then {"b":2}`, []string{`{"a":1}`, `{"b":2}`}},
		{"nested", `{"a":{"b":2}}`, []string{`{"a":{"b":2}}`}},
		{"brace in string", `{"a":"}{"}`, []string{`{"a":"}{"}`}},
		{"escaped quote in string", `{"a":"x\"}"}`, []string{`{"a":"x\"}"}`}},
		{"none", `no json here`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSONObjects(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d objects %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Errorf("obj[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseModelAnswer(t *testing.T) {
	t.Run("single valid", func(t *testing.T) {
		ma, ok := parseModelAnswer(`{"answer":"yes","evidence":[{"id":"E1","quote":"q"}]}`)
		if !ok || ma.Answer != "yes" || len(ma.Evidence) != 1 || ma.Evidence[0].ID != "E1" {
			t.Fatalf("got %+v ok=%v", ma, ok)
		}
	})
	t.Run("echo then real picks last", func(t *testing.T) {
		in := `Here is the format {"answer":"","evidence":[]} and my answer: {"answer":"real","evidence":[]}`
		ma, ok := parseModelAnswer(in)
		if !ok || ma.Answer != "real" {
			t.Fatalf("got %+v ok=%v, want answer=real", ma, ok)
		}
	})
	t.Run("object without answer or evidence skipped", func(t *testing.T) {
		ma, ok := parseModelAnswer(`{"foo":1} {"answer":"x"}`)
		if !ok || ma.Answer != "x" {
			t.Fatalf("got %+v ok=%v", ma, ok)
		}
	})
	t.Run("no valid object", func(t *testing.T) {
		if _, ok := parseModelAnswer(`no json {nope}`); ok {
			t.Fatal("expected ok=false")
		}
	})
}

func TestDeriveAnswer(t *testing.T) {
	blocks := []evidenceBlock{
		{ID: "E1", Chunk: rag.Chunk{Content: "func Foo() int { return 1 }", Source: "a.go", StartLine: 10, EndLine: 12}},
		{ID: "E2", Chunk: rag.Chunk{Content: "var Bar = 2", Source: "b.go", StartLine: 3, EndLine: 3}},
	}

	t.Run("verified -> answer_found", func(t *testing.T) {
		ma := modelAnswer{Answer: "Foo returns 1", Evidence: []modelEvidence{{ID: "E1", Quote: "return 1"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusAnswerFound || !got.AnswerFound || !got.Verified {
			t.Fatalf("status=%s found=%v verified=%v", got.Status, got.AnswerFound, got.Verified)
		}
		if got.Answer != "Foo returns 1" {
			t.Errorf("answer = %q, want populated", got.Answer)
		}
		if len(got.Sources) != 1 || got.Sources[0].Source != "a.go" || got.Sources[0].StartLine != 10 {
			t.Errorf("sources = %+v", got.Sources)
		}
		if len(got.Quotes) != 1 || got.Quotes[0].ID != "E1" {
			t.Errorf("quotes = %+v", got.Quotes)
		}
	})

	t.Run("invented quote -> unverified, answer suppressed", func(t *testing.T) {
		ma := modelAnswer{Answer: "Foo returns 99", Evidence: []modelEvidence{{ID: "E1", Quote: "return 99"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusUnverifiedAnswer || got.AnswerFound || got.Verified {
			t.Fatalf("status=%s found=%v", got.Status, got.AnswerFound)
		}
		if got.Answer != "" {
			t.Errorf("answer = %q, want empty (suppressed)", got.Answer)
		}
		if len(got.VerificationErrors) != 1 {
			t.Errorf("verification_errors = %v", got.VerificationErrors)
		}
	})

	t.Run("unknown id -> unverified with error", func(t *testing.T) {
		ma := modelAnswer{Answer: "x", Evidence: []modelEvidence{{ID: "E9", Quote: "return 1"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusUnverifiedAnswer {
			t.Fatalf("status=%s", got.Status)
		}
		if len(got.VerificationErrors) != 1 {
			t.Errorf("verification_errors = %v", got.VerificationErrors)
		}
	})

	t.Run("empty answer -> not_in_retrieved_context", func(t *testing.T) {
		got := deriveAnswer(modelAnswer{Answer: "  "}, blocks)
		if got.Status != statusNotInRetrievedContext || got.AnswerFound {
			t.Fatalf("status=%s found=%v", got.Status, got.AnswerFound)
		}
	})
}
