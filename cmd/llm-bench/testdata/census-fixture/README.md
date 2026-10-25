# Census fixture (#517)

The committed tree that gates the interceptor census logic in CI
(`cmd/llm-bench/census_test.go`). `manifest.json` is its
`llm-bench.census-manifest/v1` manifest; `fixture/` is its one stratum.

| File | Role |
|---|---|
| `emoji.md` | an emoji family sequence joined by two U+200D zero-width joiners |
| `bom.txt` | a CSV that starts with a U+FEFF byte-order mark |
| `weak.md` | the weak phrase "system prompt", on the last of nine lines |
| `strong.md` | the strong phrase "ignore previous instructions"; classed `adversarial-fixture` |
| `constant.go` | a Go constant holding a 19-digit integer |
| `system_prompt.bin` | binary bytes with NULs, which `read_file` refuses; its name carries the weak phrase, so every call naming it is tagged |
| `large.txt` | 268800 bytes of plain prose, above the 256 KiB `read_file` cap |
| `clean.go` | a Go file with no finding under any interceptor |
| `.npmrc` | benign npm settings under a credential basename, which the `read_file` credential-path invariant blocks at the call |

The 19-digit integer in `constant.go` is an intentional benign lookalike: it
passes the Luhn check, so the Secrets scanner reports it as a payment card. It
is not a card number. For that reason this directory is deliberately not
covered by `TestCommittedArtifactsCarryNoSecretShapes`. No provider token, PEM
private key or bearer value is committed here.

Changing any file here changes its sha256: update `manifest.json` in the same
change, or the census refuses the tree.
