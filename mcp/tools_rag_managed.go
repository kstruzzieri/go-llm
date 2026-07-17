package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/rag"
)

type managedOptionsArgs struct {
	Title      string   `json:"title,omitempty"`
	MIMEType   string   `json:"mime_type,omitempty"`
	Collection string   `json:"collection,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

func (a managedOptionsArgs) options() rag.DocumentOptions {
	return rag.DocumentOptions{
		Title: a.Title, MIMEType: a.MIMEType, Collection: a.Collection, Tags: a.Tags,
	}
}

func (a managedOptionsArgs) validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"title", a.Title},
		{"mime_type", a.MIMEType},
		{"collection", a.Collection},
	} {
		if err := validateManagedRAGString(field.name, field.value, rag.MaxManagedMetadataBytes); err != nil {
			return err
		}
	}
	return validateManagedRAGTags(a.Tags)
}

func validateManagedRAGString(name, value string, max int) error {
	if !utf8.ValidString(value) || len(value) > max {
		return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, max)
	}
	return nil
}

func validateManagedRAGTags(tags []string) error {
	if len(tags) > rag.MaxManagedTags {
		return fmt.Errorf("tags must contain at most %d items", rag.MaxManagedTags)
	}
	for _, tag := range tags {
		if err := validateManagedRAGString("tag", tag, rag.MaxManagedTagBytes); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) registerManagedRAGTools() {
	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_ingest_text",
		Description: "Save and index a named text document as a managed RAG source.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Document name"},
				"content":    map[string]any{"type": "string", "maxLength": rag.MaxManagedDocumentBytes, "description": "UTF-8 document content"},
				"title":      map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional display title"},
				"mime_type":  map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional MIME type"},
				"collection": map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional collection"},
				"tags":       managedTagsSchema(),
			},
			"required": []string{"name", "content"},
		},
	}, s.handleRAGIngestText)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_ingest_file",
		Description: "Save and index a local file as a managed RAG source.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Local file path"},
				"title":      map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional display title"},
				"mime_type":  map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional MIME type"},
				"collection": map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Optional collection"},
				"tags":       managedTagsSchema(),
			},
			"required": []string{"path"},
		},
	}, s.handleRAGIngestFile)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_list_documents",
		Description: "List managed RAG documents.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"collection": map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Exact collection filter"},
				"tags":       managedTagsSchema(),
				"state":      map[string]any{"type": "string", "enum": []string{"indexing", "indexed", "failed"}, "description": "Exact document state filter"},
				"freshness":  map[string]any{"type": "string", "enum": []string{"unknown", "fresh", "stale"}, "description": "Exact freshness filter"},
				"after_id":   map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Exclusive document ID cursor"},
				"limit":      map[string]any{"type": "integer", "maximum": rag.MaxManagedListLimit, "description": "Maximum documents to return"},
			},
		},
	}, s.handleRAGListDocuments)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_delete_document",
		Description: "Delete a managed RAG document and its indexed chunks.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Managed document ID"},
			},
			"required": []string{"id"},
		},
	}, s.handleRAGDeleteDocument)

	s.mcpServer.AddTool(&gomcp.Tool{
		Name:        "rag_reindex_document",
		Description: "Reindex a managed RAG document by stable ID.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "maxLength": rag.MaxManagedMetadataBytes, "description": "Managed document ID"},
			},
			"required": []string{"id"},
		},
	}, s.handleRAGReindexDocument)
}

func managedTagsSchema() map[string]any {
	return map[string]any{
		"type": "array", "maxItems": rag.MaxManagedTags, "description": "Optional tags",
		"items": map[string]any{"type": "string", "maxLength": rag.MaxManagedTagBytes},
	}
}

func (s *Server) handleRAGIngestText(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if result := s.requireRAG(); result != nil {
		return result, nil
	}
	var args struct {
		managedOptionsArgs
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if err := validateManagedRAGString("name", args.Name, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if err := validateManagedRAGString("content", args.Content, rag.MaxManagedDocumentBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if err := args.managedOptionsArgs.validate(); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if strings.TrimSpace(args.Name) == "" {
		return toolError("validation", "name must not be empty"), nil
	}
	if strings.TrimSpace(args.Content) == "" {
		return toolError("validation", "content must not be empty"), nil
	}
	if result := s.lockManagedOperation(ctx); result != nil {
		return result, nil
	}
	defer s.managedGate.unlock()
	managed := s.managedSourcesSnapshot()
	if managed == nil {
		return toolError("rag", "managed sources unavailable"), nil
	}
	document, err := managed.IngestText(ctx, args.Name, args.Content, args.options())
	if err != nil {
		return managedRAGError("ingest text", err), nil
	}
	return managedRAGResult(document), nil
}

func (s *Server) handleRAGIngestFile(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if result := s.requireRAG(); result != nil {
		return result, nil
	}
	var args struct {
		managedOptionsArgs
		Path string `json:"path"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if err := validateManagedRAGString("path", args.Path, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if err := args.managedOptionsArgs.validate(); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if strings.TrimSpace(args.Path) == "" {
		return toolError("validation", "path must not be empty"), nil
	}
	if result := s.lockManagedOperation(ctx); result != nil {
		return result, nil
	}
	defer s.managedGate.unlock()
	managed := s.managedSourcesSnapshot()
	if managed == nil {
		return toolError("rag", "managed sources unavailable"), nil
	}
	document, err := managed.IngestFile(ctx, args.Path, args.options())
	if err != nil {
		return managedRAGError("ingest file", err), nil
	}
	return managedRAGResult(document), nil
}

func (s *Server) handleRAGListDocuments(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if result := s.requireRAG(); result != nil {
		return result, nil
	}
	var args struct {
		Collection string   `json:"collection,omitempty"`
		Tags       []string `json:"tags,omitempty"`
		State      string   `json:"state,omitempty"`
		Freshness  string   `json:"freshness,omitempty"`
		AfterID    string   `json:"after_id,omitempty"`
		Limit      int      `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if err := validateManagedRAGString("collection", args.Collection, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if err := validateManagedRAGString("after_id", args.AfterID, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if err := validateManagedRAGTags(args.Tags); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if args.Limit > rag.MaxManagedListLimit {
		return toolError("validation", "limit must be at most %d", rag.MaxManagedListLimit), nil
	}
	state, ok := parseManagedDocumentState(args.State)
	if !ok {
		return toolError("validation", "invalid state %q (indexing|indexed|failed)", args.State), nil
	}
	freshness, ok := parseManagedDocumentFreshness(args.Freshness)
	if !ok {
		return toolError("validation", "invalid freshness %q (unknown|fresh|stale)", args.Freshness), nil
	}
	if result := s.lockManagedOperation(ctx); result != nil {
		return result, nil
	}
	defer s.managedGate.unlock()
	managed := s.managedSourcesSnapshot()
	if managed == nil {
		return toolError("rag", "managed sources unavailable"), nil
	}
	documents, err := managed.ListDocuments(ctx, rag.DocumentFilter{
		Collection: args.Collection,
		Tags:       args.Tags,
		State:      state,
		Freshness:  freshness,
		AfterID:    args.AfterID,
		Limit:      args.Limit,
	})
	if err != nil {
		return managedRAGError("list documents", err), nil
	}
	return managedRAGResult(documents), nil
}

func (s *Server) handleRAGDeleteDocument(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if result := s.requireRAG(); result != nil {
		return result, nil
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if err := validateManagedRAGString("id", args.ID, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if strings.TrimSpace(args.ID) == "" {
		return toolError("validation", "id must not be empty"), nil
	}
	if result := s.lockManagedOperation(ctx); result != nil {
		return result, nil
	}
	defer s.managedGate.unlock()
	managed := s.managedSourcesSnapshot()
	if managed == nil {
		return toolError("rag", "managed sources unavailable"), nil
	}
	if err := managed.DeleteDocument(ctx, args.ID); err != nil {
		return managedRAGError("delete document", err), nil
	}
	return managedRAGResult(struct {
		DeletedID string `json:"deleted_id"`
	}{DeletedID: args.ID}), nil
}

func (s *Server) handleRAGReindexDocument(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	if result := s.requireRAG(); result != nil {
		return result, nil
	}
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolError("validation", "invalid arguments: %v", err), nil
	}
	if err := validateManagedRAGString("id", args.ID, rag.MaxManagedMetadataBytes); err != nil {
		return toolError("validation", "%v", err), nil
	}
	if strings.TrimSpace(args.ID) == "" {
		return toolError("validation", "id must not be empty"), nil
	}
	if result := s.lockManagedOperation(ctx); result != nil {
		return result, nil
	}
	defer s.managedGate.unlock()
	managed := s.managedSourcesSnapshot()
	if managed == nil {
		return toolError("rag", "managed sources unavailable"), nil
	}
	document, err := managed.ReindexDocument(ctx, args.ID)
	if err != nil {
		return managedRAGError("reindex document", err), nil
	}
	return managedRAGResult(document), nil
}

// parseManagedDocumentState maps the wire value to a state filter; empty
// means no filter. Mirrors the parseAgentMemoryKind validation convention.
func parseManagedDocumentState(raw string) (rag.DocumentState, bool) {
	switch state := rag.DocumentState(raw); state {
	case "", rag.DocumentStateIndexing, rag.DocumentStateIndexed, rag.DocumentStateFailed:
		return state, true
	default:
		return "", false
	}
}

// parseManagedDocumentFreshness maps the wire value to a freshness filter;
// empty means no filter.
func parseManagedDocumentFreshness(raw string) (rag.DocumentFreshness, bool) {
	switch freshness := rag.DocumentFreshness(raw); freshness {
	case "", rag.DocumentFreshnessUnknown, rag.DocumentFreshnessFresh, rag.DocumentFreshnessStale:
		return freshness, true
	default:
		return "", false
	}
}

func (s *Server) managedSourcesSnapshot() *rag.ManagedSources {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.managedSources
}

func (s *Server) lockManagedOperation(ctx context.Context) *gomcp.CallToolResult {
	if err := s.managedGate.lock(ctx); err != nil {
		return toolError("rag", "managed operation: %v", err)
	}
	return nil
}

func managedRAGError(action string, err error) *gomcp.CallToolResult {
	if errors.Is(err, rag.ErrDocumentNotFound) {
		return toolError("not_found", "%v", err)
	}
	return toolError("rag", "%s: %v", action, err)
}

func managedRAGResult(value any) *gomcp.CallToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		return toolError("rag", "marshal result: %v", err)
	}
	return toolResult(string(data))
}
