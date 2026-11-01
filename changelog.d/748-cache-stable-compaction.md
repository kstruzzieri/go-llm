### Changed — Compaction keeps the cached prompt prefix still, inside a goal and across turns (#748)

Inside a goal, a compacting step now drops to 75% of the budget left for
messages instead of just under it, and what it dropped stays out of every later
request in that goal, so the requests after a compaction start with the same
bytes and the provider's prompt cache can reuse them. The newest tool result is
never dropped to reach that mark; when the mark cannot be reached without it,
compaction falls back to the previous just-enough fit. On local llama.cpp with
qwen3-coder-next, `llm-bench -cache-probe` (pressure) now reports 27,992 of
53,511 turn-1 prompt tokens cached (52.3%), up from 14,692 of 57,605 (25.5%).

The setting is `agent.ContextManager.CompactionLowWater` (0, the default, keeps
the previous behavior; otherwise a fraction in (0,1), and `Run` rejects anything
else). Sticky eviction applies with the built-in assembly (no custom
`Compactor`, a `RecencyCompactor`, or `Mixed`); a custom `Compactor` receives the
fraction in `TokenBudget.LowWaterFraction` but no hiding. A later step's
`Pressure` counts only what that step dropped, and mixed-assembly trace omission
reasons are judged against the mark. Golem sets 0.75 through the new
`golem.NewContextManager`, which the cache probe also uses. With a durable
summary, the legacy compactor now drops prior-turn history before current-run
tool results, as documented; it previously did the reverse.

Across turns, Golem's history compression counts only the user and assistant
text it replays (stored tool results and tool-call frames no longer count),
still triggers at half the input ceiling, and now evicts to half of that, so the
durable summary at the start of every request is rewritten far less often (3
summarizer calls instead of 56 over 60 tool-heavy turns in the new test). The
summarizer receives only that replayed text, not tool results. `golem compact`
reports the same replayed-text estimate, so tool-heavy threads show smaller
numbers. `conversation.CompressMessagesWithCost` is the new, additive form of
`CompressMessages` with a caller-supplied message cost.
