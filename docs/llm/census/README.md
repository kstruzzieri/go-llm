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
| `i18n-cldr-annotations` | `https://github.com/unicode-org/cldr-json`, shallow sparse checkout of `cldr-json/cldr-annotations-full/**`, `README.md` and `LICENSE` | `91c267402229a59e3ef2774544f001bf959e8809` | Unicode-3.0 | 176 | in the manifest |
| `i18n-gemoji` | `https://github.com/github/gemoji`, shallow sparse checkout of `db/emoji.json`, `README.md` and `LICENSE` | `fadaeaf1f1a9be82b321316a6c5502e43138b2f6` | MIT | 3 | in the manifest |
| `llm-app-claude-cookbooks` | `https://github.com/anthropics/claude-cookbooks`, shallow sparse checkout of `**/*.md`, `**/*.py` and `LICENSE` | `bfe50e59dbb4f5546a0af9ca36ebb77e637c9ce1` | MIT | 195 | in the manifest |
| `llm-app-openai-cookbook` | `https://github.com/openai/openai-cookbook`, shallow sparse checkout of `examples/**/*.md`, `examples/**/*.py` and `LICENSE` | `8495326b6f1b7cec94e7f5e393a4045e328a268e` | MIT | 626 | in the manifest |

Each stratum is reported separately; there is no pooled rate (BC5).

The spec's two third-party strata, LLM-application content (`llm-app`) and
internationalized and emoji text (`i18n-emoji`), are registered one source per
stratum, because a stratum holds one source, commit and license. Each
third-party source is therefore its own stratum, named for its family, and
the block budget is checked per source. A third-party stratum holds exactly
the files of its sparse checkout at the commit, without `.git`.

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

That is 169 files. A blind rater pass over the 110 fired `go-llm` files with a
detector finding (the protocol of the Secrets case table, spec §5.2) agreed
with this assignment on 106 of 110 paths; Keith kept the four disputed
`agent/interceptor/invariants_*_test.go` files as `adversarial-fixture`. The
32 fired files with only a Secrets finding were not rated; the path rules
alone class them. No file of the other five strata is classed. The blind
rater pass over the fired files of the four third-party strata is pending.

## Fetch and reproduce

```sh
ROOT="$HOME/projects/go-llm/census-root-517"   # outside the checkout
mkdir -p "$ROOT/go-llm" "$ROOT/go-src"
git -c core.autocrlf=false -c core.eol=lf archive 92c2483aa8f51c7b419fb346fcc202d5f5867b4c | tar -x -C "$ROOT/go-llm"
TC="$(go env GOMODCACHE)/golang.org/toolchain@v0.0.1-go1.27.1.darwin-arm64"
cp "$TC/LICENSE" "$ROOT/go-src/LICENSE"
(cd "$TC" && find src -type d -name testdata -prune -o -type f -print | tar -c -T - -f -) | tar -x -C "$ROOT/go-src"
# Every git command that writes files passes -c core.autocrlf=false -c core.eol=lf:
# some upstream repositories set `text=auto`, so a host with core.autocrlf=true
# (or Windows) would check out CRLF bytes and the census would refuse on a
# sha256 mismatch.
sparse() {  # sparse <stratum> <repository> <commit> <sparse-checkout pattern>...
  d="$ROOT/$1" repo="$2" commit="$3"; shift 3
  git init -q "$d" &&
  git -C "$d" sparse-checkout set --no-cone "$@" &&
  git -C "$d" fetch -q --depth 1 --filter=blob:none "$repo" "$commit" &&
  git -C "$d" -c core.autocrlf=false -c core.eol=lf checkout -q FETCH_HEAD &&
  rm -rf "$d/.git"   # the census refuses any file the manifest does not list
}
sparse i18n-cldr-annotations https://github.com/unicode-org/cldr-json 91c267402229a59e3ef2774544f001bf959e8809 \
  '/cldr-json/cldr-annotations-full/**' /README.md /LICENSE
sparse i18n-gemoji https://github.com/github/gemoji fadaeaf1f1a9be82b321316a6c5502e43138b2f6 \
  /db/emoji.json /README.md /LICENSE
sparse llm-app-claude-cookbooks https://github.com/anthropics/claude-cookbooks bfe50e59dbb4f5546a0af9ca36ebb77e637c9ce1 \
  '/**/*.md' '/**/*.py' /LICENSE
sparse llm-app-openai-cookbook https://github.com/openai/openai-cookbook 8495326b6f1b7cec94e7f5e393a4045e328a268e \
  '/examples/**/*.md' '/examples/**/*.py' /LICENSE
go run ./cmd/llm-bench -interceptor-census \
  -census-manifest docs/llm/census/manifest.json -census-root "$ROOT" \
  -census-out docs/llm/census
```

