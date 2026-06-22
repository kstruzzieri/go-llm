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

// golemWriteSystemPrompt is the base prompt when -allow-write is set. Unlike the
// read-only prompt it permits proposing mutations, but stresses that every change
// is shown as a diff and applied only after explicit user approval.
const golemWriteSystemPrompt = "You are Golem, a terminal coding assistant for this workspace. " +
	"Use the read-only tools to inspect files before acting. You may propose changes with write_file and edit_file; " +
	"every change is shown to the user as a diff and is applied only after they approve it, so keep edits minimal and targeted and explain what you are changing. " +
	"After a change is applied, briefly confirm what you changed. " +
	"Prefer edit_file for small changes and write_file for new files or full rewrites. " +
	"Cite file paths and line numbers when they matter, and say when the available evidence is insufficient. " +
	"Prior session messages are context only; the current user request is authoritative."

// buildWriteTools constructs the workspace-mutating tool set plus the in-session
// journal that backs /undo, both bound to one Workspace over root. Returned only
// when -allow-write is set.
func buildWriteTools(root string) ([]agent.Tool, *mutationJournal, error) {
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return nil, nil, fmt.Errorf("golem: build write tools: %w", err)
	}
	journal := newMutationJournal(ws)
	return agenttools.NewMutatingTools(ws, journal), journal, nil
}

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
//
// On success the opened store lives for the process: the retriever queries it
// for the whole session and the OS reclaims the handle at exit.
func resolveRetriever(ctx context.Context, cfg *config.Config, router *provider.Router, dbPath string) (agent.Tool, error) {
	if dbPath == "" {
		return nil, nil // retrieve is opt-in and was not requested
	}
	if cfg == nil || router == nil {
		return nil, fmt.Errorf("no provider configured for embeddings")
	}
	embChain, err := embeddingChain(cfg)
	if err != nil {
		return nil, err
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
	embedder := newChainEmbedder(func(ctx context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return router.Route(ctx, rr)
	}, embChain)
	retr, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel(embChain[0]))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("build retriever for rag-db %q: %w", dbPath, err)
	}
	return &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048}, nil
}

type embedExecutor interface {
	ExecuteEmbed(ctx context.Context) (*provider.EmbedResponse, error)
}

type embedRouteFunc func(context.Context, provider.RoutingRequest) (embedExecutor, error)

func newChainEmbedder(route embedRouteFunc, chain []string) rag.Embedder {
	chain = append([]string(nil), chain...)
	return rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		if model == "" {
			return rag.EmbedResult{}, fmt.Errorf("rag: embedder requires explicit model to prevent vector-space drift across embedding-model boundaries")
		}
		if len(inputs) == 0 {
			return rag.EmbedResult{Model: model}, nil
		}
		if route == nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: embedding router unavailable")
		}
		rr := provider.RoutingRequest{
			UseCase:        "embedding",
			Input:          inputs,
			RequiredCaps:   provider.CapEmbed,
			ExpectedOutput: provider.DefaultExpectedOutput("embedding"),
		}
		if len(chain) > 0 {
			rr.PreferredChain = append([]string(nil), chain...)
			rr.StrictChain = true
		} else {
			rr.Model = model
		}
		plan, err := route(ctx, rr)
		if err != nil {
			return rag.EmbedResult{}, err
		}
		if plan == nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: embedding route returned nil plan")
		}
		resp, err := plan.ExecuteEmbed(ctx)
		if err != nil {
			return rag.EmbedResult{}, err
		}
		if resp == nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: embedding route returned nil response")
		}
		return embedResultFromResponse(resp), nil
	})
}

func embedResultFromResponse(resp *provider.EmbedResponse) rag.EmbedResult {
	// Derive VectorSpaceID the same way the indexer did (actual routed model,
	// else provider/model) so query vectors validate against the corpus's stored
	// vector space. Mirrors mcp's ragEmbedder; omitting it makes every query
	// fail with ErrVectorSpaceMismatch on a corpus that recorded a vector-space ID.
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
	return result
}

// embeddingChain returns the configured embedding fallback chain.
func embeddingChain(cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("no provider configured for embeddings")
	}
	if _, ok := cfg.Defaults["embedding"]; !ok {
		return nil, fmt.Errorf("no embedding model configured (set defaults.embedding in models.json)")
	}
	chain, err := cfg.RoleFallbackChain("embedding")
	if err != nil || len(chain) == 0 {
		if err != nil {
			return nil, fmt.Errorf("resolve embedding chain: %w", err)
		}
		return nil, fmt.Errorf("embedding fallback chain is empty")
	}
	return chain, nil
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
