# #450 delegate proposal provenance

Status: approved for execution in an isolated worktree on 2026-09-06, including the shared transport changes. Prepared 2026-09-05 against `origin/develop` at `b97613be5eca70205fb92b0cf114236f36e37fd4`. The local `develop` checkout is older (`8848d3a`).

Revision: Gemini feedback incorporated; Keith authorized execution of this revised spec and plan on 2026-09-06.

Issue: [#450 / ZT-403](https://github.com/kstruzzieri/go-llm/issues/450). Its acceptance criterion is that delegate outputs carry an HMAC/digest of the response payload and model identifier. This spec implements emission and retention of that evidence. It does not introduce an apply-time security gate.

## Approved decisions

1. Reuse `signing.Signer` and `signing.Verifier`, with one fresh, in-memory HMAC key per `DelegateCode` instance by default. Allow an explicitly supplied matching signer/verifier pair, including Ed25519. Preserve the existing constructor signature and Golem call sites.
2. Put a plain JSON envelope in structured result metadata, with the original proposal retained independently of model-facing content. Add the small shared transport changes listed below; the handoff's original delegate-only ownership cannot provide this transport.
3. Sign the content digest, exact prompt digest, actual model identity, whole-second UTC completion timestamp, and a declaration of the content form. Use the fixed domain `go-llm/delegate-proposal/v1`.
4. Export one pure `VerifyDelegateProposal` helper for the complete typed proposal contract; use it during emission and in consumer tests. Retain accepted envelopes in `Result.ToolCalls` and therefore existing content-full traces. Leave conversation persistence and enforcement before applying a proposal to subsequent work.

The default HMAC proves that this runtime's key attested to the proposal and route identity. It does not prove that the external model possesses a signing key. HMAC verification is symmetric authority. The default identity is memory-only; retaining `ProposalVerifier` retains its symmetric key capability. Persisted traces become unverifiable when the matching key is lost. Durable offline auditing needs a caller-supplied persistent identity and separately retained trusted verification material.

## Why this approach

| Approach | Consequence |
|---|---|
| Signer/verifier pair, HMAC default — recommended | Reuses #444, requires no CLI key configuration, and supports externally managed HMAC or Ed25519 identities. |
| HMAC-only | Slightly smaller API, but excludes the asymmetric provenance and key ownership seam already supplied by #444. |
| Bare digest | Meets the issue's narrow digest wording, but someone who changes the proposal can also recompute the digest. It does not authenticate origin. |

A content trailer would change the pure proposal, consume model tokens, and mix transport evidence with presentation. Retrieval `Context`, `Preview`, and `RouteOutcome` have existing, different contracts. None will be repurposed.

## Verified source behavior

- `agent/tools/delegate.go` builds exactly a system message and the decoded user prompt; no tools are supplied to the specialist. It trims the response, strips a wrapping Markdown code fence, and returns that resulting text.
- `RouteOutcome.ActualModel` identifies the serving model, including fallback. A configured role, planned model, provider response label, or preview fallback is not a substitute.
- `agent/dispatch.go` caps tool content before observation inspection. Interceptor tags can subsequently append text; a block replaces the result.
- Contrary to the handoff's flattening description, the merged #430 implementation at this base preserves content verbatim inside a fresh fence. `agent/orchestrator.go` frames a value copy while constructing provider messages. The signed original still must be independent of capping, tagging, and later context assembly.
- Neither `ToolResult` nor `ToolCallRecord` has a suitable provenance carrier. `toolObservation` does not copy arbitrary metadata into provider messages.
- Content-full traces serialize `agent.Result` directly. Golem conversation persistence stores selected message fields and does not retain result-record metadata.

## Envelope contract

The schema lives in `agent/tools/delegate_proposal.go`; the generic agent package transports opaque JSON and does not import `agent/tools`.

The exported `DelegateProposal` is a plain struct with four JSON members:

| Member | Meaning |
|---|---|
| `domain` | Exactly `go-llm/delegate-proposal/v1`. Consumers check this against their fixed expected domain. |
| `body` | A `DelegateProposalBody`; the complete value is canonicalized and signed. |
| `content` | Exact final proposal text, retained before observation transformations. Its bytes are authenticated through `body.content_sha256`. |
| `signature` | Existing `signing.Signature`, preserving its `alg`, `kid`, and strict-base64 `sig` representation. |

The signed body contains exactly:

| JSON member | Type and definition |
|---|---|
| `content_form` | String `delegate-result/v1`: existing outer whitespace trimming and Markdown-wrapper stripping have run; dispatch capping, interceptor annotation, prompt framing, and context assembly have not. |
| `content_sha256` | Lowercase 64-character hex SHA-256 of the UTF-8 bytes of `content`. |
| `prompt_sha256` | Lowercase 64-character hex SHA-256 of the exact decoded prompt sent to the specialist, including its original whitespace. This is a prompt digest, not a hash of downstream router options or the full provider request. |
| `model` | Value copy of `RouteOutcome.ActualModel`, using `provider.ModelKey`. Freeze its existing JSON member spellings `Model` and `Provider` with literal golden bytes; no provider-type edit or duplicate wire struct. |
| `timestamp` | `time.Time` captured after successful generation as `completionTime.UTC().Truncate(time.Second)`; JSON uses whole-second RFC3339 ending in `Z`, with no fractional component. |

Whole-second precision is sufficient for this completion event and removes an unnecessary subsecond representation choice. Go's UTC conversion already removes a monotonic reading, and JSON never serializes that reading; monotonic time was not a defect in the earlier draft. This precision rule does not permit storage systems to rewrite signed timestamp strings. Readers preserve the signed representation, including the `Z` spelling, and never round or normalize received evidence before verification.

The canonical model fixture is exactly `{"Model":"coder","Provider":"local"}`. The whole-body golden includes those literal keys and must fail if a provider refactor changes their case, tags, or field set. Such a failure requires preserving this v1 schema or explicitly versioning the consumer contract; updating the fixture to match the refactor is not a compatible fix.

Use `signing.MarshalCanonical` on the whole body and sign those bytes under the fixed domain. Store the signature beside the body. Do not create a second mirror struct for the signed fields. The outer envelope uses ordinary JSON serialization; only the compact body has a cryptographic canonical-byte contract. Neither large content nor the prompt is passed through canonical JSON expansion.

Validate nonempty, valid UTF-8 model provider/name before signing. To keep the canonical body bounded, their combined byte length may not exceed 1,024 bytes; this bounds their JSON-escaped representation while leaving ample room for configured model identifiers. This is a fixed v1 input limit, not a new setting. Validate the retained content's UTF-8 before hashing or JSON encoding; otherwise JSON replacement of invalid bytes would break the binding. The existing decoded prompt is hashed exactly as supplied to `Chat`.

The complete envelope is larger than the proposal: JSON escaping and metadata add overhead. The existing 256 KiB proposal ceiling is not an envelope-size claim.

## Construction and emission

- Preserve `NewDelegateCode(caller, opts...) *DelegateCode` and `WithStream`.
- Add `WithProposalSigning(signer, verifier)` using the existing sibling signing interfaces. Both are required when this option is supplied. Nil/uninitialized identities and mismatched key IDs or algorithms are configuration errors. There is no explicit option for unsigned successful proposals.
- Apply options, then use `signing.GenerateHMAC(nil)` if no pair was supplied. Hold the generated signer by pointer and retain its verifier capability. No global state, filesystem writes, environment lookup, per-call key generation, or new dependency.
- Because the constructor currently cannot return an error, retain initialization/configuration failures on the tool. `Invoke` reports a model-visible error and makes no model call when initialization failed. Do not replace failed injected configuration with a fresh default key.
- Expose `ProposalVerifier() signing.Verifier` for the trusted owner of the tool. It returns the configured verifier for a usable instance, or nil when initialization failed. This lets the owner verify default ephemeral signatures without exporting key bytes. HMAC's verifier capability still carries symmetric authority.
- On successful model completion, apply existing content normalization. Reject empty content, invalid UTF-8, content longer than `mutateMaxBytes` (currently 262,144 bytes), and absent/incomplete/invalid actual model identity. Oversize output becomes an error instead of a silently truncated successful proposal. Nil `RouteOutcome` or a blank/whitespace-only actual provider/model returns exactly `delegate failed: missing model routing identity`. Invalid UTF-8 or the model-size limit returns exactly `delegate failed: invalid model routing identity`. Both have nil provenance and a nil Go error, following the tool's model-visible error convention.
- Build the body, canonicalize, sign, then call `VerifyDelegateProposal` with the complete constructed proposal and exact original prompt before serializing the envelope. This checks sibling content and prompt binding as well as the signature. Any initialization, canonicalization, signing, verification, or serialization error returns `IsError: true`, no proposal envelope, and no successful proposal text. Preserve the current model-visible error convention rather than introducing new Go-error behavior for ordinary tool failures.
- Return the unchanged normalized proposal in `Content`, existing preview and route outcome, and the serialized envelope in `Provenance`. Streaming remains display-only and precedes completed-proposal verification; token callbacks are not authenticated proposals.

## Transport and ownership

Add `Provenance json.RawMessage` with JSON tag `Provenance,omitempty` to `agent.ToolResult` and `agent.ToolCallRecord`. Nil/empty omission preserves old JSON for other tools. These are generic opaque carriers; this lane introduces only the delegate schema.

In `recordResult`, freeze the tool's envelope into an owned byte copy before callbacks. After the observation passes ingress inspection, attach that immutable copy to the result record. Give `OnToolResult` an independent clone. The retained record must not alias mutable tool or observer buffers. No changes to `appendToolCallRecord` signatures or parallel-dispatch call sites are needed.

| Runtime outcome | Envelope retention |
|---|---|
| Observation allowed or tagged | Preserve original envelope in the record and observer event. Tags affect presentation content only. |
| Observation blocked or interceptor-aborted | Recorded `Provenance` is nil; a synthetic blocked observer result also has nil `Provenance`, and interceptor abort emits no result observer callback. Do not retain the rejected raw content through metadata. Existing route/error bookkeeping remains. |
| Parent cancellation detected before recording; discarded trailing parallel results | No envelope is promised; preserve existing audit bookkeeping. |
| Observer returns an error after ingress acceptance | The already-appended record retains the envelope, even though the run aborts before adding that observation to State. This follows current record-before-observer ordering. |

`Result.ToolCalls` becomes the retained evidence location. There is no extra copy in model history, no metadata in provider requests, and no framing/interceptor policy change. A future verifier must use the retained original rather than attempting to strip tags or fences from presented text.

## Shared verification contract

Export `VerifyDelegateProposal(ctx context.Context, v DelegateProposalVerifier, p *DelegateProposal, expectedPrompt string) error` beside the schema. `DelegateProposalVerifier` is a consumer-owned interface containing only the existing `Verify(context.Context, string, []byte, signing.Signature) error` method. Both a `signing.Verifier` and a `*signing.Keyring` satisfy it; `Keyring` does not satisfy the larger `signing.Verifier` interface because it has no single key ID or algorithm. Keep configuration and `ProposalVerifier()` on their original `signing.Verifier` contracts, where identity matching is required. No changes to `signing/` or keyring adapter are needed.

The helper performs no direct I/O, clock reads, model calls, or writes to its arguments; the configured verifier retains its own documented side-effect and cancellation contract. The existing in-memory backends make this a pure local verification operation. It:

1. Rejects nil context/proposal/verifier and an already-canceled context. Callers supply a valid trusted verifier implementation; zero/uninitialized built-in keys must fail through verification.
2. Checks the fixed envelope domain and supported content form, nonempty valid UTF-8 content within the existing content limit, and valid nonblank model identity within the 1,024-byte limit. Requires a nonzero JSON-serializable timestamp with zero UTC offset and zero nanoseconds. It rejects a non-UTC or subsecond value rather than converting it.
3. Checks the independently supplied expected prompt is valid UTF-8 and not whitespace-only, hashes its exact bytes, and compares the canonical lowercase hex digest with `prompt_sha256`. It also recomputes `content_sha256` from the retained content and compares exactly. There is no option to skip either binding.
4. Canonicalizes the complete supplied body and calls the trusted verifier under the fixed domain constant. Errors remain errors; wrapping preserves underlying context, canonicalization, and verification errors for `errors.Is`/`errors.As` without introducing a new error taxonomy.

The helper does not trim content/prompt, strip fences, change case, round timestamps, or repair an input before verification. Creation owns normalization. The caller supplies trusted key material independently; neither an envelope nor a public key embedded in a trace can establish its own trust. The helper validates model attribution structurally and cryptographically, but matching it against an independently expected route remains a caller policy.

This helper authenticates a **typed value**, not the original bytes of arbitrary serialized input. Rejecting unknown JSON fields alone is insufficient: decoding can also discard field-name case, duplicate members, and timestamp spelling or replace malformed Unicode. A future untrusted reader must validate bounded envelope syntax/UTF-8 and supported exact schema, reject duplicate/unknown members and trailing data, retain the compact raw body, and require its canonical bytes to equal `MarshalCanonical` of the decoded body before calling the typed helper. That comparison prevents lossy decoding from changing what is being authenticated. Canonicalize only the bounded body, not the large outer envelope. Implementing that raw JSON reader is separate audit/ingestion work, not an implied capability of this helper. Trusted emission round-trips in this lane's tests are explicitly different from an untrusted reader.

## Recovering the expected prompt for audit

`ToolCallRecord` contains neither arguments nor a tool-call ID. An offline caller cannot complete prompt binding from that record alone. The package documentation will state this path for existing content-full traces:

1. Find the retained `StepRecord` whose `Index` equals the proposal record's `Step`, and read `Response.ToolCalls`. Prefer this durable per-step response over `Result.Messages`, which may be compacted.
2. A real `delegate_code` is `Read | Network`, so any batch containing it dispatches serially. Within that step, use the record's position among **all** recorded calls, including denied/synthetic calls, to select the corresponding position in the original ordered tool-call list. Accepted records are a serial prefix. Verify the selected function is `delegate_code`; matching by name alone is ambiguous when a step invokes it more than once.
3. Decode that selected call's `Function.Arguments` using the same prompt-field semantics as `Invoke` and supply its exact decoded `prompt` string to `VerifyDelegateProposal`. Never trim it, use the envelope's digest as the expected prompt, or search for a digest match to hide an ambiguous association.
4. If the step/arguments/order evidence is missing, malformed, or ambiguous, report that full prompt binding is unavailable and do not report full proposal verification. No new argument duplication or trace lookup API is introduced here. The trace must itself come from the audit caller's trusted evidence source; a per-proposal signature does not authenticate the enclosing trace or prove a run/call identity.

Tests will pin multiple delegate calls in one step with distinct prompts and an earlier synthetic result. The audit-facing package comment and approved spec will carry these instructions for #447's future audit guide. There is no existing proposal audit CLI or guide being implemented in this lane.

## Verification limits

Prompt binding lets a verifier reject reuse against a different prompt. It does not prevent replay of an earlier proposal for the same prompt; timestamp is evidence, not an expiry or uniqueness policy. There is no nonce registry, sequence ledger, session identity, or replay cache in this lane.

This PR emits evidence and supplies its typed verification helper. It does not prevent memory modification, prove model truthfulness, authorize writes, bind parent edits to the original proposal, or enforce verification when the parent chooses a write tool. Downstream audit/contract work (#447/#451) must define those checks separately. Persisted default-HMAC envelopes cannot be verified after key loss; durable identity wiring is also outside this lane.

## Approved file scope

| Surface | Change |
|---|---|
| `agent/tools/delegate.go` and tests | Signing configuration, default key lifecycle, final proposal validation/emission, compatibility tests. |
| `agent/tools/delegate_proposal.go` and tests | Plain envelope/body types, domain/content-form constants, bounded creation helper, pure typed verification helper/interface, canonical-byte contract, and audit-facing package comments. |
| `agent/tool.go` | Optional raw JSON provenance field. **Shared scope expansion.** |
| `agent/types.go` | Optional raw JSON provenance on `ToolCallRecord`. **Shared scope expansion.** |
| `agent/dispatch.go` and focused tests | Owned copies, accepted-record propagation, block/abort behavior. **Shared scope expansion.** |
| `agent/observer.go` | Update result ownership documentation. **Shared scope expansion; comment only.** |
| `internal/agenttrace` tests | Verify existing trace serialization carries accepted provenance; no production writer change expected. |
| `changelog.d/450-delegate-proposals.md` | Feature fragment with key lifetime and apply-time scope stated accurately. |

`signing/`, `cmd/golem`, `agent/orchestrator.go`, interceptor production code, provider wire types, conversation persistence, and parallel dispatch production code stay outside the implementation scope. The dispatch addition is an explicit exception to the handoff's isolated lane boundary and was approved together with this spec.

## Execution compatibility correction

The approved `json.RawMessage` additions make both `ToolResult` and `ToolCallRecord` non-comparable in Go. Callers using whole-struct `==` or `!=` must compare fields or use an appropriate deep comparison. JSON with nil/empty provenance retains its old shape.

During execution, exactly three existing comparisons in `agent/interceptor_test.go` failed to compile for this reason. The controller permitted mechanical replacements with the already-imported `reflect.DeepEqual`, preserving their assertions. This corrects an overlooked test dependency in the original file scope; interceptor production code and behavior are unchanged.

## Gemini feedback disposition

| Recommendation | Resolution |
|---|---|
| Whole-second timestamp | Adopted as an emission/schema rule, with producer normalization and verifier rejection tests. Corrected the monotonic-clock rationale and retained exact signed-representation requirements. |
| Export a complete verifier | Adopted; emission and consumer tests call it. A narrow interface supports both individual keys and the existing keyring. Raw JSON ingestion remains explicitly separate. |
| Pin model JSON casing | Adopted through exact literal model/whole-body goldens. No extra model wrapper or provider edit. |
| Document offline prompt recovery | Added the existing step/order/argument path, ambiguity handling, and trust limits; no arguments added to records. |
| Exact identity errors; block and copy tests | Made exact strings, nil metadata expectations, and mutation-detecting ownership checks explicit. |
| Content-addressed application and proposal cache | Deferred to a separately scoped application feature. It would change write-tool APIs, retention, approval, and what the parent reviews; it is not necessary for #450. Any future identifier must bind the intended envelope/request and cannot confer authority merely by matching a content hash. |
| Persistent Golem Ed25519 keys | Deferred to explicit CLI/key-lifecycle work; the injected pair supports it already. Bootstrap must use a trusted key location and independently approved trust mapping. An embedded public key is evidence to compare with trust configuration, not a root of trust. |
| Nonce/run ID for replay prevention | Not added. A fresh nonce makes envelopes distinct but does not reject replay of an intact old envelope; prevention requires an independently expected run/challenge or retained consumption state. Current same-prompt replay limits remain explicit. |
| Direct/derived application lineage | Deferred with apply-time integration. A valid content match does not make a write auto-approvable; existing path/scope/approval policy still applies. |
| Immediate approval recommendation | Recorded as external review advice only. Keith subsequently authorized execution of this revised spec and plan. |

## Completion criteria

Every successful delegate result passes the shared typed verifier, whose checks bind the exact retained proposal, expected prompt, actual model, and whole-second UTC timestamp under the fixed v1 schema. Failed calls never emit signed-success metadata. Accepted evidence survives observer mutation attempts, tagging, fencing, and result/trace serialization. Existing content, streaming, tool-effect, approval, and no-tools subrequest behavior is preserved except for the explicit provenance/size failures above. The TDD plan's mutation checks, lint, full CI, review cycles, and changelog gate pass before a PR is prepared.

## Post-implementation Gemini review disposition

| Finding | Verified disposition |
|---|---|
| CR-1 — Struct comparability | The `json.RawMessage` compatibility break is already documented above and in the public changelog. Retain the approved carrier and field/deep-comparison guidance. |
| CR-2 — Configuration errors | Retaining initialization errors and making no model call is the approved constructor contract. Invalid-pair diagnostics are fixed messages containing no key material. Do not introduce a panic or another constructor. |
| CR-3 — Prompt hashing memory | Direct string-to-byte conversion can allocate a prompt-sized copy. Use one shared SHA-256 helper with a fixed 4,096-byte buffer for content and prompt hashing in both creation and verification. This bounds extra hashing memory while preserving signed bytes and accepted prompt sizes; it does not bound the caller's input allocation or add a prompt quota. |
| SR-1 — Ephemeral identity | Key loss prevents later verification, as already documented. Caller-managed persistent HMAC or Ed25519 identities remain the supported durable-key seam. |
| SR-2 — No apply gate | Evidence emission and retention do not authorize or enforce filesystem writes. Apply-time policy remains outside this change. |
| SR-3 — Audit indexing | The current real delegate's combined read and network effects force serial dispatch. Existing tests cover the ordered prefix with an earlier synthetic call and multiple delegates, including rejection of swapped prompt associations. No extra record identifier is required for this path. |
| AR-1 — Verifier method set | The default HMAC verifier also implements signing; this is the approved trusted-owner capability, not an untrusted privilege boundary. Put that warning directly on `ProposalVerifier` as well as the verifier interface. |
| AR-2 — Replay | Same-prompt replay is a stated limitation. A nonce alone cannot prevent it; no session state or replay policy is introduced. |
| AR-3 — JSON ingestion | The tests decode trusted emission round-trips, not arbitrary untrusted envelopes. Label that boundary at the test decoding sites; a raw JSON reader remains separate work. |
| AR-4 — Argument decoding | The signature binds the exact decoded prompt sent to the specialist. A different audit interpretation fails that binding. Pin existing duplicate/case-insensitive prompt-field semantics with a benign regression; do not change argument decoding. |
| AR-5 — Timestamp freshness | Timestamp validation enforces the signed schema, not freshness. State the freshness, replay, and write-authorization limits directly on `VerifyDelegateProposal`. |
