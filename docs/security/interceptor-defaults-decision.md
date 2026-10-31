# Interceptor defaults decision (#517)

Recorded 2026-10-26. **The detectors and Secrets both stay opt-in.**

Golem installs the guards (invariants, egress, child scope denials) in every session. `-interceptors` adds the detectors (zero-width, encoding, typoglycemia), Secrets and the canary. This record states why neither moves into the default and what shows the flag's meaning is unchanged. It links the evidence and repeats only the figures the decision rests on. "Spec" below is the #517 design (REV 2, approved 2026-10-24), the document all three linked reports cite as "spec".

Amended 2026-10-28 (#424): `-interceptors` also installs the exact `configured_secrets` guard first in that chain, and the same flag refuses request bodies that carry a configured credential before they are sent to non-loopback OpenAI-compatible providers. The sentences below that describe the flag as the detectors, Secrets and the canary predate both.

## Decision

- `-interceptors` keeps its default of false. Omitting it means `-interceptors=false`, which is the guards only.
- No new flag. `interceptor.Defaults()`, every interceptor's policy and the canary lifecycle are unchanged. The only code change is a trailing clause in the `-interceptors` usage text that points here.
- Detectors reopen only when #700's v2 corpus supplies benefit evidence and the MCP block budget passes in every stratum (it fails in both i18n strata), and then with a powered model capture. Secrets reopens only after #518 lands and its failed items 3 and 4 below pass.

## Decisions K1 to K5 (approved 2026-10-24, spec §2.1)

| ID | Decision |
|---|---|
| K1 | δ = 0.10 on the primary contrast. Task-failure budget: zero terminal blocks on benign or lookalike sentinel workflows, and zero `blocked-at-response` on benign tasks. Unnecessary-block budget: no more than 1 in 100 benign MCP observations blocked by the detectors, in every corpus stratum. |
| K2 | The detectors flip only with measured benefit evidence, which can come only from #700's v2 corpus. A defense-in-depth rationale alone does not count. |
| K3 | Weak-phrase demotion is a follow-up. #517 measures the policy as shipped. |
| K4 | A terminal block on a deliberate secret-shaped fixture disqualifies a default-on Secrets. In any case Secrets cannot flip before #518 (spec §8.4). |
| K5 | (b): no model capture in #517. The powered capture moves into the flip PR, after #700. |

## Detectors: retain opt-in

Evidence: [benign report](../llm/benchmarks/2026-10-26-interceptor-benign.md), section "Decision", and [v1 adversarial report](../llm/benchmarks/2026-10-25-interceptor-adversarial-v1.md).

- **Benefit gate (spec §7.3, K2): not met.** There is no measured benefit evidence until #700's v2 corpus exists. The v1 corpus cannot supply it: every one of its 88 attack cells was resisted by all five models under the guards alone.
- **Harm gate (spec §7.2), deterministic part: failed.** The unnecessary-block budget, at most 1 in 100 benign MCP observations blocked by guards+detectors, is not met in i18n-cldr-annotations (38/176), i18n-gemoji (1/3) and the pooled i18n stratum (39/179). The other strata pass: go-llm 14/1801, go-src 26/7787 and the pooled llm-app stratum 1/821. The zero-width rule is the only detector rule that fires in either i18n stratum.
- **Model capture: deferred (K5 = b, spec §7.4).** No model ran for either report. The registered benign corpus is ready to capture, and the flip PR starts from it.

## Secrets: retain opt-in

Evidence: [secrets evidence report](secrets-evidence.md), section "Gate evaluation (spec §5.6)", and the v1 adversarial report, section "Expected mapping (spec §6.6)".

| Item | Result | Reading |
|---|---|---|
| 1. Correct detection on every mandatory case | pass | 55 mandatory cases (165 outcomes), none missed |
| 2. Zero terminal blocks on benign and lookalike sentinel workflows | pass | rows 1, 7b and 7d. No sentinel row writes a lookalike, and 14 lookalike cases fire at tool-args/output, which is terminal in production, so the item measures the rows only |
| 3. False-block rate within 1 in 100 | **fail** | over budget in 22 of 42 stratum and carrier cells. Pooled per stratum: go-llm 124/10814 (1.15%), go-src 581/46684 (1.24%), llm-app-claude-cookbooks 91/1178 (7.72%), llm-app-openai-cookbook 254/3764 (6.75%); the two i18n strata 0 |
| 4. Fixture-workflow losses within K4 | **fail** | row 3 (3a and 3b) is terminal under guards+Secrets. Rows 2c and 11 are also terminal; row 6 loses its continuation without a terminal block |
| 5. Protection benefit on expected-sensitive surrogates | pass | rows 14a, 14b and 14c protected, none unprotected |
| 6. Spec §8.4 met | **fail** | #518 has not landed |

The v1 adversarial report adds one more reading: the spec's expected mapping fails on 5 of 105 cells, where Secrets blocks a hex-encoded payload as `sensitive_payment_card`. That is a payment-card lookalike, not detection of the payload. The expectation failed and is reported, not corrected.

## What `-interceptors` means (AC4)

- Omitting `-interceptors` equals `-interceptors=false`: the guards only.
- `-interceptors` (true) means the detectors, Secrets and the canary.
- The help gains one pointer clause to this record; its semantics, the startup notice, the orchestrator factories and the rebuild paths are unchanged.

The evidence, by test:

- `TestInterceptorsFor` (`cmd/golem/interceptors_test.go`) pins the chain composition against literal lists, each interceptor once: the guards only without the flag, and the detectors, Secrets, the guards and the canary with it.
- `TestStartupNotices_Interceptors` (same file) pins the startup notice for both chains.
- `TestNewDispatchTool_ChildrenInheritInterceptors` (same file) pins that a dispatch child's chain follows the parent's flag: with it the child's foreign result is replaced and scored, without it verbatim and unscored.
- `TestInterceptorChainConstructionPaths` (`cmd/golem/interceptor_paths_test.go`) drives each CLI construction path against loopback backends: the REPL, `-p`, a dispatch child, `/model set` on the parent and on dispatch, a recipe hint, an agentflow worker, and `/resume`, `/new` and `/clear`. Every probe turn reads `.env`, then a workspace file holding a strong injection phrase and a generated OpenAI-shaped value. In all three flag columns the always-on `credential_path` guard blocks the `.env` read, and the test asserts it. With no flag and with `-interceptors=false` the workspace file reaches the model verbatim, with no trailer and no Secrets block; with `-interceptors` it arrives as the Secrets block. Secrets is observed on every path. Typoglycemia is observed through the findings (REPL, `/model set`, recipe hint, `/resume`, `/new`, `/clear`) or through the risk score alone (`-p`, dispatch child, `/model set` on dispatch) on every path except the agentflow worker. The worker row is wire-only: it returns only an error, and a Secrets block suppresses the tags on the same observation (spec F6), so the wire shows no Typoglycemia trailer. With `-interceptors`, `/resume` and `/new` republish the runtime through `renewCanary` (`runtime.Replace`), and the test checks that the chain survives; without the flag there is no canary and no republish. `/clear` does not rebuild the runtime, and the test checks that the chain is preserved. The test does not observe the canary, ZeroWidth or Encoding on any path.

Out of scope, because the library owns them: the `golem` runtime bootstrap, which installs no interceptors; orchestrators a host supplies through `golem.Options.Orchestrator`; and callers of `agenttools.NewDispatch` outside the CLI. None of them reads the CLI flag.

## Headless (AC5)

Nothing changes in headless mode. The v1 serializers, event vocabulary, envelope rules and output are untouched, and #517 adds no fields or events to v1. A future detector flip must keep the serializers, vocabulary and envelope rules and pin the output of intercepted runs (spec §8.3). A Secrets flip is hard-blocked on #518's versioned, bounded block reporting and its explicit v1/v2 selection path for Firn, coordinated with #506, with no interactive-only default (spec §8.4).

## Acceptance criteria

| AC | Evidence |
|---|---|
| 1. Attach benign on/off and adversarial reports | The [benign report](../llm/benchmarks/2026-10-26-interceptor-benign.md) and the [v1 adversarial report](../llm/benchmarks/2026-10-25-interceptor-adversarial-v1.md). Both are deterministic under K5 = b. The powered AnswerQuality and restraint capture that the 2026-09-10 comment on #517 requires is a precondition of any future flip, not of keeping opt-in. |
| 2. Expected-sensitive vs benign, Secrets accuracy and workflow-blocking impact reported separately, no live secrets | The [secrets evidence report](secrets-evidence.md): accuracy by kind and carrier, the census and the workflow table. |
| 3. Change defaults only when the evidence supports it, otherwise retain opt-in and record why | This record. |
| 4. `-interceptors=false` and split-control semantics consistent with #575; help, notices, parent/child factories and rebuild paths agree; no duplicated interceptors; canary lifecycle unchanged | Semantics, chain composition (each interceptor once), notices, children and rebuild paths: the tests listed under "What `-interceptors` means". Help: `TestParseFlags_InterceptorsHelpNamesAlwaysOnGuards` (`cmd/golem/default_guards_startup_test.go`), which pins that the `-interceptors` help names the always-on guards and says risk display does not depend on the flag. Canary lifecycle: this PR changes no canary code, and the existing lifecycle pins pass on this branch: in `cmd/golem/canary_test.go`, `TestCanaryStartup` (no canary and no entropy use without the flag; a fresh binding per startup mode with it), `TestCanaryActivation` (`/new` and `/resume` rotate it), `TestCanaryRenewal` (renewal after a burn, failing closed when entropy fails), `TestCanaryPreservedAcrossRecomposition` (mount, Git refresh, `/think` and `/clear` leave it unchanged) and `TestCanaryFactoryDispatchInheritance` (the factory and dispatch children use the active binding); in `cmd/golem/canary_abort_test.go`, `TestCanaryDetectedTurnRenewsBeforeNextGoal` (a detected canary burns the activation and the next goal renews first). |
| 5. Headless schema and protocol compatibility explicit; coordinate versioned reporting with #518 and #506 | "Headless (AC5)" above. #518 and #506 gate a Secrets flip. |

## Follow-ups

Each is its own issue, not part of this decision.

- **Weak-phrase demotion (K3).** Weak phrases tag at every origin and account for most detector fires in the go-llm and llm-app strata of the benign report (73 of 85 files, 11 of 11 and 49 of 50). Demoting them is a policy change, so it needs re-registered arms and cannot reuse this evidence.
- **Payment-card rule hardening.** `payment_card` accounts for 122 of the 221 benign (file, rule) pairs in the Secrets census, where a file blocked under two rules counts under both (go-llm 8, go-src 114; 221 is the sum of the report's rule table). Issuer and BIN checks, context words and rejecting digit tables would target it; `secret_assignment` is the second source.
- **Credential-path coverage of exec output (B5).** `read_file` of a credential path is refused by the always-on guards; exec output carries no such filter. A row for the containment inventory under #581 or #420.

## Evidence

- [Secrets evidence report](secrets-evidence.md) (PR1, #714): accuracy, lookalikes, fixtures, census, workflow table, gate evaluation.
- [Detectors on benign work](../llm/benchmarks/2026-10-26-interceptor-benign.md) (PR2, #715): fire rates, MCP block budget per stratum, identity gate, registered power.
- [Interceptor arms on the v1 injection corpus](../llm/benchmarks/2026-10-25-interceptor-adversarial-v1.md) (PR2, #715): deterministic cell classes and the expected mapping.
- [Census](../llm/census/README.md) and the [registered benign corpus](../llm/traces/interceptor-benign/README.md): the inputs both reports are generated from.
