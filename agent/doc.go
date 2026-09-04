// Package agent implements the go-llm agent runtime: a dynamic
// plan -> act -> observe loop driven by local models through provider.Router.
//
// The loop is the Orchestrator. Run is the blocking primitive; an Observer
// receives serial, in-order callbacks for live output. Six seams have working
// defaults and accept injected replacements:
//
//   - Observer     — live deltas (OnStep / OnToolCall / OnToken)
//   - ModelCaller  — the model seam; NewRouterModelCaller adapts provider.Router
//   - Tool         — effect-aware unit; optional PlanningTool for pre-invoke gating
//   - Interceptor  — deterministic middleware (#436), inspecting frozen copies
//     at every ingress: the initial input, the collected model output (also
//     the partial output of a failed stream), each tool call before Plan and
//     approval, each tool result before the observer and State, and verifier
//     output; verdicts allow/tag/block/abort; per-run RiskReport on
//     Result.Risk; opt-in, off unless WithInterceptors is used. Streamed
//     OnToken/OnThinking deltas precede output inspection and cannot be
//     retracted; consumers that must suppress blocked output buffer them
//   - Compactor    — RecencyCompactor fits the transcript to the token budget
//   - token estimator — conversation.TokenEstimator (len/4 default)
//
// Retrieval is a Tool (see agent/tools.Retrieve), never a hardcoded stage.
// Provider selection, scoring, and budgeting stay in the provider layer.
//
// Resource use and contracts:
//
//   - Result.Steps, Result.Events, and the internal transcript grow with the
//     number of steps taken (bounded by Request.MaxSteps). Each StepRecord
//     retains the full provider.ChatResponse for that turn; consumers that set
//     a large MaxSteps should budget memory accordingly.
//   - Token counts are approximate: the ContextManager re-estimates the
//     transcript each turn with a len/4 heuristic, not the provider's exact
//     tokenization.
//   - Effect.Scope is advisory; the runtime does not enforce path/cwd limits.
//     A tool must enforce its own scope.
package agent
