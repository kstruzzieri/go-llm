### Security — Gate local checks on hardening contracts (#453)

Local pre-push CI now verifies that the compiled hardening-contract aggregate
exists and runs it without cached results before lint and the repository-wide
race pass. The CI image includes Python for the audit regression, and the macOS
workflow now selects scratch and exec-backend Seatbelt tests in fresh non-race
and race runs.

The gate also rejects a skipped top-level or `Active` group and any skip outside
the declared deferred boundaries, bounds the phase with a 60-second timeout, and
neutralizes caller/persisted Go flags and stale toolchain roots so local settings
cannot silently omit tests. The GitHub `Lint & Test` job runs the same
`--mode security` gate, and the macOS background-exec step now selects the #481
exit-observer regressions.
