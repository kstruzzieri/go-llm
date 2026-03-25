package mcp

import (
	"context"
	"testing"
)

func TestNewServerDefaults(t *testing.T) {
	ctx := context.Background()
	// RAG disabled so we don't need a real SQLite DB.
	s, err := NewServer(ctx, WithRAGDisabled(true))
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.ollamaURL != defaultOllamaURL {
		t.Errorf("ollamaURL = %q, want %q", s.ollamaURL, defaultOllamaURL)
	}
	if s.client == nil {
		t.Error("client is nil, want non-nil")
	}
	if s.mcpServer == nil {
		t.Error("mcpServer is nil, want non-nil")
	}
	if s.ragDisabled != true {
		t.Error("ragDisabled = false, want true")
	}
}

func TestNewServerWithOptions(t *testing.T) {
	ctx := context.Background()
	customURL := "http://custom:1234"

	s, err := NewServer(ctx,
		WithOllamaURL(customURL),
		WithRAGDisabled(true),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.ollamaURL != customURL {
		t.Errorf("ollamaURL = %q, want %q", s.ollamaURL, customURL)
	}
	if s.ragDisabled != true {
		t.Error("ragDisabled = false, want true")
	}
	// Store should be nil when RAG is disabled.
	if s.store != nil {
		t.Error("store is non-nil, want nil when RAG disabled")
	}
}
