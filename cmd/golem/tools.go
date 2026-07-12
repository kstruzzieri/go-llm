package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// Prompt fragments composed by capability. The base read-only framing is always
// present; capability or prohibition fragments are appended per enabled flag.
const golemBasePrompt = "You are Golem, a terminal coding assistant for this workspace. " +
	"Use the read-only tools to inspect files before answering repo-specific questions. " +
	"Keep answers concise, cite file paths and line numbers when they matter, and say when the available evidence is insufficient."

const golemNoWriteFragment = " Do not claim to modify files or change project state on disk."
const golemNoExecFragment = " Do not claim to run shell commands, install packages, or otherwise execute processes."
const golemWriteFragment = " You may propose changes with write_file and edit_file; every change is shown to the user as a diff and is applied only after they approve it, so keep edits minimal and targeted and explain what you are changing. Prefer edit_file for small changes and write_file for new files or full rewrites."
const golemExecFragment = " You may run commands with run_command to build, test, or lint and verify your work; every command is shown to the user and runs only after they approve it, so prefer minimal, targeted commands. A non-zero exit code is a normal result to read and react to. Respect AGENTS.md guidance."
const golemPriorityNote = " Prior session messages are context only; the current user request is authoritative."

// buildSystemPrompt composes the base framing with capability fragments. The
// no-commands prohibition applies whenever exec is disabled (so write-only sessions
// still cannot run commands); the no-modify prohibition applies whenever write is
// disabled.
func buildSystemPrompt(allowWrite, allowExec bool) string {
	b := golemBasePrompt
	if allowWrite {
		b += golemWriteFragment
	} else {
		b += golemNoWriteFragment
	}
	if allowExec {
		b += golemExecFragment
	} else {
		b += golemNoExecFragment
	}
	return b + golemPriorityNote
}

// delegateUseCase is the routing use-case for delegated code generation. It
// names the TASK (code generation), not the model — the -delegate-role picks
// the model via the StrictChain-pinned chain.
const delegateUseCase = "coding"

// buildDelegateTool resolves the delegate role chain and returns the
// delegate_code tool backed by a caller pinned to that chain (UseCase
// delegateUseCase). It fails when the role cannot be resolved — an explicit
// -delegate must not silently no-op. The returned chain is for the caller
// wiring / the startup notice. The stream sink is optional: nil disables
// streaming (default runs unchanged); non-nil surfaces generation progress.
func buildDelegateTool(cfg *config.Config, router *provider.Router, role string, stream func(string)) (agent.Tool, []string, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("golem: -delegate requires a models.json with a %q role; none found", role)
	}
	chain, err := cfg.RoleChain(role)
	if err != nil {
		return nil, nil, fmt.Errorf("golem: -delegate-role %q: %w", role, err)
	}
	if len(chain) == 0 {
		return nil, nil, fmt.Errorf("golem: -delegate-role %q resolved to an empty chain", role)
	}
	// delegateUseCase describes the TASK, not the model; -delegate-role selects
	// which model serves it via the StrictChain-pinned chain.
	caller := newRouterChainCallerFor(router, chain, delegateUseCase)
	var opts []agenttools.DelegateOption
	if stream != nil {
		opts = append(opts, agenttools.WithStream(stream))
	}
	return agenttools.NewDelegateCode(caller, opts...), chain, nil
}

// delegateSystemFragment is appended to the system prompt only when delegation
// is enabled. Empty otherwise so default runs are byte-for-byte unchanged. The
// wording is write-aware: it only instructs the model to persist the result
// with write_file/edit_file when those tools are actually registered
// (-allow-write); otherwise it frames the output as review-and-present, so the
// prompt never tells the model to call a tool that isn't available.
func delegateSystemFragment(enabled, allowWrite bool) string {
	if !enabled {
		return ""
	}
	if allowWrite {
		return " For a well-scoped, self-contained code-generation sub-task you may call delegate_code with a precise prompt; it returns generated code from a specialist model. The result is a proposal: review it, then write it with write_file or edit_file. Use delegate_code for bulk generation, never for planning or decisions, and stay responsible for what you apply."
	}
	return " For a well-scoped, self-contained code-generation sub-task you may call delegate_code with a precise prompt; it returns generated code from a specialist model for you to review and present to the user. Use delegate_code for bulk generation, never for planning or decisions."
}

