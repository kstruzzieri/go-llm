### Security — Native sandboxes refuse writes to git metadata (#721)

A command run in the Seatbelt (macOS) or Bubblewrap (Linux) sandbox could write the workspace's git hooks and git config. Git runs those later outside the sandbox, on your next commit or checkout, so a sandboxed command, or a test run by post-write verification, could plant code that runs unsandboxed. It could also write `.git/info/exclude` or `.git/info/attributes` to hide a new file from `git status` or a change from `git diff`.

#### Behavior changes

- Both native sandboxes now refuse to change, rename, replace or delete the workspace's git metadata: hooks, anything under `info`, `config`, `config.worktree`, `commondir`, `gitdir`, the `.git` entry itself, and the `.git` file that points a submodule or linked worktree at its git directory. This holds at any depth, for nested repositories and for submodule and linked-worktree git directories, and for verification commands as well as commands. Ordinary git work in an existing repository (add, commit, branch, rebase, stash, gc) keeps working, in submodules, nested repositories and linked worktrees too.
- Seatbelt also refuses creating any of this during the run, in any letter case, including a new repository.
- Some git commands now fail inside the sandbox on purpose: `git config` (including setting `core.hooksPath`), `git sparse-checkout`, `git worktree add` and `git submodule update --init`. On macOS, `git init` and `git clone` into the workspace fail too.
- Before each spawn the sandbox reads the workspace's git layout; an error reading it, other than a missing entry, refuses the command.
- The approval preview's sandbox line states the boundary: `git_metadata=ro` for Seatbelt, `git_metadata=ro(existing)` for Bubblewrap. Approval grants are unchanged: the approval key does not move, so existing grants stay valid.

#### Remaining limits

- Bubblewrap protects the git metadata that exists when the command starts. It does not refuse a new file inside a git directory (a `commondir`, a `config.worktree`, or `hooks` or `info` when absent) or a new repository; `git init` and `git clone` into the workspace work there.
- Bubblewrap refuses to start a command when a git metadata entry (`hooks`, `info`, `config`, `config.worktree`, `commondir` or `gitdir`) is a symbolic link, because a read-only mount would protect the link's target but not the link. Keep hooks in the working tree with `core.hooksPath` instead; those are ordinary workspace files and stay writable.
- On a case-insensitive network or FUSE filesystem, a differently cased spelling can get past the Bubblewrap protection.
- Other files the host runs later, such as a `Makefile`, `package.json` scripts, `.envrc` or CI workflows, are not covered.
