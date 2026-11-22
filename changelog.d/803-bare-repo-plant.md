### Security — A bare repository a sandboxed command makes stays a documented limit (#803)

A sandboxed command can still create a bare repository in the workspace (a directory holding `HEAD`, `objects` and `refs`) or change the config or hooks of an existing one. Host Git can discover a bare repository when run from that directory or below it, including when the bare repository is the workspace root. Its configuration can cause Git to run programs outside the sandbox. Under Bubblewrap, the same limit is tracked in #802.

A Seatbelt rule for new `HEAD` entries in the workspace broke bare fixtures and operations that create entries named `head`. It still allowed a rename from private temp into the workspace. Extending the rule to private temp blocked that rename and broke Git dependencies in Cargo, direct Go module fetches, and Bundler. Both variants left the config and hooks of existing bare repositories writable, so neither rule shipped.

#### What to do

- With Git 2.38 or later, run `git config --global safe.bareRepository explicit` for your own Git commands.
- Editors, IDEs, and scripts can pass `-c safe.bareRepository=explicit` on each host Git invocation, as Golem does.

The setting rejects ordinary bare repositories discovered implicitly. Git still honors `--git-dir` and `GIT_DIR`, and can exempt paths it recognizes as `.git`, linked-worktree, or submodule metadata. See [What each backend enforces](docs/golem.md#what-each-backend-enforces).
