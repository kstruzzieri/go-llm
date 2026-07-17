package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestManagedRAGToolSchemas(t *testing.T) {
	s := &Server{
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerRAGTools()

	env := connectTestServer(t, s)
	defer env.cleanup()

	listed, err := env.session.ListTools(context.Background(), &gomcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	tools := make(map[string]*gomcp.Tool, len(listed.Tools))
	for _, tool := range listed.Tools {
		tools[tool.Name] = tool
	}

	for _, name := range []string{
		"rag_index_file",
		"rag_index_directory",
		"rag_search",
		"rag_answer",
		"rag_stats",
		"rag_delete",
	} {
		if tools[name] == nil {
			t.Errorf("legacy tool %q is not registered", name)
		}
	}

	expected := map[string]struct {
		properties map[string]string
		arrays     []string
		required   []string
		enums      map[string][]string
	}{
		"rag_ingest_text": {
			properties: map[string]string{
				"name": "string", "content": "string", "title": "string",
				"mime_type": "string", "collection": "string", "tags": "array",
			},
			arrays:   []string{"tags"},
			required: []string{"content", "name"},
		},
		"rag_ingest_file": {
			properties: map[string]string{
				"path": "string", "title": "string", "mime_type": "string",
				"collection": "string", "tags": "array",
			},
			arrays:   []string{"tags"},
			required: []string{"path"},
		},
		"rag_list_documents": {
			properties: map[string]string{
				"collection": "string", "tags": "array",
				"state": "string", "freshness": "string", "after_id": "string", "limit": "integer",
			},
			arrays: []string{"tags"},
			enums: map[string][]string{
				"state":     {"indexing", "indexed", "failed"},
				"freshness": {"unknown", "fresh", "stale"},
			},
		},
		"rag_delete_document": {
			properties: map[string]string{"id": "string"},
			required:   []string{"id"},
		},
		"rag_reindex_document": {
			properties: map[string]string{"id": "string"},
			required:   []string{"id"},
		},
	}

	for name, want := range expected {
		t.Run(name, func(t *testing.T) {
			tool := tools[name]
			if tool == nil {
				t.Fatalf("managed tool %q is not registered", name)
			}
			schema, ok := tool.InputSchema.(map[string]any)
			if !ok {
				t.Fatalf("InputSchema = %T, want map[string]any", tool.InputSchema)
			}
			if got := schema["type"]; got != "object" {
				t.Fatalf("schema type = %v, want object", got)
			}
			properties, ok := schema["properties"].(map[string]any)
			if !ok {
				t.Fatalf("properties = %T, want map[string]any", schema["properties"])
			}
			if got := propertyTypes(t, properties); !reflect.DeepEqual(got, want.properties) {
				t.Fatalf("property types = %#v, want %#v", got, want.properties)
			}
			for _, field := range want.arrays {
				property := properties[field].(map[string]any)
				items, ok := property["items"].(map[string]any)
				if !ok || items["type"] != "string" {
					t.Fatalf("%s items = %#v, want string schema", field, property["items"])
				}
				if got := property["maxItems"]; got != float64(rag.MaxManagedTags) && got != rag.MaxManagedTags {
					t.Fatalf("%s maxItems = %v, want %d", field, got, rag.MaxManagedTags)
				}
				if got := items["maxLength"]; got != float64(rag.MaxManagedTagBytes) && got != rag.MaxManagedTagBytes {
					t.Fatalf("%s item maxLength = %v, want %d", field, got, rag.MaxManagedTagBytes)
				}
			}
			for field, wantMaxLength := range map[string]int{
				"name": rag.MaxManagedMetadataBytes, "content": rag.MaxManagedDocumentBytes,
				"path": rag.MaxManagedMetadataBytes, "title": rag.MaxManagedMetadataBytes,
				"mime_type": rag.MaxManagedMetadataBytes, "collection": rag.MaxManagedMetadataBytes,
				"id": rag.MaxManagedMetadataBytes, "after_id": rag.MaxManagedMetadataBytes,
			} {
				if property, ok := properties[field].(map[string]any); ok {
					if got := property["maxLength"]; got != float64(wantMaxLength) && got != wantMaxLength {
						t.Fatalf("%s maxLength = %v, want %d", field, got, wantMaxLength)
					}
				}
			}
			if property, ok := properties["limit"].(map[string]any); ok {
				if got := property["maximum"]; got != float64(rag.MaxManagedListLimit) && got != rag.MaxManagedListLimit {
					t.Fatalf("limit maximum = %v, want %d", got, rag.MaxManagedListLimit)
				}
			}
			for field, wantEnum := range want.enums {
				property := properties[field].(map[string]any)
				if got := schemaStringSlice(t, property["enum"]); !reflect.DeepEqual(got, wantEnum) {
					t.Fatalf("%s enum = %#v, want %#v", field, got, wantEnum)
				}
			}
			gotRequired := schemaStringSlice(t, schema["required"])
			sort.Strings(gotRequired)
			if !reflect.DeepEqual(gotRequired, want.required) {
				t.Fatalf("required = %#v, want %#v", gotRequired, want.required)
			}
		})
	}
}

func TestManagedRAGListDocumentsRejectsLimitOverMaximum(t *testing.T) {
	s := newManagedRAGTestServer(t)
	result := callManagedRAGHandler(t, s.handleRAGListDocuments, fmt.Sprintf(`{"limit":%d}`, rag.MaxManagedListLimit+1))
	if !result.IsError || !strings.Contains(extractText(result), "limit") {
		t.Fatalf("result = isError:%v text:%q, want limit validation error", result.IsError, extractText(result))
	}
}

func propertyTypes(t *testing.T, properties map[string]any) map[string]string {
	t.Helper()
	types := make(map[string]string, len(properties))
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("property %q = %T, want map[string]any", name, raw)
		}
		propertyType, ok := property["type"].(string)
		if !ok {
			t.Fatalf("property %q type = %T, want string", name, property["type"])
		}
		types[name] = propertyType
	}
	return types
}

