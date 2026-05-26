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
adds `expected_answer_quality` ∈ {0.0, 0.5, 1.0}:

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
output.

### How to label

- **1.0** — fully satisfies the rubric: every required element present, no
  contradictions, no unsupported claims.
- **0.5** — partially correct: gets the gist or some required elements
  right but misses something material.
- **0.0** — wrong or absent.

## Phase 3 — Calibrate

Run this when you change the judge model (or after material judge prompt
changes).

```
llm-bench -calibrate \
  -labels docs/llm/calibration/labels.jsonl \
  -artifacts docs/llm/calibration/artifacts.jsonl \
  -judge-model ollama/gemma4:31b
```

Writes a markdown report to
`docs/llm/calibration/reports/YYYY-MM-DD-<slug>.md`. Verdict is one of:

- **PASS** — agreement ≥85% on ≥50 matched non-stale labels.
- **FAIL** — agreement <85% on ≥50 matched non-stale labels.
- **INSUFFICIENT_LABELS** — fewer than 50 matched non-stale labels.
  Never claims PASS. Label more artifacts and rerun.

A label is "stale" iff its `artifact_hash` doesn't match the current
`artifacts.jsonl`. Stale labels are listed in the report and excluded from
agreement. Usual cause: candidate model upgraded silently (floating tag).
Mitigation: pin candidates to non-floating tags or digests.

### Stability runs (diagnostic)

```
llm-bench -calibrate ... -judge-stability-runs 3
```

Runs the judge `M=3` times per artifact (cache bypassed, results NOT
persisted) and reports `max - min` spread per artifact. Does not affect
the PASS/FAIL verdict — it's a separate "is the judge stable?" check.

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
artifacts where the judge disagreed with your label or where the answer
falls in the borderline 0.4–0.6 band.