// buildExecTools constructs the approval-gated exec tool set bound to one Workspace
// over root. Returned only when -allow-exec is set.
func buildExecTools(root string) ([]agent.Tool, error) {
	tools, err := agenttools.NewExecTools(root)
	if err != nil {
		return nil, fmt.Errorf("golem: build exec tools: %w", err)
	}
	return tools, nil
}

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
		routeLabel := model
		if len(chain) > 0 {
			rr.PreferredChain = append([]string(nil), chain...)
			rr.StrictChain = true
			routeLabel = strings.Join(chain, " -> ")
		} else {
			rr.Model = model
		}
		plan, err := route(ctx, rr)
		if err != nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: route embedding via %q: %w", routeLabel, err)
		}
		if plan == nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: embedding route returned nil plan")
		}
		resp, err := plan.ExecuteEmbed(ctx)
		if err != nil {
			return rag.EmbedResult{}, fmt.Errorf("golem: execute embedding via %q: %w", routeLabel, err)
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

// buildGatedRetriever stats dbPath, opens it, probes its stored vector space,
// reads store stats for startup display, and applies the §6.1 gate against
// expected. It returns (tool, feedback, feedbackWarn, decision, stats, err):
//   - tool/decision/stats set, err nil when the corpus is registerable
//     (decision.kind may be vsLegacy, surfaced as a soft warning by the caller);
//   - nil tool, err nil when the gate disables retrieve (vsMismatch/vsInconsistent);
//   - nil tool, zero stats, err set when the DB cannot be opened/probed or the
//     embedder is unavailable.
//
// When feedbackDB != "" it best-effort opens a consume-only behavioral weighter
// and injects it into the store. feedback is non-nil only on success (the caller
// owns it and must close feedback.db); it stays nil when feedback is disabled or
// fails to open. feedbackWarn is non-empty when feedback failed to open, in which
// case retrieval still registers and ranks neutrally.
//
// The returned retrievalReader owns and closes the opened store.
func buildGatedRetriever(ctx context.Context, cfg *config.Config, router *provider.Router, dbPath string, expected []string, feedbackDB string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
	if cfg == nil || router == nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: no provider configured for embeddings")
	}
	embChain, err := embeddingChain(cfg)
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, err
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: rag-db %q: %w", dbPath, err)
	}
	if info.IsDir() {
		return nil, "", vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: rag-db %q is a directory, not a SQLite file", dbPath)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: open index db %q: %w", dbPath, err)
	}
	closeStore := func(cause error) error {
		if closeErr := store.Close(); closeErr != nil {
			return errors.Join(cause, fmt.Errorf("golem: close index db %q: %w", dbPath, closeErr))
		}
		return cause
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: read index stats %q: %w", dbPath, err))
	}
	if stats.EmbeddingFormat != rag.EmbeddingFormatEmpty && stats.EmbeddingFormat != rag.EmbeddingFormatPackedFloat32 {
		return nil, "", vsDecision{}, stats, closeStore(fmt.Errorf(
			"golem: rag-db %q uses embedding format %s; explicit indexes are read-only and will not be migrated; rebuild it deliberately or remove -rag-db to use the packed auto index",
			dbPath, stats.EmbeddingFormat))
	}
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: probe index db %q: %w", dbPath, err))
	}
	dec := vsGateDecision(probe.KnownIDs, probe.HasUnknown, expected)
	if !dec.register {
		return nil, "", dec, stats, closeStore(nil)
	}
	queryChain := embChain
	queryModel := embChain[0]
	if dec.stored != "" {
		// A known corpus is usable only in its recorded vector space. A
		// one-entry strict chain preserves provider identity and forbids fallback.
		queryChain = []string{dec.stored}
		queryModel = dec.stored
	}
	embedder := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return router.Route(rc, rr)
	}, queryChain)
	retr, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel(queryModel))
	if err != nil {
		return nil, "", vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: build retriever for %q: %w", dbPath, err))
	}
	var feedbackHandle *behavioralWeighterHandle
	feedbackWarn := ""
	if feedbackDB != "" {
		if h, warn := openBehavioralWeighter(ctx, feedbackDB); h != nil {
			store.SetBehavioralWeighter(h.weighter)
			feedbackHandle = h
		} else if warn != "" {
			feedbackWarn = warn
		}
	}
	tool := &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048}
	return newOwnedRetrievalReader(tool, store, feedbackHandle), feedbackWarn, dec, stats, nil
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
