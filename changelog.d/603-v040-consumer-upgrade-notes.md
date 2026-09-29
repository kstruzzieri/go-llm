### Changed — v0.4.0 consumer upgrade notes (#603)

Read this before upgrading from v0.3.0. Consumers on older pins must also apply
the v0.3.0 upgrade notes.

- Build consumers with Go 1.27 or newer; update their module directives, CI and
  build images together (#565).
- `conversation.Store.Save`, `conversation.SQLiteStore.Save` and
  `golem.SessionStore.Save` now return `(int64, error)` (#542). Retain the returned
  revision after success instead of incrementing locally. Custom stores must
  enforce atomic CAS and prevent revision reuse after deletion/recreation; every
  error returns zero and leaves storage unchanged. Upgrade all writers sharing
  a sessions database together and discard pre-upgrade snapshots.
- Replace `-allow-destination "provider=URL"` with the canonical
  `-allow-destination "provider/https://host"` form (#501).
- Use keyed literals for `agent.Result`, `agent.ToolResult`,
  `agent.InspectedMessage`, `consult.Consultant` and `consult.Evidence`, which gained
  fields. `consult.Consultant` now contains a slice and cannot be compared with
  `==`/`!=` or used as a map key.
- On Linux/Darwin, Workspace writes to targets admitted absent require an atomic
  no-replace operation (#552). Unsupported operations and concurrent creates are
  refused, including for unconditional `WriteFileAtomic`; there is no plain-rename
  fallback.
- `Orchestrator.Run` seals and cancels its run context on return (#449). Late
  nested admission is refused even through `context.WithoutCancel`; join nested
  work before the parent returns for complete descendant usage.
- Firn's `MemorySessionStore.Save` needs the returned-revision and true CAS
  contract above before its dependency bump. Update callers, fakes and tests,
  including conflict and deleted-ID revision-reuse cases; changing the return
  signature alone is insufficient.

The [full consumer upgrade guide](https://github.com/kstruzzieri/go-llm/blob/develop/docs/releases/v0.4.0.md)
will accompany the v0.4.0 release stamp in #603, including migration details,
other behavior changes and consumer compatibility checks.
