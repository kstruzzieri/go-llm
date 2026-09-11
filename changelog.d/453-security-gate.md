### Security — Gate local checks on hardening contracts (#453)

Local pre-push CI now verifies that the compiled hardening-contract aggregate
exists and runs it without cached results before lint and the repository-wide
race pass. The CI image includes Python for the audit regression, and the macOS
workflow now selects scratch and exec-backend Seatbelt tests in fresh non-race
and race runs.

The gate also rejects a skipped aggregate and neutralizes caller/persisted Go
flags and stale toolchain roots so local settings cannot silently omit tests.
