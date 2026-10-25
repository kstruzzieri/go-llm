# Interceptor census (#517)

The census measures how often the detector interceptors (`ZeroWidth`,
`Encoding`, `Typoglycemia`) and `Secrets` fire on benign real-world content as
Golem's production tools render it (spec §6.2). It needs no model: a scripted
caller drives the real `agent.Orchestrator` under `interceptor.Defaults()`,
and every interceptor runs on every observation, so each arm's outcome is
derived later from the per-interceptor findings.

`manifest.json` (`llm-bench.census-manifest/v1`) registers the corpus: per
stratum its source, commit, license, and every file's path and sha256. It
holds no file content. The trees themselves are fetched locally and are never
committed; excerpts of them appear only inside the PR2 traces, each with its
license notice.

## Strata

| Stratum | Source | Commit | License | Files | Status |
|---|---|---|---|---|---|
| `go-llm` | this repository, `git archive` of the PR1 base commit | `92c2483aa8f51c7b419fb346fcc202d5f5867b4c` | Apache-2.0 | 1970 | in the manifest |
| `go-src` | `golang.org/toolchain@v0.0.1-go1.27.1.darwin-arm64` from the Go module cache: `LICENSE` and `src/`, every `testdata` directory excluded | `go1.27.1` | BSD-3-Clause | 7787 | in the manifest |
| `llm-app` | two permissively licensed LLM-application repositories | | | | pending: needs Keith's approval to download |
| `i18n-emoji` | two permissively licensed emoji and i18n sources | | | | pending: needs Keith's approval to download |

Each stratum is reported separately; there is no pooled rate (BC5).

The `go-llm` stratum is every tracked regular file at the base commit (the
`DEVELOPMENT_PRINCIPLES.md` symlink is not a regular file and is not listed).
The `go-src` tree is taken from the darwin-arm64 toolchain module; the `src/`
of another platform's toolchain module is expected to be identical, and the
census refuses it if it is not.

### `adversarial-fixture` class (BC5)

Deliberate injection and Unicode test data is classed by its documented role,
assigned before any scanning, and reported apart from the false-block
denominator. In `go-llm` these are:

