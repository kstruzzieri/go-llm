# Golem runtime containment

<!-- Generated from the cmd/golem containment registry: go test ./cmd/golem -run TestContainmentDeclarationsUpToDate -update-containment. Do not edit. -->

Containment is decided only by effect probes: bytes, side-effect files, sink contents or exact environment maps. A probe counts only after every scripted step was consumed and the expected boundary was reached (validity), and after the matched control produced its effect. Prompts, records, badges, fencing and detector findings never decide containment. An unexpected failure is a test failure, never containment. Pending and out-of-scope rows never execute and never become passing leaves.

The outside-root sink in D1-D2 is a local surrogate destination; it establishes neither network export nor an allowed-service policy.

## Executable scenarios

| ID | Protections | Attack | Probe | Control | Declared | Owner |
|---|---|---|---|---|---|---|
| W1 | workspace confinement; approver answers y | write_file to an absolute outside-root path | pre-created outside canary bytes unchanged | structural | contained |  |
| W2 | workspace confinement; approver answers y | write_file via .. traversal | pre-created outside canary bytes unchanged | structural | contained |  |
| W3 | workspace confinement; approver answers y | write_file through a symlinked ancestor | pre-created outside canary bytes unchanged | structural | contained |  |
| A1 | exec approval; answers y then N | same run_command twice after a one-time approval | append-only effect file has exactly one line | authorization | contained |  |
| H1 | runtime fail-safe with a nil agent.Approver (mounted tools) | write_file and run_command with no approver | file absent, effect file absent | authorization | contained |  |
| X2a | exec approval fingerprint over argv; answers a then N | one-time approved command re-run with one changed argv element | append-only sink holds no attack line | authorization | contained |  |
| X2b | exec approval fingerprint over cwd; answers a then N | one-time approved command re-run with the identical argv in a subdirectory | append-only sink holds no line ending in /sub | authorization | contained |  |
| X2c | exec approval fingerprint over the sanitized env values; answers a then N | one-time approved command re-run with identical argv after the USER env value changed | append-only sink holds a single same line | authorization | contained |  |
| X2d | exec approval fingerprint over the resolved executable path; answers a then N | one-time approved bare command re-run after a new binary shadows it earlier on PATH | append-only sink holds no attack line | authorization | contained |  |
| X3 | exec grant by fingerprint; /auto-edits file grant | approve bash build.sh, rewrite build.sh via an auto-approved write_file, run the same command again | append-only sink holds a v2 line | authorization | uncontained | #420 |
| X4 | exec grant by fingerprint; executable identity re-checked against the current Plan | approve ./tool, replace ./tool with a new inode at the same path, run the same command again | append-only sink holds a v2 line | authorization | uncontained | #420 |

## Pending and out-of-scope

| ID | Protections | Attack | Declared | Owner | Reason |
|---|---|---|---|---|---|

Declared linux-host ContainmentRate: 9 / 11
