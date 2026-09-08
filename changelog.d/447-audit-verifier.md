### Added — Offline Golem audit verifier (#447)

- Add `golem audit` with workspace, memory, and proof scopes, text reports, and
  exits 0 (verified), 1 (observed integrity violation), and 2 (incomplete).
- Authenticate retained mutation receipts and undo lineage, verify retained
  checkpoint before-images, and compare each determinate path with its latest
  applied state. Preserve unsigned and unconfirmed history as incomplete.
- Verify every stored agent-memory record, including expired rows, tombstones,
  and other scopes, with exact partial progress and safe diagnostic categories.
- Read existing checkpointed databases and verifier files without migration,
  signing, repair, or permission changes. Refuse active journals and invalidate
  findings from sources that change during the scan.
- Add an adapter for the proposed AgentFlow integrity-only JSON verifier.
  Existing providers report incomplete; real-provider compatibility remains a
  release prerequisite. Proof assurance is structural/checksum and unsigned.
- Exclude the workspace from Python module search when using an AgentFlow source
  checkout (Python 3.11+), and reject checkout paths that would expand into
  multiple import locations.
- Stream live workspace hashes with bounded memory while preserving both stable
  observations, complete file modes, and existing path protections.
