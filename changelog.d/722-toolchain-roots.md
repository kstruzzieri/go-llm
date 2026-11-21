### Added — Declared read-only toolchain roots for the macOS Seatbelt sandbox (#722)

A host can now declare folders outside the workspace that Seatbelt commands may read and run but never write, so a Go, git or Python toolchain can run under `-sandbox seatbelt`.

#### Library

- `agent/tools`: `SandboxConfig.ReadOnlyRoots`, `SandboxConfig.DeveloperDirectory`, `SandboxConfig.Env` (`[]SandboxEnv`, a closed table: `GOMODCACHE` inside a declared root, `GOPROXY=off`, `GOTOOLCHAIN=local`, `GOFLAGS=-buildvcs=false`) and `SandboxConfig.TempEnv` (`GOCACHE` under each command's private temp).
- `SandboxConfig.Clone` copies every slice; `SandboxConfig.EffectiveEnv` lists every variable the backend adds; `CanonicalToolchainRoot` returns the canonical spelling a root must use; `ActiveDeveloperDirectory` (macOS) reads the system's developer directory.
- `golem.CommandPermissions` gains `ReadOnlyRoots`, `DeveloperDirectory` and `Env`; the approval preview shows them.
- Bubblewrap and the host runtime refuse all four fields with `ErrSandboxPolicy`. The exec constructors now wrap `ErrSandboxPolicy` too.

#### Golem

- New flags `-sandbox-read-root`, `-sandbox-env NAME=VALUE`, `-sandbox-temp-env NAME` and `-sandbox-developer-dir`, with a Go recipe in `docs/golem.md`.

#### Limits

- A declared root is a path, not a guarded copy: a process outside Golem can change its contents, so declare only trusted folders.
- Admission refuses specific folders (home, the temp folder, broad system folders, credential folders, go-llm's own stores). That is not a proof that an admitted root holds nothing sensitive.
- Roots must be canonical spellings; a changed symlink means a restart and a new approval.
- The developer-directory link-ownership check is not the confinement boundary; the declared, admitted folder is. `go.work` workspaces with modules outside the workspace and cgo are not supported.
