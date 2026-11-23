### Changed — v0.9.0 consumer upgrade notes (#858)

Read this before upgrading from v0.8.0. Consumers on older pins must also apply
the earlier upgrade notes. The
[v0.9.0 upgrade guide](docs/releases/v0.9.0.md) has the details, the known
issues and the consumer compile check.

- Declared read-only toolchain roots for the macOS Seatbelt sandbox are opt-in
  (#722). A session that declares none keeps its v0.8.0 behavior and approval
  key. Bubblewrap and the host runtime refuse the new flags, and Linux
  verification is not done yet.
- Golem's own git calls refuse a planted bare repository (#803). Parallel task
  mode now needs git 2.38 or later, a `git` inside the workspace is refused, a
  repository whose local config includes other files loses its git context
  block, and Golem strips workspace directories from the `PATH` it passes
  to git.
- Your own git, your editor's and your scripts' are not covered: with git 2.38
  or later, set `safe.bareRepository=explicit` (#803).
- Bubblewrap still cannot refuse git metadata created during a run; #802 moved
  to v0.10.0.
- `agent/tools`' sandboxed exec and background constructors now wrap
  `ErrSandboxPolicy` when the backend cannot be built (#722).