func schemaStringSlice(t *testing.T, value any) []string {
	t.Helper()
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal schema string slice: %v", err)
	}
	var values []string
	if err := json.Unmarshal(data, &values); err != nil {
		t.Fatalf("decode schema string slice: %v", err)
	}
	return values
}

type managedRAGHandler func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error)

func managedRAGHandlers(s *Server) map[string]managedRAGHandler {
	return map[string]managedRAGHandler{
		"rag_ingest_text":      s.handleRAGIngestText,
		"rag_ingest_file":      s.handleRAGIngestFile,
		"rag_list_documents":   s.handleRAGListDocuments,
		"rag_delete_document":  s.handleRAGDeleteDocument,
		"rag_reindex_document": s.handleRAGReindexDocument,
	}
}

func callManagedRAGHandler(t *testing.T, handler managedRAGHandler, arguments string) *gomcp.CallToolResult {
	t.Helper()
	result, err := handler(context.Background(), rawArgs(t, arguments))
	if err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if result == nil {
		t.Fatal("handler result = nil")
	}
	return result
}

func TestManagedRAGToolValidation(t *testing.T) {
	s := &Server{}
	for name, handler := range managedRAGHandlers(s) {
		t.Run(name+"/malformed_json", func(t *testing.T) {
			result := callManagedRAGHandler(t, handler, `{`)
			if text := extractText(result); !result.IsError || !strings.HasPrefix(text, "validation: invalid arguments:") {
				t.Fatalf("result = isError:%v text:%q, want validation error", result.IsError, text)
			}
		})
	}

	for _, tc := range []struct {
		name      string
		handler   managedRAGHandler
		arguments string
		field     string
	}{
		{"ingest_text/name", s.handleRAGIngestText, `{"name":" \t ","content":"content"}`, "name"},
		{"ingest_text/content", s.handleRAGIngestText, `{"name":"notes","content":" \n\t"}`, "content"},
		{"ingest_file/path", s.handleRAGIngestFile, `{"path":" \t "}`, "path"},
		{"delete/id", s.handleRAGDeleteDocument, `{"id":" \n "}`, "id"},
		{"reindex/id", s.handleRAGReindexDocument, `{"id":" \t "}`, "id"},
		{"list/state", s.handleRAGListDocuments, `{"state":"bogus"}`, "state"},
		{"list/freshness", s.handleRAGListDocuments, `{"freshness":"Fresh"}`, "freshness"},
	} {
		t.Run(tc.name+"/blank", func(t *testing.T) {
			result := callManagedRAGHandler(t, tc.handler, tc.arguments)
			text := extractText(result)
			if !result.IsError || !strings.HasPrefix(text, "validation:") || !strings.Contains(text, tc.field) {
				t.Fatalf("result = isError:%v text:%q, want %s validation error", result.IsError, text, tc.field)
			}
		})
	}
}

