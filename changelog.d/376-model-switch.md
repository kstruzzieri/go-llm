### Added — Switch the live model with /model (#376)

- `/model` reports the requested selector, canonical chain, use case and
  strictness, current input ceiling and its source, thinking mode, and the last
  model actually routed; it performs no lookup, probe, or model call.
  `/model set <role|name>` switches the model for the rest of the process.
  Selection resolves against the frozen startup configuration: an exact
  configured role wins and keeps its complete ordered fallback chain, a
  configured provider prefix keeps the entire remaining suffix as one model id,
  and a bare id is qualified only when exactly one provider is configured.
  `models` keys are roles, not aliases; no model name or inventory scan infers a
  provider. Startup recommendation mode becomes a strict configured chain on the
  first successful set. `/new`, `/clear`, and `/resume` do not reset the
  selection, and the command works with `--no-session`.
- Publish the execution inputs that must change together as one runtime
  snapshot. `golem.Configuration` carries System, Tools, ModelOptions,
  Orchestrator, and Budget; `Runtime.ReplaceConfiguration` replaces the whole
  tuple atomically and `Runtime.Budget()` exposes the current value for library
  hosts. Reservation remains the linearization point: a turn reserved before a
  replacement keeps its original caller, options, tools, orchestrator, and
  budget for every step, and compression uses the budget captured with its
  reservation. The existing `Runtime.Replace` API is unchanged and preserves the
  added fields.
- A failed selection preserves the caller, options, budget, tools, conversation,
  session identity, and routing metadata; switching itself neither calls the
  summarizer nor rewrites history. A destination grant explicitly approved while
  preparing that selection remains a session grant, and the failure reports
  `destination grant retained for this session; use /grants clear to revoke`.
- Destination consent is additive. `DestinationGate.Extend` publishes a superset
  manifest against the snapshot it validated while keeping the current
  revocation generation, so requests already in flight keep their capabilities.
  The candidate's routes are unioned with the session's stored edges and decided
  in exactly one consent decision and one publication. Grants for routes
  switched away from remain session authority and are listed by `/grants`;
  `/grants clear` still revokes everything.
- Govern slot capacity lazily for every configured `slot_discovery` provider
  rather than only the startup-active ones. Constructing the source and reading
  cached capacity make no network requests, inactive providers stay silent until
  admitted and used, and every later probe remains guarded by the destination
  gate. Startup inventory refresh stays limited to active providers.
- With default `-dispatch`, children follow the switched parent: the dispatch
  tool is rebuilt in place at its own index with the new chain, caller, child
  ceiling, and slot-derived fan-out, leaving every other tool, the mount
  counters, and the child-visible read-only tools where they were. An explicit
  `-dispatch-role`, delegate roles, summarizers, embeddings, retrieval, and
  grounding keep their independent startup routes.
- Accepted thinking controls carry forward and are re-gated against the new
  chain; a chain with no thinking support clears them with the existing notice.
  A value the session already rejected is not resurrected from the startup
  `-think` flag.
