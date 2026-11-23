### Fixed — A fallback model the provider no longer serves no longer cuts Golem's input ceiling to 8192 tokens (#856)

Golem budgets each turn against the smallest context window in its model chain. A chain entry whose provider answered but no longer lists the model was counted as the 8192-token safe default, so one stale fallback capped the whole session even when the serving model had a far larger window. Routing cannot pick such an entry while the provider does not list it, so it no longer sets the budget.

#### Library

- `provider`: new sentinel `ErrModelNotFound`. `ModelRegistry.Lookup` and `Refresh` wrap it when a reachable provider's model list does not include the model and, for Ollama, `/api/show` answers 404; the error text is unchanged. A timeout or server error from `/api/show` is reported as that error instead of "not found". `ModelRegistry.LookupAny` wraps the sentinel only when every provider said not found: if any provider could not be reached, that error is reported. An unqualified name missing from the model index still returns an error that is not `ErrModelNotFound`, since the index can be empty because a listing failed.

#### Golem

- The input ceiling skips chain entries that fail lookup with `ErrModelNotFound`, for the session and for `-dispatch-role` children. An entry whose provider cannot be reached still counts as the safe default, since it may come back mid-session.
- When the ceiling is the safe default, the startup line, `/model` and `/model set` name the configured entries that caused it, for example `input ceiling: 8192 tokens (safe fallback; model context metadata unavailable for remote/big-model)` when that provider is down.

#### Limits

- The ceiling is worked out when the chain is set. If a skipped model is listed again later in the session and serves a turn, its window was not counted; `/model set` with the same role recomputes it.
