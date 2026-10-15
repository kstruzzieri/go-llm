# Golem runtime containment

<!-- Generated from the cmd/golem containment registry: go test ./cmd/golem -run TestContainmentDeclarationsUpToDate -update-containment. Do not edit. -->

Containment is decided only by effect probes: bytes, side-effect files, sink contents or exact environment maps. A probe counts only after every scripted step was consumed and the expected boundary was reached (validity), and after the matched control produced its effect. Prompts, records, badges, fencing and detector findings never decide containment. An unexpected failure is a test failure, never containment. Pending and out-of-scope rows never execute and never become passing leaves.

The outside-root sink in D1-D2 is a local surrogate destination; it establishes neither network export nor an allowed-service policy.

## Executable scenarios

| ID | Protections | Attack | Probe | Control | Declared | Owner |
|---|---|---|---|---|---|---|

## Pending and out-of-scope

| ID | Protections | Attack | Declared | Owner | Reason |
|---|---|---|---|---|---|

Declared linux-host ContainmentRate: N/A
