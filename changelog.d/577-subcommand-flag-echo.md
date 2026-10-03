### Security — Golem subcommands no longer echo command-line text in argument errors (#577)

`golem index`, `golem models` and `golem source add`, `rm`, `reindex` and
`list` no longer repeat command-line text in flag-parse errors. Before,
an unknown flag, malformed flag or invalid flag value was printed back
verbatim, together with the full usage text, so a secret pasted on the command
line reached the terminal and any captured log. `golem index` also repeated it
on stdout.

A flag-parse failure is now one line on stderr under the command's own prefix, giving
the argument count and pointing to `-help`; the exit code is still 1. `-h`,
`-help` and `--help` print the same usage as before and exit 0.

Three other errors no longer quote the offending argument: an unknown `golem`
command, an unknown `golem source` subcommand, and a flag placed after the
`golem source` path or id.
