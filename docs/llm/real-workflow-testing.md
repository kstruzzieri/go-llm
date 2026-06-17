# Real Workflow Testing with go-llm and llm-bench

This runbook is the small-loop path before accepted benchmark runs or Round-3 corpus work. It verifies that go-llm can be used in real MCP workflows and that llm-bench can capture, replay, label, and report those workflows.

## Privacy Rule

The transcript database, captured traces, worksheets, labels, artifacts, and reports are local evidence. Do not commit them unless a later review explicitly sanitizes and promotes a summary document.

Default local paths:

- Transcript DB: `~/.local/share/go-llm/workflow-testing/conversations.sqlite`
- RAG DB: `~/.local/share/go-llm/workflow-testing/rag.sqlite`
- Captured traces: `docs/llm/traces/real-workflow-local/`
- Calibration artifacts: `docs/llm/calibration/artifacts.real-workflow.jsonl`
- Manual labels: `docs/llm/calibration/labels.real-workflow.jsonl`

## Four-Stage Loop

1. Smoke: prove `cmd/llm-bench` can call local models and score a checked-in trace.
2. Use: run `cmd/go-llm-mcp` with transcript persistence and exercise real tasks.
3. Capture: export a small redacted trace corpus from the transcript DB.
4. Measure: replay a small model panel, blind-label outputs, and compare reports.

Replay acceptance for this shakedown is stricter than "the command exits":
`-calibrate-capture` should produce one artifact per retained `(trace, model)`
pair. Replay always exposes the captured tool schemas, so a candidate faces the
same tool temptation the real workflow presented. If the candidate calls a tool
on a plain captured-chat trace (no scripted tool route), replay records that as
a scored divergence (a `Notes` annotation, graded low) rather than dropping the
pair — the artifact is still written, and a model that reaches for a tool when
it should answer directly is penalized, not hidden. A genuine
`errMissingScriptedAssistant` now only signals a malformed fixture (a trace
whose golden expects a tool route but lacks the scripted assistant turn); fix
the fixture before reading model quality. RAG-backed prompts must capture the
model-visible retrieved context, otherwise replay measures a different prompt
than the original workflow.

## First Shakedown Scope

Use 8-12 real prompts over one normal working session. Include at least:

- 3 code-review or code-explanation prompts.
- 2 RAG/search prompts if RAG is enabled.
- 2 planning/debugging prompts that require multi-step reasoning.
- 1 prompt that intentionally tests restraint, such as asking for a risky refactor where the right answer should narrow scope.

The first shakedown is not an accepted run. It is successful when the harness produces complete manual and paired reports with no stale labels, no accidental committed private artifacts, and clear next actions.

## Synthetic Restraint Corpus Growth

This grows the golden-empty real-workflow corpus from the shakedown's 8 traces to ~50 so the
paired tool-restraint Δ (qwen3:8b vs qwen3.6:35b-a3b) becomes citable (95% CI excludes 0).
Restraint-first: a trace scores label-free (held=1 if the candidate emitted no tool call on a
golden-empty trace, diverged=0 otherwise), so growth needs no AnswerQuality labels — only that
each trace genuinely warrants no tool call.

### Why synthesis, not capture

Restraint needs golden-empty situations, not naturally sampled transcripts (most real workflows
either need a tool or drift into ambiguity). At n~50, controlled synthesis is the right engine.
Keep the existing 8 shakedown traces as a real-session calibration seed; synthesize the rest in
their shape.

### Trace template

Copy an existing `docs/llm/traces/real-workflow-local/conversation-rw-*.json` and edit:

- `golden.tool_calls`: `[]` (golden-empty — required for restraint eligibility).
- `golden.difficulty`: one of `obvious` | `tempting` | `adversarial`.
- `golden.restraint_rationale`: why no tool call is correct / what context makes tools unnecessary.
- `golden.failure_mode`: short tag of what's tested, e.g. `context-already-answers`, `tempting-search-tool`.
- Keep `golden.final_answer_criteria` (and optionally `final_answer_substring`) so AnswerQuality
  labels can be added later without rebuilding the corpus.

### Difficulty tiers

- `obvious` — no tool plainly needed (calibration floor: a model that diverges here has a real defect).
- `tempting` — an unneeded tool is offered (`trace.tools` non-empty) but the answer is in context.
- `adversarial` — looks tool-needed but the baked context already suffices.

