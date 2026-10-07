### Security — Scratch and sandboxed exec verify the approved executable, cwd and root (#553)

On Linux and Darwin, a scratch command now checks its workspace snapshot's root,
working directory and (for a workspace-local executable) executable against the
objects approved at preview time before it runs in the clone, and the Seatbelt
and Bubblewrap backends check that the executable they resolve is the approved
object before launching it.

#### Upgrade note

- A scratch command refuses to run, with "scratch snapshot does not match
  approved ...; retry", when its root, cwd or workspace executable was replaced
  between approval and execution. Retrying plans against the current
  object.
- A sandboxed command refuses with "executable changed since approval; retry"
  when its executable target changed after approval.
- Seatbelt now runs an approved executable reached through symlinks outside
  its read roots (Homebrew style, such as `/opt/homebrew/bin/<tool>`) by
  granting metadata on each link in the chain. Dynamic libraries outside the
  read roots are still denied.
- Approval keys and grants are unchanged; a fresh plan against a same-path
  replacement is approved normally.
- An executable or working directory under an excluded `.git` directory cannot
  be used as a scratch target, since the snapshot does not clone it.
- These checks bind the approved object, not its bytes: an in-place rewrite that
  keeps the file's identity is not detected. Concurrent same-UID host mutation
  remains an accepted residual.
