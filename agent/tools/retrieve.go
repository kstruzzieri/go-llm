// Package tools provides built-in read-only agent.Tool implementations.
package tools

import (
	"context"
	"encoding/json"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	defaultRetrieveK         = 5
	defaultRetrieveMaxTokens = 2048
)

// retriever is the minimal slice of *rag.Retriever the tool needs; abstracting
// it keeps the tool unit-testable with a fake.
type retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error)
	BuildContext(results []rag.SearchResult, maxTokens int) string
}

// Retrieve is the reference read-only retrieval built-in. #95 swaps SearchMulti
// behind the same retriever seam without changing the agent.Tool contract.
type Retrieve struct {
	R         retriever
	K         int // default top-k when the call omits k
	MaxTokens int // BuildContext budget; 0 => a sane default
}

type retrieveArgs struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

func (Retrieve) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "retrieve",
		Description: "Retrieve relevant code/context chunks for a natural-language query.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string","description":"what to search for"},
    "k":{"type":"integer","description":"max results"}
  },
  "required":["query"]
}`),
	}
}

func (t Retrieve) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (t Retrieve) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args retrieveArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if args.Query == "" {
		return agent.ToolResult{IsError: true, Content: "query is required"}, nil
	}
	k := args.K
	if k <= 0 {
		k = t.K
	}
	if k <= 0 {
		k = defaultRetrieveK
	}
	maxTokens := t.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRetrieveMaxTokens
	}

	results, err := t.R.Retrieve(ctx, args.Query, k)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "retrieval failed: " + err.Error()}, nil
	}
	content := t.R.BuildContext(results, maxTokens)

	attrib := &agent.RetrievalAttribution{Sources: make([]agent.RetrievedSource, 0, len(results))}
	for _, r := range results {
		attrib.Sources = append(attrib.Sources, agent.RetrievedSource{
			StableKey: r.Chunk.StableKey,
			Source:    r.Chunk.Source,
			StartLine: r.Chunk.StartLine,
			EndLine:   r.Chunk.EndLine,
			Score:     r.Score,
		})
	}
	return agent.ToolResult{Content: content, Attrib: attrib}, nil
}
