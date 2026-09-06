### Security — signed agent-memory provenance and fail-closed recall (#446)

Agent-memory records now carry a detached signature over a canonical `body` containing content, scope, provenance, metadata, and lifecycle timestamps. Creation stamps the configured host origin and `agent-written`; one-time legacy import stamps `legacy-migration` and `legacy-unreviewed`. Source fields remain claims, and neither trust class grants instruction authority. Reads verify every selected record before returning anything, while mutations verify the preimage and atomically re-sign the changed body.

Filesystem-backed stores keep the current Ed25519 private identity in `<database path>.keys/current.pem` and retained public verifiers in `trusted/<key-id>.pem`. Initial import and its database marker commit together. An existing key without the marker returns `ErrIncompleteInitialization`; an initialized store never recreates a missing key or repairs unsigned rows. Golem disables unavailable agent memory while preserving healthy user memory, while explicitly enabled MCP agent memory fails startup if it cannot open safely.

Recall identifies agent-memory origin and unreviewed trust before content. MCP agent-memory success results now contain one `TextContent` value whose complete JSON envelope is inside matching `TOOL_RESULT` fences; clients that decoded bare JSON must validate the fence and decode the enclosed object. Promotion makes a record durable but leaves it unreviewed; human trust promotion remains separate work under #471.

#### Upgrade notes

- `NewMemoryRecordStore` and `OpenRecordStore` now require `RecordStoreConfig`. Configure either `KeyDir` or a complete injected signer/keyring pair; injected first initialization also requires `Initialize: true`, omitted on reopen.
- Back up the database and key directory together. Rotation is offline: stop writers, retain the previous public verifier, install the new current private identity, and reopen. Keys are not reloaded automatically.
- Per-record verification detects alteration of selected signed bodies. It does not detect whole-database rollback, record deletion, replay of an older valid version, FTS-only changes, or signing-key compromise.
