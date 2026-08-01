# Mixed-assembly eval corpus (legacy vs mixed context assembly)

This directory holds the paired corpus for the #331 slice-3c evaluation: does
mixed structured context assembly (`agent.ContextManager{Mixed: true}`)
preserve answer quality better than the legacy assembly path at the same
token budget, on transcripts that mix conversation, agent memory, and RAG?

Every case is one frozen `agent.State` assembled twice — a `legacy` arm
(`ContextManager.Assemble`, default compactor) and a `mixed` arm
(`AssembleWithTrace`) — at an identical, formula-derived budget. Traces are
"prefilled history": the assembled messages replay verbatim as real
multi-turn chat (system / user / assistant / tool roles, tool-call IDs
preserved), and the candidate model produces exactly one generation. A human
label on each arm's answer yields a paired quality delta per case.

- `mixed-cases.json` — the case fixture (source of truth, hand-authored)
- `traces/` — built traces, two per case (`<id>-legacy.json`,
  `<id>-mixed.json`), plus `<id>-topline.json` where declared; regenerated
  deterministically from the fixture (`llm-bench -assembly-build`)
- `report.json` — the committed `-assembly-report` verdict (added when
  labeling completes)
- `pair-preferences.jsonl` — the forced-choice sidecar labels

## Pre-registered decision rule (v2) — frozen before any labeling

The report header carries this rule verbatim, rendered from the registered
constants in `cmd/llm-bench/assembly_mixed.go` (single source of truth):

> legacy-mixed rule v2 (registered): minimum 60 complete labeled non-control
> pairs pooled and minimum 12 per stratum present; quality-improved:
> stratified-bootstrap CI low > 0; noninferior: CI low strictly > -0.10;
> materially-regressed: CI high < -0.10; else inconclusive; token and
> pressure numbers are descriptive only, never consulted; registered
> pressure fraction f=0.6

Notes on the rule:

- The paired delta is mixed − legacy on the 0/0.5/1 `AnswerQuality` scale.
- Strict inequalities: a CI lower bound of exactly −0.10 is NOT noninferior;
  exactly 0 is NOT an improvement.
- `materially-regressed` means confidently worse than the −0.10 margin. A
  confidently negative delta above −0.10 still reads `noninferior`; the
  report says so rather than hiding it.
- The bootstrap is stratified (resampling within strata) and clustered
  (cases sharing a `scenario_family` move together). The pooled statistic is
  the ONLY confirmatory number. Per-stratum output is descriptive — means,
  win/loss/tie, completeness — never a per-stratum verdict or CI (n=14 per
  stratum cannot support one).
- Completeness: the verdict requires at least 60 complete labeled
  non-control pairs pooled AND at least 12 in every stratum present;
  otherwise the report says `insufficient-corpus` or
  `insufficient-stratum-balance`. Every excluded or unlabeled pair is listed
  with a reason in the report's exclusions array.
- A **forced-choice sidecar** (A/B/tie per pair, `pair-preferences.jsonl`)
  is labeled in the same session. Its pre-registered SECONDARY analysis is a
  two-sided exact binomial sign test on non-tie preferences (sides resolved
  by the registered hash-parity function, `fcSideIsLegacyA`), reported by
  `-assembly-report` next to the primary CI via `-fc-preferences`, never
  overriding it.
- `Pressure.Cause` histograms are never compared across arms: slice 3b
  documented that mixed assembly shifts cause buckets for the same
  transcript (the same anchor can read `retrieval` under legacy and
  `tool_output` under mixed). Per-arm pressure data is descriptive only.

### Power honesty (MDE table)

60 pairs is a corpus floor, not a power guarantee. With a paired-delta
standard deviation of 0.5 on the 0..1 scale (a conservative figure for a
0/0.5/1 grid), a normal approximation gives:

| n (pairs) | 95% CI half-width | detectable at CI-low > 0 (approx.) |
|---|---|---|
| 60 | ~0.127 | delta >= ~0.13 |
| 70 | ~0.117 | delta >= ~0.12 |
| 97 | ~0.100 | delta >= ~0.10 |

Proving noninferiority when the true delta is 0 needs roughly 97 pairs at
this SD. Smaller true effects than the table's threshold will read
`inconclusive` — that is the honest outcome, not a failure of the corpus.

### Evidence scope

The verdict sentence is explicitly scoped to **this balanced synthetic
stress corpus at the registered pressure fraction f=0.6**, under greedy
decoding, for the named candidate model. The cases are an authored stress
set, not a sample from any population of real transcripts; the corpus
author also authored both assembly paths, and pre-registration is the
mitigation, not a proof of neutrality. `materially-regressed` and
`inconclusive` are valid, publishable outcomes; the instrument existing is
the deliverable, and the verdict is whatever the labels say.

### No-tuning rule

