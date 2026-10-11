### Security — SQLite stores secure the file SQLite opens, not the path as written (#648)

The stores that promise private (0600) database files used to create and
chmod the path exactly as given, while SQLite opened the file that path
resolves to. The two differ in three cases, and in each the real database
and its `-wal`/`-shm` sidecars could be left at the default 0644:

- a `file:` URI, such as `go-llm-mcp -conversation-db file:/data/t.db`: the
  store created a 0600 decoy named after the URI in the working directory,
  and SQLite created the real database beside it at 0644;
- a path with `..` after a symlink, such as `link/../t.db`: the store secured
  a file under the link's target while SQLite opened `t.db` in the working
  directory;
- a symlinked final component: SQLite names the sidecars after the link's
  target, so loose sidecars there were never secured.

This covers the transcript store (`transcript.Open`, `go-llm-mcp
-conversation-db`), agent memory (`memory.OpenRecordStore`,
`OpenHardenedDB`, `PrepareDBFile`, `SecureDBFiles`, `go-llm-mcp
-agent-memory-db`), MCP retrieval feedback, and Golem's session, feedback,
index, checkpoint and memory stores. Each now resolves the file SQLite will
open first, the way SQLite does, and secures that file and its sidecars;
the stores that open the database themselves also fail the open if SQLite
then reports a different file. A relative `file:` URI is pinned to the
working directory at open time. `memory.OpenHardenedDB` now also secures
existing `-wal` and `-shm` files during the open, so one it cannot chmod
fails the open rather than the caller's next `SecureDBFiles`.

Related fixes:

- An agent-memory store opened by `file:` URI kept its private signing key
  under the decoy directory. Keys now live beside the database SQLite opens.
  A store whose keys are still in the old directory fails to open with an
  error naming the directory and the `mv` to run; go-llm does not move
  private keys itself. An agent-memory path that names no file (an in-memory
  database) now needs an explicit `KeyDir` or injected credentials.
- Golem's `-feedback-db` workspace check now applies to the file SQLite
  opens. Before, `file:<workspace>/x.db` (with Golem started outside the
  workspace), or a URI whose `..` follows a symlink into the workspace,
  passed the check and wrote inside the workspace.
- Golem's feedback store with a `file:` path failed after creating the real
  database at 0644; its writer and reader now use the same file.
- Golem's feedback workspace check preserves the URI options passed to the
  opener, including read-only mode, locking and busy timeout settings.
- Hardened openers require an existing database for URI `mode=rw` or
  `mode=ro`, before preparing any file or directory.

Newly rejected before anything is created: `file:` URIs with a `vfs`
parameter, a repeated `mode` parameter, a NUL byte, a host other than
`localhost` or any user information, a `#` fragment, or a query Go and
SQLite would read differently (such as one containing `;`).
