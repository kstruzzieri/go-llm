### Fixed — Detect competing conversation saves (#473)

Conversation saves now compare the loaded revision before replacing a transcript
and return a typed conflict if another writer saved first. Conversation, summary,
revision, and search updates commit atomically. Existing databases migrate to
revision 1 without rewriting their transcripts.

Golem surfaces a rejected raw save as `session_conflict`, including an error exit
for one-shot use. SQLite lock timeouts also fail the CLI invocation instead of
being demoted to success. Interactive conflicts explain which history the next
turn uses. Automatic compression conflicts remain warnings after the raw turn
is durable. Custom session stores must adopt the revision contract; upgrade
all writers together. Deleting and recreating the same ID can reuse revisions
and remains outside the CAS guarantee.
