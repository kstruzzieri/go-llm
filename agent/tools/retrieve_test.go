package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

type fakeRetriever struct {
	results []rag.SearchResult
}

func (f fakeRetriever) Retrieve(context.Context, string, int) ([]rag.SearchResult, error) {
	return f.results, nil
}
func (f fakeRetriever) BuildContext(results []rag.SearchResult, _ int) string {
	out := ""
	for _, r := range results {
		out += r.Chunk.Content + "\n"
	}
	return out
}

func TestRetrieveReturnsContextAndAttribution(t *testing.T) {
	fr := fakeRetriever{results: []rag.SearchResult{
		{Chunk: rag.Chunk{StableKey: "k1", Source: "a.go", StartLine: 1, EndLine: 5, Content: "hello"}, Score: 0.9},
	}}
	tool := Retrieve{R: fr, K: 4, MaxTokens: 1000}

	if tool.Spec().Name != "retrieve" {
		t.Fatalf("spec name = %q", tool.Spec().Name)
	}
	if tool.Effect().Class != agent.Read || tool.Effect().Approval != agent.ApprovalNever {
		t.Fatalf("retrieve must be Read/ApprovalNever, got %+v", tool.Effect())
	}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Content == "" || out.IsError {
		t.Fatalf("expected content, got %+v", out)
	}
	if out.Attrib == nil || len(out.Attrib.Sources) != 1 || out.Attrib.Sources[0].StableKey != "k1" {
		t.Fatalf("expected attribution for k1, got %+v", out.Attrib)
	}
}

func TestRetrieveMalformedArgsIsError(t *testing.T) {
	tool := Retrieve{R: fakeRetriever{}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":}`))
	if err != nil {
		t.Fatalf("Invoke should not hard-error: %v", err)
	}
	if !out.IsError {
		t.Fatal("malformed args must yield IsError observation")
	}
}

type capturingRetriever struct {
	gotK         int
	gotMaxTokens int
	err          error
}

func (c *capturingRetriever) Retrieve(_ context.Context, _ string, k int) ([]rag.SearchResult, error) {
	c.gotK = k
	if c.err != nil {
		return nil, c.err
	}
	return []rag.SearchResult{{Chunk: rag.Chunk{StableKey: "k", Content: "x"}}}, nil
}
func (c *capturingRetriever) BuildContext(_ []rag.SearchResult, maxTokens int) string {
	c.gotMaxTokens = maxTokens
	return "x"
}

func TestRetrieveEmptyQueryIsError(t *testing.T) {
	tool := Retrieve{R: &capturingRetriever{}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":""}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError {
		t.Fatal("empty query must yield an IsError observation")
	}
}

func TestRetrieveKAndMaxTokensDefaults(t *testing.T) {
	cr := &capturingRetriever{}
	tool := Retrieve{R: cr} // K and MaxTokens both zero -> defaults
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if cr.gotK != defaultRetrieveK {
		t.Fatalf("k = %d, want default %d", cr.gotK, defaultRetrieveK)
	}
	if cr.gotMaxTokens != defaultRetrieveMaxTokens {
		t.Fatalf("maxTokens = %d, want default %d", cr.gotMaxTokens, defaultRetrieveMaxTokens)
	}
}

func TestRetrieveBackendErrorIsError(t *testing.T) {
	tool := Retrieve{R: &capturingRetriever{err: errors.New("boom")}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`))
	if err != nil {
		t.Fatalf("Invoke should not hard-error: %v", err)
	}
	if !out.IsError {
		t.Fatal("backend error must yield an IsError observation")
	}
}
