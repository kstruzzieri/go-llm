# Mixed-assembly eval corpus (legacy vs mixed context assembly)

This directory holds the paired corpus for the #331 slice-3c evaluation: does
mixed structured context assembly (`agent.ContextManager{Mixed: true}`)
preserve answer quality better than the legacy assembly path at the same
token budget, on transcripts that mix conversation, agent memory, and RAG?

**Status**: the registered run is COMPLETE. The full fixture
(`mixed-cases.json`, 70 primary cases plus 6 controls), all 164 built
traces, both models' capture artifacts and run manifests (with committed
digests), the absolute labels, the blind block map, the duplicate-block
audit, the sealed forced-choice sidemap (digest committed before labeling)
with `pair-preferences.jsonl`, and `report.json` are all committed — the
"Committed artifacts" and "Commit policy" lists below now describe the
current tree. No adjudication artifact exists because zero blocks were
flagged (see the verdict section). In CI, the regeneration gate
(`TestMixedCorpusRegeneration`) rebuilds the corpus from the fixture and
byte-compares every trace, and the balance gate (`TestMixedCorpusBalance`)
re-checks the registered corpus shape.

Every case is one frozen `agent.State` assembled twice — a `legacy` arm
(`ContextManager.Assemble`, default compactor) and a `mixed` arm
(`AssembleWithTrace`) — at an identical, formula-derived budget. Traces are
"prefilled history": the assembled messages replay verbatim as real
multi-turn chat (system / user / assistant / tool roles, tool-call IDs
preserved), and the candidate model produces exactly one generation. A human
label on each arm's answer yields a paired quality delta per case.

Committed artifacts of the registered run (see "Commit policy" below):

- `mixed-cases.json` — the case fixture (source of truth, hand-authored)
- `traces/` — built traces plus their manifest, two per case
  (`<id>-legacy.json`, `<id>-mixed.json`) and `<id>-topline.json` on the
  cases the registered topline rule selects; rebuilt deterministically from
  the fixture (`llm-bench -assembly-build <fixture> -assembly-out-mixed
  <traces dir>`) and byte-gated in CI (`TestMixedCorpusRegeneration`)
- capture artifacts JSONL + its sibling `.manifest.json` run manifest
- the sealed forced-choice sidemap JSON + its committed sha256 digest
- the blind worksheet block map (`-blind-blockmap-out`) — the only join
  from opaque worksheet BLOCK ids back to artifact hashes
- absolute labels JSONL, the duplicate-block audit (`-dups-out`), and —
  when any block is flagged — the adjudication worksheet and its logged
  corrections (this run flagged none, so none exists)
- `pair-preferences.jsonl` — the forced-choice sidecar labels
- `report.json` — the committed `-assembly-report` verdict

## Registered verdict (labeled 2026-08-04)

**quality-improved** for the candidate served by the configured endpoint
under selector `qwen3-coder-next:latest`: pooled mean
paired delta (mixed − legacy) **+0.279** on the 0/0.5/1 AnswerQuality scale,
95% stratified-cluster-bootstrap CI **[+0.179, +0.379]**, over **70/70
complete labeled non-control pairs with zero exclusions**. The verdict is
scoped to this balanced synthetic stress corpus at the registered pressure
fraction f=0.6, under greedy decoding, for the named candidate model.
Instruments: all 6 identical-arm control pairs scored |delta| = 0; 7/7
intra-rater duplicate blocks agreed (mean |delta| 0.000); the 12-trace
topline ceiling scored mean 1.0 (the model answers when handed the facts —
assembly was the bottleneck); the registered forced-choice secondary agrees
(34 mixed / 14 legacy / 22 ties, cluster sign-flip permutation p = 0.0049);
leave-one-family-out CI lower bounds stay in [+0.154, +0.206]. Zero blocks
were flagged for adjudication, so no adjudication log exists for this run.
No arm guesses were recorded in the forced-choice pass (arm_guess is
optional and the report shows n_guessed = 0), so only STRUCTURAL blinding
is evidenced for this run; the practical-blinding audit is vacuous here.
Per-stratum means are descriptive only: chain_retention +1.00,
conversation_only +0.50, stale_vs_fresh +0.07, memory_only −0.07,
cross_domain_join −0.11.

### Sensitivity and attribution (descriptive; added at external review)