After labeling begins, NOTHING here may change in response to results: not
the rule, not the thresholds or floors, not the strata definitions, not the
budget formula or fraction, not case budgets or content, not the sweep
fractions in the rag-eval companion. A case found to be structurally broken
(build-gate failure, candidate-ID mismatch) may be excluded WITH a listed
reason — never edited to change its outcome. Growing the corpus after a
verdict starts a new registered round, it does not amend the old one.

## Registered constants (governing sources in code)

| Constant | Value | Governing source |
|---|---|---|
| Pressure fraction f | 0.6 | `cmd/llm-bench/assembly_mixed_fixture.go` `mixedBudgetFraction` |
| Envelope tokens | 8 | same file, `mixedEnvelopeTokens` |
| Min-viable slack | 64 | same file, `mixedMinViableSlack` |
| Budget formula | `max(minViable, round(f x rawStateTokens))` | `cmd/llm-bench/assembly_mixed_build.go` `mixedCaseBudget` |
| minViable | `est(system) + est(question) + slack` | same |
| Token estimator | `(len+3)/4` | `assembly_mixed_state.go` (arm-independent registered basis) |
| Rule thresholds / floors | 0 / −0.10; 60 / 12 | `cmd/llm-bench/assembly_mixed.go` |
| Decoding | temperature 0 (greedy; no seed — transports expose none) | capture path, `runner.go` |
| Arm capture order | FNV-1a-64(PairID) parity: even = legacy first | `calibration.go` |
| Companion sweep fractions | 0.4 / 0.6 / 0.8 (unlabeled, rag-eval only) | `internal/rageval/mixed_eval.go` |

Control cases use a generous budget (`rawStateTokens + slack`) instead of
the formula; they exist to measure label noise, not assembly.

## Case schema (`mixed-cases.json`)

Top level: `{"version": 1, "kind": "mixed-assembly", "constants": {...},
"cases": [...]}`. The `constants` block restates the registered values and
must equal them exactly (the builder rejects drift).

| Field | Meaning |
|---|---|
| `id` | Unique lowercase ASCII `[a-z0-9][a-z0-9-]*`; becomes the trace filename prefix. |
| `stratum` | One of the five strata below. |
| `answer_home` | Where the answer truly lives: `conversation`, `memory`, `rag`, or `join`. Coherence with the stratum is validated (see strata). |
| `scenario_family` | Cluster-bootstrap resampling unit. MUST stay within one stratum (validated at author time AND excluded at report time if violated). Use for template siblings inside one stratum. |
| `twin_group` | Optional descriptive label for counterfactual twins that SPAN strata (same scenario, answer moved between domains). Never a clustering unit, never in the CI; recorded in trace metadata for descriptive lane-bias analysis. |
| `control` | Negative-control pair: generous budget, arms must be identical in role and content (the model-visible sequence), excluded from the verdict. Control cases must be anchor-free (turns + `fixture_echo` only) with small tool-call payloads. |
| `cap_stress` | Permits per-tool-call `output_cap` overrides (uniform: every tool call on the case carries one). |
| `system` | `State.System`. |
| `events` | Ordered array; each entry exactly one of `{"turn": {role, content}}` (role `user`/`assistant`) or `{"tool_call": {call_id, tool, args}}`, tool in `retrieve` / `agent_memory_search` / `fixture_echo`. The FINAL event must be the user question turn. |
| `memory_records` | Seeded agent-memory records `{id, content, kind, workspace_id}`; `working` kind requires a workspace. Rendered memory dates read `2025-07-27` (noon-UTC pinned epoch). |
| `rag_sources` | 3a source schema: `{path, content, language, abstract, overview}` with the atomic abstract/overview pair rule. |
| `required_evidence` | `[{domain, literal}]` — domain-tagged verbatim anchors. Anchor them ("claimBatch = 25", not "25"). |
| `forbidden_evidence` | Literals that must appear in NO domain. |
| `required_domains` | Must be exactly the set {`conversation`, `memory`, `rag`} (any order) — every case carries all three domains; the non-home domains are competing distractors. |
| `answer_turn_index` | Optional index of the answering turn; consumed by position-balance bookkeeping for `conversation_only` cases (accepted on any stratum). |
| `topline_facts` | Optional (non-control): minimal golden-support facts; must contain every required literal. Emits an unpaired `<id>-topline` ceiling trace: "Facts:\n...\n\nQuestion: ...". |
| `golden.final_answer_criteria` | The labeling rubric. |

### Evidence contract (validated mechanically, not by author discipline)

- Each `required_evidence` literal must appear verbatim (case-sensitive) in
  its declared domain's fixture content, and must be ABSENT from the other
  two domains, from the final question turn, and from the system prompt
  (contamination scans are case-insensitive — a re-cased leak is still a
  leak).
- Reachability at build time scans each arm's ASSEMBLED content excluding
  the final question message: every anchor must reach at least one arm, and
  ALL of a case's anchors must co-occur in at least one arm (join semantics
  applied uniformly; a single-anchor case degenerates to plain reach).
  Single-arm reach is reported per anchor on stderr.

