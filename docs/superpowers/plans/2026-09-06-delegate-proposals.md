# Delegate proposal provenance implementation plan

> For agentic workers: Keith approved the spec and plan on 2026-09-06; use `superpowers:subagent-driven-development` or `superpowers:executing-plans` task by task. Execution uses the isolated feat/450-delegate-proposals worktree.

Revision: incorporates Gemini's verifier, timestamp, schema, audit-context, and explicit error/ownership feedback. Keith authorized execution on 2026-09-06; Golem application features remain outside this lane.

**Goal:** Emit and retain authenticated delegate proposal envelopes for #450 without changing provider-visible proposal presentation or adding an apply-time gate.

**Architecture:** Reuse the signing package, with one ephemeral HMAC identity per tool and an optional externally supplied signer/verifier pair. A shared pure typed verifier checks the complete proposal and prompt binding; emission uses that helper before publishing. Carry a plain envelope as opaque JSON through `ToolResult` and accepted `ToolCallRecord` values, retaining the original content outside its compact signed body.

**Tech stack:** Existing Go 1.25 module, Go standard library, existing `signing` package; no dependencies added.

**Spec:** [Approved spec](../specs/2026-09-06-delegate-proposals.md). Approval includes the four shared production-file changes and dispatch exception to the handoff boundary.

## Global constraints

- Spec and TDD plan approved before implementation. This draft intentionally contains no production/test implementation code.
- Linked worktree from freshly checked `origin/develop`, at `b97613b` or later. Proposed branch: `feat/450-delegate-proposals`; never commit on `develop`, never prefix the branch with `codex/`.
- Start every worktree command with `cd <worktree> &&`. Use `rtk` for invoked shell tools; use `rtk proxy` where raw output is needed. Every Go invocation uses `env -u GOROOT go ...` behind that wrapper.
- No edits to `signing/`, `cmd/golem`, `agent/orchestrator.go`, interceptor production code, provider wire types, conversation persistence, or parallel dispatch production code. Execution required exactly three mechanical `ToolCallRecord` comparison updates in `agent/interceptor_test.go`; see the spec’s execution compatibility correction.
- Content is limited by existing `mutateMaxBytes` (262,144 bytes); model provider/name combined have a 1,024-byte v1 limit. No new runtime settings or dependencies.
- Domain `go-llm/delegate-proposal/v1`; content form `delegate-result/v1`; body JSON contains `content_form`, `content_sha256`, `model`, `prompt_sha256`, `timestamp`; `model` uses existing `Model`/`Provider` spellings.
- Creation uses `completionTime.UTC().Truncate(time.Second)`; verification rejects nonzero UTC offsets and subsecond timestamps and never repairs evidence. Whole-second `Z` timestamps and model JSON casing are frozen by literal fixtures.
- `VerifyDelegateProposal` is the common semantic verifier. Its narrow `DelegateProposalVerifier` interface accepts an existing single-key verifier or keyring. Keep raw untrusted JSON ingestion, replay enforcement, proposal caches/application tools, and persistent CLI key setup outside this lane.
- Every new behavior starts with a failing check. Expected canonical bytes and cryptographic fixtures must be literals independently obtained, not computed by the helper under test. Prove each assertion detects its identified implementation mutation.
- After each task: `/code-review` fix/review cycles until clean, followed by `/criticize-review`; resolve findings and recheck affected tests before proceeding.
- Add only `changelog.d/450-delegate-proposals.md`; never edit `CHANGELOG.md`. No emojis in PR material.

## Preparation

- [x] Read the worktree and implementation/review skill instructions and any newly applicable repository guidance.
- [x] Fetch `origin/develop`, inspect changes since the spec base on owned/shared surfaces, and reconcile the approved design if a new transport seam has landed. A material contract change returns for review.
- [x] Confirm `.worktrees` is ignored and the proposed path/branch are unused. Create `.worktrees/450-delegate-proposals` on `feat/450-delegate-proposals` from `origin/develop` using the repository's worktree workflow. Leave existing untracked `copilot.json` and `output/` alone.
- [x] Record the actual base SHA and copy the approved spec/plan into the worktree's `docs/superpowers/specs/` and `docs/superpowers/plans/` using the approval date. Update the plan's spec link to the copied spec's actual location. Commit documentation only on the feature branch.
- [x] Run the existing focused baseline: `env -u GOROOT go test ./agent/tools ./agent ./signing ./internal/agenttrace`. Capture its real exit status. Diagnose pre-existing failures separately before attributing failures to #450.

For the proposed worktree, every verification command below expands to `cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/450-delegate-proposals && rtk proxy <command>`.

