### Security — A bare repository a sandboxed command makes stays a documented limit (#803)

A sandboxed command can still create a bare repository in the workspace (a directory holding `HEAD`, `objects` and `refs`), or change the config or hooks of one already there. Git run on the host inside that directory, or at a workspace root with no `.git` of its own, reads its config, which can run commands outside the sandbox. Golem's own git calls refuse such a repository (see the host git hardening entry). The sandbox does not: a Seatbelt rule refusing new `HEAD` entries was measured and rejected, because it broke git dependencies in Cargo, Go and Bundler, bare test fixtures and any file named `head`, and a command could still build the directory in its private temp and move it in. Under Bubblewrap the same limit is tracked with #802. See [What each backend enforces](docs/golem.md#what-each-backend-enforces).

#### What to do

- Run `git config --global safe.bareRepository explicit` (git 2.38 or later). Git then uses a bare repository only when you name it with `--git-dir` or `GIT_DIR`; a repository named that way is not checked.
- Programs that run git in a workspace where sandboxed commands run (editors, IDE git panels, scripts) should pass `-c safe.bareRepository=explicit` on every git call, as Golem does.
