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

// resolveRetriever builds a retrieve tool when a RAG DB exists at dbPath AND
// defaults.embedding resolves to a model. Otherwise it returns (nil, false) so
// the tool is omitted (weak local models waste turns on an unavailable tool).
// On success the opened store lives for the process: the retriever queries it
// for the whole session and the OS reclaims the handle at exit.
func resolveRetriever(ctx context.Context, cfg *config.Config, router *provider.Router, dbPath string) (agent.Tool, bool) {
	if cfg == nil || dbPath == "" || router == nil {
		return nil, false
	}
	embModel := embeddingSelector(cfg)
	if embModel == "" {
		return nil, false
	}
	if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
		return nil, false
	}
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, false
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
		return nil, false
	}
	return &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048}, true
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