The pooled effect is concentrated where the corpus designed legacy's
structural weakness to bite. Summed per-stratum delta contributions (out of
the pooled +19.5 total over 70 pairs): chain_retention +14.0,
conversation_only +7.0, stale_vs_fresh +1.0, memory_only −1.0,
cross_domain_join −1.5 — the 42 cases outside the two positive strata sum
to −1.5. Stratum-exclusion sensitivity over the same stratified-cluster-bootstrap
scheme (descriptive, not registered; one- and two-stratum removals): dropping chain_retention gives mean
+0.098 with CI approximately [−0.03, +0.22]; dropping chain_retention AND
conversation_only gives −0.036 with CI approximately [−0.20, +0.13].
Reading: chain_retention and conversation_only are precisely the strata
whose definitions target what the legacy recency compactor structurally
discards (whole answer-bearing chains; older history spans), while
cross_domain_join and memory_only were authored with registered
legacy-favoring polarities and landed negative — the corpus contains
designed legacy wins, and the pooled verdict holds over the registered
mixture. The bootstrap holds that authored stratum mixture fixed and the labels
are fixed inputs; it quantifies resampling variability among the observed
authored families, not label noise and not uncertainty about the mixture
itself. The registered decision applies to the pooled corpus as
registered; within this corpus, the cases outside the two designed
pressure shapes show no measured gain (the tables above), and any claim
about other transcript distributions is outside this instrument's
evidence.

### Candidate identity (narrowed claim + post-hoc addendum)

The committed identity evidence for this run is: the selector
(`openai-compat/qwen3-coder-next:latest`), the endpoint
(`http://127.0.0.1:8090`, llama-swap), and the per-request transport tag in
every artifact row. The manifest's `/props` probe returned `status 404`
(llama-swap does not proxy it) and the selector digest is empty, so the
identity claim is NARROWED to "the model the configured endpoint served
under this selector." Post-hoc addendum, an operator-reported observation (a post-hoc hash
cannot establish that the file is the one served): the GGUF file resident
at the serving path hashes to
sha256:4bb93f0a0221ef4ff963ca9094df629c8dfdfabc3b4fdd85c1a2e4c0624fce36
(unsloth Qwen3-Coder-Next UD-Q4_K_XL), served by llama.cpp build
b10210-000547513 per live probes during the same session; neither fact
rides the sealed manifest. Future registered runs must commit a pre-run
server identity (per-upstream /props body or equivalent).

## Pre-registered decision rule (v2) — frozen before any labeling

The report header carries this rule verbatim, rendered from the registered
constants in `cmd/llm-bench/assembly_mixed.go` (single source of truth; the
gate test `TestReadmeRuleTextMatches` byte-compares this quote against the
rendered rule):

> legacy-mixed rule v2 (registered): minimum 60 complete labeled
> non-control pairs pooled and minimum 12 per stratum present; minimum 6
> scenario-family clusters per stratum and maximum cluster size 3;
> quality-improved: stratified-bootstrap CI low > 0; noninferior: CI low
> strictly > -0.10; materially-regressed: CI high < -0.10; else
> inconclusive; token and pressure numbers are descriptive only, never
> consulted; registered pressure fraction f=0.6

Notes on the rule:

- The paired delta is mixed − legacy on the 0/0.5/1 `AnswerQuality` scale.
- Strict inequalities: a CI lower bound of exactly −0.10 is NOT noninferior;
  exactly 0 is NOT an improvement.
- `materially-regressed` means confidently worse than the −0.10 margin. A
  confidently negative delta above −0.10 still reads `noninferior`; the
  report says so rather than hiding it.
- The pooled statistic is the ONLY confirmatory number. Per-stratum output
  is descriptive — means, win/loss/tie, completeness — never a per-stratum
  verdict or CI (n=14 per stratum cannot support one).
- `Pressure.Cause` histograms are never compared across arms: slice 3b
  documented that mixed assembly shifts cause buckets for the same
  transcript (the same anchor can read `retrieval` under legacy and
  `tool_output` under mixed). Per-arm pressure data is descriptive only.

### Decision statuses (closed set)

The per-model `decision` field takes exactly one of these values. Gates run
in registered order BEFORE the rule, so an earlier status masks a later one:

1. `incomplete-labeling` — at least one complete-built non-control pair is
   missing a label on either arm. The verdict is suppressed entirely:
   labeling must be complete over every eligible built pair.
2. `insufficient-corpus` — fewer than 60 complete labeled non-control pairs
   pooled.
