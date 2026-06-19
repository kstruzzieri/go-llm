// Package agent implements the go-llm agent runtime: a dynamic
// plan -> act -> observe loop driven by local models through provider.Router.
//
// The loop is the Orchestrator. Run is the blocking primitive; an Observer
// receives serial, in-order callbacks for live output. Five seams have working
// defaults and accept injected replacements:
//
//   - Observer     — live deltas (OnStep / OnToolCall / OnToken)
//   - ModelCaller  — the model seam; NewRouterModelCaller adapts provider.Router
//   - Tool         — effect-aware unit; optional PlanningTool for pre-invoke gating
//   - Compactor    — RecencyCompactor fits the transcript to the token budget
//   - token estimator — conversation.TokenEstimator (len/4 default)
//
// Retrieval is a Tool (see agent/tools.Retrieve), never a hardcoded stage.
// Provider selection, scoring, and budgeting stay in the provider layer.
package agent
