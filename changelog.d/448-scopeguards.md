### Security — Scoped read-only dispatch children (#448)

`dispatch` accepts mixed legacy task strings and `{"task":"…","scope":"subdirectory"}` objects. Scoped tasks pin an existing real subdirectory, inherit the parent workspace policy, and expose only `read_file`, `search`, `glob`, and `list`; retrieval is unavailable. All tasks are validated and roots acquired before any child starts, and child roots remain open until workers finish, including cancellation and timeouts. Legacy strings retain their original tools and result format.

Pinned scoped dispatch is supported on Linux and macOS; other platforms reject scoped tasks before starting children. The shared readers also pin directory traversal, reject symlink replacement and special-file reads, and preserve search-only directory access.

Nonzero `scope_denials` reports denied policy evaluations per scoped child, including enumeration pruning and repeated checks. It is diagnostic metadata, not a count of unique paths, failed tool calls, malicious probes, or durable audit events. Existing `risk_score` behavior is unchanged. Scoped preflight reserves the full count width within the configured result cap; the independent raw argument cap remains 197632 bytes.
