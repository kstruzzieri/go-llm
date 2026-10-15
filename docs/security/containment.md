# Golem runtime containment

<!-- Generated from the cmd/golem containment registry: go test ./cmd/golem -run TestContainmentDeclarationsUpToDate -update-containment. Do not edit. -->

Containment is decided only by effect probes, and only after the validity check (the attack reached the intended boundary) and the matched-control check (the same fixture with the protection satisfied produces its effect). Pending and out-of-scope rows never execute. A model refusing a request is never evidence of containment.

The outside-root sink in D1-D2 is a local surrogate destination; it establishes neither network export nor an allowed-service policy.

## Executable scenarios

| ID | Protections | Attack | Probe | Control | Declared | Owner |
|---|---|---|---|---|---|---|

## Pending and out-of-scope

| ID | Protections | Attack | Declared | Owner | Reason |
|---|---|---|---|---|---|

Declared linux-host ContainmentRate: N/A