- `agent/interceptor/*_test.go` (the interceptor package's own tests);
- `agent/hardening_contract_test.go`;
- `agent/testdata/hardening/**`;
- `docs/llm/traces/injection/**`.

That is 169 files. The independent rater pass over this assignment (the
protocol of the Secrets case table, spec §5.2) is pending. No `go-src` file is
classed.

## Fetch and reproduce

```sh
ROOT="$HOME/projects/go-llm/census-root-517"   # outside the checkout
mkdir -p "$ROOT/go-llm" "$ROOT/go-src"
git archive 92c2483aa8f51c7b419fb346fcc202d5f5867b4c | tar -x -C "$ROOT/go-llm"
TC="$(go env GOMODCACHE)/golang.org/toolchain@v0.0.1-go1.27.1.darwin-arm64"
cp "$TC/LICENSE" "$ROOT/go-src/LICENSE"
(cd "$TC" && find src -type d -name testdata -prune -o -type f -print | tar -c -T - -f -) | tar -x -C "$ROOT/go-src"
go run ./cmd/llm-bench -interceptor-census \
  -census-manifest docs/llm/census/manifest.json -census-root "$ROOT" \
  -census-out docs/llm/census
```

`-census-root` holds one subdirectory per stratum name. Before observing
anything, the census checks every listed file's sha256 and refuses any
regular file the manifest does not list, naming the first mismatching path.
The verifier carrier writes and then removes `.census-verify-scratch` in each
stratum root, so the root must be writable. The CLI points `XDG_DATA_HOME` at
a temporary directory for the run, so the loopback MCP server's trust pin
never lands in the user's data directory. The census refuses to run on
Windows, where the exec carriers are unsupported.

`-census-out` receives `census-results.json` (committed) and
`full/observations.jsonl` (gitignored), the full per-observation output, one
JSON observation per line.

## Results

`census-results.json` (`llm-bench.census-results/v1`) holds no file content.
`manifest_sha256` binds the manifest bytes and `full_sha256` the bytes of
`full/observations.jsonl`. The verifier observation's byte count includes the
absolute stratum root, so `full_sha256` holds only for the same root path, and
`fired` records every verifier row's `bytes` as 0; the rest of
`census-results.json` does not depend on the root.

The committed results cover the two local strata, `go-llm` and `go-src`. They
are regenerated, with every count, when the pending strata are approved and
added to the manifest.

Each arm is a chain filtered from `interceptor.Defaults()` in library order,
without the canary: `golem-default-guards` (G: `invariants`, `egress`,
`child_scope_denials`), `guards+detectors` (G+D, adding `zero_width`,
`encoding`, `typoglycemia`), `guards+secrets` (G+S, adding `secrets`) and
`guards+detectors+secrets` (G+D+S). The census records every interceptor's
findings once, under G+D+S, and derives each arm's outcome on each
observation from its members' findings:

| Outcome | Rule |
|---|---|
| `call_blocked` | a member blocks the scripted call, at model output or at the tool-call hook |
| `blocked` | a member blocks the observation (message or alternative); the tags it carried are suppressed (F6) |
| `trailer` | members tag the observation; one trailer per distinct (interceptor, rule) |
| `identical` | none of the above; tags on the call itself are telemetry and add no trailer (F5) |
| `unobserved` | a non-member blocked the call, so the hooks this arm would have reached never ran |

`aggregates` has one row per (stratum, carrier, arm), sorted by those three
names. Every observation counts in exactly one of `refused` (a tool refusal,
never a denominator), `adversarial_fixture` (a file classed
`adversarial-fixture`, excluded from every other count), `unobserved`, or
`observations`, which splits into `trailer`, `blocked`, `call_blocked` and the
identical remainder. `files` is the number of files behind `observations`
(0 for `search`, whose observations are per query). `suppressed` counts the
blocked observations that carried at least one member ingress tag, and
`trailer_bytes` sums the trailer bytes the arm adds. For mixed assembly the
trailer bytes count every distinct pair across the alternatives, an upper
bound on what any one selected alternative gains.

`fired` lists every observation with at least one finding, including
adversarial fixtures, sorted by stratum, carrier and path, with its findings
and no content. The S-over-D interaction (F6) is the set of rows whose
outcome is `trailer` under G+D and `blocked` under G+D+S.

## Carriers

Each observation is one Orchestrator run. Call 1 is the carrier's scripted
tool call (two calls for `command_tail`); the last turn ends the run.

| Carrier | Production path | Origin |
|---|---|---|
| `read_file` | `agent/tools` `read_file` (256 KiB cap with marker, binary refusal) | workspace |
| `search` | `agent/tools` `search`, once per registered query per stratum: `func `, `TODO`, `error`, `return`, `import`, `config`, `test`, `http` | workspace |
| `run_command` | `cat <path>` through the host exec runtime (`SandboxConfig{}`), approved | workspace |
| `command_tail` | `start_command` of `cat <path>`, then `command_tail` with default arguments once the job exits | workspace |
| `verifier` | the session `VerifierSlot` armed with `cat <path> .census-verify-missing`, triggered by a `write_file` of an unrelated scratch file | workspace |
| `mcp` | a loopback go-sdk streamable-HTTP server with one tool, `fetch(path)`, returning the file as text, connected through `mcpclient.Connect` | foreign |
| `mixed` | the `read_file` rendering returned with a registered `ContextSet`: one group whose verbatim alternatives are the rendering's line thirds; the fallback is the first third | workspace |

Notes on fidelity:

- The verifier renders a passing check's status only, never its output, so
  the census arms a check that fails (the missing operand makes `cat` exit 1)
  in order to render the file. Its observation is then the failing-check
  rendering: the header, the `command:` and `target:` lines, stderr, and the
  head of stdout within the 4 KiB observation cap. The `target:` line names
  the absolute stratum root, so verifier bytes depend on where the root is
  (`fired` records them as 0).
- `command_tail` renders the job's random handle. A handle whose hex holds a
  digit run that Secrets would flag by itself is redrawn, so findings remain a
  function of the file.
- Findings cover the scripted tool calls (for example the `read_file`
  credential-path guard, or a detector tagging a path) and the carrier's final
  observation; the `command_tail` start result and the verifier's triggering
  write result are not part of the observation.
- A tool refusal, such as `read_file` on a binary file, is recorded as a
  non-observation with no bytes and no findings.
- `retrieve` is excluded (spec §6.2): it needs a built index, and the indexer
  scrubs secret-shaped content first. Its chunk text is covered by
  `read_file` and `search` over the same files.

## Licenses

- `go-llm`: Apache License 2.0, `LICENSE` at the base commit.
- `go-src`: the Go BSD-style license (BSD-3-Clause), `LICENSE` in the stratum.