`-census-root` holds one subdirectory per stratum name. Before observing
anything, the census checks every listed file's sha256 and refuses any
regular file the manifest does not list, naming the first mismatching path.
Listed nonregular files and either reserved verifier path are refused before
reading content. Each stratum is copied into an isolated workspace under
`/tmp` (canonical `/tmp` or `/private/tmp`), with an 80-byte neutral canonical
path. Copied bytes are checked against the manifest again. The verifier's
scratch write and cleanup affect only this copy; the source may be read-only.
After the file carriers finish, adversarial fixtures are removed from the
copy before search, so they consume neither its output cap nor its benign
denominator. The CLI points `XDG_DATA_HOME` at
a temporary directory for the run, so the loopback MCP server's trust pin
never lands in the user's data directory. The census refuses to run on
Windows, where the exec carriers are unsupported.

`-census-out` receives `census-results.json` (committed) and
`full/observations.jsonl` (gitignored), the full per-observation output, one
JSON observation per line.

## Results

`census-results.json` (`llm-bench.census-results/v1`) holds no file content.
`manifest_sha256` binds the manifest bytes and `full_sha256` the bytes of
`full/observations.jsonl`. The neutral workspace keeps the verifier's findings,
rendered byte counts and truncation boundary independent of the source root.
Both output files, including `full_sha256`, are root-independent. For v1
format compatibility, `fired` still records verifier `bytes` as 0.

The committed results cover all six strata of the manifest.

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
| `search` | `agent/tools` `search` over the benign-only copy, once per registered query per stratum: `func `, `TODO`, `error`, `return`, `import`, `config`, `test`, `http` | workspace |
| `run_command` | `cat ./<path>` through the host exec runtime (`SandboxConfig{}`), approved | workspace |
| `command_tail` | `start_command` of `cat ./<path>`, then `command_tail` with default arguments once the job exits | workspace |
| `verifier` | the session `VerifierSlot` armed with `cat ./<path> .census-verify-missing`, triggered by a `write_file` of an unrelated scratch file | workspace |
| `mcp` | a loopback go-sdk streamable-HTTP server with one tool, `fetch(path)`, returning the file as text, connected through `mcpclient.Connect` | foreign |
| `mixed` | the `read_file` rendering returned with a registered `ContextSet`: one group whose verbatim alternatives are the rendering's line thirds; the fallback is the first third | workspace |

Notes on fidelity:

- The verifier renders a passing check's status only, never its output, so
  the census arms a check that fails (the missing operand makes `cat` exit 1)
  in order to render the file. Its observation is then the failing-check
  rendering: the header, the `command:` and `target:` lines, stderr, and the
  head of stdout within the 4 KiB observation cap. The `target:` line names
  the neutral copy's canonical root at a fixed width, so neither the
  interceptors nor the cap depend on the caller's path.
- `command_tail` renders the job's random handle. A handle that any default
  interceptor flags by itself is redrawn, so findings remain a function of the
  file.
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
- `i18n-cldr-annotations`: Unicode License V3 (Unicode-3.0), `LICENSE` in the
  stratum.
- `i18n-gemoji`, `llm-app-claude-cookbooks` and `llm-app-openai-cookbook`: the
  MIT License, `LICENSE` in each stratum.
