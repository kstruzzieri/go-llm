### Added — Conversations remember whether outside content entered them (#583)

Conversations now store their exposure record on the conversation row, bound to the row's revision, so a write by an older binary reads back as no record rather than leaving a stale record beside newer content. The Golem runtime carries the record from turn to turn, across resume and across compaction: a turn starts from the stored record and saves the record its run reports, and a failed run, or one with no answer, leaves the stored record as it was. `Runtime.ThreadExposure` reads the record the next turn on a thread would start from.

- The conversation store schema moves to version 6 with two added nullable columns.
- Conversations saved before this version, or last written by an older binary, read as unknown (and so as exposed) until a new or cleared conversation replaces them. A custom `SessionStore` that does not keep the record reads the same way.
- Consumers with a custom `SessionStore` should read the "Exposure record" section of `docs/library.md` for what a store and a conversation-writing host must carry.
- `Runtime.ThreadExposure` never writes a conversation row, does not reserve the thread and needs no compression setup; on a runtime with no `SessionStore` it opens the default session database on first use, creating and migrating the file if absent, as `CompactThread` does. A well-formed thread ID with no stored conversation reads as clean.

Nothing changes for a session yet: nothing reads the record until the strict approval mode lands (#584).
