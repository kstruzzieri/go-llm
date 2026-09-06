### Added — Portable signed mutation receipts (#445)

- Add typed intent and applied mutation receipts with complete-body signatures,
  strict JSON decoding, and trusted-key verification through the shared signing
  package. Receipts bind mutation lineage, workspace, path, content hashes, UTC
  observation time, and optional tracked create permissions.
- Bound portable envelopes to 32 KiB before canonicalization and preserve the
  distinction between absent files, empty files, and mode 0000 versus null.
