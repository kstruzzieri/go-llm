### Security — Secret and payment-card detection (#437)

- Add opt-in inference blocking for supported secrets and payment-card numbers,
  with safe CLI history, trace, checkpoint, and machine diagnostics.
- Make filesystem RAG indexing skip detected content by default, with per-kind
  library redaction and typed policy outcomes.
- Retire affected managed indexes and reject stale readers after policy
  failures. Retirement is logical and does not securely erase stored bytes.
- Document numeric-identifier false positives, detector coverage, and the
  completed-response inspection boundary for streaming output.
