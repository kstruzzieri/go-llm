### Added — Signed durable Golem mutation receipts (#445)

- Add typed intent and applied mutation receipts with complete-body signatures,
  strict JSON decoding, and trusted-key verification through the shared signing
  package. Receipts bind mutation lineage, workspace, path, content hashes, UTC
  observation time, and optional tracked create permissions.
- Bound portable envelopes to 32 KiB before canonicalization and preserve the
  distinction between absent files, empty files, and mode 0000 versus null.
- Automatically persist signed intents before durable Golem write/edit calls
  (interactive, headless `-allow-tool`, late `/allow-write`), startup-enabled
  scratch promotion, and physical `/undo` restores/deletions. Success requires
  an observed, durable applied receipt; post-write failures halt further writes
  and recovery preserves missing applied evidence without inventing signatures.
- Use the persistent per-user Ed25519 identity at
  `<dataDirBase>/golem/signing/agent-ed25519.pem`. Retained current-workspace
  history requires its matching key; missing/mismatched keys disable writes with
  public-ID/path recovery diagnostics, without replacement or unsigned fallback.
- Add `/checkpoints` evidence labels (`unsigned`, `unconfirmed`, `receipts
  verified`, `invalid receipts`) without claiming to verify live files. Any
  unauthenticatable retained history makes labels unavailable for the command.
- Upgrade checkpoints additively to schema v3, which older binaries refuse.
  Finish interrupted legacy recovery/undo with the previous binary first;
  completed unsigned history remains visible but authenticated `/undo` refuses
  it. Downgrading requires a pre-upgrade backup.
- Retain receipt metadata after completed undo and snapshot pruning without
  automatic expiry. The 50-checkpoint / 64 MiB limits bound undo snapshots, not
  total database size. Completed inverse evidence prevents replay while earlier
  uncertain attempts remain unconfirmed.
- Keep AgentFlow task/RAM undo and parallel promotion/rollback, direct embedders,
  arbitrary subprocess/external-editor writes, scratch copies/cleanup, and Golem
  metadata outside this scope. AgentFlow proof receipts are separate. No audit
  chain, completeness, trusted-time, complete process-attribution, power-loss,
  whole-ledger deletion/reordering/rollback/truncation detection, or standalone
  public-key retention/export guarantee is added; approval behavior is unchanged.