## Task 1 — define the signed proposal contract

**Files:** new `agent/tools/delegate_proposal.go`, new `agent/tools/delegate_proposal_test.go`.

**Consumes:** `signing.Signer`, `signing.Verifier`, `signing.Signature`, `signing.MarshalCanonical`, `provider.ModelKey`, and existing `mutateMaxBytes`.

**Produces:** exported `DelegateProposal` and `DelegateProposalBody`, fixed domain/content-form constants, one unexported creation helper accepting context/signer/verifier/content/prompt/model/completion time and returning a proposal or error, plus `VerifyDelegateProposal(ctx context.Context, v DelegateProposalVerifier, p *DelegateProposal, expectedPrompt string) error`. The new consumer-owned interface contains only `Verify(context.Context, string, []byte, signing.Signature) error`, so single-key verifiers and `*signing.Keyring` both satisfy it. No new cryptographic backend, JSON ingestion API, or model call.

- [x] Write the initial golden test using normalized content `abc`, decoded prompt `x`, actual model `local/coder`, and completion input `2026-09-05T08:00:00.987654321-04:00`. Pin the complete canonical body with exact output timestamp `2026-09-05T12:00:00Z` and nested model bytes `{"Model":"coder","Provider":"local"}`. Content SHA-256 is `ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad`; prompt SHA-256 is `2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881`. The expected field names must be literal, never extracted from `provider.ModelKey` or generated from the body under test.
- [x] Use a clearly named fixed test-only HMAC key. Obtain the expected key ID/signature independently of production helpers, following #444's documented framing, then store literal expectations. Retain direct existing-backend verification of that literal contract as an independent oracle; add full-proposal checks through `VerifyDelegateProposal` after trusted JSON round-trip. Do not duplicate primitive implementations in production, use the new creator/verifier pair as its own only oracle, or add dependencies.
- [x] Run `env -u GOROOT go test ./agent/tools -run 'TestDelegateProposal' -count=1` and observe the missing contract failure. Once names compile, require the assertions to fail on absent/wrong behavior rather than accepting a build error as final red evidence.
- [x] Implement the smallest plain types and creation helper: validate content/model, construct the single body with whole-second UTC time, canonicalize, sign, and return only a proposal that passes the shared verifier. Pass time into creation so tests need no public clock option or mutable global. Add the verifier's public checks through separate red/green cases: fixed domain/form, schema/limits, exact content and expected-prompt digests, whole-body canonicalization, and trusted verification. Do not normalize inputs inside verification.
- [x] Add one table of focused creation failures, one red/green cycle at a time: empty/invalid UTF-8/oversize content; empty/partial/invalid/oversize model identity; canceled context; signer error; verifier error; zero timestamp; an unmarshalable timestamp outside JSON's supported year range. Errors never yield a usable proposal. Check exact-limit content/model identity succeed; one byte over fails. Pin truncation rather than rounding with the fractional/non-UTC golden input.
- [x] Call the public helper for HMAC, Ed25519, and a populated purpose-scoped keyring. Table-test nil context/proposal/verifier, canceled context, zero/uninitialized or wrong/unknown keys, and wrapped verifier errors. Verify input values and signature bytes remain unchanged after both success and failure.
- [x] Table-test modified retained `content` with its body/signature unchanged, mismatched expected prompt (including whitespace-only changes), wrong envelope domain, modified signed form/digests/provider/model/timestamp, and altered signature bytes/key ID/algorithm. Every complete-proposal failure must be asserted on `VerifyDelegateProposal`, not a test-only digest check.
- [x] Include cryptographically valid but schema-invalid fixtures: unsupported form, nonzero timezone offset, subsecond timestamp, zero timestamp, and invalid content/model bounds. Sign those fixture bodies directly with the existing backend so tests prove the public helper rejects schema violations even when the signature itself is valid. A validly signed proposal for a different prompt must likewise fail the expected-prompt check. Do not normalize altered fixtures before passing them to the helper.
- [x] Document that the helper verifies typed values. Future raw readers must validate exact schema/duplicates/Unicode, retain the compact raw body, and compare its canonical bytes with the decoded-body canonical bytes before this helper. Unknown-field rejection alone does not establish that equivalence. There is no raw JSON decoder to implement or imply through trusted round-trip tests in this task.
- [x] Prove the fixture catches removal/substitution of each signed field, changed provider JSON casing, skipped UTC conversion/truncation, verifier-side normalization, omitted content/prompt/schema checks, skipped error handling, and wrong limit comparisons. Restore every temporary mutation and rerun the focused suite.
- [x] Complete review/criticize-review cycles and commit the contract plus its tests on the feature branch.