3. `insufficient-stratum-balance` — some registered stratum has fewer than
   12 complete pairs; a registered stratum entirely absent counts as zero
   and trips this the same way.
4. `insufficient-cluster-diversity` — some stratum has fewer than 6
   distinct scenario-family clusters among its complete pairs.
5. Otherwise the rule: `quality-improved`, `noninferior`,
   `materially-regressed`, or `inconclusive`.

### Exclusions (closed reason set)

Every pair excluded from the analysis is listed in the report's
`exclusions` array with one of these registered reasons — exclusions are
loud and itemized, never silent:

- `missing-legacy-arm`, `missing-mixed-arm` — one arm absent from the
  artifacts (e.g. a capture that failed its retry, below).
- `unverified-capture` — the run manifest (`-capture-manifest`) does not
  list both arms with reported token usage.
- `temperature-mismatch` — either arm's capture provenance is missing a
  temperature or differs from the registered temperature 0.
- `candidate-ids-mismatch`, `state-digest-mismatch`, `budget-mismatch`,
  `stratum-mismatch`, `scenario-family-mismatch`, `twin-group-mismatch`,
  `control-flag-mismatch` — cross-arm pair-integrity invariants.
- `unregistered-stratum` — a stratum outside the registered five is never
  silently pooled.
- `missing-scenario-family` — a non-control pair without a scenario family
  cannot cluster.
- `unlabeled` — a pair missing a label on either arm (non-control
  occurrences also trip the `incomplete-labeling` gate above).
- `scenario-family-crosses-strata` — defense in depth behind the fixture
  validator: every pair such a family touches is excluded.
- `oversized-cluster` — a scenario family holding more than 3 complete
  pairs would dominate its stratum's resampling; every pair it holds is
  excluded, never silently down-weighted.

## Registered primary analysis (stratified cluster bootstrap)

- **Estimand**: the unweighted mean of the per-case deltas over all
  complete labeled non-control pairs.
- **Replicates**: each stratum is resampled independently — k-of-k draws of
  its scenario-family clusters with replacement (all pairs sharing a family
  move together), the stratum mean taken as the ratio of sums over the
  resampled pairs — and the strata recombine with FIXED weights n_s/N,
  where n_s is the stratum's ORIGINAL complete-pair count. Replicate noise
  therefore reflects within-stratum cluster resampling only, never
  accidental stratum re-weighting.
- **Registered parameters**: seed = 1, B = 10,000 replicates
  (`pairedBootstrapSeed` / `pairedBootstrapN`, `cmd/llm-bench/paired.go`);
  the CI is the nearest-rank 2.5 / 97.5 percentile pair of the replicate
  means. The CI is pooled only, by pre-registration.
- **Cluster floors**: at least 6 distinct scenario-family clusters per
  stratum among the complete pairs, and no cluster larger than 3 pairs
  (enforced by the gate and the exclusion above).
- **Known variance understatement, disclosed**: k-of-k cluster resampling
  understates variance by roughly a (k−1)/k factor per stratum — about 9%
  of SD at the k=6 floor, shrinking as strata hold more clusters. The CI is
  therefore slightly narrower than the truth at the registered floors: a
  lower bound that clears a threshold by less than ~10% of the CI
  half-width is a borderline call and should be read as such. The report's
  descriptive leave-one-group-out band
  (`cluster_diagnostics.leave_one_out`) is the sensitivity companion; it
  never feeds the decision.

## Registered secondary analysis (forced choice)

A forced-choice sidecar (A/B/tie per pair, `pair-preferences.jsonl`) is
labeled in the same session, sides assigned by the sealed random sidemap
(workflow below). Its REGISTERED secondary p-value is the cluster sign-flip
permutation test (`fcClusterPermutationP`): non-tie signs (+1 mixed win,
−1 legacy win) aggregate per independence group — scenario families, with
`twin_group` merging the families it touches — each of B = 10,000 seeded
permutations flips every group's aggregate sign independently at p = 0.5,
and the two-sided p is the add-one-smoothed fraction (count+1)/(B+1) of
permutations at least as extreme as observed. The permutation shares the
report's seed, so a sensitivity rerun moves the CI and the permutation p
together. Monte-Carlo resolution: the standard error of a permutation p at
B = 10,000 is at most 0.005, so p-values are meaningful to about two
decimal places, no further.

