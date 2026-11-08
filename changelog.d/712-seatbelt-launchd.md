### Security — Seatbelt commands can no longer reach launchd (#712)

A command run under the macOS Seatbelt sandbox inherited a connection to launchd. Through it the command could start (`kickstart`) a launchd job you had already loaded, which then ran outside the sandbox, switch jobs on or off persistently (`enable`, `disable`), and list your launchd domain (`print`), including any environment variables set there, which can hold secrets.

#### Behavior changes

- Seatbelt commands now start through a native launcher with no launchd connection and no exception ports, and close-on-exec by default. Start, enable, disable and list requests from inside the sandbox are refused. The sandbox profile itself is unchanged.

#### Remaining limits

- A launchd job you set up to watch a path inside the workspace (`WatchPaths`, `QueueDirectories`) can still be triggered by a sandboxed write.
- With network allowed, a sandboxed command can connect to Unix-domain sockets at paths the profile lets it resolve: the workspace, the private temp and the reviewed system roots.

#### For embedders

The Seatbelt backend empties the host process's legacy registered Mach port table before each spawn; nothing in go-llm reads it. It does this under Go's `syscall.ForkLock`, so forks from C code in the host that do not take that lock are not serialized against Seatbelt spawns: such a fork running at the same time as a Seatbelt spawn can hand that command the host's launchd connection.
