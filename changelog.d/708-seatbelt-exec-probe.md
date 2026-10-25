### Security — Finish Seatbelt exec scoping and probe the real profile at startup (#708)

Follow-up to the native command sandbox (#576). Two hardening items for the
macOS Seatbelt backend.

#### Exec confinement coverage

The exec allowance was scoped to the read allowance in #576. This adds the
behavioral coverage that pins it: a child cannot run a binary outside the
reviewed roots, the workspace, its private temp or the approved executable's
canonical path, whether it is reached directly, through a workspace or
private-temp symlink, through an outside shebang interpreter, or through
`posix_spawn` (including `POSIX_SPAWN_SETEXEC`). Scratch execution is confined
the same way: a clone-local executable runs, an absolute path into the
canonical workspace is refused. Each refusal is asserted with its route's
exit code and diagnostic.

A program refused because it resolves, through `PATH`, to a location outside
the allowances reads as `No such file or directory` (exit 127), not "not
permitted": the C library's `execvp` falls back to a `stat` the profile also
denies. A program run by an explicit path reports `Operation not permitted`
(exit 126), and an approved script whose shebang interpreter is outside the
roots fails as `sandbox-exec: execvp() of '<script>' failed: Operation not
permitted` (exit 71). The diagnostics and compatibility notes document this.

#### Startup probe runs the backend's profile

`probeSeatbelt` applied a fixed deny-default profile, so a macOS release that
rejected a rule the real profile uses would pass startup and then fail every
command. The probe now renders and applies the backend's own profile — same
system roots, private temp, ancestor rules and network setting — to a
throwaway workspace and runs `/usr/bin/env /usr/bin/true` under it. A probe
cleanup failure fails construction. It still proves only that the profile
applies and a program starts under it, not that a given toolchain loads, and
it does not detect read-root drift.