## Task 2 — sign every successful delegate output

**Files:** `agent/tools/delegate.go`, its existing tests, `agent/tool.go` for the optional `Provenance` carrier.

**Consumes:** Task 1's creation helper and existing delegate option/caller seams.

**Produces:** `WithProposalSigning` accepting a matching `signing.Signer` and `signing.Verifier`; `DelegateCode.ProposalVerifier()` returning `signing.Verifier`; default per-tool HMAC creation; initialized error state; successful `ToolResult.Provenance` JSON.

- [x] Extend the existing success fixture so planned and actual models differ. Assert exact normalized `Content`, expected actual identity in the signed body, preserved preview/route outcome, and valid metadata. Observe red before emission is implemented.
- [x] Add the nil/empty-omitted `ToolResult.Provenance` field and minimal option/state wiring. Preserve the constructor signature. Generate a default HMAC only when no pair was supplied. Report configuration/initialization failure before invoking the fake caller; never silently fall back from a bad explicit pair.
- [x] Route the existing successful post-normalization return through creation and its `VerifyDelegateProposal` self-check with the exact original prompt, then serialize the envelope. Return only an error result on any failure. Validate identity from `ActualModel`, never `PlannedModel` or preview text.
- [x] Test same-instance key ID stability across calls and different-instance key IDs; verify both outputs using the tool owner's verifier. Test explicit HMAC/Ed25519 configuration, missing pair members, mismatched key/algorithm, zero-value keys, and retained initialization failure. Assert failed initialization prevents `Chat` and failed emission has no provenance.
- [x] Update fixtures that previously succeeded without routing identity. Nil outcome and each blank/whitespace-only provider/model case must return exactly `delegate failed: missing model routing identity`, `IsError: true`, nil `Provenance`, and nil Go error. Invalid UTF-8 or oversize model identity must return exactly `delegate failed: invalid model routing identity` with the same error flags. Preserve direct preview fallback behavior where relevant without using it to invent signed identity.
- [x] Pin normalization with a wrapped `abc` response, exact prompt whitespace, and preserved internal marker-shaped text. Cover 262,144-byte success and 262,145-byte failure, empty fenced output, invalid UTF-8 output, caller errors, and empty prompts. Streaming still displays tokens but does not affect final authenticated bytes.
- [x] Preserve no tools in the subrequest; `Read | Network`, timeout, approval policy, output cap, and `OriginModel`; no `PlanningTool` behavior. Assert exact expected fields rather than only substring matches where #450 changes the contract.
- [x] Run `env -u GOROOT go test ./agent/tools ./cmd/golem -count=1`, then `env -u GOROOT go test -race ./agent/tools -run 'TestDelegate' -count=1`. Mutate the relevant wiring/guards to prove the assertions fail, restore, review until clean, criticize-review, and commit.

## Task 3 — retain accepted evidence without changing model input

**Files:** `agent/types.go`, `agent/dispatch.go`, `agent/observer.go` comment; focused tests in `agent/dispatch_test.go`, `agent/tool_result_observer_test.go`, a new `agent/tools/delegate_proposal_run_test.go` if needed for real-tool end-to-end coverage, and existing `internal/agenttrace` test files.

**Consumes:** `ToolResult.Provenance`, serialized Task 1 envelopes, existing `recordResult`, observer, and trace writer seams.

**Produces:** optional `ToolCallRecord.Provenance`, owned-copy propagation after ingress acceptance, independent observer copies, documented suppression/retention rules. Existing append helper and parallel dispatch production signatures stay unchanged.

