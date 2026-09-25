### Fixed — Serialize feedback, fingerprint, and routing-feedback migration runners (#549)

Feedback, fingerprint, and provider routing-feedback stores now claim each
migration version under SQLite's write lock before applying schema changes.
Concurrent openers skip steps another opener committed; failed steps roll back
both the version claim and schema changes while earlier steps remain intact.
Constructor contexts now reach the migration runners, and current-schema opens
only read.

Provider routing-feedback legacy tables retain column/CHECK validation, rows,
and existing indexes. Validation and creation of missing baseline indexes now
commit with the v1 claim, so incompatible tables are never stamped as migrated.

Callers must configure a positive `busy_timeout` on every migrating connection
(e.g. `_pragma=busy_timeout(5000)` with modernc SQLite) and complete journal-mode
setup before concurrent opens. Lock and I/O errors still propagate.

`provider.OpenSQLiteFeedbackStore`, `memory.OpenHardenedDB`, and the Golem
session/feedback, MCP retrieval-feedback, and transcript openers now set
`busy_timeout` in the connection DSN instead of with a one-off PRAGMA. The
`journal_mode=WAL` PRAGMA previously ran without a busy handler, so reopening
an existing WAL database while another connection held its lock failed at once
with `SQLITE_BUSY`. The one-off PRAGMA also did not survive connection
replacement: `database/sql` discards a connection after a context-cancelled
statement, and the replacement started with `busy_timeout=0`.

This does not change RAG migrations, serialize concurrent `journal_mode=WAL`
setup, or serialize the transcript store's legacy audit-column upgrade.