The report also prints a two-sided exact binomial sign test over the
non-tie preferences. It is DESCRIPTIVE only: it treats every preference as
independent, which the clustered corpus design does not guarantee. Neither
secondary number ever feeds the primary decision, which is final before the
forced-choice section attaches.

Preference rows on report-excluded pairs or negative-control pairs are
skipped and counted (`skipped_excluded`); they enter neither test. Every
row's A/B hashes are bound to its pair's exact arms under the sealed side
assignment — a mismatch is a loud error naming the pair.

## Completeness and missingness

- Labeling must be COMPLETE over every eligible built pair: any complete
  non-control pair missing a label suppresses the verdict
  (`incomplete-labeling`). There is no "label what you get to" mode.
- Structural and capture exclusions (the registered reasons above) consume
  the corpus slack: 70 authored primary pairs over a 60-pair floor leaves
  10 invalidations before the verdict degrades to `insufficient-corpus`.
- **Capture policy (registered)**: a failed capture run for an arm gets
  exactly ONE retry; a second failure leaves the arm uncaptured and the
  pair exits through `missing-legacy-arm` / `missing-mixed-arm` with its
  reason listed. No third attempts, no substitutions.

## Power honesty (MDE table)

60 pairs is a corpus floor, not a power guarantee. Normal-approximation 95%
CI half-widths for the paired delta, at two planning SDs:

| SD (paired delta) | n (pairs) | 95% CI half-width | detectable at CI-low > 0 (approx.) |
|---|---|---|---|
| 0.5 (one planning scenario) | 60 | ~0.127 | delta >= ~0.13 |
| 0.5 (one planning scenario) | 70 | ~0.117 | delta >= ~0.12 |
| 0.5 (one planning scenario) | 97 | ~0.100 | delta >= ~0.10 |
| 1.0 (conservative) | 60 | ~0.253 | delta >= ~0.26 |
| 1.0 (conservative) | 70 | ~0.234 | delta >= ~0.24 |
| 1.0 (conservative) | 97 | ~0.199 | delta >= ~0.20 |

SD 0.5 is one planning scenario for a 0/0.5/1 grid, not a guarantee; SD 1.0
is the conservative bound (the scale's maximum possible paired-delta SD is
1). Clustering further reduces the effective n below the raw pair count, so
true half-widths sit somewhat above these rows. Proving noninferiority when
the true delta is 0 needs roughly 97 pairs at SD 0.5. Smaller true effects
than the table's threshold will read `inconclusive` — that is the honest
outcome, not a failure of the corpus.

## Evidence scope

The verdict sentence is explicitly scoped to **this balanced synthetic
stress corpus at the registered pressure fraction f=0.6**, under greedy
decoding, for the named candidate model. The cases are an authored stress
set, not a sample from any population of real transcripts; the corpus
author also authored both assembly paths, and pre-registration is the
mitigation, not a proof of neutrality. `materially-regressed` and
`inconclusive` are valid, publishable outcomes; the instrument existing is
the deliverable, and the verdict is whatever the labels say.

## No-tuning rule

After labeling begins, NOTHING here may change in response to results: not
the rule, not the thresholds or floors, not the strata definitions, not the
budget formula or fraction, not case budgets or content, not the sweep
fractions in the rag-eval companion, not the sealed sidemap or its
committed digest, not the capture manifest or the artifacts it vouches for.
A case found to be structurally broken (build-gate failure, candidate-ID
mismatch) may be excluded WITH a listed reason — never edited to change its
outcome. Growing the corpus after a verdict starts a new registered round,
it does not amend the old one.

## Registered constants (governing sources in code)

