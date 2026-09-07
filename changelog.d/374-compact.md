### Added — Compact persisted session history from the REPL (#374)

- Add `/compact` to the REPL. It keeps the newest four completed exchanges,
  including complete tool chains, preserves system messages and unresolved
  tool-call tails, and folds older history into the existing progressive
  summary. Repeated compactions may invoke the summarizer even when the result
  is unchanged; a changed result can have an equal or greater stored-history
  token estimate. Each repeat with an existing summary can incur another model
  request, including its latency and provider charges. For small histories, the
  summary's trust-boundary wrapper can cost more tokens than the messages removed
  (the five-exchange test fixture reports `100 -> 121`, then `121 -> 121`).
- Report stored-history estimates for non-system message contents, tool
  metadata, and the rendered summary. These estimates exclude the live prompt,
  tool schemas, transport framing, and current turn. `/compact` respects
  `--no-session` and `--no-compress`; cancellation or failure before a
  successful save leaves the prior session snapshot intact.
- Preserve the historical-data trust boundary during manual and automatic
  summarization: frame all summarizer input and quote generated summaries
  under an explicit data-only instruction when restoring agent context.
- Include the rendered summary envelope in automatic compression's reserve and
  fold additional history when actual summary quoting exceeds that allowance.
  Retention floors still take priority; a failed additional pass preserves the
  original snapshot.
