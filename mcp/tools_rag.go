package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/rag"
)

type queryContextArgs struct {
	CurrentFile   string   `json:"current_file,omitempty"`
	WorkspaceRoot string   `json:"workspace_root,omitempty"`
	OpenFiles     []string `json:"open_files,omitempty"`
}

func (a queryContextArgs) queryContext() rag.QueryContext {
	return rag.QueryContext{
		CurrentFile: a.CurrentFile, WorkspaceRoot: a.WorkspaceRoot, OpenFiles: a.openFiles(), Timestamp: time.Now(),
	}
}

// openFiles returns the open-file paths with empty entries dropped. Routing both
// empty() and queryContext() through it keeps the contextual-signal decision and
// the QueryContext handed to the scorers agreed on what counts as an open file.
func (a queryContextArgs) openFiles() []string {
	var files []string
	for _, file := range a.OpenFiles {
		if file != "" {
			files = append(files, file)
		}
	}
	return files
}

func (a queryContextArgs) empty() bool {
	return a.CurrentFile == "" && a.WorkspaceRoot == "" && len(a.openFiles()) == 0
}

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
				"query":          map[string]any{"type": "string", "description": "Search query"},
				"top_k":          map[string]any{"type": "integer", "description": "Number of results (default: 5)"},
				"current_file":   map[string]any{"type": "string", "description": "File currently being edited for contextual ranking"},
				"workspace_root": map[string]any{"type": "string", "description": "Workspace root used to normalize contextual paths"},
				"open_files":     map[string]any{"type": "array", "description": "Files currently open for contextual ranking", "items": map[string]any{"type": "string"}},
				"explain_scores": map[string]any{"type": "boolean", "description": "Return fused rank and per-signal scores"},
				"collection":     map[string]any{"type": "string", "description": "Managed document collection to search", "maxLength": rag.MaxManagedMetadataBytes},
				"tags": map[string]any{"type": "array", "description": "Managed document tags; every tag must match", "maxItems": rag.MaxManagedTags,
					"items": map[string]any{"type": "string", "maxLength": rag.MaxManagedTagBytes}},
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

	s.registerManagedRAGTools()
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
		queryContextArgs
		Query         string   `json:"query"`
		TopK          int      `json:"top_k,omitempty"`
		ExplainScores bool     `json:"explain_scores,omitempty"`
		Collection    string   `json:"collection,omitempty"`
		Tags          []string `json:"tags,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if args.Query == "" {
		return toolError("validation", "query must not be empty"), nil
	}
	if err := validateRAGSearchScope(args.Collection, args.Tags); err != nil {
		return toolError("validation", "%v", err), nil
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

	var results []rag.SearchResult
	contextual := !args.empty()
	scope := rag.RetrievalScope{Collection: args.Collection, Tags: args.Tags}
	scoped := args.Collection != "" || len(args.Tags) != 0
	if args.ExplainScores || contextual {
		var scored []rag.ScoredResult
		var err error
		if scoped {
			scored, err = retriever.RetrieveScoredScoped(ctx, args.Query, topK, scope, args.queryContext())
		} else {
			scored, err = retriever.RetrieveScored(ctx, args.Query, topK, args.queryContext())
		}
		if err != nil {
			return toolError("rag", "search: %v", err), nil
		}
		if args.ExplainScores {
			data, err := json.Marshal(scored)
			if err != nil {
				return toolError("rag", "marshal results: %v", err), nil
			}
			return toolResult(string(data)), nil
		}
		if scored != nil {
			results = make([]rag.SearchResult, len(scored))
			for i := range scored {
				results[i] = scored[i].SearchResult
			}
		}
	} else {
		var err error
		if scoped {
			results, err = retriever.RetrieveScoped(ctx, args.Query, topK, scope)
		} else {
			results, err = retriever.Retrieve(ctx, args.Query, topK)
		}
		if err != nil {
			return toolError("rag", "search: %v", err), nil
		}
	}

	data, err := json.Marshal(results)
	if err != nil {
		return toolError("rag", "marshal results: %v", err), nil
	}
	return toolResult(string(data)), nil
}

func validateRAGSearchScope(collection string, tags []string) error {
	if !utf8.ValidString(collection) || len(collection) > rag.MaxManagedMetadataBytes {
		return fmt.Errorf("collection exceeds %d-byte limit or is not valid UTF-8", rag.MaxManagedMetadataBytes)
	}
	if len(tags) > rag.MaxManagedTags {
		return fmt.Errorf("tags exceed %d-tag limit", rag.MaxManagedTags)
	}
	for _, tag := range tags {
		if !utf8.ValidString(tag) || len(tag) > rag.MaxManagedTagBytes {
			return fmt.Errorf("tag exceeds %d-byte limit or is not valid UTF-8", rag.MaxManagedTagBytes)
		}
	}
	return nil
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
