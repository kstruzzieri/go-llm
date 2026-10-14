### Fixed — concurrent first opens of an agent-memory record store (#631)

Processes that open one new agent-memory record store at the same time
(`memory.OpenRecordStore`, for example two MCP servers starting with agent
memory on a fresh path) no longer fail with
`memory.ErrIncompleteInitialization` and a request for an operator recovery
that was not needed. The first opener now claims signing initialization inside
the database's write lock before it creates the signing key. The others wait
for it, up to the store's 5 s busy timeout, and then open the initialized
store. If that initialization, a large legacy import for example, takes longer
than the timeout, the others fail with `SQLITE_BUSY` instead and can retry
once it finishes. This closes the #631 caveat in the v0.5.0 notes. A first-time
initialization also no longer fails with `SQLITE_BUSY` when another connection
is writing to the database. It waits for that writer instead. A signing key
left without a committed marker, by an attempt that crashed or failed, still
returns `ErrIncompleteInitialization`.
