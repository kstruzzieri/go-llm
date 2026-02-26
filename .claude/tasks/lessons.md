# Lessons Learned

## 1. ALWAYS search the existing codebase before writing new utility code
**Date**: 2026-02-06
**Trigger**: User correction — rewrote GeckoTerminal rate limiting from scratch instead of using existing `trading.defi.rate_limiter.RateLimiter`
**Pattern**: When writing a new script that calls an external API, I invented my own rate limiting (6.5s delay, exponential backoff) instead of checking what the codebase already had. The existing `DexDataProvider` in `backend/python/trading/data/dex_data_provider.py` already handles GeckoTerminal rate limiting at 2.1s with `GECKOTERMINAL_RATE_LIMIT_DELAY_SECONDS` env var, and `trading.defi.rate_limiter.RateLimiter` provides a reusable token bucket implementation.
**Rule**: Before writing ANY utility code (rate limiting, retry logic, HTTP helpers, DB connection patterns), search the codebase for existing implementations:
  - `grep -r "rate.limit\|RateLimiter" backend/python/`
  - Check `trading/defi/rate_limiter.py` for async rate limiters
  - Check `trading/data/dex_data_provider.py` for GeckoTerminal-specific patterns
  - Check `scripts/seed_coingecko_top500.py` for CoinGecko-specific patterns
  - Import and reuse existing code. Do not reinvent.

## 2. Review existing similar scripts before writing new ones
**Date**: 2026-02-06
**Related to**: Lesson 1
**Pattern**: The `seed_coingecko_top500.py` script already demonstrated the DB connection pattern, error handling, and argument structure for bulk data scripts. The `DexDataProvider` class already wraps GeckoTerminal API calls with proper rate limiting. New scripts should compose these existing pieces, not rewrite them.
**Rule**: Before writing a new script in `scripts/`, review:
  - Other scripts in `scripts/` for patterns (CLI args, DB connection, logging)
  - `backend/python/trading/data/` for API provider classes
  - `backend/python/trading/defi/` for shared utilities (rate_limiter, retry)

## 3. QuestDB ILP requires Symbol columns before all other columns
**Date**: 2026-02-10
**Trigger**: Gate B manual verification — bars streaming from Alpaca but failing to write to QuestDB
**Error**: `"symbols have to be written before any other column: invalid message"`
**Root cause**: In the ILP writer, `Symbol("source", bar.Source)` was placed after Float64/Int64 columns. QuestDB ILP protocol requires: Table → Symbol columns → other columns (Float, Int, String, Timestamp) → At.
**Rule**: When constructing QuestDB ILP messages via `go-questdb-client`, always order: `.Table(name).Symbol(...).Symbol(...).[Float64Column|Int64Column|StringColumn|TimestampColumn](...)...At(ctx, ts)`

## 4. QuestDB DDL: DEDUP UPSERT KEYS (not DEDUP KEYS)
**Date**: 2026-02-10
**Trigger**: Gate B manual verification — QuestDB migration failed
**Error**: `expected 'upsert'` on `DEDUP KEYS(symbol, ts)`
**Fix**: Correct syntax is `DEDUP UPSERT KEYS(ts, symbol)` — the designated timestamp must be included and listed first. Also: dedup key columns must be fixed-size types (SYMBOL, UUID, LONG, etc.), not STRING.
**Rule**: For QuestDB 7.3+ dedup syntax: `CREATE TABLE ... PARTITION BY DAY WAL DEDUP UPSERT KEYS(ts, col1, col2)` where `ts` is the designated timestamp.

## 5. Always kill stale server processes before starting new ones
**Date**: 2026-02-10
**Trigger**: API routes returning 404 despite correct code — turned out an OLD server process was occupying port 8081
**Pattern**: `go run` compiles to a temp binary. When killed via pkill, the parent `go run` may die but the compiled binary keeps running. Use `lsof -i :PORT` to check what's actually listening, then kill by PID.
**Rule**: Before starting a dev server, always: `lsof -i :PORT | grep LISTEN` → kill the PID → verify port is free → then start.

