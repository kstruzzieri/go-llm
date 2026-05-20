// routing_request.go defines the types and helpers for constructing routing
// requests. A RoutingRequest captures everything the router needs to select
// the best provider/model for a given task: capability requirements, priority,
// budget class, and a deterministic sticky key for affinity routing.
package provider

import (
	"crypto/sha256"
	"fmt"
)

// ---------------------------------------------------------------------------
// Priority
// ---------------------------------------------------------------------------

// Priority indicates the urgency of a routing request. Higher values are
// more urgent and may receive preferential treatment during contention.
type Priority int

const (
	// PriorityBackground is for low-urgency tasks (e.g. background indexing).
	PriorityBackground Priority = iota
	// PriorityNormal is the default priority for interactive requests.
	PriorityNormal
	// PriorityHigh is for user-visible, latency-sensitive requests.
	PriorityHigh
	// PriorityCritical is for requests that must not be dropped or delayed.
	PriorityCritical
)

// String returns the human-readable name of the priority level.
func (p Priority) String() string {
	switch p {
	case PriorityBackground:
		return "background"
	case PriorityNormal:
		return "normal"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// RouteKind
// ---------------------------------------------------------------------------

// RouteKind identifies the category of LLM operation being routed.
type RouteKind int

const (
	// RouteKindChat routes to a chat completion endpoint.
	RouteKindChat RouteKind = iota
	// RouteKindGenerate routes to a raw text generation endpoint.
	RouteKindGenerate
	// RouteKindEmbed routes to an embedding generation endpoint.
	RouteKindEmbed
)

// String returns the human-readable name of the route kind.
func (k RouteKind) String() string {
	switch k {
	case RouteKindChat:
		return "chat"
	case RouteKindGenerate:
		return "generate"
	case RouteKindEmbed:
		return "embed"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// RoutingRequest
// ---------------------------------------------------------------------------

// RoutingRequest captures all the information the router needs to select the
// best provider and model for a given task. It is provider-agnostic and
// contains capability requirements, budget hints, and affinity metadata.
//
// NOTE: Phase 2 intentionally omits a ThinkBudget field. Extended thinking
// budget integration will be added in a future phase once the router's
// budget adaptation logic is proven.
type RoutingRequest struct {
	// Model is the preferred model name (may be empty to let the router choose).
	Model string
	// UseCase identifies the caller's intent (e.g. "chat", "fim", "code-review").
	UseCase string
	// RequiredCaps is the bitmask of capabilities the target must support.
	RequiredCaps Capability
	// Messages is the conversation history for chat-style requests.
	Messages []ChatMessage
	// Prompt is the raw prompt for generate-style requests.
	Prompt string
	// System is the system prompt, if any.
	System string
	// Suffix is the suffix for FIM (Fill-in-the-Middle) requests.
	Suffix string
	// Input is the list of texts for embedding requests.
	Input []string
	// Tools is the list of tools available for tool-calling requests.
	Tools []Tool
	// Options holds generation parameters (temperature, num_predict, etc.).
	Options ModelOptions
	// ExpectedOutput is the estimated output token count for budget planning.
	ExpectedOutput int
	// PreferWarm hints that the router should prefer a model already loaded in memory.
	PreferWarm bool
	// Priority indicates the urgency of this request.
	Priority Priority
	// AffinityKey is a caller-supplied key for sticky routing (e.g. session ID).
	AffinityKey string
	// PreferredChain is an ordered list of model selectors (same syntax as
	// Model: "provider/model" qualified, "model" unqualified) derived from a
	// higher layer such as config.Config.RoleFallbackChain. When non-empty,
	// the Router walks chain entries in order (chain-first routing) and
	// suppresses sticky routing for this request. Empty preserves the
	// existing global-scoring path.
	PreferredChain []string
	// StrictChain, when true, suppresses the Recommend safety-net tail that
	// the chain-first router otherwise appends when every chain entry is
	// gated out. Has no effect when PreferredChain is empty.
	StrictChain bool
	// Provider scopes routing to a specific provider *instance* (the
	// config-time name, e.g. "ollama-local-a", "vllm-prod-1"), enforced as
	// a hard pre-score filter rather than a scoring boost:
	//
	//   Model == "" && Provider != ""        → Recommend is restricted to
	//                                           profiles owned by Provider.
	//   unqualified Model && Provider != ""  → Lookup uses ModelKey{Provider,
	//                                           Model} directly.
	//   qualified Model && Provider == ""    → unchanged (current behavior).
	//   qualified Model && Provider != ""    → must agree with the qualified
	//                                           prefix; mismatch returns
	//                                           ErrProviderMismatch from
	//                                           Router.Route before candidate
	//                                           resolution.
	//
	// When PreferredChain is non-empty, Provider is IGNORED: chain selectors
	// carry their own provider identity and authoritative ordering, and the
	// chain-first path (including its non-strict Recommend tail) must not be
	// silently scope-narrowed by a per-request hint. This matches the
	// "invariants enforced inside Router, not via caller convention" pattern
	// from feedback_invariants_at_provider.
	//
	// Provider also participates in StickyKey derivation so two scoped
	// requests with identical affinity/model/use-case keep independent
	// sticky entries (scoping by Provider implies separate affinity slots).
	Provider string
	// DryRun, when true, returns the routing decision without executing the request.
	DryRun bool
}

// ---------------------------------------------------------------------------
// Default expected outputs
// ---------------------------------------------------------------------------

// defaultExpectedOutputs maps well-known use cases to their typical output
// token budget. The "chat" entry doubles as the fallback for unknown use cases.
var defaultExpectedOutputs = map[string]int{
	"fim":         200,
	"chat":        2048,
	"embedding":   0,
	"code-review": 4096,
	"reasoning":   4096,
}

// DefaultExpectedOutput returns the default expected output token count for
// the given use case. Unknown use cases fall back to the chat default (2048).
func DefaultExpectedOutput(useCase string) int {
	if v, ok := defaultExpectedOutputs[useCase]; ok {
		return v
	}
	return defaultExpectedOutputs["chat"]
}

// ---------------------------------------------------------------------------
// Budget classification
// ---------------------------------------------------------------------------

// inputBudgetClass categorizes the estimated input token count into a coarse
// bucket for use in sticky key computation and routing heuristics.
func inputBudgetClass(tokens int) string {
	switch {
	case tokens < 2048:
		return "small"
	case tokens < 8192:
		return "medium"
	default:
		return "large"
	}
}

// outputBudgetClass categorizes the expected output token count into a coarse
// bucket for use in sticky key computation and routing heuristics.
func outputBudgetClass(tokens int) string {
	switch {
	case tokens < 512:
		return "small"
	case tokens < 2048:
		return "medium"
	default:
		return "large"
	}
}

// ---------------------------------------------------------------------------
// Sticky key
// ---------------------------------------------------------------------------

// StickyKey computes a deterministic 128-bit hex string that uniquely
// identifies the routing characteristics of this request. Two requests with
// the same sticky key should be routed to the same provider/model, enabling
// session affinity and cache reuse.
//
// The key is derived from: affinity_key, model, use_case, required_caps,
// priority, input budget class, output budget class, and Provider when set.
// Including Provider ensures that scoped requests (RoutingRequest.Provider
// pinning two distinct instances with the same model name) keep independent
// sticky entries rather than churning a shared slot.
func StickyKey(req RoutingRequest) string {
	expectedOut := req.ExpectedOutput
	if expectedOut == 0 {
		expectedOut = DefaultExpectedOutput(req.UseCase)
	}

	inputTokens := estimateRoutingInputTokens(req)
	inClass := inputBudgetClass(inputTokens)
	outClass := outputBudgetClass(expectedOut)

	data := fmt.Sprintf("%s|%s|%s|%d|%d|%s|%s",
		req.AffinityKey,
		req.Model,
		req.UseCase,
		req.RequiredCaps,
		req.Priority,
		inClass,
		outClass,
	)
	if req.Provider != "" {
		// Appended conditionally so requests that do not set Provider keep
		// byte-identical sticky keys to the pre-Provider behavior — this
		// preserves any existing affinity warmth from prior sessions.
		data += "|provider=" + req.Provider
	}

	hash := sha256.Sum256([]byte(data))
	// Return first 128 bits (16 bytes) as hex.
	return fmt.Sprintf("%x", hash[:16])
}

// ---------------------------------------------------------------------------
// Token estimation
// ---------------------------------------------------------------------------

// estimateRoutingInputTokens provides a rough estimate of the total input
// tokens across all text-bearing fields of the routing request. It uses the
// package-level estimateTokens heuristic (len/4).
func estimateRoutingInputTokens(req RoutingRequest) int {
	total := 0
	for _, m := range req.Messages {
		total += estimateTokens(m.Content)
	}
	total += estimateTokens(req.Prompt)
	total += estimateTokens(req.System)
	total += estimateTokens(req.Suffix)
	for _, s := range req.Input {
		total += estimateTokens(s)
	}
	return total
}