| Constant | Value | Governing source |
|---|---|---|
| Pressure fraction f | 0.6 | `cmd/llm-bench/assembly_mixed_fixture.go` `mixedBudgetFraction` |
| Envelope tokens | 8 | same file, `mixedEnvelopeTokens` |
| Min-viable slack | 64 | same file, `mixedMinViableSlack` |
| Budget formula | `max(minViable, round(f x rawStateTokens))` | `cmd/llm-bench/assembly_mixed_build.go` `mixedCaseBudget` |
| minViable | `est(system) + est(question) + slack` | same |
| Token estimator | `(len+3)/4` | `assembly_mixed_state.go` `mixedEstTokens` (arm-independent registered basis) |
| Rule thresholds / floors | 0 / −0.10; 60 pooled / 12 per stratum | `cmd/llm-bench/assembly_mixed.go` |
| Cluster floors | >= 6 clusters per stratum; cluster size <= 3 | same file |
| Bootstrap seed / B / percentiles | 1 / 10,000 / nearest-rank 2.5 & 97.5 | `paired.go`, `percentiles.go` |
| Decoding | temperature 0.0 sent explicitly on every capture request (greedy; no seed — neither transport exposes one) | `runner.go` `assemblyCaptureTemperature` |
| Arm capture order | pairs sorted by FNV-1a-64(PairID) ascending; first arm alternates by position (even = legacy first, odd = mixed first) — balanced within 1 | `calibration.go` `counterbalancePairOrder` |
| Topline quotas | {conversation_only 3, memory_only 2, cross_domain_join 3, stale_vs_fresh 2, chain_retention 2} = 12 | `assembly_mixed_fixture.go` `mixedToplineQuotas` |
| Companion sweep fractions | 0.4 / 0.6 / 0.8 (unlabeled, rag-eval only) | `internal/rageval/mixed_eval.go` |

Control cases use a generous budget (`rawStateTokens + slack`) instead of
the formula; they exist to measure noise, not assembly (see Limitations for
what that noise actually is).

## Case schema (`mixed-cases.json`)

Top level: `{"version": 1, "kind": "mixed-assembly", "constants": {...},
"cases": [...]}`. The `constants` block restates the registered values and
must equal them exactly (the builder rejects drift; the Go constants
govern).

| Field | Meaning |
|---|---|
| `id` | Unique lowercase ASCII `[a-z0-9][a-z0-9-]*`; becomes the trace filename prefix. |
| `stratum` | One of the five strata below. |
| `answer_home` | Where the answer truly lives: `conversation`, `memory`, `rag`, or `join`. Coherence with the stratum is validated (see strata). |
| `scenario_family` | Cluster-bootstrap resampling unit. MANDATORY on non-control cases (a family-free pair could only be excluded at report time); optional on controls. MUST stay within one stratum (validated at author time AND excluded at report time if violated). Use for template siblings inside one stratum. |
| `twin_group` | Descriptive label for counterfactual twins that SPAN strata. Contract, enforced corpus-wide: at most 4 distinct groups; each group 2–3 members; every member in a DIFFERENT stratum drawn from {`conversation_only`, `memory_only`, `cross_domain_join`}; all members share the identical `rag_sources` path set and `memory_records` id set (same distractor pool). Never a clustering unit for the CI — but the forced-choice permutation MERGES the scenario families a twin group touches into one independence group. |
| `control` | Negative-control pair: generous budget, arms asserted byte-identical, ZERO shed asserted in both arms (the inverse of the pressure gate), excluded from the verdict and from both forced-choice tests. Anchor-free by rule: conversation turns + `fixture_echo` only, with small tool-call payloads (fat args overflow the generous budget and fail the build loudly). At most 2 controls per stratum. `pressure_target` is illegal on controls. |
| `cap_stress` | Permits per-tool-call `output_cap` overrides (uniform: every tool call on the case carries one). |
| `system` | `State.System`. |
| `events` | Ordered array; each entry exactly one of `{"turn": {role, content}}` (role `user`/`assistant`) or `{"tool_call": {call_id, tool, args}}`, tool in `retrieve` / `agent_memory_search` / `fixture_echo`. The FINAL event must be the user question turn. |
| `memory_records` | Seeded agent-memory records `{id, content, kind, workspace_id}`; `working` kind requires a workspace. Rendered memory dates read `2025-07-27` (noon-UTC pinned epoch). |
| `rag_sources` | 3a source schema: `{path, content, language, abstract, overview}` with the atomic abstract/overview pair rule. |
| `required_evidence` | `[{domain, literal}]` — domain-tagged verbatim anchors. Anchor them ("claimBatch = 25", not "25"). |
| `forbidden_evidence` | Literals that must appear in NO domain. |
| `pressure_target` | MANDATORY on non-control cases (illegal on controls): a registered `{domain, literal}` the budget must actually bite. The literal must appear case-sensitively in its declared domain's fixture content and be absent (case-insensitively) from the final question and the system prompt. ANSWER-RELEVANCE is validated mechanically: the target must EQUAL one of the case's `required_evidence` entries (domain AND literal), or — on `stale_vs_fresh` only — appear case-sensitively in the abstract/overview of a rag source WHOSE CONTENT carries at least one `required_evidence` literal (the stale summary of the very source holding the fresh evidence; a summary on an evidence-free source is a decoy and rejected). Stale MEMORY carriers do not qualify: a stale-memory case registers its pressure on the fresh evidence instead. The build's carrier-change gate (below) then requires that in at least one arm some built-State message carrying it is dropped, truncated, or re-rendered. |
| `required_domains` | Must be exactly the set {`conversation`, `memory`, `rag`} (any order) — every case carries all three domains; the non-home domains are competing distractors. |
| `answer_turn_index` | REQUIRED on `conversation_only` cases (controls included), optional elsewhere. Must point at a turn event that is not the final question; on `conversation_only` the indexed turn must contain every conversation-domain `required_evidence` literal (case-sensitive) — the declared answer position and the anchors' actual home may not diverge. Feeds the answer-position-thirds balance bookkeeping. |
| `topline_facts` | Present on EXACTLY the cases the registered topline rule selects (see below) — presence anywhere else, or absence on a selected case, is an authoring error. Must contain every `required_evidence` literal case-sensitively. Emits an unpaired `<id>-topline` ceiling trace: "Facts:\n...\n\nQuestion: ...". Only legal on non-control cases. |
| `golden.final_answer_criteria` | The labeling rubric. |

