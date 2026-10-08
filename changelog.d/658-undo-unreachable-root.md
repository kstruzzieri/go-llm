### Fixed — Undo refuses while the workspace root is unreachable (#658)

Undo treated any "not found" result for a file golem created as "already
removed". A workspace root renamed or moved away fails with the same error. In
that case the REPL's `/undo` reported `undid <path>` and deleted the
checkpoint, and AgentFlow task mode's in-memory undo printed
`undid <path> (already absent)` and dropped its record. The file still existed
in the moved directory. Both journals now count a missing file as already
undone only while the workspace root still names the directory golem started
in. Otherwise they refuse with `cannot undo <path>: file changed since golem
wrote it` and keep the record, so the undo succeeds once the root is back.

The check is the new read-only `agent/tools.Workspace.VerifyRoot`. It returns
nil when the root is intact, the not-exist error when the root is gone, and
`ErrRootReplaced` when another directory or a non-directory occupies the path.
Windows captures the original directory identity when the workspace is
constructed, so replacing the root before the first undo cannot make the new
directory look like the original. Mutation behavior is unchanged.
