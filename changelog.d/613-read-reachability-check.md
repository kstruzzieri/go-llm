### Security — Reads verify the guarded path still reaches the opened file (#613)

On Linux and Darwin, the read-only file tools (`read_file`, `search`, `glob`,
`list`), scoped dispatch construction and readers, and the internal reads behind
`write_file`/`edit_file` previews and undo now resolve the guarded path again
from the top-level workspace root after the guard decides and the file is opened
or the directory enumerated, one component at a time without following
symlinks. If the path no longer reaches the same object, the read fails closed.
A directory renamed into a denied location therefore can no longer serve
content under its old, allowed name. `list` rechecks its directory after its
entry guards, and `glob` rechecks each matching name's parent after that name's
guard. This reverses #448's rule that a pinned directory or root keeps serving
reads after it moves.

This is decision integrity, not adversary resistance: a process that can rename
can still place content at an allowed name, hard links make a name unreliable
provenance, the re-resolution is a sequence of lookups rather than an atomic
snapshot, bytes are read after it, and case-only or normalization-only renames
on case-insensitive filesystems are not detected. Writes and deletes keep the
#552 mutation boundary. Other platforms are unchanged. See
docs/least-privilege.md.

#### Upgrade notes

- `read_file`, `search`, `glob` and `list` can now fail with
  `path changed during access` when the target or one of its directories moves
  during the call. Retry the call. A directory that vanished or became a symlink
  between a walk's enumeration and its recursion also reports this text instead
  of `path not found` or a generic failure.
- `write_file`/`edit_file` previews, and the exported
  `Workspace.ReadFileForUndo`, `ReadFileWithModeForUndo` and `HashFileWithMode`,
  fail in the same cases with the existing error text
  `file identity changed between stat and open`. That error is unexported; only
  `ErrRootReplaced` can be matched with `errors.Is`.
- A scoped dispatch child's reads fail once its scope directory is renamed or
  moved, even when a symlink now leads to it; previously the child kept reading
  the moved directory. A workspace root replaced while a scoped child runs now
  fails its reads with `ErrRootReplaced` instead of serving the old tree.
- `search` skips a file whose own check fails, as it already skips unreadable
  files. A directory whose check fails after enumeration aborts the `search`,
  `glob` or `list` call.
- A workspace root replaced during a `search`, `glob` or `list` now aborts the
  call instead of continuing through the old tree; `search` never skips files
  for it, so a replaced root cannot yield partial results or `no matches`. It
  still reports `ErrRootReplaced` (tool output `path changed during access`);
  hosts should build a new `Workspace`.
- Writes and deletes are unchanged.
