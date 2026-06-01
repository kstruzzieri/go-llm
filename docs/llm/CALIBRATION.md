# Judge Calibration Workflow

The `cmd/llm-bench` LLM-judge scorer needs periodic calibration against
human-labeled ground truth before its agreement numbers can be trusted in
[recommendation.md](recommendation.md). This doc describes the two-phase
workflow.

## Storage layout

| Path | Checked in? | Purpose |
|---|---|---|
| `docs/llm/CALIBRATION.md` | ✅ this file | Format spec + workflow |
| `docs/llm/calibration/artifacts.jsonl` | ❌ gitignored | Frozen candidate outputs (Phase 1 output) |
| `docs/llm/calibration/labels.jsonl` | ❌ gitignored | Human labels (you hand-edit) |
| `docs/llm/calibration/reports/*.md` | ❌ gitignored | Per-run calibration reports |

## Phase 1 — Capture frozen artifacts

Run this when you change the set of candidate models you care about.

```
llm-bench -calibrate-capture \
  -traces 'docs/llm/traces/*.json' \
  -models 'ollama/qwen3-coder-next:latest,ollama/gemma4:31b' \
  -labels-out docs/llm/calibration/artifacts.jsonl
```

Writes one JSON object per line to `artifacts.jsonl`:

```json
{
  "trace_id": "fim-go-handler-001",
  "candidate_model": "ollama/qwen3-coder-next:latest",
  "artifact_hash": "sha256:...",
  "trace": {
    "id": "fim-go-handler-001",
    "system": "...",
    "turns": [{"role": "user", "content": "..."}],
    "golden": {"final_answer_criteria": "...", "tool_calls": ["read_file"]}
  },
  "actual_final_answer": "...",
  "actual_tool_calls": ["read_file", "search_code"],
  "actual_transcript": [{"role": "...", "content": "..."}],
  "captured_at": "2026-05-25T14:00:00Z"
}
```

## Phase 2 — Label the artifacts

Open `artifacts.jsonl` and create a parallel `labels.jsonl` where each line
adds `expected_answer_quality` exactly equal to one of `{0.0, 0.5, 1.0}`:

```json
{
  "trace_id": "fim-go-handler-001",
  "candidate_model": "ollama/qwen3-coder-next:latest",
  "artifact_hash": "sha256:...",
  "expected_answer_quality": 0.5,
  "label_notes": "correct high-level answer, missed required caveat",
  "labeled_at": "2026-05-25T14:30:00Z",
  "labeler": "manual"
}
```

The `artifact_hash` MUST match the corresponding artifact's hash — that's
how the calibration loop knows the label is still valid for the frozen
output. Each current artifact hash may appear only once in `labels.jsonl`;
duplicate matched labels are rejected so one artifact cannot be double-counted.
Labels with `expected_answer_quality` outside `{0.0, 0.5, 1.0}` are rejected.

### How to label

- **1.0** — fully satisfies the rubric with no material technical error.
- **0.5** — partially correct but missing important requirements, or
  contains a contained technical flaw.
- **0.0** — wrong, fabricated, absent, or materially misleading.

Round 2 uses `label_notes` tokens for calibration-report stratification
without changing the label schema:

- `r1-anchor` marks the 60 labels carried from Round 1.
- `judge-validation-fixture` marks curated P1 adversarial fixtures that
  validate the judge and stay out of the P2 accepted-run corpus unless they
  are real captured artifacts.

## Phase 3 — Calibrate

Run this when you change the judge model (or after material judge prompt
changes).

```
llm-bench -calibrate \
  -labels docs/llm/calibration/labels.jsonl \
  -artifacts docs/llm/calibration/artifacts.jsonl \
  -judge-model ollama/gemma4:31b \
  -calibrate-agreement exact
```

Writes a markdown report to
`docs/llm/calibration/reports/YYYY-MM-DDTHHMMSSZ-<slug>.md`. If a report
already exists for the same timestamp and judge model, a numeric suffix is
added so prior reports are not overwritten. Verdict is one of:

- **PASS** — exact categorical agreement ≥85% on ≥50 matched non-stale
  labels, borderline/fail agreement ≥80% when that subset is present, and
  no known subtle-bug fixture judged as `1.0`.
- **FAIL** — enough labels exist, but overall exact agreement, the
  borderline/fail gate, or a known subtle-bug fixture gate failed.
- **INSUFFICIENT_LABELS** — fewer than 50 matched non-stale labels.
  Never claims PASS. Label more artifacts and rerun.

Primary agreement is `judge == expected`. The report also prints the retired
tolerance diagnostic, `|judge - expected| <= 0.25`, so historical comparisons
remain visible without gating the verdict. `-calibrate-agreement tolerance`
is retained for historical comparison runs and must not be used for Round 2
acceptance.

The report includes overall exact agreement, R1-anchor agreement,
borderline/fail agreement (`expected_answer_quality` of `0.0` or `0.5`),
clear-1.0 agreement, harsh and lenient disagreement counts, stratified gate
failures, and a roll-call for the known subtle-bug fixtures (`fa-f03`,
`fa-c05`, `fa-g04`).

A label is "stale" iff its `artifact_hash` doesn't match the current
`artifacts.jsonl`. Stale labels are listed in the report and excluded from
agreement. Usual cause: candidate model upgraded silently (floating tag).
Mitigation: pin candidates to non-floating tags or digests.
Labels whose candidate model is the same selector as the judge model are also
skipped and reported separately; they are excluded from agreement and label
sufficiency math.

### Stability runs (diagnostic)

```
llm-bench -calibrate ... -judge-stability-runs 3
```

Runs the judge exactly `M=3` times per artifact (cache bypassed, results
NOT persisted) and reports `max - min` spread across those samples. The
first sample is also the score used for agreement. Does not affect the
PASS/FAIL verdict — it's a separate "is the judge stable?" check.

## Floating-tag caution

Two distinct floating-tag risks:

1. **Judge model drift** — `ollama pull gemma4:31b` may resolve to a
   different digest. Mitigation: the judge cache key includes the model
   digest when `/api/show` exposes one, so cached judgments from a
   different digest miss instead of being reused incorrectly.
2. **Candidate model drift** — `ollama pull qwen3-coder-next:latest` may
   resolve to a different digest, producing different artifacts that no
   longer match your old labels. Mitigation: `artifact_hash` mismatch
   surfaces as "stale labels" in the report.

For trustworthy accepted-run calibrations, pin both judge and candidate
to non-floating tags or digests.

## Active labeling loop (deferred)

`-calibrate-suggest` (an "active learning" subcommand that highlights
artifacts most worth labeling) is a planned follow-up. For now, label
incrementally — start with 20 artifacts, run `-calibrate`, then label
artifacts where the judge disagreed with your label or where exact labels
are hardest to assign, especially borderline/fail cases (`0.0` or `0.5`).
