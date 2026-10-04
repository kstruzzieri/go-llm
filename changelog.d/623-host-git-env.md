### Security — Golem's own git calls no longer inherit its environment (#623)

The `git` processes Golem starts itself, for parallel task mode's worker
worktrees and for the session's git context snapshot, now receive an
environment built from scratch: `PATH`, `HOME`, `USER`, `TMPDIR`, `LANG`,
`XDG_CONFIG_HOME`, `GIT_CONFIG_GLOBAL`, `GIT_CONFIG_SYSTEM` and
`GIT_CONFIG_NOSYSTEM` (plus `SYSTEMROOT`, `TEMP`, `TMP`, `PATHEXT`,
`USERPROFILE`, `COMSPEC`, `LOCALAPPDATA`, `APPDATA`, `HOMEDRIVE` and
`HOMEPATH` on Windows) and `GIT_TERMINAL_PROMPT=0`; the snapshot also sets
`LC_ALL=C` and `GIT_NO_LAZY_FETCH=1`. Repository hooks, filters and
`core.fsmonitor` helpers that those calls run no longer see provider API keys
or other parent variables. `-agentflow-env` does not apply to these calls.

#### Upgrade note

Hooks, filters and helpers that relied on other variables now run without
them. An SSH agent, proxy and certificate variables, `GIT_ASKPASS`,
`GIT_LFS_SKIP_SMUDGE`, a custom `GIT_EXEC_PATH`, `LC_*` and `SUDO_UID` are not
passed: a worker checkout that needs the network (git-lfs over SSH with
agent-held keys, a partial clone's lazy fetch) can fail, and git-lfs may
download objects it used to skip. Git configuration injected through the
environment (`GIT_CONFIG_COUNT` with `GIT_CONFIG_KEY_<n>`/`GIT_CONFIG_VALUE_<n>`,
or `GIT_CONFIG_PARAMETERS`) is no longer passed to worker worktrees; the
snapshot already ignored it. Put such settings, for example `safe.directory`,
in your global git configuration instead. Run without `-plan-workers` to avoid
worker worktrees, or pass `-no-git-context` to skip the snapshot. This narrows
the environment only: hooks and filters still run as you, and your global and
system git configuration stays trusted.

#### Fixed

- When a worker worktree's `post-checkout` hook fails, git keeps the worktree.
  Golem now reports it as preserved, like the other worker roots a failed run
  keeps, instead of leaving it behind unreported.