## 6. Cross-reference plan spec examples against code defaults
**Date**: 2026-02-11
**Trigger**: Codex regression review found `ErrDataUnavailable` was `retryable: true` but the plan's Gate B example JSON showed `"retryable": false`.
**Pattern**: I implemented the error constructor with a generic default (`true` for "unavailable" sounds transient) without checking the plan's explicit example output. The plan at line 791 specifies the exact JSON contract: `{"code":"DATA_UNAVAILABLE","message":"no data for symbol INVALID","retryable":false}`.
**Rule**: When a plan includes example API responses or JSON contracts, treat every field as a spec. Cross-reference constructor defaults against those examples before marking done. Missing data is not transient — retrying won't create data that doesn't exist.

## 7. Align ALL config layers, not just the one you're fixing
**Date**: 2026-02-11
**Trigger**: Codex found `ADMIN_API_KEY: ${ADMIN_API_KEY:-}` in docker-compose while other required vars used `${VAR:?msg}`. I had fixed the Go config validation to require the key but didn't check docker-compose for consistency.
**Pattern**: Config flows through multiple layers: `.env.defaults` → `docker-compose.yml` → Go `config.go` → runtime validation. Fixing one layer without checking the others creates inconsistency. The `ALPACA_API_KEY` and `POSTGRES_PASSWORD` already used `:?` syntax — I should have followed the established pattern.
**Rule**: When making a config value required, grep ALL config layers (`docker-compose.yml`, `.env.defaults`, `config.go`, any deploy manifests) and ensure they all enforce the requirement consistently. Follow the pattern already established by sibling vars.

## 9. DEX buy orders: token quantity vs quote currency quantity
**Date**: 2026-02-11
**Trigger**: Code review found DEX buy orders were using `tokenAmount(order.Quantity, quoteInfo.Decimals)` — treating token quantity as USDC amount.
**Pattern**: `order.Quantity` represents the desired number of tokens (e.g., 10 WETH). For buy orders, the adapter was converting 10 using USDC decimals (6), asking "how much WETH for 10 USDC?" instead of "what's the price of 1 WETH?"
**Fix**: Always quote 1 unit of the token → USDC to get the per-unit price. Use that price (with slippage) for the simulated fill. `order.Quantity` stays in token units for both sides.
**Rule**: When interfacing with DEX quoters, always clarify: is `AmountIn` the input currency amount or the desired output? For price discovery, quote a unit amount to get the rate, then scale.

## 10. Partial fill pricing: cumulative vs marginal
**Date**: 2026-02-11
**Trigger**: Code review found that the order poller passed the venue's cumulative average price as the fill price for the delta quantity, causing incorrect position weighted averages.
**Pattern**: Alpaca reports cumulative `FilledAvgPrice` across all fills. When computing the delta fill for position tracking, we need the marginal price: `(newTotal - oldTotal) / deltaQty`. The DB should store the cumulative avg price (as the venue reports it), while position tracking gets the marginal price for the delta.
**Rule**: When handling partial fills from venues that report cumulative averages, always compute the marginal price for position tracking. Separate the DB update (cumulative) from the position update (marginal).

## 11. Race conditions in test code: use sync/atomic for shared goroutine state
**Date**: 2026-02-11
**Trigger**: `go test -race` caught data race in bot orchestrator test — `evaluateCallCount` accessed from both test goroutine and bot loop goroutine without synchronization.
**Pattern**: Test closures that capture mutable variables are shared with goroutines spawned by the system under test. The race detector catches these even in test code.
**Rule**: Any variable shared between a test's main goroutine and any goroutine spawned by the SUT must use `sync/atomic`, `sync.Mutex`, or channels. Use `atomic.Int64`, `atomic.Bool`, `atomic.Pointer[T]` for simple counters/flags.

