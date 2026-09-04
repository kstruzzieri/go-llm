### Added — Golem interceptor wiring, risk-aware approver, local tool provenance (#514)

Golem activates the #436 pipeline behind `-interceptors` (off by default).

- `-interceptors` installs `interceptor.Defaults()` on the startup
  orchestrator, on every orchestrator the session factory builds (REPL, `-p`,
  `-goal`, `-plan`, parallel workers), and on every dispatch child; the
  startup notice names the installed detectors. With the defaults, workspace
  content and model-origin tool results are tagged and scored, and an injection
  in a foreign (MCP) result is replaced before the model reads it. Raw model
  output produces no findings under the shipped detectors.
- The interactive approver implements `agent.RiskApprover`: a report with
  findings prints `interceptor risk <score>` between the preview and the
  question on tool-call and interactive `-goal` lock prompts, including
  grant-covered calls. Verifier approval prompts have no report seam;
  non-interactive `-approve-plan-lock` output is unchanged. Prompts without
  findings are byte-identical to before.
- Successful REPL and `-p` stderr footers append ` · risk <score>` when their parent
  run produced findings. Dispatch child scores remain scoped to each result's
  existing `risk_score` envelope field; they do not aggregate into the parent.
- `readyRetrieve`, the agent-memory sidecar wrapper and `submit_plan` now
  declare provenance (workspace; the sidecar forwards the wrapped tool's
  declaration), so their output is tagged rather than blocked as unknown.

#### Not in this change

The headless v1 schema and telemetry are unchanged. An enabled headless run can
have a non-nil internal `Result.Risk`, but v1 omits it. Exposing that report,
buffering streamed tokens for a future output-blocking detector, telemetry
spans for findings, and per-tool thresholds (#439) remain follow-ups.
