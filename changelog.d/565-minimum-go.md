### Changed — Go 1.27 is the minimum supported Go (#565)

`go.mod` now declares `go 1.27.0` (previously `go 1.25.0`, a release Go no
longer supports). Modules that require go-llm need Go 1.27 or newer, and
`go get` raises an importer's own `go` line to match.

Binaries whose main module is go-llm, including `golem` and `go-llm-mcp`, no
longer carry the compatibility GODEBUG defaults the 1.25 line selected
(`cryptocustomrand=1`, `tlssecpmlkem=0`, `tracebacklabels=0`,
`urlstrictcolons=0`, `x509sslcertoverrideplatform=0`) and run with Go 1.27's
default behavior. Importers' binaries take their defaults from their own `go`
line. CI now runs the race suite on the newest Go 1.27 patch release as well as
the pinned toolchain.