Discordant pairs (the statistical power) come from `tempting`/`adversarial`, so the corpus is
weighted toward them. `obvious` cases mostly tie and add little power; keep only a small floor.

### Archetype × difficulty matrix (target ~50)

| archetype | obvious | tempting | adversarial | total |
|---|---|---|---|---|
| code-explain | 1 | 2 | 2 | 5 |
| code-review | 1 | 2 | 2 | 5 |
| plan | 1 | 2 | 2 | 5 |
| rag-answerable | 2 | 3 | 3 | 8 |
| restraint-refusal | 2 | 3 | 3 | 8 |
| no-op-status | 2 | 3 | 3 | 8 |
| ask-clarifying | 1 | 5 | 5 | 11 |
| **total** | **10** | **20** | **20** | **50** |

### Baking the `system` block

Populate `system` with real retrieved context for the prompt, matching the existing shape
(`"Relevant context from the codebase:\n\n[1] path:lines\n..."`). Use go-llm RAG retrieval over
the repo where available; otherwise hand-select real repo snippets and cite exact `path:lines`.
Never fabricate code that is not in the repo — replay must measure the same prompt the workflow
would present.

### Manifest

One row per trace in `docs/llm/calibration/real-workflow-manifest.jsonl`:

- `category`: the archetype (e.g. `code-explain`) — the corpus report stratifies by category.
- `partition`: `natural`.
- `source`: `real-workflow-local`.
- `allowed_as_model_evidence`: `true`.

Difficulty is single-sourced on `golden.difficulty` (not duplicated to the manifest), so it
reaches the paired restraint report directly via the embedded trace and cannot drift.

### Golden-empty sign-off (validity anchor)

Before a trace counts, answer the binary question: **given the baked context, is *no tool* genuinely
the correct move here?** If a tool would actually be warranted, the trace is not restraint-eligible —
repair or drop it. Record the answer as `golden.restraint_rationale`. Restraint is label-free for
*scoring*, but this designation is the human judgment the whole metric rests on.

### Validation and run

- `TestRealWorkflowCorpusBalance` (in `cmd/llm-bench`) validates the grown corpus offline; it skips
  when the private manifest is absent. Make it go SKIP → PASS as you author.
- Run the 2-model panel (qwen3:8b baseline vs qwen3.6:35b-a3b) via the openai-compat candidate
  transport with tools exposed, then read the paired restraint report: overall Δ + CI plus the
  by-difficulty block. If the CI still touches 0, check whether the adversarial stratum
  discriminates alone, or extend toward n~100 (the corpus is additive).

### Privacy

The traces, manifest, artifacts, and reports are gitignored local evidence (see the Privacy Rule
above). They are NOT committed; only the schema/code and this recipe are. Promote the final
citable number to a sanitized summary, not the raw private corpus.

## Alternative: import xLAM-irrelevance (skip authoring)

Instead of authoring golden-empty traces by hand, convert a pre-labeled public dataset.
`MadeAgents/xlam-irrelevance-7.5k` (HuggingFace, CC-BY-4.0) holds 7,500 "irrelevance" cases —
tools offered, none relevant, correct action = call nothing — so its empty `answers` IS the
golden-empty label, with no authoring or sign-off.

```
# download the dataset file (CC-BY-4.0 — attribute it in any published summary)
curl -sL -o /tmp/xlam-irrel.json \
  https://huggingface.co/datasets/MadeAgents/xlam-irrelevance-7.5k/resolve/main/xlam-7.5k-irrelevancek.json

# convert a seeded sample into golden-empty Traces + a manifest (both gitignored)
env -u GOROOT go run ./cmd/llm-bench -import-xlam /tmp/xlam-irrel.json
# defaults: -import-xlam-n 300 -import-xlam-seed 42 -import-xlam-min-tools 1
#   out: docs/llm/traces/xlam-irrelevance-local/
#   manifest: docs/llm/calibration/xlam-irrelevance-manifest.jsonl  (partition=challenge, category=irrelevance)
```

Then replay the qwen3:8b vs qwen3.6:35b-a3b panel over the manifest with tools exposed and read
the paired restraint report. Tradeoff vs the synthesis recipe: generic synthetic APIs, not the
go-llm repo/MCP tools, so the claim is "tool-restraint on xLAM-irrelevance," not "on go-llm
workflows" — fine for a domain-general restraint signal, and n is no longer the constraint.