- [x] Write a small runtime regression showing a completed delegate envelope is absent from `Result.ToolCalls` today. Use a real delegate tool with fake model callers in package `agent/tools`; keep generic dispatch/observer tests in `agent` to avoid an import cycle. Observe red.
- [x] Add the record field. Freeze metadata before callbacks, attach it to the accepted record after observation inspection, and independently clone it for the observer. Leave synthetic block/abort replacements without raw proposal metadata. Update the observer ownership comment.
- [x] Prove the tool-owned and observer-owned byte buffers cannot modify the recorded envelope. In `OnToolResult`, alter bytes in the callback's `Result.Provenance`, then assert exact retained bytes and successful shared verification after the callback. Separately alter the original tool-owned buffer. Removing either copy must make its assertion fail.
- [x] Table-test allow/tag retention, block/interceptor-abort suppression, cancellation before recording, discarded trailing results, and observer-error retention after ingress acceptance. For blocked/aborted records assert `Provenance == nil`, not only zero length; blocked observer events also have nil provenance and interceptor abort emits no result callback. Keep existing route/error bookkeeping assertions. A forced content cap on a generic result changes presentation but leaves original envelope bytes intact; the real delegate already rejects oversize successes.
- [x] Drive a tagged real proposal through the parent runtime with both legacy and mixed assembly. Pass retained proposals and their independently known prompts to `VerifyDelegateProposal`; the captured provider message must have the ordinary fenced presentation and no separate provenance field. Confirm internal marker-shaped content remains data and does not change the signed original. Reuse existing fence assertions rather than adding a new fence parser.
- [x] Marshal/unmarshal `Result` and use the existing content-full trace writer to confirm the envelope survives. Ordinary tools retain their previous JSON because provenance is omitted. Confirm content-light telemetry does not newly contain raw proposal content. This is not a conversation DB persistence test.
- [x] Add one trace regression with an initial unknown-tool synthetic result followed by two delegate calls with distinct prompts in the same step. Locate each original request through `StepRecord.Index == ToolCallRecord.Step` and its position among all calls in that serial step, decode the selected original `Function.Arguments`, and pass its exact prompt to the public verifier. Swapping the two expected prompts must fail. Assert the retained `StepRecord` preserves all ordered calls/arguments independently of the final model-message view. This test pins existing evidence needed by the documented future audit path; do not implement a general trace lookup API or infer association by a matching digest.
- [x] Add the prompt lookup, missing/ambiguous evidence handling, trace-trust caveat, and raw-reader prerequisites to the new package API documentation and approved spec. The future #447 audit guide should reuse this contract. Missing expected-prompt evidence cannot be reported as full verification; no prompt-less helper mode is added.
- [x] Run `env -u GOROOT go test ./agent ./agent/tools ./internal/agenttrace -count=1`, then `env -u GOROOT go test -race ./agent ./agent/tools ./internal/agenttrace`. Mutate record assignment, block suppression, and clone operations in turn; verify their specific tests fail and restore them.
- [x] Add `changelog.d/450-delegate-proposals.md`, stating default key lifetime, shared typed verification, structured retained evidence, explicit new error cases, and the absence of an apply-time gate. Complete code-review and criticize-review cycles and commit.

## Final gates and PR preparation

- [x] Reconcile the approved spec with the actual diff and confirm no unapproved shared surfaces changed. Check each Gemini disposition against the implemented scope: verifier/timestamp/schema/audit documentation included; proposal cache, new apply APIs, persistent CLI keys, nonce state, and edited-proposal lineage excluded. Confirm no mutation remains and no key material is printed or committed outside clearly identified test fixtures.
- [x] Run formatting on changed Go files and the required lint command: `golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`.
- [x] Run the required full gate: `docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`. Save output as needed and capture the command's exit code directly; no `go test | tail` or pipeline that masks failure.
- [x] Resolve failures, rerun affected checks, and repeat the full gate only when fixes require it. Run final code-review cycles until clean and criticize-review; carry any material limitation into the PR description.
- [x] Prepare the PR against `develop` with the concrete behavior, evidence of passing gates/mutation checks, four shared-file scope additions, and key lifetime/verification-at-apply limits. Do not describe the change as preventing unauthorized application or as enabling offline verification with a lost ephemeral key. Do not close #450 or publish implementation before the user's approval gate has been satisfied.

## Approval record

Keith approved the spec and TDD plan on 2026-09-06, including the additive shared transport changes in `agent/tool.go`, `agent/types.go`, `agent/dispatch.go`, and the ownership comment in `agent/observer.go`. Execution is authorized in an isolated git worktree.

## Execution result — 2026-09-06

Implemented on `feat/450-delegate-proposals` from `b97613be5eca70205fb92b0cf114236f36e37fd4`. The approved documentation was committed first (`b817d08`), followed by the proposal contract (`ce3532c`), delegate signing (`278fe76c`), and runtime retention (`c0cff4b`).

Each task passed independent spec/code-quality and skeptical review; the final whole-branch review also found no actionable defects. Reported behavioral RED checks and restored mutations cover the contract, binding, limits, initialization, metadata retention, rejection, and ownership. Required host lint with unlimited issue flags and full Docker CI both exited 0. Docker CI passed formatting, lint, all-package race tests, and compile smoke. Scoped package/race/vet/format and changelog checks also passed.

The sole execution scope correction was three mechanical comparisons in `agent/interceptor_test.go`, required by the new non-comparable public structs; see the specification's compatibility note. No prohibited production surface changed. The PR description is prepared for `develop`; the branch and isolated worktree are preserved for review. No push or merge was performed.