## Strata (14 primary cases each; >= 12 complete labeled pairs each)

1. `conversation_only` (`answer_home: conversation`) — the answer exists
   only in a conversation turn; no tool chain carries it. Answer positions
   are balanced across the oldest/middle/newest thirds of the history, and
   several cases place the answer OLDER than spans the legacy compactor
   drops — retention order is exactly what this stratum tests.
2. `memory_only` (`memory`) — the answer exists only in an agent-memory
   record; RAG sources are present but irrelevant.
3. `cross_domain_join` (`join`) — the answer requires combining facts from
   two domains; golden criteria name both halves.
4. `stale_vs_fresh` (any home) — a stale compact representation (stale
   summary / memory card) conflicts with fresh verbatim evidence; the
   correct answer follows the fresh verbatim, and the rubric scores a
   stale-summary answer 0. Authoring must confirm the stale representation
   actually RENDERS in both arms.
5. `chain_retention` (`rag`/`memory`/`join`) — the answer-bearing tool
   chain sits where legacy's recency compactor evicts it whole, while mixed
   can retain a compact representation. At least 4 cases use `fixture_echo`
   so the unstructured chain-level subject path gets corpus coverage.

### Authoring rules the build gates enforce (and why)

- **Every non-control case needs at least one `retrieve` or
  `agent_memory_search` call.** A tool-free case takes the legacy path in
  both arms and always fails the arms-differ gate — the corpus exists to
  exercise mixed assembly.
- **Budgets are computed, not authored.** The builder derives each case's
  budget from the formula; the pressure-evidence gate then requires BOTH
  arms to shed messages or bytes. `pressure_level` in the trace metadata
  often reads `ok` even on genuinely pressured cases (it reflects
  post-assembly usage); `shed_messages`/`shed_bytes` are the pressure
  evidence.
- **Arms must differ** (non-control) or the case is dead weight — a
  guaranteed zero delta.
- **The question survives structurally**: the final assembled message must
  BE the original question message in both arms (identity, not substring).
- **Control cases**: anchor-free, small payloads (tool-call args are priced
  by the runtime's estimator; fat args overflow the generous budget and the
  build fails loudly), arms asserted byte-identical, zero shed asserted.
- Builds are only supported in timezones UTC−11..+11 (rendered memory dates
  are local-time; the noon-UTC epoch pin keeps the date stable in that
  band).

## Corpus composition

- 70 primary cases (14 x 5 strata) — 10 pairs of slack over the 60-pair
  floor for invalidations.
- 6 control pairs (label-noise floor; excluded from the verdict).
- 12 topline traces (one per sampled scenario family across strata) — an
  unpaired model-competence ceiling: if legacy ≈ mixed ≪ topline, assembly
  is the bottleneck; if topline is low, the model is.
- Strata 1–3 are authored predominantly as `twin_group` sets (same
  scenario, same distractors, answer moved between domains) for descriptive
  lane-bias analysis.

## Workflow

Build (deterministic; the regeneration gate in CI rebuilds and byte-compares):

```
llm-bench -assembly-build docs/llm/assembly-corpus/mixed/mixed-cases.json
```

Capture (both models; greedy decoding, counterbalanced order, provenance
recorded; only qwen3-coder-next is labeled first — the gemma4:31b artifacts
wait for a follow-up session):

```
llm-bench -calibrate-capture -traces 'docs/llm/assembly-corpus/mixed/traces/*.json' ...
```

Labeling, three passes:

1. **Primary (promptless)** — `-blind-render -blind-dups 7`: blocks show
   question, rubric, and candidate output only (no prompt bytes; model AND
   arm identity hidden). Score 0 / 0.5 / 1 against the rubric; optionally
   `flag: grounding-check` to queue a block for adjudication. Seven
   duplicated blocks measure intra-rater consistency. Ingest with
   `-blind-ingest -dups-out ...`.
2. **Forced choice** — `-fc-render` / `-fc-ingest -fc-out
   pair-preferences.jsonl`: per pair, prefer A / B / tie. Sides are
   assigned by hash parity; the sidecar never records which side was which
   arm (the report recomputes it).
3. **Adjudication** — `-adjudicate-render` / `-adjudicate-ingest`: only
   flagged blocks, now WITH the prompt, prefilled score, required reason;
   corrections are logged in the label notes.

Report:

```
llm-bench -assembly-report -artifacts ... -labels ... -report ...
```

## Limitations (stated before labels exist)

- Greedy decoding only; one decoding configuration. The 6 identical-arm
  control pairs and 7 duplicate blocks quantify residual label noise.
- Single labeler (intra-rater controls ship; a second rater on a subset is
  optional and non-gating).
- Authored corpus by the author of both assembly paths — mitigated by
  mechanical gates, twins, position balancing, and pre-registration;
  not eliminated.
- The verdict is per-model and scoped to this corpus at f=0.6; it is
  evidence about assembly under designed pressure, not a general claim.
