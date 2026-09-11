### Added — Reusable recipe bundles (#66)

Added the version 1 JSON recipe format with bounded `Load` and `Parse` APIs for
disk-backed and embedded bundles, ordered input declarations, and optional
advisory role or use-case routing hints.

Malformed prompt containers are rejected without reflecting their contents in
diagnostics. Unix file opens reject raced FIFOs without waiting for a writer.
