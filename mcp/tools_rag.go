package mcp

import (
	"context"
	"encoding/json"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/rag"
)

func (s *Server) registerRAGTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_index_file",
		Description: "Index a single file into the RAG vector store.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "File path to index"},
			},
			"required": []string{"path"},
		},
	}, s.handleRAGIndexFile)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_index_directory",
		Description: "Index all supported files in a directory tree into the RAG vector store.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Directory path to index"},
				"extensions":  map[string]any{"type": "array", "description": "File extensions to include (e.g. [\".go\", \".py\"])", "items": map[string]any{"type": "string"}},
				"concurrency": map[string]any{"type": "integer", "description": "Number of concurrent workers (default: 4)"},
			},
			"required": []string{"path"},
		},
	}, s.handleRAGIndexDirectory)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_search",
		Description: "Search the RAG index for relevant chunks.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Search query"},
				"top_k": map[string]any{"type": "integer", "description": "Number of results (default: 5)"},
			},
			"required": []string{"query"},
		},
	}, s.handleRAGSearch)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_answer",
		Description: "Audited RAG answer: retrieves context, asks the model for an answer with supporting quotes, verifies each quote against retrieved context, and returns machine-readable JSON with a server-derived status.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question":            map[string]any{"type": "string", "description": "The question to answer from indexed context"},
				"model":               map[string]any{"type": "string", "description": "Model name (uses configured default if omitted)"},
				"top_k":               map[string]any{"type": "integer", "description": "Number of chunks to retrieve (default: 5)"},
				"include_diagnostics": map[string]any{"type": "boolean", "description": "Include retrieval/verification diagnostics in the response"},
			},
			"required": []string{"question"},
		},
	}, s.handleRAGAnswer)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_stats",
		Description: "Get vector store statistics (chunk count, source count, dimensions, storage size).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, s.handleRAGStats)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_delete",
		Description: "Remove all chunks for a source from the vector store.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"source": map[string]any{"type": "string", "description": "Source identifier to delete"},
			},
			"required": []string{"source"},
		},
	}, s.handleRAGDelete)
}

// requireRAG checks that RAG is enabled and the relevant component is available.
// Returns a tool error result if RAG is unavailable, or nil if OK.
func (s *Server) requireRAG() *gomcp.CallToolResult {
	if s.ragDisabled {
		return toolError("rag", "RAG is disabled on this server")
	}
	return nil
}

func (s *Server) handleRAGIndexFile(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Path == "" {
		return toolError("validation", "path must not be empty"), nil
	}

	s.mu.RLock()
	indexer := s.indexer
	s.mu.RUnlock()

	if indexer == nil {
		return toolError("rag", "indexer unavailable; embedding model may not be resolved"), nil
	}

	if err := indexer.IndexFile(ctx, args.Path); err != nil {
		return toolError("rag", "index file: %v", err), nil
	}

	return toolResult("indexed " + args.Path), nil
}

func (s *Server) handleRAGIndexDirectory(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	var args struct {
		Path        string   `json:"path"`
		Extensions  []string `json:"extensions,omitempty"`
		Concurrency int      `json:"concurrency,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Path == "" {
		return toolError("validation", "path must not be empty"), nil
	}

	s.mu.RLock()
	indexer := s.indexer
	s.mu.RUnlock()

	if indexer == nil {
		return toolError("rag", "indexer unavailable; embedding model may not be resolved"), nil
	}

	var opts []rag.IndexDirOption
	if len(args.Extensions) > 0 {
		opts = append(opts, rag.WithExtensions(args.Extensions...))
	}
	if args.Concurrency > 0 {
		opts = append(opts, rag.WithConcurrency(args.Concurrency))
	}

	if err := indexer.IndexDirectory(ctx, args.Path, opts...); err != nil {
		return toolError("rag", "index directory: %v", err), nil
	}

	return toolResult("indexed directory " + args.Path), nil
}

func (s *Server) handleRAGSearch(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	var args struct {
		Query string `json:"query"`
		TopK  int    `json:"top_k,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Query == "" {
		return toolError("validation", "query must not be empty"), nil
	}

	s.mu.RLock()
	retriever := s.retriever
	s.mu.RUnlock()

	if retriever == nil {
		return toolError("rag", "retriever unavailable; embedding model may not be resolved"), nil
	}

	topK := args.TopK
	if topK <= 0 {
		topK = 5
	}

	results, err := retriever.Retrieve(ctx, args.Query, topK)
	if err != nil {
		return toolError("rag", "search: %v", err), nil
	}

	data, err := json.Marshal(results)
	if err != nil {
		return toolError("rag", "marshal results: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handleRAGStats(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()

	if store == nil {
		return toolError("rag", "vector store unavailable"), nil
	}

	stats, err := store.Stats(ctx)
	if err != nil {
		return toolError("rag", "stats: %v", err), nil
	}

	data, err := json.Marshal(stats)
	if err != nil {
		return toolError("rag", "marshal stats: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func (s *Server) handleRAGDelete(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if r := s.requireRAG(); r != nil {
		return r, nil
	}

	var args struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Source == "" {
		return toolError("validation", "source must not be empty"), nil
	}

	s.mu.RLock()
	store := s.store
	s.mu.RUnlock()

	if store == nil {
		return toolError("rag", "vector store unavailable"), nil
	}

	if err := store.DeleteBySource(ctx, args.Source); err != nil {
		return toolError("rag", "delete: %v", err), nil
	}

	return toolResult("deleted source " + args.Source), nil
}