### Registered topline selection (the rule, not the author, chooses)

Eligible: non-control cases with a non-empty `scenario_family` (i.e. every
non-control case — the family is mandatory). Per stratum, the eligible
families order by FNV-1a-64(family) ascending (family name breaks ties);
the first `quota` families are selected; within each selected family the
case with the lexicographically smallest id carries `topline_facts`. Quotas
are the registered {3, 2, 3, 2, 2} split (12 total — the ceiling does not
divide evenly over 5 strata, so the split is registered, not derived).
Precondition for "exactly 12": every stratum must hold at least its quota
of eligible families. That holds for the registered 70-case corpus — each
stratum carries 14 non-control family-bearing cases, far above every quota
— and the validator's exact-quota floor makes any shortfall an author-time
error, never a silent under-fill.

### Evidence contract (validated mechanically, not by author discipline)

- Each `required_evidence` literal must appear verbatim (case-sensitive) in
  its declared domain's fixture content, and must be ABSENT from the other
  two domains, from the final question turn, and from the system prompt
  (contamination scans are case-insensitive — a re-cased leak is still a
  leak).
- Tool-call args are model-visible (they ride the assistant tool-call
  turn), so required and forbidden literals are contamination-scanned
  against every `tool_call`'s argument payload too — BOTH the raw bytes and
  every string in the decoded JSON tree, keys included, so a
  `\uXXXX`-escaped spelling cannot smuggle a literal past the raw scan.
- Reachability at build time scans each arm's ASSEMBLED content excluding
  the final question message — message content plus tool-call argument
  bytes: every anchor must reach at least one arm, and ALL of a case's
  anchors must co-occur in at least one arm (join semantics applied
  uniformly; a single-anchor case degenerates to plain reach). Single-arm
  reach is reported per anchor on stderr.

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
  exercise mixed assembly. (The rag-eval companion now obeys the same rule:
  every companion case engages the mixed path.)
- **Budgets are computed, not authored.** The builder derives each case's
  budget from the formula; the pressure-evidence gate then requires BOTH
  arms to shed messages or bytes. `pressure_level` in the trace metadata
  often reads `ok` even on genuinely pressured cases (it reflects
  post-assembly usage); `shed_messages`/`shed_bytes` are the pressure
  evidence.
- **The pressure must bite the registered target** (carrier-change gate):
  in at least one arm, at least one built-State message carrying the
  `pressure_target` literal must be dropped, truncated, or re-rendered.
  Both arms shedding only answer-irrelevant filler is "pressure theater"
  and fails the build. Precisely what the gate guarantees: the REGISTERED
  target's carrier changes in at least one arm, and that target is
  answer-relevant by the relevance rule above (an evidence anchor, or the
  stale summary of the evidence-carrying source). It does NOT guarantee
  that everything shed is answer-relevant — other dropped content may
  still be decoys. The gate bounds irrelevant shedding (at least one
  answer-relevant carrier moved); it does not eliminate it.
- **Arms must differ** (non-control) or the case is dead weight — a
  guaranteed zero delta. The compare is deep: role, content, tool ids and
  names, tool-call argument bytes.
