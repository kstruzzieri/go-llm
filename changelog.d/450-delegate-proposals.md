### Security — Authenticate and retain delegate proposals (#450)

Successful `delegate_code` results now carry signed, structured evidence binding the exact proposal, prompt, actual model, and completion time. Delegate generation reports signing configuration failures explicitly; the shared typed verifier rejects invalid proposal content, prompt, model identity, timestamp, and signatures. Accepted evidence is retained in runtime results and content-full traces without entering provider prompts or content-light telemetry.

Each delegate tool creates a fresh in-memory HMAC identity by default. Retaining its verifier retains symmetric signing capability; persisted traces require that matching key and become unverifiable if it is lost. Callers can instead supply matching persistent HMAC or Ed25519 identities. This change adds evidence only; it does not add an apply-time gate.

`ToolResult` and `ToolCallRecord` now contain a `json.RawMessage` and therefore cannot be compared with `==` or `!=`; compare fields or use an appropriate deep comparison. Ordinary results and records still omit provenance from JSON.
