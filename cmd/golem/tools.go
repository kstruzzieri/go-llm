package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// buildTools returns golem's read-only tool set: the file tools
// (read_file, search, glob, list) always, plus retrieve appended last when a
// non-nil retriever tool is supplied. The retrieve argument must be a true nil
// interface (not a typed-nil pointer) when no retriever is available; a
// typed-nil would be treated as present.
func buildTools(root string, retrieve agent.Tool) ([]agent.Tool, error) {
	fileTools, err := agenttools.NewFileTools(root)
	if err != nil {
		return nil, fmt.Errorf("golem: build file tools: %w", err)
	}
	if retrieve != nil {
		return append(fileTools, retrieve), nil
	}
	return fileTools, nil
}

// resolveRetriever builds a retrieve tool from an explicitly configured RAG DB.
// retrieve is opt-in, so it reports three distinct outcomes:
//   - (nil, nil): not requested (dbPath == ""); the caller omits retrieve quietly.
//   - (nil, err): requested via -rag-db but could NOT be enabled; the reason is
//     returned so the caller can surface it rather than mislead the user with a
//     generic "no RAG index configured" line.
//   - (tool, nil): enabled.
// On success the opened store lives for the process: the retriever queries it
// for the whole session and the OS reclaims the handle at exit.
func resolveRetriever(ctx context.Context, cfg *config.Config, router *provider.Router, dbPath string) (agent.Tool, error) {
	if dbPath == "" {
		return nil, nil // retrieve is opt-in and was not requested
	}
	if cfg == nil || router == nil {
		return nil, fmt.Errorf("no provider configured for embeddings")
	}
	embModel := embeddingSelector(cfg)
	if embModel == "" {
		return nil, fmt.Errorf("no embedding model configured (set defaults.embedding in models.json)")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("rag-db %q: %w", dbPath, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("rag-db %q is a directory, not a SQLite file", dbPath)
	}
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open rag-db %q: %w", dbPath, err)
	}
	embedder := rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		resp, err := router.Embed(ctx, provider.EmbedRequest{Model: model, Input: inputs})
		if err != nil {
			return rag.EmbedResult{}, err
		}
		// Derive VectorSpaceID the same way the indexer did (actual routed
		// model, else provider/model) so query vectors validate against the
		// corpus's stored vector space. Mirrors mcp's ragEmbedder; omitting it
		// makes every query fail with ErrVectorSpaceMismatch on a corpus that
		// recorded a vector-space ID.
		result := rag.EmbedResult{
			Embeddings: resp.Embeddings,
			Model:      resp.Model,
			Provider:   resp.Provider,
		}
		if resp.RouteOutcome != nil {
			if am := resp.RouteOutcome.ActualModel; am.Provider != "" && am.Model != "" {
				result.VectorSpaceID = am.String()
			}
		}
		if result.VectorSpaceID == "" && result.Provider != "" && result.Model != "" {
			result.VectorSpaceID = result.Provider + "/" + result.Model
		}
		return result, nil
	})
	retr, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel(embModel))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("build retriever for rag-db %q: %w", dbPath, err)
	}
	return &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048}, nil
}

// embeddingSelector returns the head of the embedding fallback chain, or "".
func embeddingSelector(cfg *config.Config) string {
	if _, ok := cfg.Defaults["embedding"]; !ok {
		return ""
	}
	chain, err := cfg.RoleFallbackChain("embedding")
	if err != nil || len(chain) == 0 {
		return ""
	}
	return chain[0]
}

// effectClassName renders an agent.EffectClass bitset for /tools. The agent
// package has no String() for it, so golem provides one.
func effectClassName(c agent.EffectClass) string {
	var parts []string
	if c&agent.Read != 0 {
		parts = append(parts, "read")
	}
	if c&agent.Write != 0 {
		parts = append(parts, "write")
	}
	if c&agent.Exec != 0 {
		parts = append(parts, "exec")
	}
	if c&agent.Network != 0 {
		parts = append(parts, "network")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}