- **The question survives structurally**: the final assembled message must
  BE the original question message in both arms (identity, not substring).
- **Assembly is a subsequence** (always on, controls included): each arm's
  assembled messages must map to distinct full-State indices in increasing
  order — no reordering, no duplication, no synthesized messages. Mixed may
  rewrite a tool observation's content in place (matched by tool-call ID);
  assistant tool-call turns must survive byte-identical — content, per-call
  IDs, tool names, and argument bytes are all compared.
- **Control cases**: anchor-free, small payloads (tool-call args are priced
  by the runtime's estimator; fat args overflow the generous budget and the
  build fails loudly), arms asserted byte-identical, zero shed asserted in
  both arms.
- Builds are only supported in timezones UTC−11..+11 (rendered memory dates
  are local-time; the noon-UTC epoch pin keeps the date stable in that
  band).

## Corpus composition

- 70 primary cases (14 x 5 strata) — 10 pairs of slack over the 60-pair
  floor for invalidations.
- 6 control pairs (noise floor; excluded from the verdict; at most 2 per
  stratum).
- 12 topline traces, placed by the registered selection rule above (exact
  on the registered corpus; see the precondition there) — an unpaired
  model-competence ceiling: if legacy ≈ mixed ≪ topline, assembly is the
  bottleneck; if topline is low, the model is.
- Up to 4 twin groups (2–3 members each, spanning distinct strata among
  {`conversation_only`, `memory_only`, `cross_domain_join`}, identical
  distractor pools) for descriptive lane-bias analysis.

## Workflow

Build (deterministic; the CI regeneration gate rebuilds and byte-compares
every trace and the manifest):

```
llm-bench -assembly-build docs/llm/assembly-corpus/mixed/mixed-cases.json \
  -assembly-out-mixed docs/llm/assembly-corpus/mixed/traces
```

Capture (basis of the registered run; only qwen3-coder-next is labeled
first — the gemma4:31b artifacts are captured and committed, and their
LABELING waits for a follow-up session):

```
llm-bench -calibrate-capture -traces 'docs/llm/assembly-corpus/mixed/traces/*.json' ...
```

- Greedy decoding: temperature 0.0 is sent explicitly on every request.
- Pairs run in the registered counterbalanced order (constants table);
  `captured_order` provenance is derived from the actual order.
- The capture writes a sibling run manifest
  (`<artifacts-out>.manifest.json`) — REQUIRED for the registered run —
  recording endpoint, transport, the llama.cpp `/props` server probe (or
  the probe error), decoding, the counterbalance scheme, and per-artifact
  rows with an explicit usage-present bit. Its `manifest_digest` line
  (sha256 of the file) is committed alongside the artifacts.
- One retry per failed arm, then the pair exits via a missing-arm
  exclusion (Completeness above).

Seal the forced-choice sides BEFORE labeling:

```
llm-bench -fc-sidemap-generate -artifacts <artifacts.jsonl> -fc-sidemap-out <sidemap.json>
```

One crypto/rand boolean per complete pair, plus the pair's two arm artifact
hashes in the drawn A/B order — the sidemap is the forced-choice flow's
ONLY hash carrier (worksheet PAIR headers carry pairID and modelKey alone,
because a header hash would join the committed artifacts JSONL, whose
`assembly_eval.mode` names the arm). The printed sha256 digest is COMMITTED
before any labeling starts and verified at every later step via
`-fc-sidemap-digest` (mismatch is a hard error). `-fc-render` and
`-fc-ingest` refuse to run without `-fc-sidemap`.

Labeling, three passes (use `-model` to restrict any worksheet to one
candidate model — per-model worksheets keep sessions bounded):

1. **Primary (promptless)** — `-blind-render -blind-dups 7
   -blind-blockmap-out <blockmap.json>`: blocks show question, rubric, and
   candidate output only (no prompt bytes; model AND arm identity hidden).
   Promptless blocks are addressed by an OPAQUE id (`=== BLOCK
   sha256(hash|salt)[:16] ===`, DUP blocks included) instead of the
   artifact hash — the render-emitted block map is the only id-to-hash
   join, and it is REQUIRED whenever a promptless block renders.
   Non-assembly and 3a flat/progressive blocks keep their hash-addressed
   headers byte-identical to the established convention. Score 0 / 0.5 / 1
   against the rubric; optionally `flag: grounding-check` to queue a block
   for adjudication. Seven duplicated blocks measure intra-rater
   consistency. Ingest with `-blind-ingest -blind-blockmap <blockmap.json>
   -dups-out ...` (the duplicate audit is committed).
