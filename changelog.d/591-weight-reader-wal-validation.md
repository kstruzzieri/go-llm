### Fixed — Validate the feedback weight reader schema through the WAL (#591)

`feedback.NewSQLiteWeightReader` now validates the schema on its serving
read-only connection, which reads committed WAL pages under SQLite's locks,
instead of an immutable preflight that ignored the WAL and so missed
migrations not yet checkpointed into the main file. Concurrent Golem sessions
on one workspace no longer start with behavioral feedback disabled
(`version 0, want 1`). The reader rejects an empty or truncated database file,
or a path that is not a regular file, before opening it, and resolves a
relative path against the working directory. It may now create SQLite's `-shm`
file, and an empty `-wal` for a WAL-mode database that has none, beside a
database it rejects; outside a concurrent truncation of the main file, it
still never writes or deletes the main file or an existing WAL. Golem no
longer checkpoints before opening the reader.
