### Fixed — Concurrent conversation and memory migrations (#543)

Conversation and memory stores now claim each schema version before applying
its migration, so concurrent openers skip steps another opener has committed.
Each step and its version row commit or roll back together; existing migration
SQL and stored data are preserved. Current-schema opens require only reads.
Callers must configure SQLite's busy timeout on every migrating connection and
complete initial database/WAL setup before racing migration runners.
