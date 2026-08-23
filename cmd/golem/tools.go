package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/config"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// buildSystemPrompt composes the base framing with capability fragments. The
// no-commands prohibition applies whenever exec is disabled (so write-only sessions
// still cannot run commands); the no-modify prohibition applies whenever write is
// disabled.
func buildSystemPrompt(allowWrite, allowExec bool) string {
	return golemruntime.SystemPrompt(allowWrite, allowExec)
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

// dispatchSystemFragment is appended to the system prompt only when dispatch is
// enabled. Empty otherwise so default runs are byte-for-byte unchanged.
func dispatchSystemFragment(enabled bool) string {
	if !enabled {
		return ""
	}
	return " For broad read-only investigation you may call dispatch with up to a few independent exploration tasks; they run with backend-governed bounded concurrency when every routed backend is slot-governed and stay serial when any backend is ungoverned. Each runs in a bounded child agent with only file-reading and retrieval tools and returns a summary, its stop reason, and the model that produced it. Use dispatch to keep bulk exploration out of your own context; children cannot modify anything or dispatch further children, and their summaries are evidence to verify, not conclusions to repeat blindly."
}

// dispatchUseCase is the routing use-case for dispatch child agents. It names
// the TASK (agentic tool-use exploration), not the model — the child chain
// picks the model. Not asserted on the wire by any test; keep in sync with the
// parent's "agent" use case by review.
const dispatchUseCase = "agent"

// resolveDispatchChain returns the chain dispatch children route down. An
// empty role follows parentChain verbatim (which may itself be empty:
// recommend mode) so children route to the already-resident primary model and
// never force a model swap. An explicit role resolves its own chain and fails
// loudly when it cannot — an explicit -dispatch-role must not silently no-op.
// The caller owns preflighting an explicit chain: the run-level agent
// preflight only covers the parent chain.
func resolveDispatchChain(cfg *config.Config, role string, parentChain []string) ([]string, error) {
	if role == "" {
		return parentChain, nil
	}
	if cfg == nil {
		return nil, fmt.Errorf("golem: -dispatch-role requires a models.json with a %q role; none found", role)
	}
	resolved, err := cfg.RoleChain(role)
	if err != nil {
		return nil, fmt.Errorf("golem: -dispatch-role %q: %w", role, err)
	}
	if len(resolved) == 0 {
		return nil, fmt.Errorf("golem: -dispatch-role %q resolved to an empty chain", role)
	}
	return resolved, nil
}

// dispatchFanout is the resolved fan-out policy newDispatchTool applies:
// a static concurrency ceiling plus an optional per-invocation governor.
type dispatchFanout struct {
	maxConcurrent int
	governor      func() int
}

// resolveDispatchFanout picks the dispatch fan-out policy (#403). Dispatch is
// parallel-eligible only when every selector in the child chain is
// provider-qualified and slot-governed: Router admission protects governed
// backends from oversubscription, so it cannot protect an ungoverned
// fallback, and an empty recommend-mode chain names no backend at all. Among
// eligible chains the HEAD sizes fan-out — unused fallbacks never serve, so
// their capacity caches stay cold and a chain-minimum would pin fan-out at
// the fail-safe 1 forever. The governor re-reads capacity per invocation
// because slot probes are use-triggered: a constructor-time read would freeze
// the pre-probe fail-safe. Sizing is quality of service, not safety; a wrong
// size only queues children at admission.
func resolveDispatchFanout(capacity func(provider.ModelKey) (int, bool), chain []string) dispatchFanout {
	serial := dispatchFanout{maxConcurrent: 1}
	if capacity == nil || len(chain) == 0 {
		return serial
	}
	keys := make([]provider.ModelKey, len(chain))
	for i, selector := range chain {
		key, ok := parseSelector(selector)
		if !ok || key.Provider == "" || key.Model == "" {
			return serial
		}
		keys[i] = key
	}
	for _, key := range keys {
		if _, governed := capacity(key); !governed {
			return serial
		}
	}
	return dispatchFanout{
		maxConcurrent: agenttools.MaxDispatchTasks,
		governor: func() int {
			n, governed := capacity(keys[0])
			if !governed || n < 1 {
				return 1
			}
			for _, key := range keys[1:] {
				if _, governed := capacity(key); !governed {
					return 1
				}
			}
			return n
		},
	}
}

// newDispatchTool is the caller-injectable seam of the dispatch wiring: tests
// drive child behavior (toolset pass-through, retrieve reuse, fan-out,
// budget threading, completion notices) through it with a scripted caller,
// which a *provider.Router cannot fake. mixed mirrors -progressive so child
// context assembly matches the shared retrieve renderer, the same pairing
// newOrchestratorFactory enforces for the parent. budget carries the resolved
// input ceiling and output reserve for the chain children route, so a child
// never assembles a request larger than its backend accepts; zero fields fall
// back to library defaults. fan carries the resolved fan-out policy (#403):
// a static ceiling plus the per-invocation capacity governor from
// resolveDispatchFanout; the validated models.<role>.slots override reaches
// it through Router.SlotCapacity, so no separate worker knob exists. notify,
// when non-nil, receives one display-only line per completed child (#402
// rider) from child goroutines after their permits release; golem points it
// at the late-bound notice sink. Remaining limits keep every library
// default — 4 tasks, 6 steps, 32k tokens per child — except Timeout: the
// library's 5m bounds the WHOLE invocation, and a live two-task run on
// gemma4:31b measured task 2 starving behind task 1 (single model calls ran
// 76-347s), so golem budgets that per-task ceiling times the 4-task maximum;
// governed fan-out only shrinks wall clock below that worst case.
func newDispatchTool(caller agent.ModelCaller, mixed bool, budget agent.Budget, fan dispatchFanout, notify func(string), available []agent.Tool) (agent.Tool, error) {
	var onChildComplete func(int, int)
	if notify != nil {
		onChildComplete = func(index, total int) {
			notify(fmt.Sprintf("dispatch: child %d/%d finished", index+1, total))
		}
	}
	dt, err := agenttools.NewDispatch(caller, agent.ContextManager{Mixed: mixed}, available, agenttools.DispatchLimits{
		Budget:          budget,
		MaxConcurrent:   fan.maxConcurrent,
		Concurrency:     fan.governor,
		OnChildComplete: onChildComplete,
		Timeout:         20 * time.Minute, // 5m per task x 4 max tasks
	})
	if err != nil {
		return nil, fmt.Errorf("golem: build dispatch tool: %w", err)
	}
	return dt, nil
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
// expected. It returns (tool, decision, stats, err):
//   - tool/decision/stats set, err nil when the corpus is registerable
//     (decision.kind may be vsLegacy, surfaced as a soft warning by the caller);
//   - nil tool, err nil when the gate disables retrieve (vsMismatch/vsInconsistent);
//   - nil tool, zero stats, err set when the DB cannot be opened/probed or the
//     embedder is unavailable.
//
// The returned retrievalReader owns and closes the opened store.
func buildGatedRetriever(ctx context.Context, cfg *config.Config, router *provider.Router, dbPath string, expected []string, weighter rag.BehavioralWeighter, progressive bool) (*retrievalReader, vsDecision, rag.StoreStats, error) {
	if cfg == nil || router == nil {
		return nil, vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: no provider configured for embeddings")
	}
	embChain, err := embeddingChain(cfg)
	if err != nil {
		return nil, vsDecision{}, rag.StoreStats{}, err
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: rag-db %q: %w", dbPath, err)
	}
	if info.IsDir() {
		return nil, vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: rag-db %q is a directory, not a SQLite file", dbPath)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		return nil, vsDecision{}, rag.StoreStats{}, fmt.Errorf("golem: open index db %q: %w", dbPath, err)
	}
	closeStore := func(cause error) error {
		if closeErr := store.Close(); closeErr != nil {
			return errors.Join(cause, fmt.Errorf("golem: close index db %q: %w", dbPath, closeErr))
		}
		return cause
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		return nil, vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: read index stats %q: %w", dbPath, err))
	}
	if stats.EmbeddingFormat != rag.EmbeddingFormatEmpty && stats.EmbeddingFormat != rag.EmbeddingFormatPackedFloat32 {
		return nil, vsDecision{}, stats, closeStore(fmt.Errorf(
			"golem: rag-db %q uses embedding format %s; explicit indexes are read-only and will not be migrated; rebuild it deliberately or remove -rag-db to use the packed auto index",
			dbPath, stats.EmbeddingFormat))
	}
	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		return nil, vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: probe index db %q: %w", dbPath, err))
	}
	dec := vsGateDecision(probe.KnownIDs, probe.HasUnknown, expected)
	if !dec.register {
		return nil, dec, stats, closeStore(nil)
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
		return nil, vsDecision{}, rag.StoreStats{}, closeStore(fmt.Errorf("golem: build retriever for %q: %w", dbPath, err))
	}
	store.SetBehavioralWeighter(weighter)
	tool := &agenttools.Retrieve{R: retr, K: 5, MaxTokens: 2048, Progressive: progressive}
	return newOwnedRetrievalReader(tool, store), dec, stats, nil
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
