### Security — Native sandboxes refuse writes to git metadata (#721)

A command run in the Seatbelt (macOS) or Bubblewrap (Linux) sandbox could write the workspace's git hooks and git config. Git runs those later outside the sandbox, on your next commit or checkout, so a sandboxed command, or a test run by post-write verification, could plant code that runs unsandboxed. It could also write `.git/info/exclude` or `.git/info/attributes` to hide a new file from `git status` or a change from `git diff`.

#### Behavior changes

- Both native sandboxes now refuse to change, rename, replace or delete the workspace's git metadata: hooks, anything under `info`, the `config`, `config.worktree`, `commondir` and `gitdir` files, the `.git` entry itself, and the `.git` file that points a submodule or linked worktree at its git directory. This covers the top-level repository, nested repositories, and the submodule and linked-worktree git directories git keeps under `.git/modules` and `.git/worktrees`, for verification commands as well as commands. The exceptions are under Remaining limits.
- Seatbelt also refuses creating any of this during the run, in any letter case, including a new repository.
- Ordinary git work in an existing repository keeps working. On Linux the tests run add, commit, checkout, stash, gc and rebase, commits in a submodule, a nested repository and a linked worktree, and gc, commit-graph and a new branch in a submodule. On macOS the tests check that the writes git makes in normal use (the index, objects, refs, a submodule's commit-graph, a linked worktree's index) stay allowed; real git under the macOS rules was measured while the change was designed.
- Some git commands now fail inside the sandbox on purpose: `git config` (including setting `core.hooksPath`), `git sparse-checkout`, `git worktree add` and `git submodule update --init`. On macOS, `git init` and `git clone` into the workspace fail too.
- Before each spawn the sandbox reads the workspace's git layout; an error reading it, other than a missing entry, refuses the command.
- The approval preview's sandbox line states the boundary: `git_metadata=ro` for Seatbelt, `git_metadata=ro(existing)` for Bubblewrap. Approval grants are unchanged: the approval key does not move, so existing grants stay valid.

#### Remaining limits

- Bubblewrap protects only the git metadata that exists when the command starts. A command can still create a new file inside a git directory there, such as a `commondir` (which points git at another directory's configuration) or a `config.worktree` (which adds configuration), and git configuration can make git run commands on the host. It can also create a `hooks` or `info` directory that was absent, or a new repository; `git init` and `git clone` into the workspace work under Bubblewrap.
- Hooks you keep in the working tree through `core.hooksPath` are ordinary workspace files: a sandboxed command can change them on either backend.
- Symbolic links among the git metadata are outside the protection. On macOS, if `.git/hooks`, `.git/info` or `.git/config` is a symbolic link into the workspace, a sandboxed command can write through it to the link's target, and git then uses what it wrote. Bubblewrap instead refuses to start a command when a git metadata entry (`hooks`, `info`, `config`, `config.worktree`, `commondir` or `gitdir`) is a symbolic link, because a read-only mount would protect the target but not the link. A `.git` that is itself a symbolic link is not protected on either backend.
- A git directory kept inside the workspace somewhere other than `.git`, `.git/modules` or `.git/worktrees`, for example one made with `git init --separate-git-dir` into the workspace, is not recognized: its hooks and config stay writable on both backends. Only the `.git` file that points at it is protected.
- On a case-insensitive network or FUSE filesystem, a differently cased spelling can get past the Bubblewrap protection.
- Other files the host runs later, such as a `Makefile`, `package.json` scripts, `.envrc` or CI workflows, are not covered.