func TestManagedRAGToolsDisabledOrUnavailable(t *testing.T) {
	validArguments := map[string]string{
		"rag_ingest_text":      `{"name":"notes","content":"content"}`,
		"rag_ingest_file":      `{"path":"notes.md"}`,
		"rag_list_documents":   `{}`,
		"rag_delete_document":  `{"id":"missing"}`,
		"rag_reindex_document": `{"id":"missing"}`,
	}

	t.Run("disabled", func(t *testing.T) {
		s := &Server{ragDisabled: true}
		for name, handler := range managedRAGHandlers(s) {
			t.Run(name, func(t *testing.T) {
				result := callManagedRAGHandler(t, handler, validArguments[name])
				if text := extractText(result); !result.IsError || text != "rag: RAG is disabled on this server" {
					t.Fatalf("result = isError:%v text:%q, want disabled RAG error", result.IsError, text)
				}
			})
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		s := &Server{}
		for name, handler := range managedRAGHandlers(s) {
			t.Run(name, func(t *testing.T) {
				result := callManagedRAGHandler(t, handler, validArguments[name])
				if text := extractText(result); !result.IsError || text != "rag: managed sources unavailable" {
					t.Fatalf("result = isError:%v text:%q, want unavailable managed sources error", result.IsError, text)
				}
			})
		}
	})

	t.Run("sqlite_store_without_indexer", func(t *testing.T) {
		store, err := rag.NewSQLiteStore(":memory:")
		if err != nil {
			t.Fatalf("NewSQLiteStore() error = %v", err)
		}
		s := &Server{store: store}
		t.Cleanup(func() { _ = s.Close() })

		s.rebuildDerivedClients(context.Background())
		if s.managedSources == nil {
			t.Fatal("managedSources = nil for SQLite store without indexer")
		}
		if s.indexer != nil {
			t.Fatal("indexer != nil without a resolved embedding model")
		}

		list := callManagedRAGHandler(t, s.handleRAGListDocuments, `{}`)
		if list.IsError || extractText(list) != "[]" {
			t.Fatalf("list result = isError:%v text:%q, want empty JSON array", list.IsError, extractText(list))
		}

		ingest := callManagedRAGHandler(t, s.handleRAGIngestText, validArguments["rag_ingest_text"])
		if text := extractText(ingest); !ingest.IsError || !strings.HasPrefix(text, "rag:") || !strings.Contains(text, "indexer is unavailable") {
			t.Fatalf("ingest result = isError:%v text:%q, want managed RAG error", ingest.IsError, text)
		}

		if err := s.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
		if s.managedSources != nil {
			t.Fatal("managedSources != nil after Close()")
		}
	})
}

func TestManagedRAGToolsNotFound(t *testing.T) {
	s := newManagedRAGTestServer(t)
	for _, tc := range []struct {
		name    string
		handler managedRAGHandler
	}{
		{"delete", s.handleRAGDeleteDocument},
		{"reindex", s.handleRAGReindexDocument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := callManagedRAGHandler(t, tc.handler, `{"id":"missing"}`)
			if text := extractText(result); !result.IsError || !strings.HasPrefix(text, "not_found:") || !strings.Contains(text, "missing") {
				t.Fatalf("result = isError:%v text:%q, want not_found error", result.IsError, text)
			}
		})
	}
}

