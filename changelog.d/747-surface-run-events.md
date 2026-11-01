### Added — Golem and the cache probe show compaction and fence-rotation counts (#747)

Golem's end-of-turn footer, in the REPL and on `-p` stderr, now counts the
turn's `compaction` and `fence_rotation` events from `agent.Result.Events`:
`done · 4 steps · 9.2s · 18342 tok (12800 cached) · 1 compaction · 1 fence
rotation`. Both re-render earlier request bytes, so each explains a
prompt-cache miss. The counts sit after the token count and before
` · risk N` and the stop suffix; a turn with neither event keeps its footer
unchanged. The `-telemetry` run span gains `compactions` and
`fence_rotations`, omitted when zero, so a run with neither writes the same
bytes as before; the schema version stays 2.

`llm-bench -cache-probe` ends its table with `Fence rotations: N.` A scripted
probe run quotes no frame key, so it reports 0; a non-zero count means a
rotation re-framed an earlier tool result and the cache missed from there.
