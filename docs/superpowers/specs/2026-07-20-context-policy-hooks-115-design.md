# Retrieval Context Policy Hooks (#115)

**Date:** 2026-07-20  
**Issue:** [#115](https://github.com/kstruzzieri/go-llm/issues/115)  
**Base:** `origin/develop@e8bbc6306a3ef1be573de525db29bc0ee4bc81a0`  
**Status:** Reviewed design with G1-G5 and E1-E4 amendments folded; awaiting final spec approval

## Problem

RAG retrieval currently has no request-level governance boundary. Callers can
select a managed collection and tags, but cannot attach principal/session
context, require verified freshness, impose governance limits, obtain a typed
denial, filter or redact results, or observe a content-safe policy outcome.

Enforcement in MCP alone would leave direct RAG consumers ungoverned.
Enforcement in `VectorStore` would be too low: stores do not have the complete
request, managed-source freshness is finalized after store search, and policy
would have to be duplicated across dense, hybrid, and scoped implementations.

The boundary must also account for two existing bypasses:

- `prefetch.Engine` can return a cached result before the underlying retriever,
  and its key has no authorization dimension.
- `rag_answer` can probe the whole store with `KeywordPresence` after governed
  retrieval, revealing that excluded documents exist.

## Goals

- Add one consumer-installed policy evaluator to `rag.Retriever`.
- Add an explicit request/response surface carrying source scope, freshness,
  result/cost bounds, audit labels, and optional principal/session identifiers.
- Apply request denial before embedding/search and result filtering/redaction
  after managed freshness stamping.
- Route all existing `rag.Retriever` methods through one canonical pipeline.
- Preserve exact unrestricted behavior when no evaluator or policy metadata is
  present.
- Return typed policy/freshness failures and structured outcome counts.
- Emit one content-safe terminal observer event on success and failure.
- Carry policy metadata through MCP's native namespaced `_meta` without adding
  model-visible tool arguments.
- Prevent cache and whole-corpus diagnostic bypasses.
- Never mutate caller-owned requests, returned store objects, stored chunks,
  chunk metadata, or score-signal maps.

## Non-goals

- RBAC, users, a policy DSL, policy persistence, network authentication, or
  mandatory multitenancy.
- Making low-level `VectorStore` APIs authorization-aware. Code holding a raw
  store remains inside the trust boundary.
- Policy-partitioned prefetch caching. Governed retrieval bypasses the cache in
  v1; an opaque cache partition can be added only when measured demand exists.
- Hierarchical retrieval or trace integration from #190.
- Changes under `agent/`, `agentflow/`, `cmd/golem/`, or `internal/agenttrace/`.
- A monetary/token cost meter. `MaxCost` is an opaque non-negative unit agreed
  by the consumer and evaluator; core carries and tightens it but cannot meter
  a cost model that RAG does not have.

## Chosen approach

Add one canonical request-aware method on `*rag.Retriever`. The four existing
methods remain source-compatible wrappers. A configured evaluator has two
fixed phases: a pre-retrieval decision and a positional post-retrieval decision
list. Core, rather than the evaluator, composes constraints and applies result
changes.

Rejected alternatives:

1. Extending `QueryContext`: it is scorer-facing, mixes ranking with access
   control, risks leaking identity into custom scorers, and adding fields can
   break external unkeyed literals.
2. An MCP-only decorator: direct RAG consumers, prompts, analysis callers, and
   store-adjacent paths could bypass it.
3. A parallel governed retriever wrapper: raw `Retriever` remains an attractive
   bypass and every retrieval variant must be mirrored.

## Public RAG contract

Names below are normative; comments will document zero-value behavior.

```go
type RetrievalRequest struct {
    Query        string
    K            int
    Scope        RetrievalScope
    QueryContext QueryContext
    Policy       RetrievalPolicyRequest
}

type RetrievalPolicyRequest struct {
    PrincipalID  string
    SessionID    string
    Scope        RetrievalScope
    RequireFresh bool
    MaxResults   int
    MaxCost      int64
    AuditLabels  map[string]string
}

type RetrievalPolicyEvaluator interface {
    Evaluate(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error)
    EvaluateResults(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error)
}

type RetrievalPolicyDecision struct {
    Allow        bool
    Scope        RetrievalScope
    RequireFresh bool
    MaxResults   int
    MaxCost      int64
}

type RetrievalResultDecision struct {
    Keep            bool
    RedactedContent *string
}

type RetrievalPolicyDisposition string

const (
    RetrievalPolicyAllowed RetrievalPolicyDisposition = "allowed"
    RetrievalPolicyDenied  RetrievalPolicyDisposition = "denied"
    RetrievalPolicyFailed  RetrievalPolicyDisposition = "failed"
)

type RetrievalPolicyOutcome struct {
    Applied              bool
    Disposition          RetrievalPolicyDisposition
    ReasonCode           string
    CandidateCount       int
    CandidateSourceCount int
    ReturnedCount        int
    ReturnedSourceCount  int
    FilteredCount        int
    RedactedCount        int
    StaleDroppedCount    int
    AuditLabelCount      int
}

type RetrievalResponse struct {
    Results []ScoredResult
    Policy  RetrievalPolicyOutcome
}

type RetrievalPolicyObserver interface {
    OnRetrievalPolicy(context.Context, RetrievalPolicyEvent) error
}

type RetrievalPolicyEvent struct {
    Outcome RetrievalPolicyOutcome
}

func WithRetrievalPolicyEvaluator(RetrievalPolicyEvaluator) RetrieverOption
func WithRetrievalPolicyObserver(RetrievalPolicyObserver) RetrieverOption
func (*Retriever) RetrieveRequest(context.Context, RetrievalRequest) (RetrievalResponse, error)
func (*Retriever) PolicyActive() bool
```

`MaxResults == 0` and `MaxCost == 0` mean unspecified. Negative limits are
invalid. `K <= 0` retains the current RAG meaning of unbounded retrieval.
`MaxCost` is evaluated and monotonically tightened as metadata; v1 does not
claim to meter actual retrieval cost.

An installed evaluator must return `Allow: true`. Its zero-value decision is a
denial. Outcome `ReasonCode` is assigned only by core from a fixed set such as
`default_allow`, `allowed`, `denied`, `evaluator_failed`, `decision_invalid`,
`retrieval_failed`, and `freshness_unknown`. Evaluators cannot supply observable
reason text. Raw evaluator errors and messages never enter MCP output or
observer events.
`PolicyActive` reports whether an evaluator is installed; an observer alone
does not change retrieval or cache safety.

`Policy.Applied` is true when an evaluator is installed or
`RetrievalPolicyRequest` has any non-zero field, including policy scope. It is
false for an observer alone and for legacy `RetrievalRequest.Scope` alone.

### Structural result decisions

`EvaluateResults` receives a deep-cloned `[]Chunk` in retrieval order and must
return exactly one decision per candidate. Core applies decisions positionally
to a separate deep clone of the scored results:

- `Keep: false` drops that candidate.
- `Keep: true, RedactedContent: nil` keeps it unchanged.
- `Keep: true, RedactedContent: &text` replaces only `Chunk.Content`.
- `Keep: false` with a redaction and a decision-list length mismatch are
  invalid and fail closed.

The evaluator cannot add/reorder candidates or change IDs, sources,
attribution, metadata, rank, scores, or signals by construction. A configured
evaluator is trusted to supply arbitrary replacement content through
`RedactedContent`; core applies it only to the corresponding candidate. A
policy that must conceal source or metadata drops the entire chunk.
Post-filtering preserves rank and does not backfill beyond the retrieved
candidate window.

## Constraint composition

Evaluator fields are additional constraints, never replacements:

- Collection: exact intersection across caller scope, policy-request scope, and
  decision scope. Different non-empty collections produce an empty governed
  result, never an empty/unrestricted scope.
- Tags: normalized union, because every tag in `RetrievalScope` is required.
- Result limit: minimum positive value among `K`, request `MaxResults`, and
  decision `MaxResults`.
- Cost limit: minimum positive request/decision value, passed through as opaque
  evaluator metadata.
- Freshness: logical OR of request and decision requirements.

A decision ceiling larger than the caller's ceiling is harmless and remains
monotonic through `min`; it is not rejected as malformed. Negative bounds,
invalid UTF-8, oversized scope/tags/labels, and structurally invalid result
decisions fail closed.
Different valid collection constraints are not malformed; core short-circuits
their empty intersection without searching.

## Canonical retrieval flow

`RetrieveRequest` is the only policy implementation. A private scored base
helper owns current dense/hybrid, scoped/unscoped, and freshness-finalization
behavior. Existing methods delegate once:

- `RetrieveScored` and `RetrieveScoredScoped` return `response.Results`.
- `Retrieve` and `RetrieveScoped` flatten the canonical scored results while
  preserving current semantic `Score`, `Distance`, ordering, nil/empty behavior,
  and error wrapping.

The ordered pipeline is:

1. Validate, normalize, and clone request slices/maps.
2. If no evaluator, observer, or policy-request fields are present, take the
   exact legacy fast path; `Policy.Applied` remains false and no policy `_meta`
   is emitted.
3. If an evaluator is installed, invoke `Evaluate` before embedding/search.
   Otherwise use the permissive default decision while still enforcing
   caller-supplied scope, result, and freshness constraints.
4. On denial or evaluator failure, construct the safe outcome, notify the
   observer, and return a typed failure without searching.
5. Validate and mechanically compose additional scope, freshness, and limits.
6. Retrieve/rank once within the effective declarative scope.
7. Run the existing managed-source freshness finalization and retain a private
   per-candidate trusted freshness sidecar from the managed registry/live-hash
   path. Do not infer trust from caller/store-controlled chunk metadata.
8. If fresh results are required, drop registry-confirmed known-stale managed
   chunks. Any candidate without a trusted managed freshness result, including
   unmanaged/custom-store chunks or forged `managed_freshness` metadata, fails
   the whole request with `ErrFreshnessUnknown`; no content is returned.
9. If an evaluator is installed, deep-clone candidates and invoke
   `EvaluateResults` with the effective request; otherwise keep freshness-
   eligible candidates unchanged.
10. Validate and apply positional keep/redact decisions, preserving order and
    scores with no backfill.
11. Construct outcome/source counts and invoke one terminal observer event.
12. Only then return results for JSON, context/evidence building, prompts,
    routing, or transcript persistence.

The observer is synchronous and consumer-owned, matching the repository's
`OnX(ctx, event) error` convention. It receives no query, content, source,
principal/session identifier, audit-label value, or raw error. A callback error
fails closed. When a primary typed policy error already exists, `errors.Join`
preserves it while retaining the observer error. Best-effort sinks may retain
their own write error and return nil.

Exactly one terminal event is attempted for default allow (when an observer is
installed), allow, deny, evaluator failure, invalid decision, freshness failure,
retrieval failure, and successful filtered/redacted retrieval. Telemetry is not
success-only.

`CandidateCount` and `CandidateSourceCount` describe post-retrieval,
post-freshness-stamp candidates before freshness withholding or evaluator
result decisions. `FilteredCount` counts only evaluator drops;
`StaleDroppedCount` is separate. Returned source counts are unique source
strings after all decisions. No withheld-count field is stored because it is
derivable from the other counts.

## Error contract

Add four exported sentinels to `rag/errors.go`:

```go
var (
    ErrPolicyDenied          = errors.New("rag: retrieval policy denied")
    ErrPolicyEvaluatorFailed = errors.New("rag: retrieval policy evaluator failed")
    ErrPolicyDecisionInvalid = errors.New("rag: retrieval policy decision invalid")
    ErrFreshnessUnknown      = errors.New("rag: retrieval freshness unknown")
)
```

Evaluator causes remain available to trusted Go callers through wrapping, but
MCP maps each sentinel to a fixed, distinct tool-error category and never
interpolates the underlying error:

- `policy_denied`
- `policy_evaluator_failed`
- `policy_decision_invalid`
- `freshness_unknown`

The structured response/outcome is returned alongside typed failures where an
outcome exists. Known-stale chunks are withheld and counted; unverifiable
freshness uses the typed failure above.

MCP maps principal-resolution failure to fixed `policy_identity_failed`. Any
other non-sentinel failure while policy is applied, including observer failure,
uses fixed `policy_failed`; raw errors are never interpolated.

## Enforcement-point inventory

| Path | Governing rule |
| --- | --- |
| `Retriever.Retrieve` | Delegates to `RetrieveRequest` |
| `Retriever.RetrieveScoped` | Delegates to `RetrieveRequest` |
| `Retriever.RetrieveScored` | Delegates to `RetrieveRequest` |
| `Retriever.RetrieveScoredScoped` | Delegates to `RetrieveRequest` |
| Store `Search` / `SearchMulti` used by Retriever | Called only inside the canonical scored base path |
| MCP `rag_search` | Builds `RetrievalRequest`; marshals only returned results |
| MCP chat `use_rag` | Builds context only from returned results |
| MCP `rag_answer` | Builds evidence/diagnostics only from returned results |
| MCP `rag-query` prompt | Builds prompt only from returned results |
| MCP analysis retrieval callers | Legacy methods still cross the configured canonical pipeline |
| MCP `KeywordPresence` refinement | Skipped whenever `response.Policy.Applied` or effective scope is non-empty |
| `prefetch.Engine` | Cache read, cache write, and background warming bypassed when the underlying retriever reports `PolicyActive()` |
| Direct `VectorStore` consumers | Explicitly ungoverned low-level primitives inside the trust boundary |

For skipped answer refinement, keep `not_in_retrieved_context` and omit corpus
terms/counts. Do not add another response field. This includes nil-evaluator
requests carrying only freshness/result/cost policy metadata. Legacy requests
with `Policy.Applied == false` and empty effective scope retain refinement.

## Prefetch safety

`prefetch.Engine` detects an optional private `PolicyActive() bool` capability
on its underlying retriever. When true:

- foreground retrieval does not read or write `WarmCache`;
- background prefetch does not run or populate shared entries;
- every foreground request reaches the underlying configured retriever.

This deliberately gives up warm-cache latency rather than create a
cross-principal oracle. `SkipCache` alone is insufficient because current code
still writes cold results. Policy-aware cache partitions are deferred.
The in-package scored adapter forwards `PolicyActive` from its underlying
`*rag.Retriever`.

`prefetch` has no request-aware metadata surface in #115. Consumers needing
per-request principal/scope/freshness metadata call the canonical RAG request
API; a later #190 integration may add a request-aware prefetch adapter. The v1
engine must not be described as supporting such metadata.

## MCP metadata and identity trust

Use the SDK's native metadata at one exported key:

```go
const RetrievalPolicyMetaKey = "go-llm/retrieval-policy"
```

The value is a bounded JSON object matching `RetrievalPolicyRequest`, including
its additional source scope. Existing explicit `rag_search` collection/tags
remain caller scope; core intersects them with metadata scope and the evaluator
decision. Existing tool argument schemas remain unchanged.

The MCP layer uses a private wire struct; the public RAG types do not acquire
JSON tags:

```json
{
  "principal_id": "local-claim",
  "session_id": "local-session",
  "scope": {"collection": "docs", "tags": ["public"]},
  "require_fresh": true,
  "max_results": 10,
  "max_cost": 100,
  "audit_labels": {"purpose": "support"}
}
```

Boundary limits are concrete:

- the serialized namespaced object is at most 16 KiB and rejects unknown fields;
- principal/session IDs are valid UTF-8 and at most 4 KiB each;
- collection and tags use existing RAG limits: 4 KiB collection, 64 tags,
  256 bytes per tag;
- audit labels contain at most 64 entries with valid UTF-8 keys/values of at
  most 256 bytes each;
- `max_results` is non-negative and at most 100 in MCP metadata;
- `max_cost` is a non-negative signed 64-bit integer.

Wrong JSON types, fractional integers, overflow, and excess bounds return a
fixed `validation` tool error before policy evaluation. Tags retain existing
trim/deduplicate/sort normalization.

Policy results use the same result `_meta` key but expose only disposition,
core-defined reason code, and aggregate counts. They never echo principal/session,
audit labels, query, chunk content, source, removed IDs, evaluator text, or
underlying errors. With no evaluator and no policy metadata, request/result
`Meta` remains nil for byte-compatible legacy behavior.

Add one narrow MCP option:

```go
type RetrievalPrincipalResolver func(context.Context, gomcp.Request) (string, error)

func WithRetrievalPrincipalResolver(RetrievalPrincipalResolver) Option
func WithRetrievalPolicyEvaluator(rag.RetrievalPolicyEvaluator) Option
func WithRetrievalPolicyObserver(rag.RetrievalPolicyObserver) Option
```

Identity precedence is:

1. An installed principal resolver is authoritative; errors fail closed.
2. Otherwise, authenticated SDK `TokenInfo.UserID`, when present, is the
   principal.
3. Client `_meta.principal_id` is accepted only for local/in-memory requests
   with no HTTP request extra; it is a self-asserted local identity.
4. Remote requests without a resolver or authenticated token leave the
   optional principal empty; client claims are ignored.
5. A non-empty SDK server session ID always overrides `_meta.session_id`.
   Local metadata session is only a fallback when transport supplies none.

TLS and a server-generated session ID do not authenticate a person. #115 adds
the binding hook but no verifier, middleware framework, or auth subsystem.

Evaluator, observer, and resolver fields live on `mcp.Server`. Every retriever
created by `rebuildDerivedClients` receives the evaluator/observer options, so
model refresh cannot silently remove enforcement.

If chat RAG returns zero candidates while `response.Policy.Applied` is true,
the handler returns fixed `policy_no_context: no permitted RAG context` and
does not call the router. It must not reuse the legacy “RAG index is empty”
message. Search still returns an empty governed result with safe outcome
metadata; audited answer retains its existing
`not_in_retrieved_context` status.

## Compatibility

- Existing public retrieval signatures are unchanged.
- `QueryContext`, `VectorStore`, and public `ScoredRetriever` are not expanded.
- Existing JSON, prompts, scoring semantics, nil/empty slices, and error
  wrapping are unchanged when evaluator, observer, and policy-request metadata
  are all absent.
- Empty scope retains support for custom stores. Non-empty scope retains the
  existing managed `SQLiteStore` requirement.
- `QueryContext.Timestamp` behavior is unchanged.
- Policy output is additive and only appears when policy is applied.
- Filtering/redaction occurs before `BuildContext`, answer evidence labels,
  prompt construction, router calls, diagnostics, and transcript capture.

## Test strategy

Implementation follows strict red-green-refactor. Each behavior gets a failing
test for the intended reason before production code.

### RAG

- All four legacy paths remain exact when evaluator, observer, and
  policy-request metadata are absent.
- Default/explicit allow and normalized cloned request propagation.
- Denial performs no embedding or store search and emits one event.
- Request/decision scope, limits, cost metadata, and freshness compose
  monotonically.
- Positional filter preserves order and under-fills without backfill.
- Content-only redaction preserves attribution, scores, and signals.
- Invalid decision lists and evaluator failures fail closed with sentinels.
- Known stale chunks are withheld; unknown freshness returns the typed error.
- Evaluator/observer receive no aliased request or stored-result maps.
- A second retrieval proves stored chunks, metadata, and signal maps were not
  mutated.
- Terminal observer coverage includes deny/failure paths and content-leak
  assertions.

### MCP

- Namespaced `_meta` reaches the evaluator for search, chat, answer, and prompt
  paths without entering `QueryContext.Metadata` or tool schemas.
- Transport/authenticated identity precedence overrides claimed metadata.
- Evaluator configuration survives retriever rebuild.
- Denial/failure categories and result `_meta` are fixed and content-safe.
- Filtered/redacted content never appears in search JSON, chat/answer prompts,
  diagnostics, router requests, or persisted rendered transcripts.
- Filtered-to-zero chat returns fixed `policy_no_context` and makes no router
  call; it never claims the index is empty.
- `rag_answer` never calls `KeywordPresence` when `Policy.Applied` is true or
  effective scope is non-empty.
- Response bytes and prompt text remain unchanged when evaluator, observer,
  and policy-request metadata are absent.

### Prefetch

- A policy-active underlying retriever produces no warm-cache hit/write.
- Background warming is skipped while policy is active.
- Legacy policy-inactive cache behavior remains unchanged.

## Verification

Run from the isolated worktree:

```text
rtk go test ./rag ./mcp ./prefetch
rtk go test -race ./rag ./mcp ./prefetch
rtk go test ./...
rtk go vet ./...
rtk git diff --check
```

Also search test outputs and constructed payloads for known denied/redacted
sentinels to prove removed content never reaches MCP output or prompts.

## Deferred work

- #190 hierarchical retrieval and policy-decision trace integration.
- Cache partitions keyed by an evaluator-supplied opaque policy fingerprint.
- A request-aware prefetch adapter.
- Policy-aware low-level corpus diagnostics if whole-corpus refinement is ever
  required under governance.
- Consumer-specific authentication configuration built outside go-llm.
