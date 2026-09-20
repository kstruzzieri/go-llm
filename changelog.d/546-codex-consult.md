### Added — Codex subscription consultation (#546)

Add the `codex` consultant adapter for native Codex CLI 0.153.4 and requested model
`gpt-6-astra`, with explicit `trusted_vendor_runtime` consent in addition to
process-egress consent. Advisory text uses the existing bounded runner,
interceptors, unsigned receipt and next-goal fence. Native vendor configuration
and host-resource access remain trusted; no serving-model or billing attestation
is inferred from the transcript.

Add opt-in `transport: "app-server"` with an independently pinned stdio RPC
profile, bounded model discovery, one ephemeral text turn and bounded shutdown.
The actual `/consult` path displays `codex app-server 0.153.4`, retains mandatory
interceptor checks and stages advice for one goal. This does not add `models.json`
routing. Omitted/empty transport retains exec; remove the entire transport key
before rolling back to the old strict configuration parser. Receipts distinguish
App Server usage absence from measured zero without changing the answer hash.

Require independent stdout/stderr EOF checks so a nonzero exit cannot hide an
incomplete pipe drain. Preserve Claude configuration and admission behavior.

Identify the first App Server rejection with fixed phase and validation-point
labels, including MCP startup validation, error notifications and system-error
status, without exposing vendor diagnostics or changing admission rules.

Add optional `disabled_mcp_servers` for Codex App Server consultants. Validated,
bounded server names become per-launch disable overrides; native global settings,
the version probe and failed-startup admission remain unchanged. Omitted or empty
lists preserve the existing profile. Remove the key before using an older binary.
