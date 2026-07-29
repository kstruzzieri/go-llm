# Assembly-eval corpus (flat vs progressive context rendering)

This directory holds the paired corpus for the #331 assembly evaluation: does
progressive (summary-first) context rendering match or beat the frozen flat
`BuildContext` rendering at the same token budget? Every case is rendered
twice from the SAME retrieved candidate set — a `flat` arm and a
`progressive` arm — so a human label on each arm yields a paired quality
delta per case.

- `cases.json` — the QA case fixture (source of truth, hand-authored)
- `traces/` — built paired traces, two files per case
  (`<case-id>-flat.json`, `<case-id>-progressive.json`), regenerated
  deterministically from `cases.json`

The committed corpus is a 16-case seed. It exists to land the workflow and
the case-authoring conventions. **16 cases cannot support any quality
decision**: the report gate below requires at least 60 complete labeled
pairs per model, and until then `-assembly-report` says
`insufficient-corpus` for that model. Growing the corpus toward 60+ starts
only after the seed cases have been reviewed by the repo owner — cases are
inspected at PR review before more are added in the same style.

## Case schema (`cases.json`)

A JSON array of case objects:

| Field | Meaning |
|---|---|
| `id` | Unique, filename-safe, single path segment (it becomes the trace filename prefix). |
| `category` | Stratum: `content_only`, `metadata`, `distractor`, or `no_answer`. |
| `question` | The user question both arms are asked. Non-blank. |
| `golden.final_answer_criteria` | What a correct answer must contain (the labeling rubric). Non-blank. |
| `max_tokens` | Context token budget applied identically to both arms. Positive. |
| `sources` | 3–6 source objects (below). Fixture order IS retrieval order: the builder assigns rank embeddings so source 0 is the top retrieval hit. |

Each source object:

| Field | Meaning |
|---|---|
| `path` | Invented repo-relative path, unique within the case. |
| `content` | Whole-source body (realistic multi-line code or docs). Non-blank. |
| `language` | Language tag carried into the store and rendered headers. |
| `abstract` | L0 summary (the `purpose:` line in the progressive arm). Blank = this source has no summary row. |
| `overview` | L1 summary. Must be set iff `abstract` is set (atomic pair — the builder rejects one without the other). |

Roughly half the sources in each case carry summaries, at varying positions,
so both arms of the summary ladder (summarized and unsummarized sources) are
exercised in every stratum.

### Strata

- `content_only` — the answer exists ONLY in a source body, never in any
  path, abstract, or overview. This is the #246 failure stratum: when the
  progressive renderer demotes the answer-bearing source to an
  orientation-only block, the answer becomes unreachable and the label
  shows it. Authoring rule: grep the answer literal against every path,
  abstract, and overview in the case — zero hits allowed.
- `metadata` — the answer is orientation-shaped ("which file defines X")
  and answerable from paths and summaries alone; progressive should match
  or beat flat here at fewer tokens.
- `distractor` — 4–6 sources where most are genuinely near-topic noise
  (same domain vocabulary, tempting wrong numbers); the golden criteria
  name the one real source.
- `no_answer` — a plausible question the corpus genuinely cannot answer;
  the criteria require an explicit "not in the provided context" answer
  and forbid inventing one (restraint framing, mirroring the golden-empty
  xLAM traces). Authoring rule: grep that no source mentions the answer or
  a near-synonym.

### Reachability (the rule that keeps a case alive)

A case is only useful if at least one arm can actually answer it. The flat
arm renders full bodies in retrieval order until the budget runs out, which
in this fixture shape means it reaches roughly **index 0-1**; the
progressive arm renders full bodies for the first source or two and demotes
the rest to orientation-only blocks. Two authoring rules follow:

1. **Place the answer-bearing source at index 0 or 1.** Index 2 survives
   only while the earlier bodies stay short (`co-signature-header` is the
   one committed case relying on that) and dies the moment an earlier body
   grows.
2. **After building, grep the answer literal in the built
   `<case-id>-flat.json`.** Absent from BOTH arms means the case is dead:
   it contributes a guaranteed 0/0 delta, and a rubric demanding the value
   then penalizes a model that correctly says "not in the provided
   context". Fix the case before labeling — usually by moving the
   answer-bearing source earlier.

Three dead cases have been caught so far, all the same shape — an answer
source sitting past the flat arm's reach with a summary that deliberately
omitted the value. Two were `content_only` cases the author caught while
building; the third, the `distractor` case `dx-dedup-window`, survived the
author's own checks and was caught in review. None is visible in
`cases.json`; only the built trace shows it. Run the grep even when the
case looks obviously fine.

### Guessability: four channels, not three

Answer values must not be derivable from any of:

1. the project name,
2. the question wording,
3. common convention, or
4. **a cross-reference from another source admitted in the same arm.**

An arm that can emit the right answer without ever seeing the answer-bearing
content inflates exactly the arm the stratum exists to stress. Channel 1 is
why the signing header is `X-Ledger-Digest` and not the `X-Beacon-*` a model
could guess from the project name.

Channel 4 is live in the committed corpus: `md-deploy-doc` asks which
document describes deployment, and its flat arm admits `docs/README.md`,
whose line 25 reads "See the operations guide for deployment." A flat-arm
model can answer "the operations guide" from that cross-reference alone,
without the `docs/operations.md` path ever appearing in its context — and a
lenient labeler reading the criteria "Identifies docs/operations.md" might
accept it. The case stays as-is (it is still a legitimate orientation
discriminator: only the progressive arm sees the actual path), but label it
strictly — credit the flat arm only for the path, not for the paraphrase.
New cases should avoid the leak rather than document it.

