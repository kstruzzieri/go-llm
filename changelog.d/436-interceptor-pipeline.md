### Added — agent interceptor and gateway pipeline (#436)

`agent.Orchestrator` gains an opt-in deterministic middleware seam. Nothing
is active unless a consumer installs interceptors; Golem wiring lands in a
follow-up after #372. The types below are public contract for #438, #451
and #452.

- `agent.Interceptor` (`InspectInput` / `InspectOutput` / `InspectToolCall`),
  installed with `agent.New(..., agent.WithInterceptors(...))`. Every
  interceptor runs on every hook in registration order, even after a block
  or an earlier error; findings carry a verdict (`VerdictAllow` /
  `VerdictTag` / `VerdictBlock` / `VerdictAbort`), a 0-100 risk contribution,
  the provenance (`agent.Origin`) of the inspected content, and a validated
  target (`agent.TargetKind` plus state index, tool-call id, and context-set
  alternative).
- Inspection at ingress on frozen values: the initial input before assembly;
  the collected model response (content, thinking, tool calls) before it is
  recorded or reaches `OnStep` (streamed `OnToken`/`OnThinking` deltas
  precede inspection and cannot be retracted), including the partial output
  of a failed stream;
  each tool call before `Plan` and approval on both dispatch paths, with
  `Plan`, the approver, `OnToolCall` and `OnStep` receiving their own copies;
  each tool result before the observer, State and the governor, with
  `OnToolResult` receiving a clone of the final observation; verifier
  output before it is appended.
- Fail closed: a terminal block or an abort returns `*agent.BlockedError`
  with the partial `Result` and a `"blocked"` event, joined with any
  interceptor errors; a blocked tool call or tool result becomes a fixed
  model-visible error observation (`ToolCallRecord.Blocked`). A malformed
  `ContextSet` is now rejected before any consumer sees it.
- Tags append a fixed trailer to the observation and every mixed
  alternative (widening `OutputCap` by the trailer bytes per group) and emit
  `InterceptionEvent` to an `InterceptionObserver`.
- Per-run `RiskReport` on `Result.Risk` (nil when nothing was found); an
  optional `RiskApprover` receives the cumulative snapshot with each
  approval; `RunScopedInterceptor.ForRun(ctx, RunScope)` returns a per-run
  instance and a system-prompt addendum (a canary nonce's seam).
- Provenance: `ToolResult.Origin` per invocation, else the tool's
  `OriginTool`, else unknown; invalid values normalize to unknown, which
  detectors treat as foreign. Every built-in in `agent/tools` declares:
  workspace for file, search, exec, background, scratch, retrieve and memory
  tools; model for `delegate_code` and `dispatch`; MCP observations are
  foreign. Golem's own tool wrappers follow in the wiring follow-up.
- `tools.NewDispatch(..., interceptors ...agent.Interceptor)` installs them
  on every child; child envelopes report `risk_score` when non-zero.
- New `agent/interceptor` package: `ZeroWidth`, `Encoding`, `Typoglycemia`
  (`Defaults()`); strong phrases block foreign/unknown content and tag the
  rest, weak indicators only tag, a strong phrase dominates a weak one.
