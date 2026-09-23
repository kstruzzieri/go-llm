### Security — Anchor workspace mutations and undo checks (#552)

Linux and Darwin Workspace writes, deletes and temporary cleanup now use the same
validated parent directory descriptor. Preview and undo hash/mode checks run
inside that boundary, preventing substituted ancestors from redirecting approved
changes. Observed foreign temp entries are preserved, and failures retain journal
and cleanup evidence.

Absent targets use atomic no-replace installation even for unconditional writes;
concurrent creates and unsupported no-replace operations are refused. Guarded
existing names require verified canonical spelling; unguarded search-only parent
permissions remain supported. Conditional operations require readable content.

Authority stays with an admitted directory if it is renamed. Overwrite/delete
are not leaf/content compare-and-swap, journals do not track renames, and other
platforms retain best-effort checked-path behavior. See the workspace mutation
boundary in `docs/least-privilege.md` for these limits and absent-name guard policy.