func TestManagedRAGToolsLifecycleEndToEnd(t *testing.T) {
	s := newManagedRAGTestServer(t)
	env := connectTestServer(t, s)
	defer env.cleanup()

	text, isError := callTool(t, env.session, "rag_ingest_text", map[string]any{
		"name":       "runbook.md",
		"content":    "restart safely",
		"title":      "Restart Runbook",
		"mime_type":  "text/markdown",
		"collection": "ops",
		"tags":       []string{"restart", "safe"},
	})
	if isError {
		t.Fatalf("rag_ingest_text returned tool error: %s", text)
	}
	var ingested rag.Document
	if err := json.Unmarshal([]byte(text), &ingested); err != nil {
		t.Fatalf("decode ingested document: %v (%s)", err, text)
	}
	if ingested.ID == "" || ingested.Title != "Restart Runbook" || ingested.Collection != "ops" ||
		!reflect.DeepEqual(ingested.Tags, []string{"restart", "safe"}) ||
		ingested.State != rag.DocumentStateIndexed || ingested.Freshness != rag.DocumentFreshnessFresh {
		t.Fatalf("ingested document = %#v", ingested)
	}

	text, isError = callTool(t, env.session, "rag_search", map[string]any{
		"query": "restart safely",
		"top_k": 1,
	})
	if isError {
		t.Fatalf("rag_search returned tool error: %s", text)
	}
	var results []rag.SearchResult
	if err := json.Unmarshal([]byte(text), &results); err != nil {
		t.Fatalf("decode search results: %v (%s)", err, text)
	}
	if len(results) != 1 || results[0].Chunk.Content != "restart safely" ||
		results[0].Chunk.Metadata["managed_document_id"] != ingested.ID {
		t.Fatalf("search results = %#v, want managed document %q", results, ingested.ID)
	}

	text, isError = callTool(t, env.session, "rag_list_documents", map[string]any{
		"collection": "ops",
		"tags":       []string{"safe"},
		"state":      "indexed",
		"freshness":  "fresh",
	})
	if isError {
		t.Fatalf("rag_list_documents returned tool error: %s", text)
	}
	var documents []rag.Document
	if err := json.Unmarshal([]byte(text), &documents); err != nil {
		t.Fatalf("decode document list: %v (%s)", err, text)
	}
	if len(documents) != 1 || documents[0].ID != ingested.ID {
		t.Fatalf("documents = %#v, want one document with ID %q", documents, ingested.ID)
	}

	text, isError = callTool(t, env.session, "rag_reindex_document", map[string]any{"id": ingested.ID})
	if isError {
		t.Fatalf("rag_reindex_document returned tool error: %s", text)
	}
	var reindexed rag.Document
	if err := json.Unmarshal([]byte(text), &reindexed); err != nil {
		t.Fatalf("decode reindexed document: %v (%s)", err, text)
	}
	if reindexed.ID != ingested.ID {
		t.Fatalf("reindexed ID = %q, want stable ID %q", reindexed.ID, ingested.ID)
	}

	text, isError = callTool(t, env.session, "rag_delete_document", map[string]any{"id": ingested.ID})
	if isError {
		t.Fatalf("rag_delete_document returned tool error: %s", text)
	}
	var deleted map[string]string
	if err := json.Unmarshal([]byte(text), &deleted); err != nil {
		t.Fatalf("decode delete result: %v (%s)", err, text)
	}
	if !reflect.DeepEqual(deleted, map[string]string{"deleted_id": ingested.ID}) {
		t.Fatalf("delete result = %#v, want only deleted_id %q", deleted, ingested.ID)
	}

	text, isError = callTool(t, env.session, "rag_list_documents", map[string]any{})
	if isError {
		t.Fatalf("final rag_list_documents returned tool error: %s", text)
	}
	if err := json.Unmarshal([]byte(text), &documents); err != nil {
		t.Fatalf("decode final document list: %v (%s)", err, text)
	}
	if len(documents) != 0 {
		t.Fatalf("final documents = %#v, want empty list", documents)
	}
}

