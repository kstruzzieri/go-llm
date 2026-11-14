### Added — Conversations remember whether outside content entered them (#583)

Conversations now store their exposure record on the conversation row, bound to the row's revision, so a stale or foreign write cannot hand a conversation a record it did not earn. The Golem runtime carries the record from turn to turn, across resume and across compaction: a turn starts from the stored record and saves the record its run reports, and a failed run, or one with no answer, leaves the stored record as it was. `Runtime.ThreadExposure` reads the record the next turn on a thread would start from.

- The conversation store schema moves to version 6 with two added nullable columns.
- Conversations saved before this version, or last written by an older binary, read as unknown (and so as exposed) until a new or cleared conversation replaces them. A custom `SessionStore` that does not keep the record reads the same way.
- Consumers with a custom `SessionStore` should read the "Exposure record" section of `docs/library.md` for what a store and a conversation-writing host must carry.
- `Runtime.ThreadExposure` is read-only: it does not reserve the thread, needs no compression setup and never creates a conversation. A well-formed thread ID with no stored conversation reads as clean.

Nothing changes for a session yet: nothing reads the record until the strict approval mode lands (#584).
