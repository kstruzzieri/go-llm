### Fixed — Bound routing-feedback lock waits by the write deadline (#592)

`SQLiteFeedbackStore` writes now cap SQLite's busy wait to the time left on the
caller's context deadline, so a contended feedback database adds about the
router's one-second feedback budget to a routed request instead of up to the
store's five-second `busy_timeout`. The cap applies to caller-owned handles as
well and is restored afterward; a connection whose transaction or timeout
cleanup fails is discarded. Writes without a deadline are unchanged. A frozen
filesystem is still not bounded.