2. **Forced choice** — `-fc-render -fc-sidemap ... -fc-sidemap-digest ...`
   / `-fc-ingest -fc-require-complete -fc-out pair-preferences.jsonl`: per
   pair, prefer A / B / tie; `-fc-require-complete` makes any blank block a
   loud error (the registered workflow labels every pair). The optional
   `arm_guess` line (a/b) is the blinding audit: the report's descriptive
   `arm_guess_accuracy` section tallies how often the labeler correctly
   guessed the mixed arm's side under the sealed assignment — it feeds no
   decision or test. Neither the sidecar rows nor the worksheet record
   which side was which arm — the worksheet carries no hashes at all, and
   the sealed sidemap is the only artifact that resolves sides to arms.
3. **Adjudication** — `-adjudicate-render` / `-adjudicate-ingest`: only
   flagged blocks, now WITH the prompt, prefilled score, required reason;
   every correction is logged in the label notes
   (`adjudicated(old->new): reason`). Adjudication blocks stay
   hash-addressed by design: the pass deliberately reveals the full
   prompt, so an opaque id would hide nothing.

Blinding honesty: this scheme is STRUCTURAL protection against casual
inference — no worksheet byte joins the committed artifacts or names an
arm. It is not cryptographic protection against the operator, who owns the
disk and can open the sidemap or block map at will; the `arm_guess` audit
is the registered check that the blinding held in practice.

Report (the registered invocation carries ALL verification inputs):

```
llm-bench -assembly-report -artifacts ... -labels ... \
  -fc-preferences pair-preferences.jsonl \
  -fc-sidemap <sidemap.json> -fc-sidemap-digest <committed sha256> \
  -capture-manifest <artifacts>.manifest.json -report report.json
```

`-capture-manifest` verification excludes any pair whose arms are absent
from the manifest, lack reported usage, or disagree with the registered
temperature; the report embeds the manifest's digest and artifact count.

### Commit policy

The registered run commits ALL of: the capture artifacts and their
manifest (digest included), the absolute labels, the blind worksheet block
map (`-blind-blockmap-out`), the duplicate-block audit (`-dups-out`), any
adjudication worksheet and its logged corrections (only when blocks were
flagged; this run flagged none),
`pair-preferences.jsonl`, the sealed sidemap and its committed digest, and
`report.json`. An uncommitted input is an unverifiable input. (This list is
the registered END state, and the tree now matches it; see the Status
note.)

## Provenance and limitations (stated before labels exist)

- **Model identity**: the openai-compat transport exposes no model content
  digest endpoint, so the registered identity record is the capture
  manifest — the `/props` probe body when the endpoint serves it, or the
  recorded probe error — plus the committed manifest digest. In THIS run
  the probe returned 404 through llama-swap, so the identity claim is
  narrowed; see "Candidate identity" in the verdict section. Ollama ShowModel digests are recorded when
  an ollama target is captured, but registered runs use openai-compat /
  llama.cpp.
- **Ollama tool-arg bytes are semantic-only**: the frozen ollama wire type
  re-encodes tool arguments (sorted keys, canonical whitespace), so
  byte-exact fixture args are achievable only on openai-compat — one more
  reason the registered transport is llama.cpp.
- Greedy decoding only; one decoding configuration; no sampling seed knob
  exists on either transport, so temperature 0 is the sole registered
  decoding control.
- **What the controls measure**: the 6 identical-prompt control pairs
  measure backend nondeterminism and label noise COMBINED — an
  identical-arm pair re-decodes the same prompt twice, so any |delta| mixes
  serving-stack nondeterminism at temperature 0 with labeling noise; the 7
  duplicate blocks isolate intra-rater label noise alone.
- Single labeler (intra-rater controls ship; a second rater on a subset is
  optional and non-gating).
- Authored corpus by the author of both assembly paths — mitigated by
  mechanical gates, twins, position balancing, and pre-registration;
  not eliminated.
- Corpus builds are supported only in timezones UTC−11..+11 (the noon-UTC
  epoch pin; outside that band rendered memory dates flip and the byte
  gates fail).
- The verdict is per-model and scoped to this corpus at f=0.6; it is
  evidence about assembly under designed pressure, not a general claim.
