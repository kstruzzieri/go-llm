### Security — Inline-shell guards read common shell option forms (#622)

The egress classifier and the `remote_script_execution` invariant now read an
outer `sh`/`bash`/`dash`/`ksh`/`zsh` started with `-c` among `-e`, `-u`, `-x`,
`-l` and `-i` (separate or clustered), `-o errexit`/`nounset`/`xtrace`,
`-o pipefail` (bash, zsh and ksh only) and `--`, taking the first operand as
the script. `bash -e -c`, `bash -o pipefail -c`, `sh -xc` and `bash -lic`
fetch-into-shell pipelines are now blocked and labeled like `bash -c`, and
`bash -c -e '<script>'`, previously misread with `-e` as the script, is read
correctly. Any other shell option form, including parse-only `-n`, `-o noexec`
and `fish -c`, is now labeled `unknown` 10 instead of `interpreter` 0 and is
never blocked.

`curl … | sh -s -- -y` is now recognized as a stdin sink and blocked like
`| sh`. A quoted assignment-shaped word such as `"TAG=x"` before the fetch or
the sink is the command in shell grammar, not an assignment, so
`"TAG=x" curl … | sh` is no longer blocked as a pipeline whose fetch never
runs; its egress label is unchanged.
