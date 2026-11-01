### Changed — Per-result fence keys with scan-and-rotate, so the prompt cache holds inside a goal (#745)

Each tool result is now framed (#430) under its own key, minted once the
result is final and reused on every later request of the run, instead of one
fresh key per request for every frame. Earlier results therefore reach the
provider with the same bytes on each request, and the provider's prompt cache
can reuse them: on local llama.cpp with qwen3-coder-next, `llm-bench
-cache-probe` (uncompacted) now reports 60,914 of 77,717 turn-1 prompt tokens
cached (78.4%), up from about 1,000 (1.3%). `/consult` advice gets one key per
run; keys differ across runs and are never saved. Compaction that drops or
re-renders earlier results still moves the prefix.

Before each request the runtime scans every string of the new run text that
is replayed to the provider or rendered into the prompt (message content, tool
names, tool-call IDs and arguments, structured-context text, and selected
content joined across context groups). It checks nested `\uXXXX` escapes,
fullwidth ASCII forms, any letter case, and the text reduced to the key
alphabet so spaces, hyphens, digits or invisible characters between key
letters do not hide a key. Every live key it finds is replaced with a fresh
one (never a retired one), and the library records one `fence_rotation` entry
per rotated frame in `Result.Events`. If the bounded scan reaches its limit
(for example, with deeply nested escapes or an oversized result), it resets
the live keys once, records `fence_scan_limit`, then resumes
stable reuse. Other compatibility forms and cross-script look-alike letters
are not matched. A custom `Compactor` is trusted: text it synthesizes while
building a request is not scanned.

The `ToolTrustContract` wording and every committed trace are unchanged. The
containment inventory, hardening contract and mutation ledger record the new
checks (M-028 A/B, M-067 to M-074).

`llm-bench` gains a separate forged-close corpus,
`docs/llm/traces/injection-forged-close/` (10 traces, its own registration),
built with `-build-injection-corpus` from a cases file whose `source` is
`forged-close`; any `source` other than `injection` or `forged-close` is now
rejected before anything is written. `-injection-report` with that
registration adds a within-model forged-live vs forged-stale table. Results on
two local models and one cloud model (kimi-k2.7-code on OpenCode Go), and a
one-model #452 rerun, are in `docs/llm/benchmarks/2026-10-31-forged-close.md`.
