### Fixed — Serialize the transcript audit-column upgrade (#590)

Opening a legacy transcript database from two processes at once no longer fails
with `duplicate column name`. The audit-column upgrade now re-checks the
conversations table under SQLite's write lock and adds every missing column in
one transaction; a failed upgrade adds none. Opening an already-upgraded
database still takes no write lock.
