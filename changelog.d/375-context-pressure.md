### Added — Inspect the last assembled context estimate (#375)

- Add `/context` to display the latest attempted turn's last assembled request,
  human step number, pressure classification, input estimate and retained bucket
  estimates. It performs no model, summarizer, registry, or session-store calls
  and works with `--no-session` and pressure warnings disabled.
- Keep historical samples through compaction and model or observer failures;
  successful `/new`, `/clear`, and `/resume` clear them. A turn that fails before
  assembly has no sample. Configured limits are shown separately from the
  retained assembly budget and `/compact`'s stored-history estimates.
- Retain value-copy bucket accounting in `agent.Pressure` for request observers
  without adding it to JSON traces or telemetry. Assemblies lacking a valid
  complete breakdown report it as unavailable.