## 12. Fill accounting: always compute deltas from cumulative venue data
**Date**: 2026-02-11
**Trigger**: Code review found order poller's Filled case passing cumulative FilledQuantity to ApplyFill, which expects deltas. If order went partial (5 of 10 filled) then filled (10 of 10), position gets +10 instead of +5.
**Pattern**: Venues report cumulative fills. The Partial case correctly computed delta = new_cumulative - old_cumulative, but the Filled case just passed the cumulative quantity straight through.
**Rule**: When processing any fill status transition (partial→filled or submitted→filled), always compute delta = new_cumulative - old_cumulative for position tracking. Never pass cumulative quantity to position tracker as-is.

## 13. Don't write to DB after submission if there's no fill data
**Date**: 2026-02-11
**Trigger**: Code review found ExecuteOrder Step 8 unconditionally calling UpdateOrderFill with zero fills after Alpaca returns a freshly submitted order (filled_qty=0).
**Pattern**: `UpdateOrderFill` auto-promotes status to `partial` since `0 < quantity`, corrupting the order from `submitted` to `partial` immediately.
**Rule**: Only persist fill info when there's actual fill data (`FilledQuantity.IsPositive()`). For freshly submitted orders, just update status and venue_order_id.

## 14. Mock repos in tests need synchronization
**Date**: 2026-02-11
**Trigger**: `go test -race` caught data race in `ptBotRepo.bots` map accessed by test goroutine and bot loop goroutine concurrently.
**Pattern**: In-memory mock repositories in test code share mutable state (maps) between the test's goroutine and goroutines spawned by the system under test.
**Rule**: All test mock repos with in-memory maps must use `sync.RWMutex` to protect concurrent access. The race detector catches these even in test code.

## 15. Schema migrations must match code writes AND reads
**Date**: 2026-02-11
**Trigger**: Code review found `realized_pnl` column being written via ILP and read via SQL, but missing from the QuestDB migration DDL.
**Pattern**: QuestDB auto-creates columns on ILP write, but SQL reads against a fresh deployment will fail until the first write creates the column. The migration gap creates a race between first read and first write.
**Rule**: If production code writes/reads a column, the schema migration must explicitly define it. Always create a new migration file for columns added in new blocks.

## 16. Per-bot strategy config: use existing parsers
**Date**: 2026-02-11
**Trigger**: Code review found `ParseSMAPeriods` utility existed but was never called. All bots shared a single `SMACrossoverStrategy(10, 20)` instance.
**Pattern**: A utility function for parsing per-bot config existed but the orchestrator wasn't wired to use it. The bot registry pattern (one shared instance per strategy name) conflicts with per-bot configuration.
**Rule**: When a utility parser exists for bot config, use it at bot startup to create per-bot strategy instances. Fall back to the registered default only when bot config doesn't specify parameters.

## 17. Risk limit policy: all-or-nothing
**Date**: 2026-02-11
**Trigger**: Code review found `DEFAULT_MAX_OPEN_ORDERS` used `getEnvInt("...", 10)` (with fallback) while all other risk limits used `getEnv("...", "")` with fail-fast validation.
**Pattern**: One risk limit slipped through with a hardcoded default of 10, violating the "no defaults" policy that all other limits enforced.
**Rule**: If the plan says "no hardcoded defaults", every risk limit must follow the same pattern. After implementing, grep all limit vars to verify consistency: `grep -n "DEFAULT_MAX\|DEFAULT_DAILY\|DEFAULT_MAX" config.go`.

## 8. Defense-in-depth: add DB constraints for business rules, not just app-layer validation
**Date**: 2026-02-11
**Trigger**: Codex found that `symbols` and `bots` tables allowed invalid `asset_class`/`venue` pairings (e.g. `stock+DEX`) at the database level, even though the app layer validated correctly.
**Pattern**: I wrote separate CHECK constraints (`asset_class IN (...)` and `venue IN (...)`) but didn't add a cross-column constraint enforcing valid combinations. Direct SQL inserts or future code paths could bypass app validation.
**Rule**: When two columns have a business-rule dependency (valid combinations), always add a cross-column CHECK constraint at the DB level. App-layer validation is necessary but not sufficient — the database is the last line of defense.