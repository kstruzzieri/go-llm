### Fixed — Detect competing conversation saves (#473)

Conversation saves now compare the loaded revision before replacing a transcript
and return a typed conflict if another writer saved first. Conversation, summary,
revision, and search updates commit atomically. Existing databases migrate to
revision 1 without rewriting their transcripts. `Save` is no longer an upsert:
callers must pass the `Revision` returned by `Load`, and a revision-zero save
over an existing ID is refused as a conflict.

Golem surfaces a rejected raw save as `session_conflict`. In the REPL, a lost
save or a SQLite lock timeout is reported as a turn error instead of a "session
not saved" warning, and a notice explains which history the next turn uses;
other disk failures stay warnings. One-shot `-p` runs imply `-no-session` and
are unaffected. Automatic compression conflicts remain warnings after the raw
turn is durable. Custom session stores must adopt the revision contract; upgrade
all writers together. Deleting and recreating the same ID can reuse revisions
and remains outside the CAS guarantee (#542).
