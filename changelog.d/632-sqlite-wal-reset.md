### Security — SQLite 3.51.3: WAL-reset corruption fix (#632)

go-llm now links modernc.org/sqlite v1.46.2, which embeds SQLite 3.51.3.
Unpatched SQLite releases from 3.7.0 through 3.51.2 can corrupt a WAL-mode
database when two or more connections on the same file write or checkpoint
at the same instant (https://www.sqlite.org/wal.html, section 11). go-llm's
stores run in WAL mode, and some deployments meet that trigger, for example
two MCP servers recording chats to one transcript database. SQLite describes
the race as rare. modernc.org/sqlite still embeds older SQLite on
netbsd/amd64 (3.40.0) and on freebsd/386 and freebsd/arm (3.41.2); builds for
those targets do not get the fix. The released golem and go-llm-mcp binaries
(linux, macOS and Windows on amd64 and arm64) all do.

modernc.org/libc moves to v1.70.0 with it: modernc requires a module using
modernc.org/sqlite v1.46.2 to select the libc version that release requires.
If your module requires modernc.org/libc directly, set it to v1.70.0.
