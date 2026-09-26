### Changed — Conversation revisions survive deletion (#542)

**Breaking:** `conversation.Store.Save`, `SQLiteStore.Save`, and
`golem.SessionStore.Save` now return `(int64, error)`. Retained callers must assign
the returned revision after success; custom stores must enforce CAS and return
the committed revision, or zero on error.

A constant-size durable revision floor prevents stale snapshots from overwriting
a recreated conversation ID. Migration v5 preserves live conversation and search
data, and snapshot, summary, revision metadata, and search updates remain atomic.
Golem and CLI sessions retain the returned revision after saving or clearing.

Upgrade and restart all writers sharing a sessions database together and discard
pre-upgrade snapshots. The floor starts at 1 or higher, so creation never commits
revision 1, and released v0.1/v0.2 upserts and v0.3 creates fail with an upgrade
message even on a fresh database; matching v0.3 updates still work. Revisions
erased before migration cannot be recovered, and Delete remains unconditional.
See the conversation persistence section in `docs/library.md` for consumer
migration and exhaustion behavior.
