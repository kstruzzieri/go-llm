### Security — Golem's own git calls refuse a planted bare repository (#803)

Every git command Golem runs on the host now passes `-c safe.bareRepository=explicit -c core.fsmonitor=false`. This covers the startup and `/git-context refresh` snapshot and parallel task mode. A bare repository that a sandboxed command created in a workspace with no `.git` of its own is refused, and no `core.fsmonitor` helper runs.

Golem also refuses to run git when the `git` found on `PATH` is older than 2.38, or lies inside the workspace once symbolic links are resolved. Golem also drops empty, relative and inside-the-workspace entries from `PATH` for every git it runs, so a filter, `ssh` or hook command that git starts by name is not taken from the workspace. Snapshot capture refuses a repository whose local config includes other files (`include.path` or `includeIf.<condition>.path`).

Parallel task mode still runs the repository's own hooks, filters (git-lfs keeps working) and lazy fetch, with the isolated environment. See [Golem's own git calls](docs/golem.md#golems-own-git-calls).

#### Upgrade note

- Parallel task mode now needs git 2.38 or later. Snapshot capture already needed 2.45.
- A `git` executable inside the workspace (for example a workspace directory on `PATH`, or `~/bin/git` and asdf or mise shims when `-root` is your home directory) is refused.
- A repository that uses `include.path` or `includeIf` in its local config gets a startup warning (`git context disabled: ...`) instead of the git context block.
- Parallel task mode no longer runs `core.fsmonitor` helpers. macOS and Windows users with git's built-in monitor may see slower `status` in parallel task mode.
- Hooks and filters run by Golem's git no longer find programs in workspace directories, or through relative or empty `PATH` entries (for example a hook relying on direnv's `PATH_add bin`). Install such tools outside the workspace or call them by path.
- Version-manager shims (asdf, mise) that pick the real git from files in the workspace remain a trust input, as does a program your config names by an absolute path inside the workspace.
