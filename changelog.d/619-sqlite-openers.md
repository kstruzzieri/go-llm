### Fixed — SQLite stores open concurrently and on Windows (#619)

- Two processes opening a new transcript, memory, feedback, session,
  routing-feedback or RAG database at the same time no longer fail with
  `SQLITE_BUSY`, as long as the other opener finishes within the store's busy
  timeout. For example, two MCP servers starting on a new transcript path both
  start. Agent-memory record stores (`memory.OpenRecordStore`) can still fail a
  concurrent first open during signing initialization (#631).
- File-backed stores open on Windows. The RAG index had failed to open there
  since v0.1.0, and the other stores since v0.4.0. UNC and device paths
  (`\\server\share\...`, `\\?\...`), including `file:` URIs that name them, are
  now rejected with an error: use a local drive path. This includes a relative
  or rooted `file:` URI when the working directory is a UNC share. A drive
  letter mapped to a network share is not detected and has the same WAL limits.
- A relative path now works for read-only RAG stores
  (`rag.OpenSQLiteStoreReadOnly`).
- An open no longer fails when another process closes the same database at the
  same moment (a WAL sidecar disappeared before its permissions were tightened).
- A store whose switch to WAL reports any journal mode other than `wal` now
  fails to open instead of silently running with a rollback journal. This
  includes `file:` URIs with `immutable=1`, which used to open an existing WAL
  database read-only through `provider.OpenSQLiteFeedbackStore`,
  `rag.NewSQLiteStore`, `transcript.Open` or `memory.OpenHardenedDB`, and
  `nolock=1`, which used to create a new database in rollback mode with locking
  off. For read-only access, pass your own handle to
  `provider.NewSQLiteFeedbackStore` or use `rag.OpenSQLiteStoreReadOnly`.
- Opening a store now stops waiting for the WAL switch at the caller's context
  deadline, if that is sooner than the store's busy timeout. An open that hits
  the deadline while another process holds the database can report
  `SQLITE_BUSY` rather than `context.DeadlineExceeded`.
- `rag.OpenSQLiteStoreReadOnly` now accepts a `file:` URI as well as a path. It
  always opens read-only and immutable with a private cache, whatever the URI's
  own `mode` or `cache` parameters say. Caller-supplied `_pragma` URI options
  are rejected before connection setup, so they cannot execute SQL that
  modifies the source database.
- Plain read-only RAG paths retain filesystem symlink and `..` resolution,
  so opening an index selects the same database as the supplied path.
- `rag.NewSQLiteStore("")` (a temporary database) now keeps one connection, so
  the store no longer sees an empty, unmigrated database on a second pooled
  connection.