func TestManagedRAGToolsSerializeAcrossRebuild(t *testing.T) {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	embedStarted := make(chan struct{})
	releaseEmbed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEmbed) }) }
	indexer, err := rag.NewIndexerWithEmbedder(rag.EmbedderFunc(
		func(ctx context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
			close(embedStarted)
			select {
			case <-releaseEmbed:
			case <-ctx.Done():
				return rag.EmbedResult{}, ctx.Err()
			}
			embeddings := make([][]float64, len(inputs))
			for i := range embeddings {
				embeddings[i] = []float64{1, float64(i + 1)}
			}
			return rag.EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/old"}, nil
		},
	), store, rag.WithEmbeddingModel("old"))
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewIndexerWithEmbedder() error = %v", err)
	}
	managedSources, err := rag.NewManagedSources(indexer, store)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewManagedSources() error = %v", err)
	}
	s := &Server{
		store:          store,
		indexer:        indexer,
		managedSources: managedSources,
		resolved: map[string]config.ResolvedModel{
			"embedding": {Name: "new"},
		},
		router: newRecordingRouteEngine(""),
	}
	t.Cleanup(func() {
		release()
		_ = s.Close()
	})

	type outcome struct {
		result *gomcp.CallToolResult
		err    error
	}
	ingestDone := make(chan outcome, 1)
	go func() {
		result, err := s.handleRAGIngestText(
			context.Background(),
			rawArgs(t, `{"name":"runbook.md","content":"restart safely"}`),
		)
		ingestDone <- outcome{result: result, err: err}
	}()

	select {
	case <-embedStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation ingest did not reach the blocking embedder")
	}

	oldManagedSources := s.managedSources
	s.rebuildDerivedClients(context.Background())
	if s.managedSources == nil || s.managedSources == oldManagedSources {
		t.Fatal("rebuild did not publish a new managed source service")
	}

	listStarted := make(chan struct{})
	listDone := make(chan outcome, 1)
	go func() {
		close(listStarted)
		result, err := s.handleRAGListDocuments(context.Background(), rawArgs(t, `{}`))
		listDone <- outcome{result: result, err: err}
	}()
	<-listStarted

	select {
	case result := <-listDone:
		t.Fatalf("new-generation list completed during old-generation ingest: result=%v err=%v", result.result, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	for name, done := range map[string]<-chan outcome{"ingest": ingestDone, "list": listDone} {
		select {
		case result := <-done:
			if result.err != nil || result.result == nil || result.result.IsError {
				t.Fatalf("%s result = %#v, err = %v", name, result.result, result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s handler did not finish", name)
		}
	}
}

func TestManagedRAGToolsCloseWaitsForInFlightOperation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rag.db")
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	embedStarted := make(chan struct{})
	releaseEmbed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEmbed) }) }
	indexer, err := rag.NewIndexerWithEmbedder(rag.EmbedderFunc(
		func(ctx context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
			close(embedStarted)
			select {
			case <-releaseEmbed:
			case <-ctx.Done():
				return rag.EmbedResult{}, ctx.Err()
			}
			embeddings := make([][]float64, len(inputs))
			for i := range embeddings {
				embeddings[i] = []float64{1, float64(i + 1)}
			}
			return rag.EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/close"}, nil
		},
	), store, rag.WithEmbeddingModel("test"))
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewIndexerWithEmbedder() error = %v", err)
	}
	managedSources, err := rag.NewManagedSources(indexer, store)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewManagedSources() error = %v", err)
	}
	s := &Server{store: store, indexer: indexer, managedSources: managedSources}
	t.Cleanup(func() {
		release()
		_ = s.Close()
	})

	type outcome struct {
		result *gomcp.CallToolResult
		err    error
	}
	ingestDone := make(chan outcome, 1)
	go func() {
		result, err := s.handleRAGIngestText(
			context.Background(),
			rawArgs(t, `{"name":"runbook.md","content":"restart safely"}`),
		)
		ingestDone <- outcome{result: result, err: err}
	}()

	select {
	case <-embedStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not reach the blocking embedder")
	}

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- s.Close()
	}()
	<-closeStarted

	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned during managed ingest: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case result := <-ingestDone:
		if result.err != nil || result.result == nil || result.result.IsError {
			t.Fatalf("ingest result = %#v, err = %v", result.result, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not finish")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not finish")
	}

	reopened, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("reopen SQLite store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	managed, err := rag.NewManagedSources(nil, reopened)
	if err != nil {
		t.Fatalf("NewManagedSources() after reopen error = %v", err)
	}
	documents, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	if err != nil {
		t.Fatalf("ListDocuments() after reopen error = %v", err)
	}
	if len(documents) != 1 || documents[0].State != rag.DocumentStateIndexed {
		t.Fatalf("documents after Close() = %#v, want one indexed document", documents)
	}
}

