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
