### Security — Exact configured-credential blocking in tool calls (#424)

Golem's `-interceptors` chain now starts with a `configured_secrets` guard that
blocks a tool call carrying a credential go-llm resolved from `models.json`:
every configured provider's effective `api_key`, and each value a `${NAME}`
reference in an `api_key` resolved to. Matching is exact and does not depend on
the value's shape: a case-sensitive substring of the call's raw arguments, of
every decoded JSON key and string value, and of the call's ID, function name
and type. Values shorter than eight bytes get no exact protection. The guard
runs on the agent and on dispatch children, on every path that builds an
orchestrator, including `/model set`.

A match drops the whole model response before it is recorded and before any
of its calls is shown, planned, approved or run, so no call in it runs, clean
calls beside it included; answer text the model already streamed is outside
this check. The run fails with the fixed `run blocked: sensitive content detected` message,
the goal is kept out of CLI history, and the content-full trace is not
written, as for a Secrets block. A blocked dispatch child reports
`Partial result unavailable: blocked: sensitive content` instead of falling
back to its last message. Findings carry only the interceptor
`configured_secrets` and the rule `configured_secret`, never the value or where
it came from. The startup notice lists the guard first, with no member count:
`interceptors: enabled (configured_secrets, zero_width, ...)`. The guards-only
line now ends `(always on; -interceptors adds detectors, secrets, configured secrets, canary)`.

This protection is available, not on by default: without `-interceptors`
nothing checks tool arguments for configured credentials. It does not scan
input, answers, thinking or tool results, and it does not inspect request
bodies Golem sends to providers; that outbound check is a separate, later
#424 change. The set is fixed when Golem starts, so a credential changed in
the environment takes effect at the next start.

#### Library

- `config.Config.SecretMatcher()` returns an immutable `func(string) bool`
  over the resolved set, never the values, and
  `interceptor.NewConfiguredSecrets` builds the guard from it. Loading a config installs nothing: install the guard on the
  orchestrator and pass it to `agenttools.NewDispatch`.
- When an interceptor implementing the new `agent.SensitiveTextInterceptor`
  blocks, that hook keeps only bounded metadata from every finding
  (interceptor, rule, verdict and numeric fields) and drops all free text, from
  every interceptor and the observer, before risk, telemetry and errors are
  built. Its hook and observer errors become `agent.ErrDiagnosticsSuppressed`.
- `interceptor.SensitiveBlocked` reports a `secrets` or `configured_secrets`
  block. A dispatched child blocked either way, `Secrets` included, now
  reports the fixed marker instead of its last message, with no setting.