func TestManagedRAGToolsShutdownExpiredContextClosesWhenIdle(t *testing.T) {
	s := newManagedRAGTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil with idle managed gate", err)
	}
	s.mu.RLock()
	closed := s.closed
	store := s.store
	managed := s.managedSources
	s.mu.RUnlock()
	if !closed || store != nil || managed != nil {
		t.Fatalf("server after Shutdown() = closed:%v store:%v managed:%v", closed, store, managed)
	}
}

func TestManagedRAGToolsQueuedCancellationAndShutdownDeadline(t *testing.T) {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	embedStarted := make(chan struct{})
	releaseEmbed := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEmbed) }) }
	indexer, err := rag.NewIndexerWithEmbedder(rag.EmbedderFunc(
		func(ctx context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
			close(embedStarted)
			select {
			case <-releaseEmbed:
			case <-ctx.Done():
				return rag.EmbedResult{}, ctx.Err()
			}
			embeddings := make([][]float64, len(inputs))
			for i := range embeddings {
				embeddings[i] = []float64{1, float64(i + 1)}
			}
			return rag.EmbedResult{Embeddings: embeddings, VectorSpaceID: "test/cancel"}, nil
		},
	), store, rag.WithEmbeddingModel("test"))
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewIndexerWithEmbedder() error = %v", err)
	}
	managedSources, err := rag.NewManagedSources(indexer, store)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewManagedSources() error = %v", err)
	}
	s := &Server{store: store, indexer: indexer, managedSources: managedSources}
	t.Cleanup(func() {
		release()
		_ = s.Close()
	})

	type outcome struct {
		result *gomcp.CallToolResult
		err    error
	}
	ingestDone := make(chan outcome, 1)
	go func() {
		result, err := s.handleRAGIngestText(
			context.Background(),
			rawArgs(t, `{"name":"runbook.md","content":"restart safely"}`),
		)
		ingestDone <- outcome{result: result, err: err}
	}()
	select {
	case <-embedStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not reach blocking embedder")
	}

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	cancelQueued()
	listDone := make(chan outcome, 1)
	go func() {
		result, err := s.handleRAGListDocuments(queuedCtx, rawArgs(t, `{}`))
		listDone <- outcome{result: result, err: err}
	}()
	select {
	case result := <-listDone:
		if result.err != nil || result.result == nil || !result.result.IsError ||
			!strings.Contains(extractText(result.result), context.Canceled.Error()) {
			t.Fatalf("canceled list result = %#v, err = %v", result.result, result.err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("canceled list remained queued behind ingest")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShutdown()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- s.Shutdown(shutdownCtx) }()
	select {
	case err := <-shutdownDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown() error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Shutdown() exceeded its context deadline")
	}

	release()
	select {
	case result := <-ingestDone:
		if result.err != nil || result.result == nil || result.result.IsError {
			t.Fatalf("ingest result = %#v, err = %v", result.result, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ingest did not finish")
	}
}

func newManagedRAGTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	embedder := rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		if err := ctx.Err(); err != nil {
			return rag.EmbedResult{}, err
		}
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{1, float64(i + 1)}
		}
		return rag.EmbedResult{
			Embeddings: embeddings, Model: model, Provider: "test", VectorSpaceID: "test/v1",
		}, nil
	})
	indexer, err := rag.NewIndexerWithEmbedder(embedder, store, rag.WithEmbeddingModel("test"))
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewIndexerWithEmbedder() error = %v", err)
	}
	retriever, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel("test"))
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewRetrieverWithEmbedder() error = %v", err)
	}
	managedSources, err := rag.NewManagedSources(indexer, store)
	if err != nil {
		_ = store.Close()
		t.Fatalf("NewManagedSources() error = %v", err)
	}
	s := &Server{
		store:          store,
		indexer:        indexer,
		retriever:      retriever,
		managedSources: managedSources,
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerRAGTools()
	t.Cleanup(func() { _ = s.Close() })
	return s
}