Content rules: invented project material only — no real client data, no
secrets, no real hostnames or keys (example domains per RFC 2606).

## Building the traces

```
go run ./cmd/llm-bench -assembly-build docs/llm/assembly-corpus/cases.json \
  -assembly-out docs/llm/assembly-corpus/traces
```

The build is deterministic (pinned store timestamps, rank-derived
embeddings): rebuilding from an unchanged `cases.json` reproduces the trace
files byte-for-byte. Both arms of a pair share `assembly_eval.pair_id` and
identical `candidate_ids`; the report asserts this and invalidates any pair
where the arms diverge.

## Labeling flow

Three steps, mirroring the existing calibration workflow. Output paths
below sit under `docs/llm/calibration/`, which is gitignored — capture and
label files stay local.

1. **Capture** — replay every trace (both arms) against a local candidate
   model and freeze the transcripts:

   ```
   go run ./cmd/llm-bench -calibrate-capture \
     -traces "docs/llm/assembly-corpus/traces/*.json" \
     -models <model> \
     -labels-out docs/llm/calibration/assembly-artifacts.jsonl
   ```

2. **Blind-label** — render a worksheet with model identity hidden, fill in
   the quality labels by hand, then ingest it back. Note the worksheet's
   `trace:` line shows the trace ID, whose `-flat`/`-progressive` suffix
   reveals the arm — labeling is blind to the model, not to the arm, so
   score strictly against `final_answer_criteria`:

   ```
   go run ./cmd/llm-bench -blind-render \
     -artifacts docs/llm/calibration/assembly-artifacts.jsonl \
     -report docs/llm/calibration/assembly-worksheet.md

   # fill in the worksheet, then:

   go run ./cmd/llm-bench -blind-ingest \
     -worksheet docs/llm/calibration/assembly-worksheet.md \
     -artifacts docs/llm/calibration/assembly-artifacts.jsonl \
     -labels-out docs/llm/calibration/assembly-labels.jsonl \
     -labeler <your-name>
   ```

3. **Report** — pair the arms, join the labels, and apply the
   pre-registered decision rule:

   ```
   go run ./cmd/llm-bench -assembly-report \
     -labels docs/llm/calibration/assembly-labels.jsonl \
     -artifacts docs/llm/calibration/assembly-artifacts.jsonl \
     -report docs/llm/calibration/assembly-report.json
   ```

## Pre-registered decision rule

Per candidate model (models are never pooled), verbatim from the report's
`decision_rule` field:

> minimum 60 complete pairs per model; quality-improved: CI low > 0 and
> median token reduction >= 20%; efficient-noninferior: CI low > -0.10 and
> reduction >= 20%; regressed: CI high < -0.10; else inconclusive

Where the CI is the bootstrap confidence interval on the paired quality
delta (progressive minus flat, on the harness's 0..1 AnswerQuality scale;
-0.10 is the non-inferiority margin) and token reduction is the median of
`1 - progressive_tokens / flat_tokens` across complete pairs.

The 60-pair minimum is enforced in code, not by convention: any model with
fewer than 60 complete labeled pairs is reported as `insufficient-corpus`
and no other decision word is used. A complete pair means both arms
captured AND both arms labeled; pairs missing an arm or a label count as
pairing gaps, and pairs whose arms disagree on candidate IDs count as
invalid — both are reported and excluded from the deltas.

The committed 16-case seed therefore yields at most 16 pairs per model:
enough to exercise capture, labeling, and reporting end to end, and
deliberately not enough to claim anything about progressive rendering.

## Regime warning for whoever grows this corpus

Measured over the 16 committed pairs (estimated prompt tokens straight out
of the built traces):

| Statistic | Value |
|---|---|
| Median token reduction | +0.003 |
| Mean token reduction | -0.051 |
| Range | -56.3% to +27.6% |
| Cases at or above the 20% threshold | 1 of 16 |
| Mean reduction, 3-source cases (n=6) | +0.049 |
| Mean reduction, 4-source cases (n=5) | -0.033 |
| Mean reduction, 5-source cases (n=4) | -0.184 |
| Mean reduction, 6-source case (n=1) | -0.197 |

The mechanism: at a 512-token budget the flat arm simply truncates after
about two full bodies and stops paying for the rest, while the progressive
arm emits an orientation block (header, purpose, language, indexed,
summary) for EVERY selected source. The more sources a case has, the more
orientation overhead progressive pays against a flat arm that already
stopped — so reduction trends negative as source count rises.

Both `quality-improved` and `efficient-noninferior` require a median
reduction of at least 20%. At this corpus shape only `regressed` and
`inconclusive` are reachable.

**Do not tune the pre-registered rule to fix this.** The thresholds were
registered before any labels existed and moving them after seeing the data
is how a null result gets laundered into a positive one. If a positive
verdict is reachable at all, it has to come from cases where progressive
has real budget pressure to exploit: corpora where the candidate set is far
larger than the budget, so the flat arm loses whole sources that
progressive can still orient the model toward. Growing this corpus with
more 3-to-6-source, 512-token cases will not produce one.
