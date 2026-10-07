### Security — Reads verify the guarded path still reaches the opened file (#613)

On Linux and Darwin, the read-only file tools (`read_file`, `search`, `glob`,
`list`), scoped dispatch construction and readers, the reads behind
`write_file`/`edit_file` previews and pre-apply re-reads, and the exported undo
and hash readers now resolve the guarded path again from the top-level
workspace root after the guard decides and the file is opened or the directory
enumerated, one component at a time without following symlinks. If the path no
longer reaches the same object, the read fails closed. A directory renamed into
a denied location therefore can no longer serve content under its old, allowed
name. `list` rechecks its directory after its entry guards, and `glob` rechecks
the parent of each name it returns after that name's guard. This reverses
#448's rule that a pinned directory or root keeps serving reads after it moves.

This is decision integrity, not adversary resistance: a process that can rename
can still place content at an allowed name, hard links make a name unreliable
provenance, the re-resolution is a sequence of lookups rather than an atomic
snapshot, bytes are read after it, and case-only or normalization-only renames
on case-insensitive filesystems are not detected. Other platforms are
unchanged. See docs/least-privilege.md.

#### Upgrade notes

- `read_file`, `glob` and `list` can now fail with `path changed during access`
  when the target or one of its directories moves during the call. Retry the
  call. A directory that vanished or became a symlink between a walk's
  enumeration and its recursion also reports this text instead of
  `path not found` or a generic failure.
- `write_file`/`edit_file` previews and pre-apply re-reads, and the exported
  `Workspace.ReadFileForUndo`, `ReadFileWithModeForUndo` and `HashFileWithMode`,
  fail in the same cases with the existing error text
  `file identity changed between stat and open`, prefixed
  `workspace root replaced: ` for `ErrRootReplaced`. That error is unexported;
  only `ErrRootReplaced` can be matched with `errors.Is`.
- Golem's `/undo` refuses earlier when the file's directory is moved or swapped
  during the undo's precondition read. RAM undo prints only
  `cannot undo <path>: file changed since golem wrote it`, without the
  `undo failed for <path>: file precondition mismatch` line; checkpoint undo
  prints `undo failed for <path>: file identity changed between stat and open`
  and `undo interrupted; run /undo to resume`. Previously the late precondition
  check refused. Nothing is mutated, the record or checkpoint is kept, and
  `/undo` can be retried once the layout is restored.
- A scoped dispatch child's reads fail with `ErrRootReplaced` once its path no
  longer reaches the pinned scope directory, for example because the scope was
  moved or replaced, even when a symlink now leads to it; previously the child
  kept reading the moved directory. A workspace root replaced while a scoped
  child runs also fails its reads with `ErrRootReplaced` instead of serving the
  old tree.
- `search` skips a file whose own check fails, as it already skips unreadable
  files. A directory whose check fails after enumeration aborts the `search`,
  `glob` or `list` call.
- A workspace root that is replaced, renamed away or turned into a symlink
  during a `search`, `glob` or `list`, or a scoped child's scope that moves or
  is replaced, now aborts the call instead of continuing through the old tree;
  `search` never skips files for it, so it cannot yield partial results or
  `no matches`. It reports `ErrRootReplaced` (tool output
  `path changed during access`); after a top-level root replacement, hosts
  should build a new `Workspace`. Losing access to the root or scope path
  during the call, such as search permission on a parent directory, also aborts
  it, with the cause's text (`path is not accessible`).
- The #552 write and delete operations themselves are unchanged.
